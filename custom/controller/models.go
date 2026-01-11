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

	if common.DebugEnabled {
		var channelNames []string
		for _, ch := range channels {
			channelNames = append(channelNames, fmt.Sprintf("%s(group=%s,models=%s)", ch.Name, ch.Group, ch.Models))
		}
		var modelNames []string
		for m := range aggregatedModels {
			modelNames = append(modelNames, m)
		}
		common.SysLog(fmt.Sprintf("[GetModels] 用户分组: %s, 匹配渠道: %v, 聚合模型: %v",
			tokenGroup, channelNames, modelNames))
	}

	// 依次请求各渠道上游模型列表，按优先级合并（同名模型保留第一个渠道的结果）
	mergedUpstreamModels := make(map[string]interface{})
	successCount := 0
	var lastErr error
	for _, ch := range channels {
		resp, err := proxyGetModelsRequest(ch, tokenKey)
		if err != nil {
			lastErr = err
			common.SysLog(fmt.Sprintf("[GetModels] 渠道[%d]透传请求失败: %s", ch.Id, err.Error()))
			continue
		}

		upstreamBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			common.SysLog(fmt.Sprintf("[GetModels] 渠道[%d]读取上游响应失败: %s", ch.Id, err.Error()))
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("status=%d", resp.StatusCode)
			common.SysLog(fmt.Sprintf("[GetModels] 渠道[%d]上游返回非200: %d, 响应: %s", ch.Id, resp.StatusCode, string(upstreamBody[:min(len(upstreamBody), 500)])))
			continue
		}

		var upstreamModels map[string]interface{}
		if err := json.Unmarshal(upstreamBody, &upstreamModels); err != nil {
			lastErr = err
			common.SysLog(fmt.Sprintf("[GetModels] 渠道[%d]解析上游响应失败: %s, 原始响应: %s", ch.Id, err.Error(), string(upstreamBody[:min(len(upstreamBody), 500)])))
			continue
		}

		for modelName, modelInfo := range upstreamModels {
			if _, exists := mergedUpstreamModels[modelName]; exists {
				continue
			}
			mergedUpstreamModels[modelName] = modelInfo
		}
		successCount++
	}

	if successCount == 0 {
		errMsg := "所有渠道上游请求失败"
		if lastErr != nil {
			errMsg = fmt.Sprintf("%s: %s", errMsg, lastErr.Error())
		}
		c.JSON(http.StatusBadGateway, gin.H{
			"success": false,
			"message": errMsg,
		})
		return
	}

	// 添加调试日志：上游返回的模型列表
	var upstreamModelNames []string
	for modelName := range mergedUpstreamModels {
		upstreamModelNames = append(upstreamModelNames, modelName)
	}
	common.SysLog(fmt.Sprintf("[GetModels] 上游返回模型数量: %d, 模型列表: %v", len(mergedUpstreamModels), upstreamModelNames))

	// 添加调试日志：渠道模型集合
	var channelModelNames []string
	for modelName := range aggregatedModels {
		channelModelNames = append(channelModelNames, modelName)
	}
	common.SysLog(fmt.Sprintf("[GetModels] 渠道允许模型数量: %d, 模型列表: %v", len(aggregatedModels), channelModelNames))

	// 过滤模型列表（使用聚合后的渠道模型）
	filteredModels := filterModelsWithAggregated(mergedUpstreamModels, aggregatedModels, token)

	// 添加调试日志：过滤后的模型列表
	var filteredModelNames []string
	for modelName := range filteredModels {
		filteredModelNames = append(filteredModelNames, modelName)
	}
	common.SysLog(fmt.Sprintf("[GetModels] 过滤后模型数量: %d, 模型列表: %v", len(filteredModels), filteredModelNames))

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
	groupPattern := "%," + group + ",%"
	err := model.DB.Where("status = ?", 1).
		Where(groupCondition, groupPattern).
		Where(tagCondition, "%bugment%").
		Order("priority DESC").
		Find(&channels).Error

	// 始终输出日志以便排查问题
	common.SysLog(fmt.Sprintf("[GetModels] 查询分组 %s 的渠道, SQL条件: %s, 参数: %s, 查询结果数量: %d",
		group, groupCondition, groupPattern, len(channels)))
	for i, ch := range channels {
		common.SysLog(fmt.Sprintf("[GetModels] 渠道[%d]: id=%d, name=%s, group=%s, tag=%v, models=%s",
			i, ch.Id, ch.Name, ch.Group, ch.Tag, ch.Models))
	}

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
