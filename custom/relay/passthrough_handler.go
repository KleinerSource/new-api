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

// PassthroughHelperWithUsage 传透模式处理器（带 usage 提取）
func PassthroughHelperWithUsage(c *gin.Context, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
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

// passthroughResponseWithUsage 透传响应并提取 usage 信息
func passthroughResponseWithUsage(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
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

// passthroughStreamResponseWithUsage 流式响应透传（带 usage 提取）
func passthroughStreamResponseWithUsage(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	helper.SetEventStreamHeaders(c)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logger.LogWarn(c, "streaming not supported, falling back to buffered response")
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
		}
		c.Writer.Write(responseBody)
		return extractUsageFromStreamData(responseBody), nil
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

	usage := extractUsageFromStreamData(allData.Bytes())
	return usage, nil
}

// passthroughNonStreamResponseWithUsage 非流式响应透传（带 usage 提取）
func passthroughNonStreamResponseWithUsage(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	info.SetFirstResponseTime()

	if common.DebugEnabled {
		logger.LogDebug(c, fmt.Sprintf("passthrough response body: %s", string(responseBody)))
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)

	usage := GetPassthroughUsage(responseBody, info)
	return usage, nil
}

// extractUsageFromStreamData 从流式数据中提取 usage 信息
func extractUsageFromStreamData(data []byte) *dto.Usage {
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if bytes.HasPrefix(line, []byte("data: ")) {
			jsonData := bytes.TrimPrefix(line, []byte("data: "))
			if bytes.Equal(jsonData, []byte("[DONE]")) {
				continue
			}
			var streamResp struct {
				Usage *dto.Usage `json:"usage"`
			}
			if err := common.Unmarshal(jsonData, &streamResp); err == nil && streamResp.Usage != nil {
				return streamResp.Usage
			}
		}
	}
	return nil
}

// GetPassthroughUsage 从响应中提取 usage 信息
func GetPassthroughUsage(responseBody []byte, info *relaycommon.RelayInfo) *dto.Usage {
	var simpleResponse struct {
		Usage *dto.Usage `json:"usage"`
	}

	if err := common.Unmarshal(responseBody, &simpleResponse); err == nil && simpleResponse.Usage != nil {
		return simpleResponse.Usage
	}

	return &dto.Usage{
		PromptTokens:     info.GetEstimatePromptTokens(),
		CompletionTokens: 0,
		TotalTokens:      info.GetEstimatePromptTokens(),
	}
}

// DoPassthroughRequest 执行传透请求（供外部调用）
func DoPassthroughRequest(adaptor channel.Adaptor, c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoApiRequest(adaptor, c, info, requestBody)
}

