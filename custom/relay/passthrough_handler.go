package relay

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// PassthroughEndpoint 透传端点路径
const PassthroughEndpoint = "/chat-stream"

// PassthroughHelperWithUsage 传透模式处理器（带 usage 和内容提取）
// 自定义 URL 构建逻辑：始终使用 base_url + /chat-stream，不依赖 Adaptor.GetRequestURL()
func PassthroughHelperWithUsage(c *gin.Context, info *relaycommon.RelayInfo) (*PassthroughResult, *types.NewAPIError) {
	info.InitChannelMeta(c)

	adaptor := relay.GetAdaptor(info.ApiType)
	if adaptor == nil {
		return nil, types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)

	body, err := common.GetRequestBody(c)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	if common.DebugEnabled {
		logger.LogDebug(c, fmt.Sprintf("passthrough request body: %s", string(body)))
	}

	// 使用自定义请求发送逻辑，绕过 Adaptor.GetRequestURL()
	resp, err := doPassthroughRequestWithCustomURL(c, adaptor, info, body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	var httpResp *http.Response
	if resp != nil {
		httpResp = resp
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")

		if httpResp.StatusCode != http.StatusOK {
			newApiErr := service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			return nil, newApiErr
		}
	}

	return passthroughResponseWithUsage(c, httpResp, info)
}

// doPassthroughRequestWithCustomURL 使用自定义 URL 构建逻辑发送透传请求
// URL 构建规则：base_url + /chat-stream
// 这样可以让渠道测试使用标准的 /v1/chat/completions，而透传请求使用 /chat-stream
func doPassthroughRequestWithCustomURL(c *gin.Context, adaptor channel.Adaptor, info *relaycommon.RelayInfo, body []byte) (*http.Response, error) {
	// 1. 构建 URL：base_url + /chat-stream
	baseURL := strings.TrimSuffix(info.ChannelBaseUrl, "/")
	fullRequestURL := baseURL + PassthroughEndpoint

	if common.DebugEnabled {
		logger.LogDebug(c, fmt.Sprintf("passthrough custom URL: %s (base: %s)", fullRequestURL, info.ChannelBaseUrl))
	}

	// 2. 创建 HTTP 请求
	req, err := http.NewRequest(c.Request.Method, fullRequestURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}

	// 3. 处理请求头覆盖（支持渠道配置的 headers_override）
	headers := req.Header
	if info.HeadersOverride != nil {
		for k, v := range info.HeadersOverride {
			if str, ok := v.(string); ok {
				// 替换支持的变量
				if strings.Contains(str, "{api_key}") {
					str = strings.ReplaceAll(str, "{api_key}", info.ApiKey)
				}
				headers.Set(k, str)
			}
		}
	}

	// 4. 使用 Adaptor 设置请求头（保留认证逻辑）
	if err := adaptor.SetupRequestHeader(c, &headers, info); err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}

	// 5. 发送请求
	return channel.DoRequest(c, req, info)
}

// passthroughResponseWithUsage 透传响应并提取 usage 和内容信息
func passthroughResponseWithUsage(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*PassthroughResult, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	for key, values := range resp.Header {
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}

	if info.IsStream {
		return passthroughStreamResponseWithUsage(c, resp, info)
	}

	return passthroughNonStreamResponseWithUsage(c, resp, info)
}

// passthroughStreamResponseWithUsage 流式响应透传（带 usage 和内容提取）
func passthroughStreamResponseWithUsage(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*PassthroughResult, *types.NewAPIError) {
	helper.SetEventStreamHeaders(c)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logger.LogWarn(c, "streaming not supported, falling back to buffered response")
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
		}
		c.Writer.Write(responseBody)
		return GetPassthroughResult(responseBody, true), nil
	}

	var allData bytes.Buffer
	buffer := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			info.SetFirstResponseTime()
			if _, writeErr := c.Writer.Write(buffer[:n]); writeErr != nil {
				logger.LogError(c, "passthrough stream write error: "+writeErr.Error())
				break
			}
			flusher.Flush()
			allData.Write(buffer[:n])
		}
		if err != nil {
			if err != io.EOF {
				logger.LogError(c, "passthrough stream read error: "+err.Error())
			}
			break
		}
	}

	result := GetPassthroughResult(allData.Bytes(), true)
	return result, nil
}

// passthroughNonStreamResponseWithUsage 非流式响应透传（带 usage 和内容提取）
func passthroughNonStreamResponseWithUsage(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*PassthroughResult, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	info.SetFirstResponseTime()

	if common.DebugEnabled {
		logger.LogDebug(c, fmt.Sprintf("passthrough response body: %s", string(responseBody)))
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)

	result := GetPassthroughResult(responseBody, false)
	return result, nil
}

// PassthroughResult 传透模式结果，包含 usage 和响应内容
type PassthroughResult struct {
	Usage           *dto.Usage
	ResponseContent string // 响应中的文本内容，用于本地计算 token
}

// extractUsageAndContentFromStreamData 从流式数据中提取 usage 和内容
func extractUsageAndContentFromStreamData(data []byte) *PassthroughResult {
	result := &PassthroughResult{}
	var contentBuilder bytes.Buffer

	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		var jsonData []byte
		if bytes.HasPrefix(line, []byte("data: ")) {
			jsonData = bytes.TrimPrefix(line, []byte("data: "))
		} else if bytes.HasPrefix(line, []byte("{")) {
			jsonData = line
		} else {
			continue
		}
		if bytes.Equal(jsonData, []byte("[DONE]")) {
			continue
		}

		// 支持 usage 和 token_usage 两种字段名
		var streamResp struct {
			Usage      *dto.Usage `json:"usage"`
			TokenUsage *dto.Usage `json:"token_usage"`
			Text       string     `json:"text"`
			Nodes      []struct {
				Content string `json:"content"`
			} `json:"nodes"`
			Choices    []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Text string `json:"text"`
			} `json:"choices"`
		}
		if err := common.Unmarshal(jsonData, &streamResp); err == nil {
			// 优先使用 usage，其次使用 token_usage
			if streamResp.Usage != nil && (streamResp.Usage.PromptTokens > 0 || streamResp.Usage.CompletionTokens > 0) {
				result.Usage = streamResp.Usage
			} else if streamResp.TokenUsage != nil && (streamResp.TokenUsage.PromptTokens > 0 || streamResp.TokenUsage.CompletionTokens > 0) {
				result.Usage = streamResp.TokenUsage
			}
			// 累积所有 delta.content 与 text
			hasChunkContent := false
			if streamResp.Text != "" {
				contentBuilder.WriteString(streamResp.Text)
				hasChunkContent = true
			}
			for _, choice := range streamResp.Choices {
				if choice.Delta.Content != "" {
					contentBuilder.WriteString(choice.Delta.Content)
					hasChunkContent = true
				}
				if choice.Text != "" {
					contentBuilder.WriteString(choice.Text)
					hasChunkContent = true
				}
			}
			if !hasChunkContent {
				for _, node := range streamResp.Nodes {
					if node.Content != "" {
						contentBuilder.WriteString(node.Content)
					}
				}
			}
		}
	}

	result.ResponseContent = contentBuilder.String()

	// 调试日志
	if common.DebugEnabled {
		common.SysLog(fmt.Sprintf("[Passthrough Stream] ResponseContent length: %d, has usage: %v",
			len(result.ResponseContent), result.Usage != nil))
		if result.Usage != nil {
			common.SysLog(fmt.Sprintf("[Passthrough Stream] Usage: prompt=%d, completion=%d",
				result.Usage.PromptTokens, result.Usage.CompletionTokens))
		}
	}

	return result
}

// extractUsageAndContentFromResponse 从非流式响应中提取 usage 和内容
func extractUsageAndContentFromResponse(responseBody []byte) *PassthroughResult {
	result := &PassthroughResult{}

	var response struct {
		Usage      *dto.Usage `json:"usage"`
		TokenUsage *dto.Usage `json:"token_usage"`
		Text       string     `json:"text"`
		Nodes      []struct {
			Content string `json:"content"`
		} `json:"nodes"`
		Choices    []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
	}

	if err := common.Unmarshal(responseBody, &response); err == nil {
		// 优先使用 usage，其次使用 token_usage
		if response.Usage != nil && (response.Usage.PromptTokens > 0 || response.Usage.CompletionTokens > 0) {
			result.Usage = response.Usage
		} else if response.TokenUsage != nil && (response.TokenUsage.PromptTokens > 0 || response.TokenUsage.CompletionTokens > 0) {
			result.Usage = response.TokenUsage
		}
		hasResponseContent := false
		if response.Text != "" {
			result.ResponseContent += response.Text
			hasResponseContent = true
		}
		for _, choice := range response.Choices {
			if choice.Message.Content != "" {
				result.ResponseContent += choice.Message.Content
				hasResponseContent = true
			}
			if choice.Text != "" {
				result.ResponseContent += choice.Text
				hasResponseContent = true
			}
		}
		if !hasResponseContent {
			for _, node := range response.Nodes {
				if node.Content != "" {
					result.ResponseContent += node.Content
				}
			}
		}
	}

	// 调试日志
	if common.DebugEnabled {
		common.SysLog(fmt.Sprintf("[Passthrough NonStream] ResponseContent length: %d, has usage: %v",
			len(result.ResponseContent), result.Usage != nil))
		if result.Usage != nil {
			common.SysLog(fmt.Sprintf("[Passthrough NonStream] Usage: prompt=%d, completion=%d",
				result.Usage.PromptTokens, result.Usage.CompletionTokens))
		}
	}

	return result
}

// GetPassthroughResult 从响应中提取完整结果
func GetPassthroughResult(responseBody []byte, isStream bool) *PassthroughResult {
	if isStream {
		return extractUsageAndContentFromStreamData(responseBody)
	}
	return extractUsageAndContentFromResponse(responseBody)
}

// DoPassthroughRequest 执行传透请求（供外部调用）
func DoPassthroughRequest(adaptor channel.Adaptor, c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoApiRequest(adaptor, c, info, requestBody)
}
