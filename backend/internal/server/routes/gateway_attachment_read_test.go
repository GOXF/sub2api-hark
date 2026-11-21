package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func TestAttachmentIntakeRoutesBypassModelHandler(t *testing.T) {
	t.Setenv("HARK_DATA_DIR", t.TempDir())
	gin.SetMode(gin.TestMode)
	router := gin.New()
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxBodySize: 1 << 20}}
	cfg.Server.FrontendURL = "https://trusted.example"
	authCalls := 0
	auth := middleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		authCalls++
		id := int64(1)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &id, Group: &service.Group{Platform: service.PlatformHark}})
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.APIKeyID, int64(424242)))
		c.Next()
	})
	// Nil model handlers make accidental scheduling fail rather than masquerade as success.
	RegisterGatewayRoutes(router, &handler.Handlers{}, auth, nil, nil, nil, nil, nil, cfg)
	body := `{"model":"claude-opus-5-5","stream":true,"input":"# Files mentioned by the user:\n\n## fixture.xls: C:/selected/fixture.xls\n\n## My request:\nread it","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}]}`
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		r := httptest.NewRequest("POST", "https://evil.example"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-Host", "evil.example")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get(gateway.AttachmentIntakeHeader) == "" || !strings.Contains(w.Body.String(), "response.function_call_arguments.done") || strings.Contains(w.Body.String(), "https://evil.example/v1/attachment") {
			t.Fatalf("%s HTTP %d: %s", path, w.Code, w.Body)
		}
	}
	before := authCalls
	r := httptest.NewRequest(http.MethodPut, "/v1/attachment-reads/unknown", strings.NewReader("binary"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "attachment_upload_expired") || authCalls != before {
		t.Fatalf("PUT went through model/API-key middleware: %d %s", w.Code, w.Body)
	}
}
