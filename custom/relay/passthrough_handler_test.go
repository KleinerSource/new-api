package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

type passthroughTestAdaptor struct{}

func (a passthroughTestAdaptor) Init(info *relaycommon.RelayInfo) {}
func (a passthroughTestAdaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return "http://should-not-be-used.invalid", nil
}
func (a passthroughTestAdaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	req.Set("Authorization", "Bearer "+info.ApiKey)
	return nil
}
func (a passthroughTestAdaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	return nil, nil
}
func (a passthroughTestAdaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, nil
}
func (a passthroughTestAdaptor) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return nil, nil
}
func (a passthroughTestAdaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, nil
}
func (a passthroughTestAdaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, nil
}
func (a passthroughTestAdaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	return nil, nil
}
func (a passthroughTestAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return nil, nil
}
func (a passthroughTestAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	return nil, nil
}
func (a passthroughTestAdaptor) GetModelList() []string { return nil }
func (a passthroughTestAdaptor) GetChannelName() string { return "passthrough-test" }
func (a passthroughTestAdaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	return nil, nil
}
func (a passthroughTestAdaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	return nil, nil
}

func TestGetPassthroughResultExtractsNonStreamUsageAndContent(t *testing.T) {
	body := []byte(`{
		"usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
		"choices": [{"message": {"content": "hello"}}]
	}`)

	got := GetPassthroughResult(body, false)

	if got.Usage == nil {
		t.Fatal("expected usage to be extracted")
	}
	if got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 7 {
		t.Fatalf("unexpected usage: %#v", got.Usage)
	}
	if got.ResponseContent != "hello" {
		t.Fatalf("expected response content %q, got %q", "hello", got.ResponseContent)
	}
	if got.ResponseBody == "" {
		t.Fatal("expected raw response body to be preserved")
	}
}

func TestGetPassthroughResultExtractsStreamUsageAndContent(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}],\"token_usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n" +
		"data: [DONE]\n")

	got := GetPassthroughResult(body, true)

	if got.Usage == nil {
		t.Fatal("expected usage to be extracted")
	}
	if got.Usage.PromptTokens != 3 || got.Usage.CompletionTokens != 2 {
		t.Fatalf("unexpected usage: %#v", got.Usage)
	}
	if got.ResponseContent != "hello" {
		t.Fatalf("expected response content %q, got %q", "hello", got.ResponseContent)
	}
}

func TestGetPassthroughResultAllowsNullStopReason(t *testing.T) {
	body := []byte(`{
		"text": "hello",
		"stop_reason": null
	}`)

	got := GetPassthroughResult(body, false)

	if got.UpstreamError {
		t.Fatalf("expected null stop_reason to be billable, got error %q", got.UpstreamErrorMessage)
	}
	if got.ResponseContent != "hello" {
		t.Fatalf("expected response content %q, got %q", "hello", got.ResponseContent)
	}
}

func TestGetPassthroughResultAllowsEmptyStopReasonValues(t *testing.T) {
	cases := []string{
		`{"text":"hello","stop_reason":0}`,
		`{"text":"hello","stop_reason":false}`,
		`{"text":"hello","stop_reason":""}`,
	}

	for _, body := range cases {
		got := GetPassthroughResult([]byte(body), false)
		if got.UpstreamError {
			t.Fatalf("expected stop_reason value to be billable for body %s, got error %q", body, got.UpstreamErrorMessage)
		}
		if got.ResponseContent != "hello" {
			t.Fatalf("expected response content %q, got %q", "hello", got.ResponseContent)
		}
	}
}

func TestGetPassthroughResultAllowsToolStopReasonWithoutWarningText(t *testing.T) {
	body := []byte(`{
		"text": "tool call completed",
		"stop_reason": 3
	}`)

	got := GetPassthroughResult(body, false)

	if got.UpstreamError {
		t.Fatalf("expected tool stop_reason without warning text to be billable, got error %q", got.UpstreamErrorMessage)
	}
	if got.ResponseContent != "tool call completed" {
		t.Fatalf("expected response content %q, got %q", "tool call completed", got.ResponseContent)
	}
}

func TestGetPassthroughResultMarksWarningTextWithStopReasonAsUpstreamError(t *testing.T) {
	body := []byte(`{
		"text": "⚠️ **An error occurred: unknown provider** ⚠️",
		"stop_reason": 1
	}`)

	got := GetPassthroughResult(body, false)

	if !got.UpstreamError {
		t.Fatal("expected non-null stop_reason to mark upstream error")
	}
	if got.UpstreamErrorMessage != "⚠️ **An error occurred: unknown provider** ⚠️" {
		t.Fatalf("unexpected upstream error message %q", got.UpstreamErrorMessage)
	}
	if got.UpstreamStopReason != "1" {
		t.Fatalf("expected stop_reason %q, got %q", "1", got.UpstreamStopReason)
	}
	if got.ResponseBody == "" {
		t.Fatal("expected raw response body to be preserved")
	}
}

func TestGetPassthroughResultMarksStreamNonNullStopReasonAsUpstreamError(t *testing.T) {
	body := []byte("data: {\"text\":\"⚠️ An error occurred\",\"stop_reason\":1}\n")

	got := GetPassthroughResult(body, true)

	if !got.UpstreamError {
		t.Fatal("expected stream non-null stop_reason to mark upstream error")
	}
	if got.ResponseContent != "⚠️ An error occurred" {
		t.Fatalf("expected response content to be preserved, got %q", got.ResponseContent)
	}
	if got.UpstreamStopReason != "1" {
		t.Fatalf("expected stop_reason %q, got %q", "1", got.UpstreamStopReason)
	}
	if got.ResponseBody == "" {
		t.Fatal("expected raw stream response body to be preserved")
	}
}

func TestGetPassthroughResultMarksPrettyJSONStreamStopReasonAsUpstreamError(t *testing.T) {
	body := []byte(`{
		"stream_id": "95457b86-19e3-4210-9eeb-878150e49c2f",
		"seq": 1,
		"text": "⚠️ An error occurred: unknown provider",
		"stop_reason": 1
	}`)

	got := GetPassthroughResult(body, true)

	if !got.UpstreamError {
		t.Fatal("expected pretty JSON stream response with non-null stop_reason to mark upstream error")
	}
	if got.UpstreamStopReason != "1" {
		t.Fatalf("expected stop_reason %q, got %q", "1", got.UpstreamStopReason)
	}
	if got.ResponseContent != "⚠️ An error occurred: unknown provider" {
		t.Fatalf("expected response content to be preserved, got %q", got.ResponseContent)
	}
}

func TestDetectPassthroughErrorResponseReturnsOpenAIErrorOnHttp200(t *testing.T) {
	body := []byte(`{"error":{"message":"The model 'not-exists' does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}`)

	apiErr := detectPassthroughErrorResponse(body, http.StatusOK)
	if apiErr == nil {
		t.Fatal("expected api error to be detected")
	}
	if apiErr.ToOpenAIError().Message != "The model 'not-exists' does not exist" {
		t.Fatalf("unexpected error message: %#v", apiErr.ToOpenAIError())
	}
}

func TestPassthroughNonStreamResponseWithUsageReturnsErrorForHttp200ErrorPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":{"message":"model unavailable","type":"invalid_request_error","code":"model_not_found"}}`)),
	}
	info := &relaycommon.RelayInfo{}

	result, apiErr := passthroughNonStreamResponseWithUsage(c, resp, info)
	if apiErr == nil {
		t.Fatal("expected api error for http 200 error payload")
	}
	if result != nil {
		t.Fatalf("expected nil result, got %#v", result)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected no body written on detected error, got %q", recorder.Body.String())
	}
}

func TestPassthroughResponseWithUsageMarksAugmentStatus500AsUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":     []string{"application/json"},
			"X-Augment-Status": []string{"500"},
		},
		Body: io.NopCloser(strings.NewReader(`{"text":"upstream failed"}`)),
	}
	info := &relaycommon.RelayInfo{}

	result, apiErr := passthroughResponseWithUsage(c, resp, info)
	if apiErr != nil {
		t.Fatalf("unexpected api error: %v", apiErr)
	}
	if result == nil || !result.UpstreamError {
		t.Fatalf("expected X-Augment-Status=500 to mark upstream error, got %#v", result)
	}
	if result.UpstreamErrorMessage != "upstream X-Augment-Status=500" {
		t.Fatalf("unexpected upstream error message %q", result.UpstreamErrorMessage)
	}
	if result.ResponseContent != "upstream failed" {
		t.Fatalf("expected response content to be preserved, got %q", result.ResponseContent)
	}
	if recorder.Body.String() != `{"text":"upstream failed"}` {
		t.Fatalf("expected response body to be proxied, got %q", recorder.Body.String())
	}
}

func TestPassthroughResponseWithUsagePrioritizesAugmentStatus500OverStopReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":     []string{"application/json"},
			"X-Augment-Status": []string{"500"},
		},
		Body: io.NopCloser(strings.NewReader(`{"text":"⚠️ stop reason would normally match","stop_reason":1}`)),
	}
	info := &relaycommon.RelayInfo{}

	result, apiErr := passthroughResponseWithUsage(c, resp, info)
	if apiErr != nil {
		t.Fatalf("unexpected api error: %v", apiErr)
	}
	if result == nil || !result.UpstreamError {
		t.Fatalf("expected X-Augment-Status=500 to mark upstream error, got %#v", result)
	}
	if result.UpstreamErrorMessage != "upstream X-Augment-Status=500" {
		t.Fatalf("expected augment status error to take priority, got %q", result.UpstreamErrorMessage)
	}
}

func TestPassthroughResponseWithUsageChecksStopReasonWhenAugmentStatusIsNot500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":     []string{"application/json"},
			"X-Augment-Status": []string{"200"},
		},
		Body: io.NopCloser(strings.NewReader(`{"text":"⚠️ stop reason should match","stop_reason":1}`)),
	}
	info := &relaycommon.RelayInfo{}

	result, apiErr := passthroughResponseWithUsage(c, resp, info)
	if apiErr != nil {
		t.Fatalf("unexpected api error: %v", apiErr)
	}
	if result == nil || !result.UpstreamError {
		t.Fatalf("expected stop_reason warning to mark upstream error, got %#v", result)
	}
	if result.UpstreamErrorMessage != "⚠️ stop reason should match" {
		t.Fatalf("unexpected upstream error message %q", result.UpstreamErrorMessage)
	}
}

func TestDoPassthroughRequestWithCustomURLUsesChatStreamEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()
	var seenPath string
	var seenAuth string
	var seenCustomHeader string
	var seenBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenCustomHeader = r.Header.Get("X-Upstream-Key")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		seenBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/chat-stream", strings.NewReader(`{"model":"gpt-4o"}`))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: upstream.URL + "/",
			ApiKey:         "test-key",
			HeadersOverride: map[string]interface{}{
				"X-Upstream-Key": "key:{api_key}",
			},
		},
	}

	resp, err := doPassthroughRequestWithCustomURL(c, passthroughTestAdaptor{}, info, strings.NewReader(`{"model":"gpt-4o"}`))
	if err != nil {
		t.Fatalf("unexpected passthrough request error: %v", err)
	}
	defer resp.Body.Close()

	if seenPath != PassthroughEndpoint {
		t.Fatalf("expected path %q, got %q", PassthroughEndpoint, seenPath)
	}
	if seenAuth != "Bearer test-key" {
		t.Fatalf("expected Authorization header to be set, got %q", seenAuth)
	}
	if seenCustomHeader != "key:test-key" {
		t.Fatalf("expected header override placeholder replacement, got %q", seenCustomHeader)
	}
	if seenBody != `{"model":"gpt-4o"}` {
		t.Fatalf("expected request body to be forwarded, got %q", seenBody)
	}
}
