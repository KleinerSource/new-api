package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// GetTokenBalance 获取 Token 余额信息
// GET /usage/api/balance
func GetTokenBalance(c *gin.Context) {
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

	// 强制从数据库读取，确保余额数据准确（不使用 Redis 缓存）
	token, err := model.GetTokenByKey(strings.TrimPrefix(tokenKey, "sk-"), true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	var remainQuota int
	var remainAmount float64

	if token.UnlimitedQuota {
		// unlimited_quota 为 true，token 无限额度，显示用户总金额
		userQuota, err := model.GetUserQuota(token.UserId, true)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
		remainQuota = userQuota
		remainAmount = float64(userQuota) / common.QuotaPerUnit
	} else {
		// unlimited_quota 为 false，token 有限额度，显示 token 的 remain_quota
		remainQuota = token.RemainQuota
		remainAmount = float64(token.RemainQuota) / common.QuotaPerUnit
	}

	// 获取状态文本
	statusText := getTokenStatusText(token.Status)

	// 构建响应数据
	responseData := gin.H{
		"name":          token.Name,
		"remain_quota":  remainQuota,
		"remain_amount": remainAmount,
		"unlimited":     token.UnlimitedQuota,
		"expired_time":  token.ExpiredTime,
		"status":        token.Status,
		"status_text":   statusText,
	}

	// 尝试从上游获取 login_token
	tokenGroup := token.Group
	if tokenGroup == "" {
		tokenGroup = "default"
	}

	loginToken := getUpstreamLoginToken(c, tokenGroup, tokenKey)
	if loginToken != nil {
		responseData["login_token"] = loginToken
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    responseData,
	})
}

// getUpstreamLoginToken 从上游 Bugment 渠道获取 login_token
func getUpstreamLoginToken(c *gin.Context, tokenGroup string, tokenKey string) interface{} {
	// 获取 Bugment 渠道
	channel, err := getBugmentChannelByGroup(tokenGroup)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("获取 Bugment 渠道失败: %s", err.Error()))
		return nil
	}

	// 验证渠道标签是否包含 bugment
	if !isBugmentChannel(channel) {
		logger.LogWarn(c, fmt.Sprintf("分组 %s 下的渠道标签不包含 bugment", tokenGroup))
		return nil
	}

	// 透传请求到上游
	resp, err := proxyGetBalanceRequest(channel, tokenKey)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("透传 balance 请求失败: %s", err.Error()))
		return nil
	}
	defer resp.Body.Close()

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("读取上游响应失败: %s", err.Error()))
		return nil
	}

	// 解析上游响应
	var upstreamResp struct {
		Success bool `json:"success"`
		Data    struct {
			LoginToken interface{} `json:"login_token"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &upstreamResp); err != nil {
		logger.LogWarn(c, fmt.Sprintf("解析上游响应失败: %s", err.Error()))
		return nil
	}

	if !upstreamResp.Success {
		logger.LogWarn(c, "上游返回失败状态")
		return nil
	}

	return upstreamResp.Data.LoginToken
}

// proxyGetBalanceRequest 透传获取余额请求到上游
func proxyGetBalanceRequest(channel *model.Channel, originalToken string) (*http.Response, error) {
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("渠道 Base URL 为空")
	}

	// 清理 baseURL：移除末尾的斜杠和 /chat-stream 路径
	baseURL = strings.TrimSuffix(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/chat-stream")

	// 构建上游请求 URL
	upstreamURL := fmt.Sprintf("%s/usage/api/balance", baseURL)

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

// getTokenStatusText 根据状态码返回状态文本
func getTokenStatusText(status int) string {
	switch status {
	case common.TokenStatusEnabled:
		return "enabled"
	case common.TokenStatusDisabled:
		return "disabled"
	case common.TokenStatusExpired:
		return "expired"
	case common.TokenStatusExhausted:
		return "exhausted"
	default:
		return "unknown"
	}
}

