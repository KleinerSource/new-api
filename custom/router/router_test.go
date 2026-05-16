package router

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSetCustomRouterRegistersCustomEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	SetCustomRouter(r)

	routes := map[string]bool{}
	for _, route := range r.Routes() {
		routes[route.Method+" "+route.Path] = true
	}

	expected := []string{
		http.MethodGet + " /usage/api/balance",
		http.MethodGet + " /usage/api/get-models",
		http.MethodPost + " /chat-stream",
	}
	for _, route := range expected {
		if !routes[route] {
			t.Fatalf("expected route %q to be registered, got %#v", route, routes)
		}
	}
}
