//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHarkToolContractFailureDoesNotRotateAccounts(t *testing.T) {
	service := &OpenAIGatewayService{}
	account := &Account{Platform: "hark"}
	for _, code := range []string{"tool_call_missing", "tool_call_mismatch", "upstream_compaction_declined", "upstream_declined"} {
		body := []byte(`{"error":{"code":"` + code + `","message":"Client tool execution has not started; continue explicitly"}}`)
		if service.shouldFailoverOpenAIUpstreamResponse(account, http.StatusUnprocessableEntity, "", body) {
			t.Fatalf("accepted tool contract failure rotated accounts: %s", code)
		}
	}
}

func TestHarkToolContractForwardingKeepsFailureAndDoesNotReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, code := range []string{"tool_call_missing", "tool_call_mismatch", "upstream_compaction_declined", "upstream_declined"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, code), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"fixture-model","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"Inspect fixture only"}]}`, stream))
					if path == "/v1/responses" {
						body = []byte(fmt.Sprintf(`{"model":"fixture-model","stream":%t,"input":"Inspect fixture only"}`, stream))
					}
					status, kind := http.StatusUnprocessableEntity, "application/json"
					payload := `{"error":{"code":"` + code + `","message":"Required client tool call absent; execution has not started"}}`
					if stream {
						status, kind = http.StatusOK, "text/event-stream"
						payload = strings.ReplaceAll(harkBudgetForwardSSE(path, false), "upstream_budget_exhausted", code)
						payload = strings.ReplaceAll(payload, "Hark generation budget exhausted (cost_capped); continue explicitly", "Required client tool call absent; execution has not started")
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: status, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(payload)),
					}}
					cfg := testConfig()
					repo := &harkBudgetForwardHealthRepo{}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector(), rateLimitService: NewRateLimitService(repo, nil, cfg, nil, nil)}
					account := harkRoutingTestAccount()
					account.Schedulable = true
					account.Credentials["temp_unschedulable_enabled"] = true
					account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{
						"error_code": float64(http.StatusUnprocessableEntity), "keywords": []any{"client tool"},
						"duration_minutes": float64(1), "scope": "account",
					}}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
					c.Request.Header.Set("Content-Type", "application/json")
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					var err error
					switch path {
					case "/v1/chat/completions":
						_, err = svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
					case "/v1/responses":
						_, err = svc.Forward(ctx, c, account, body)
					default:
						_, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
					}
					require.Error(t, err, "the host must not record a tool contract failure as success")
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover), "an accepted turn must not be replayed")
					require.Len(t, upstream.requests, 1)
					require.Equal(t, status, recorder.Code)
					require.Contains(t, recorder.Body.String(), code)
					require.Zero(t, repo.tempCalls+repo.errorCalls+repo.rateLimitCalls)
				})
			}
		}
	}
}
