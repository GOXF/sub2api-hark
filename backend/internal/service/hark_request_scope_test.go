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

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHarkRequestScopeFailureDoesNotRotateAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, code := range []string{"session_label_history_mismatch", "compaction_tool_choice_conflict"} {
			for _, wrapped := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/wrapped=%t", path, code, wrapped), func(t *testing.T) {
					status := http.StatusConflict
					if code == "compaction_tool_choice_conflict" {
						status = http.StatusBadRequest
					}
					if wrapped {
						status = http.StatusServiceUnavailable
					}
					payload := `{"error":{"code":"` + code + `","message":"history protection requires explicit continuation"}}`
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}}
					cfg := testConfig()
					repo := &harkBudgetForwardHealthRepo{}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector(), rateLimitService: NewRateLimitService(repo, nil, cfg, nil, nil)}
					account := harkRoutingTestAccount()
					account.Schedulable = true
					account.Credentials["temp_unschedulable_enabled"] = true
					account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{"error_code": float64(status), "keywords": []any{"history protection"}, "duration_minutes": float64(1), "scope": "account"}}
					body := []byte(`{"model":"fixture-model","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"Continue the retained task"}]}`)
					if path == "/v1/responses" {
						body = []byte(`{"model":"fixture-model","stream":false,"input":"Continue the retained task"}`)
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
					c.Request.Header.Set("Content-Type", "application/json")
					var err error
					switch path {
					case "/v1/chat/completions":
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
					case "/v1/responses":
						_, err = svc.Forward(context.Background(), c, account, body)
					default:
						_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
					}
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover), "retained context must not be retried on another account")
					require.Len(t, upstream.requests, 1)
					require.Equal(t, status, recorder.Code)
					require.Contains(t, recorder.Body.String(), code)
					require.Zero(t, repo.tempCalls+repo.errorCalls+repo.rateLimitCalls)
				})
			}
		}
	}
}
