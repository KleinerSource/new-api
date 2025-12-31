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

// PassthroughHelperWithUsage 传透模式处理器（带 usage 和内容提取）
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

	requestBody := bytes.NewBuffer(body)

	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	var httpResp *http.Response
	if resp != nil {
		httpResp = resp.(*http.Response)
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")

		if httpResp.StatusCode != http.StatusOK {
			newApiErr := service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			return nil, newApiErr
		}
	}

	return passthroughResponseWithUsage(c, httpResp, info)
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
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		jsonData := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(jsonData, []byte("[DONE]")) {
			continue
		}

		// 支持多种 usage 字段名：usage, token_usage
		var streamResp struct {
			Usage      *dto.Usage `json:"usage"`
			TokenUsage *dto.Usage `json:"token_usage"`
			Choices    []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				// 支持部分上游直接返回 text 字段
				Text string `json:"text"`
			} `json:"choices"`
			// 支持部分上游直接在根级别返回 content
			Content string `json:"content"`
			// 支持部分上游返回 response 字段
			Response string `json:"response"`
		}
		if err := common.Unmarshal(jsonData, &streamResp); err == nil {
			// 优先使用 usage，其次使用 token_usage
			if streamResp.Usage != nil && (streamResp.Usage.PromptTokens > 0 || streamResp.Usage.CompletionTokens > 0) {
				result.Usage = streamResp.Usage
			} else if streamResp.TokenUsage != nil && (streamResp.TokenUsage.PromptTokens > 0 || streamResp.TokenUsage.CompletionTokens > 0) {
				result.Usage = streamResp.TokenUsage
			}

			// 提取内容：支持多种格式
			for _, choice := range streamResp.Choices {
				if choice.Delta.Content != "" {
					contentBuilder.WriteString(choice.Delta.Content)
				}
				if choice.Text != "" {
					contentBuilder.WriteString(choice.Text)
				}
			}
			if streamResp.Content != "" {
				contentBuilder.WriteString(streamResp.Content)
			}
			if streamResp.Response != "" {
				contentBuilder.WriteString(streamResp.Response)
			}
		}
	}

	result.ResponseContent = contentBuilder.String()
	return result
}

// extractUsageAndContentFromResponse 从非流式响应中提取 usage 和内容
func extractUsageAndContentFromResponse(responseBody []byte) *PassthroughResult {
	result := &PassthroughResult{}

	// 支持多种 usage 字段名和内容格式
	var response struct {
		Usage      *dto.Usage `json:"usage"`
		TokenUsage *dto.Usage `json:"token_usage"`
		Choices    []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
		// 支持部分上游直接在根级别返回 content
		Content string `json:"content"`
		// 支持部分上游返回 response 字段
		Response string `json:"response"`
		// 支持部分上游返回 output 字段
		Output string `json:"output"`
	}

	if err := common.Unmarshal(responseBody, &response); err == nil {
		// 优先使用 usage，其次使用 token_usage
		if response.Usage != nil && (response.Usage.PromptTokens > 0 || response.Usage.CompletionTokens > 0) {
			result.Usage = response.Usage
		} else if response.TokenUsage != nil && (response.TokenUsage.PromptTokens > 0 || response.TokenUsage.CompletionTokens > 0) {
			result.Usage = response.TokenUsage
		}

		// 提取内容：支持多种格式
		var contentBuilder bytes.Buffer
		for _, choice := range response.Choices {
			if choice.Message.Content != "" {
				contentBuilder.WriteString(choice.Message.Content)
			}
			if choice.Text != "" {
				contentBuilder.WriteString(choice.Text)
			}
		}
		if response.Content != "" {
			contentBuilder.WriteString(response.Content)
		}
		if response.Response != "" {
			contentBuilder.WriteString(response.Response)
		}
		if response.Output != "" {
			contentBuilder.WriteString(response.Output)
		}
		result.ResponseContent = contentBuilder.String()
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

