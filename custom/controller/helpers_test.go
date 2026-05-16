package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func TestRequireBearerTokenTrimsSkPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/api/balance", nil)
	c.Request.Header.Set("Authorization", "Bearer sk-test-token")

	tokenKey, ok := requireBearerToken(c)
	if !ok {
		t.Fatal("expected bearer token to be accepted")
	}
	if tokenKey != "test-token" {
		t.Fatalf("expected token key %q, got %q", "test-token", tokenKey)
	}
}

func TestRequireBearerTokenRejectsMissingAndInvalidHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name    string
		header  string
		message string
	}{
		{name: "missing", message: "No Authorization header"},
		{name: "invalid", header: "Token abc", message: "Invalid Bearer token"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/usage/api/balance", nil)
			if tc.header != "" {
				c.Request.Header.Set("Authorization", tc.header)
			}

			if tokenKey, ok := requireBearerToken(c); ok || tokenKey != "" {
				t.Fatalf("expected auth failure, got ok=%v token=%q", ok, tokenKey)
			}
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, recorder.Code)
			}

			var body struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to decode response body: %v", err)
			}
			if body.Success {
				t.Fatal("expected success=false")
			}
			if body.Message != tc.message {
				t.Fatalf("expected message %q, got %q", tc.message, body.Message)
			}
		})
	}
}

func TestProxyUpstreamRequestNormalizesURLAndSetsChannelAuth(t *testing.T) {
	var seenPath string
	var seenAuth string
	var seenContentType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	baseURL := upstream.URL + "/chat-stream/"
	channel := &model.Channel{
		Key:     "channel-key",
		BaseURL: &baseURL,
	}

	resp, err := proxyUpstreamRequest(context.Background(), channel, http.MethodGet, "/usage/api/balance")
	if err != nil {
		t.Fatalf("unexpected proxy request error: %v", err)
	}
	defer resp.Body.Close()

	if seenPath != "/usage/api/balance" {
		t.Fatalf("expected path %q, got %q", "/usage/api/balance", seenPath)
	}
	if seenAuth != "Bearer channel-key" {
		t.Fatalf("expected upstream Authorization header, got %q", seenAuth)
	}
	if seenContentType != "application/json" {
		t.Fatalf("expected JSON content type, got %q", seenContentType)
	}
}
