package controller

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// GetModels 获取模型列表（透传到上游 Bugment 渠道）
// GET /usage/api/get-models
func GetModels(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "No Authorization header",
		})
		return
	}

	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "Invalid Bearer token",
		})
		return
	}
	tokenKey := parts[1]

	// 获取 Token 信息
	token, err := model.GetTokenByKey(strings.TrimPrefix(tokenKey, "sk-"), true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	// 获取 Token 关联的 Group
	tokenGroup := token.Group
	if tokenGroup == "" {
		tokenGroup = "default"
	}

	// 查找该 Group 下的 Bugment 渠道
	channel, err := getBugmentChannelByGroup(tokenGroup)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": fmt.Sprintf("此接口仅支持 bugment 渠道: %s", err.Error()),
		})
		return
	}

	// 透传请求到上游
	resp, err := proxyGetModelsRequest(channel, tokenKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("透传请求失败: %s", err.Error()),
		})
		return
	}
	defer resp.Body.Close()

	// 复制上游响应头
	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	// 返回上游响应
	c.Status(resp.StatusCode)
	io.Copy(c.Writer, resp.Body)
}

// getBugmentChannelByGroup 根据 Group 获取标签包含 bugment 的渠道
func getBugmentChannelByGroup(group string) (*model.Channel, error) {
	// 构建兼容不同数据库的 group 查询条件
	var groupCondition string
	groupCol := "`group`"
	tagCol := "`tag`"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
		tagCol = `"tag"`
	}

	if common.UsingMySQL {
		// MySQL: CONCAT(',', group, ',') LIKE '%,default,%'
		groupCondition = fmt.Sprintf("CONCAT(',', %s, ',') LIKE ?", groupCol)
	} else {
		// SQLite, PostgreSQL: (',' || group || ',') LIKE '%,default,%'
		groupCondition = fmt.Sprintf("(',' || %s || ',') LIKE ?", groupCol)
	}

	// 构建标签查询条件（不区分大小写）
	tagCondition := fmt.Sprintf("LOWER(%s) LIKE ?", tagCol)

	// 查询该 Group 下所有标签包含 bugment 的渠道
	var channels []*model.Channel
	err := model.DB.Where("status = ?", 1).
		Where(groupCondition, "%,"+group+",%").
		Where(tagCondition, "%bugment%").
		Order("priority DESC").
		Find(&channels).Error

	if err != nil {
		return nil, fmt.Errorf("查询渠道失败: %w", err)
	}

	// 进一步过滤，确保标签包含 bugment（双重验证）
	var matchedChannels []*model.Channel
	for _, ch := range channels {
		if isBugmentChannel(ch) {
			matchedChannels = append(matchedChannels, ch)
		}
	}

	if len(matchedChannels) == 0 {
		return nil, fmt.Errorf("分组 %s 下没有标签包含 bugment 的可用渠道", group)
	}

	return matchedChannels[0], nil
}

// proxyGetModelsRequest 透传获取模型列表请求到上游
func proxyGetModelsRequest(channel *model.Channel, originalToken string) (*http.Response, error) {
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("渠道 Base URL 为空")
	}

	// 构建上游请求 URL
	upstreamURL := fmt.Sprintf("%s/usage/api/get-models", strings.TrimSuffix(baseURL, "/"))

	// 创建请求
	req, err := http.NewRequest(http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 使用渠道的 Key 作为上游认证
	req.Header.Set("Authorization", "Bearer "+channel.Key)
	req.Header.Set("Content-Type", "application/json")

	// 发送请求
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	return client.Do(req)
}

