package controller

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

var customUpstreamHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
}

func requireBearerToken(c *gin.Context) (string, bool) {
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "No Authorization header",
		})
		return "", false
	}

	parts := strings.Fields(authHeader)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "Invalid Bearer token",
		})
		return "", false
	}

	return strings.TrimPrefix(parts[1], "sk-"), true
}

func proxyUpstreamRequest(channel *model.Channel, method string, path string) (*http.Response, error) {
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("渠道 Base URL 为空")
	}

	baseURL = strings.TrimSuffix(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/chat-stream")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	upstreamURL := baseURL + path

	req, err := http.NewRequest(method, upstreamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+channel.Key)
	req.Header.Set("Content-Type", "application/json")

	return customUpstreamHTTPClient.Do(req)
}

func listBugmentChannelsByGroup(group string) ([]*model.Channel, error) {
	var groupCondition string
	groupCol := "`group`"
	tagCol := "`tag`"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
		tagCol = `"tag"`
	}

	if common.UsingMySQL {
		groupCondition = fmt.Sprintf("CONCAT(',', %s, ',') LIKE ?", groupCol)
	} else {
		groupCondition = fmt.Sprintf("(',' || %s || ',') LIKE ?", groupCol)
	}
	tagCondition := fmt.Sprintf("LOWER(%s) LIKE ?", tagCol)
	groupPattern := "%," + group + ",%"

	var channels []*model.Channel
	err := model.DB.Where("status = ?", 1).
		Where(groupCondition, groupPattern).
		Where(tagCondition, "%bugment%").
		Order("priority DESC").
		Find(&channels).Error

	if common.DebugEnabled {
		common.SysLog(fmt.Sprintf("[BugmentChannels] 查询分组 %s 的渠道, SQL条件: %s, 参数: %s, 查询结果数量: %d",
			group, groupCondition, groupPattern, len(channels)))
		for i, ch := range channels {
			common.SysLog(fmt.Sprintf("[BugmentChannels] 渠道[%d]: id=%d, name=%s, group=%s, tag=%v, models=%s",
				i, ch.Id, ch.Name, ch.Group, ch.Tag, ch.Models))
		}
	}

	if err != nil {
		return nil, fmt.Errorf("查询渠道失败: %w", err)
	}

	var matchedChannels []*model.Channel
	for _, ch := range channels {
		if isBugmentChannel(ch) {
			matchedChannels = append(matchedChannels, ch)
		}
	}

	return matchedChannels, nil
}
