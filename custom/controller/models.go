package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// GetModels 获取模型列表（透传到上游 Bugment 渠道，并根据渠道和 Token 配置过滤）
// GET /usage/api/get-models
// 流程：上游响应 → 检查渠道开放模型 → 检查 Token 模型限制 → 返回过滤后的数据
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

	// 查找该 Group 下的所有 Bugment 渠道
	channels, err := getBugmentChannelsByGroup(tokenGroup)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": fmt.Sprintf("此接口仅支持 bugment 渠道: %s", err.Error()),
		})
		return
	}

	// 聚合所有渠道的模型列表（取并集）
	aggregatedModels := aggregateChannelModels(channels)

	// 使用优先级最高的渠道透传请求到上游
	primaryChannel := channels[0]
	resp, err := proxyGetModelsRequest(primaryChannel, tokenKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("透传请求失败: %s", err.Error()),
		})
		return
	}
	defer resp.Body.Close()

	// 读取上游响应体
	upstreamBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": fmt.Sprintf("读取上游响应失败: %s", err.Error()),
		})
		return
	}

	// 如果上游返回非 200，直接透传
	if resp.StatusCode != http.StatusOK {
		for key, values := range resp.Header {
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
		c.Status(resp.StatusCode)
		c.Writer.Write(upstreamBody)
		return
	}

	// 解析上游模型列表
	var upstreamModels map[string]interface{}
	if err := json.Unmarshal(upstreamBody, &upstreamModels); err != nil {
		// 解析失败，直接透传原始响应
		c.Header("Content-Type", "application/json")
		c.Status(resp.StatusCode)
		c.Writer.Write(upstreamBody)
		return
	}

	// 过滤模型列表（使用聚合后的渠道模型）
	filteredModels := filterModelsWithAggregated(upstreamModels, aggregatedModels, token)

	// 返回过滤后的模型列表
	c.Header("Content-Type", "application/json")
	c.JSON(http.StatusOK, filteredModels)
}

// aggregateChannelModels 聚合多个渠道的模型列表（取并集）
func aggregateChannelModels(channels []*model.Channel) map[string]bool {
	modelSet := make(map[string]bool)
	for _, ch := range channels {
		for _, m := range ch.GetModels() {
			m = strings.TrimSpace(m)
			if m != "" {
				modelSet[m] = true
			}
		}
	}
	return modelSet
}

// filterModelsWithAggregated 根据聚合的渠道模型和 Token 限制过滤模型列表
// 过滤规则：
// 1. 聚合渠道模型（所有渠道模型的并集）：用户分组下所有渠道开放的模型
// 2. Token 模型限制（token.ModelLimits）：用户 Token 允许使用的模型（仅当 ModelLimitsEnabled=true 时生效）
// 返回：同时满足两个条件的模型
func filterModelsWithAggregated(upstreamModels map[string]interface{}, channelModelSet map[string]bool, token *model.Token) map[string]interface{} {
	// 获取 Token 模型限制列表
	tokenModelSet := make(map[string]bool)
	tokenLimitsEnabled := token.ModelLimitsEnabled && token.ModelLimits != ""
	if tokenLimitsEnabled {
		tokenModels := strings.Split(token.ModelLimits, ",")
		for _, m := range tokenModels {
			m = strings.TrimSpace(m)
			if m != "" {
				tokenModelSet[m] = true
			}
		}
	}

	// 过滤模型
	filteredModels := make(map[string]interface{})
	for modelName, modelInfo := range upstreamModels {
		// 检查渠道是否开放该模型（如果渠道没有配置模型列表，则允许所有）
		if len(channelModelSet) > 0 && !channelModelSet[modelName] {
			continue
		}

		// 检查 Token 是否允许该模型（如果 Token 没有启用模型限制，则允许所有）
		if tokenLimitsEnabled && !tokenModelSet[modelName] {
			continue
		}

		// 通过所有检查，保留该模型
		filteredModels[modelName] = modelInfo
	}

	return filteredModels
}

// getBugmentChannelsByGroup 根据 Group 获取所有标签包含 bugment 的渠道
func getBugmentChannelsByGroup(group string) ([]*model.Channel, error) {
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

	return matchedChannels, nil
}

// proxyGetModelsRequest 透传获取模型列表请求到上游
func proxyGetModelsRequest(channel *model.Channel, originalToken string) (*http.Response, error) {
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("渠道 Base URL 为空")
	}

	// 清理 baseURL：移除末尾的斜杠和 /chat-stream 路径
	baseURL = strings.TrimSuffix(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/chat-stream")

	// 构建上游请求 URL
	upstreamURL := fmt.Sprintf("%s/usage/api/get-models", baseURL)

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

