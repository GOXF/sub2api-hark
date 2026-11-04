//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// harkRoutingTestAccount 是未设置 api_protocol 的 Hark 账号（base_url 为默认内嵌网关）。
func harkRoutingTestAccount() *Account {
	return &Account{
		ID:          9001,
		Name:        "hark",
		Platform:    PlatformHark,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "hark-cookie", "base_url": DefaultHarkBaseURL},
	}
}

// 强制开启出站白名单（且不含 hark.internal），确认内嵌网关仍被放行。
func harkStrictAllowlistConfig() *config.Config {
	return &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
		Enabled:       true,
		UpstreamHosts: []string{"api.openai.com"},
	}}}
}

func TestHarkAccountDefaultsToAdaptiveNativeRouting(t *testing.T) {
	account := harkRoutingTestAccount()
	require.True(t, account.IsHark())
	require.Equal(t, APIProtocolAdaptive, account.GetAPIProtocol())
	require.True(t, account.UsesNativeCNResponses())
	require.False(t, shouldForwardOpenAIResponsesViaRawChatCompletions(account))

	require.Equal(t, APIProtocolResponses, resolveUpstreamProtocol(account, APIProtocolResponses, "", nil))
	require.Equal(t, APIProtocolAnthropic, resolveUpstreamProtocol(account, APIProtocolAnthropic, "", nil))
	require.Equal(t, APIProtocolChatCompletions, resolveUpstreamProtocol(account, APIProtocolChatCompletions, "", nil))

	require.Equal(t, DefaultHarkBaseURL, account.GetCNProtocolBaseURL(APIProtocolChatCompletions))
	require.Equal(t, DefaultHarkBaseURL, account.GetCNProtocolBaseURL(APIProtocolResponses))
	require.Equal(t, "http://hark.internal", account.GetAnthropicProtocolBaseURL())

	// 显式协议仍优先。
	account.Credentials["api_protocol"] = APIProtocolChatCompletions
	require.Equal(t, APIProtocolChatCompletions, account.GetAPIProtocol())
	require.True(t, shouldForwardOpenAIResponsesViaRawChatCompletions(account))
	require.Equal(t, APIProtocolChatCompletions, resolveUpstreamProtocol(account, APIProtocolResponses, "", nil))
}

func TestHarkNativeAnthropicMessagesGoesToEmbeddedGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := harkRoutingTestAccount()
	svc := &OpenAIGatewayService{cfg: harkStrictAllowlistConfig()}

	targetURL, err := svc.nativeAnthropicTargetURL(account)
	require.NoError(t, err)
	require.Equal(t, "http://hark.internal/v1/messages", targetURL)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("X-Claude-Code-Session-Id", "cc-session-1")
	body := []byte(`{"model":"hark","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	req, _, err := svc.buildNativeAnthropicUpstreamRequest(context.Background(), c, account, body, "hark-cookie", targetURL)
	require.NoError(t, err)
	require.Equal(t, "http://hark.internal/v1/messages", req.URL.String())
	require.Equal(t, "cc-session-1", req.Header.Get("X-Claude-Code-Session-Id"))
}

func TestHarkNativeResponsesGoesToEmbeddedGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := harkRoutingTestAccount()
	svc := &OpenAIGatewayService{cfg: harkStrictAllowlistConfig()}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("session-id", "codex-session-1")
	c.Request.Header.Set("session_id", "codex-session-1")
	body := []byte(`{"model":"hark","input":"hi","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`)
	req, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "hark-cookie", false, "", false)
	require.NoError(t, err)
	require.Equal(t, "http://hark.internal/v1/responses", req.URL.String())
	require.Equal(t, "codex-session-1", req.Header.Get("session-id"))
}

func TestAccountTestServiceAllowsHarkInternal(t *testing.T) {
	svc := &AccountTestService{cfg: harkStrictAllowlistConfig()}
	got, err := svc.validateUpstreamBaseURL("http://hark.internal")
	require.NoError(t, err)
	require.Equal(t, "http://hark.internal", got)
}
