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

func TestHarkUnacceptedRateLimitStillFailsOver(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := []byte(`{"error":{"code":"upstream_rate_limited","message":"before acceptance"}}`)
	require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(&Account{Platform: "hark"}, http.StatusTooManyRequests, "", body))
	for _, code := range []string{"session_turn_pending", "session_account_conflict", "session_owner_mismatch", "recovery_delivery_uncertain", "recovery_store_failed", "recovery_submission_uncertain", "recovery_submission_rejected", "recovery_submission_not_sent", "recovery_error_record_failed", "recovery_receipt_invalid"} {
		body := []byte(`{"error":{"code":"` + code + `","message":"retained context"}}`)
		require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(&Account{Platform: "hark"}, http.StatusBadGateway, "", body))
	}
}

func TestHarkAcceptedInterruptionDoesNotRotateAccounts(t *testing.T) {
	service := &OpenAIGatewayService{}
	account := &Account{Platform: "hark"}
	for _, code := range []string{"upstream_turn_interrupted"} {
		body := []byte(`{"error":{"code":"` + code + `","message":"Client tool execution has not started; continue explicitly"}}`)
		if service.shouldFailoverOpenAIUpstreamResponse(account, http.StatusBadGateway, "", body) {
			t.Fatalf("accepted tool contract failure rotated accounts: %s", code)
		}
	}
}

func TestHarkAcceptedInterruptionForwardingDoesNotReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, code := range []string{"upstream_turn_interrupted", "recovery_delivery_uncertain", "recovery_receipt_invalid", "recovery_submission_uncertain", "recovery_submission_rejected", "recovery_submission_not_sent", "recovery_error_record_failed", "recovery_store_failed", "session_owner_mismatch"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, code), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"fixture-model","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"Inspect fixture only"}]}`, stream))
					if path == "/v1/responses" {
						body = []byte(fmt.Sprintf(`{"model":"fixture-model","stream":%t,"input":"Inspect fixture only"}`, stream))
					}
					status, kind := http.StatusConflict, "application/json"
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
						"error_code": float64(http.StatusConflict), "keywords": []any{"client tool"},
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
