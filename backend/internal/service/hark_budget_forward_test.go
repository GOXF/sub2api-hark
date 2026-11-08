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

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

type harkBudgetForwardHealthRepo struct {
	tempUnschedulableOpenAIAccountRepo
	tempCalls, errorCalls, rateLimitCalls int
}

func (r *harkBudgetForwardHealthRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.tempCalls++
	return nil
}

func (r *harkBudgetForwardHealthRepo) SetError(context.Context, int64, string) error {
	r.errorCalls++
	return nil
}

func (r *harkBudgetForwardHealthRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.rateLimitCalls++
	return nil
}

const harkBudgetForwardError = `{"code":"upstream_budget_exhausted","type":"server_error","message":"Hark generation budget exhausted (cost_capped); continue explicitly"}`

// Exercise the public dispatchers with the default Hark adaptive routing.
// Only the HTTPUpstream boundary and account persistence are fixtures.
func TestHarkBudgetForwardNeverReplaysAcceptedTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, mode := range []string{"http429", "sse_before_text", "sse_after_text"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				stream := mode != "http429"
				body := []byte(fmt.Sprintf(`{"model":"fixture-model","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"Continue fixture checkpoint"}]}`, stream))
				if path == "/v1/responses" {
					body = []byte(fmt.Sprintf(`{"model":"fixture-model","stream":%t,"instructions":"Use fixture only","input":"Continue fixture checkpoint"}`, stream))
				}
				status, contentType, upstreamBody := http.StatusTooManyRequests, "application/json", `{"error":`+harkBudgetForwardError+`}`
				if stream {
					status, contentType = http.StatusOK, "text/event-stream"
					upstreamBody = harkBudgetForwardSSE(path, mode == "sse_after_text")
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": {contentType}, "X-Request-Id": {"fixture-budget"}},
					Body:       io.NopCloser(strings.NewReader(upstreamBody)),
				}}
				cfg := testConfig()
				repo := &harkBudgetForwardHealthRepo{}
				svc := &OpenAIGatewayService{
					cfg: cfg, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector(),
					rateLimitService: NewRateLimitService(repo, nil, cfg, nil, nil),
				}
				account := harkRoutingTestAccount()
				account.Schedulable = true
				account.Credentials["temp_unschedulable_enabled"] = true
				account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{
					"error_code": float64(http.StatusTooManyRequests), "keywords": []any{"cost_capped"},
					"duration_minutes": float64(1), "scope": "account",
				}}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				ruleSvc := &ErrorPassthroughService{}
				ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{
					newNonFailoverPassthroughRule(http.StatusBadGateway, "cost_capped", http.StatusBadGateway, "generic upstream failure"),
				})
				BindErrorPassthroughService(c, ruleSvc)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var result *OpenAIForwardResult
				var err error
				switch path {
				case "/v1/chat/completions":
					result, err = svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
				case "/v1/responses":
					result, err = svc.Forward(ctx, c, account, body)
				case "/v1/messages":
					result, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
				}
				var failover *UpstreamFailoverError
				assert.Error(t, err, "an exhausted accepted turn must not be recorded as successful")
				if err != nil {
					assert.Contains(t, err.Error(), "budget", "budget failure must retain its cause")
				}
				assert.False(t, errors.As(err, &failover), "accepted budget failure must not ask the host to replay: %v", err)
				assert.Len(t, upstream.requests, 1, "the accepted turn must have exactly one upstream submission")
				if upstream.lastReq != nil {
					assert.Equal(t, path, upstream.lastReq.URL.Path, "Hark must use the endpoint's native protocol")
				}
				assert.Equal(t, status, recorder.Code, "budget response/stream status changed: %s", recorder.Body)
				if stream {
					var budgetCode bool
					for _, line := range strings.Split(recorder.Body.String(), "\n") {
						if strings.HasPrefix(line, "data: ") {
							payload := strings.TrimPrefix(line, "data: ")
							budgetCode = budgetCode || gjson.Get(payload, "error.code").Str == "upstream_budget_exhausted" || gjson.Get(payload, "response.error.code").Str == "upstream_budget_exhausted"
						}
					}
					assert.True(t, budgetCode, "budget code must survive native SSE forwarding: %s", recorder.Body)
				} else {
					assert.Equal(t, "upstream_budget_exhausted", gjson.GetBytes(recorder.Body.Bytes(), "error.code").Str)
				}
				assert.Contains(t, recorder.Body.String(), "cost_capped")
				if mode == "sse_after_text" {
					assert.Contains(t, recorder.Body.String(), "checkpoint visible", "partial text must survive the terminal budget error")
				}
				assert.Zero(t, repo.tempCalls, "project budget exhaustion must bypass the matching account-cooldown rule")
				assert.Zero(t, repo.errorCalls)
				assert.Zero(t, repo.rateLimitCalls)
				assert.Zero(t, repo.modelRateLimitAccountID)
				assert.True(t, account.Schedulable)
				assert.Equal(t, StatusActive, account.Status)
				t.Logf("submissions=%d status=%d failover=%t error=%v result_present=%t health_mutations=%d", len(upstream.requests), recorder.Code, failover != nil, err, result != nil, repo.tempCalls+repo.errorCalls+repo.rateLimitCalls)
			})
		}
	}
}

func harkBudgetForwardSSE(path string, partial bool) string {
	frame := func(event, payload string) string {
		if event != "" {
			return "event: " + event + "\ndata: " + payload + "\n\n"
		}
		return "data: " + payload + "\n\n"
	}
	switch path {
	case "/v1/chat/completions":
		body := frame("", `{"id":"chatcmpl_fixture","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
		if partial {
			body += frame("", `{"id":"chatcmpl_fixture","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"checkpoint visible"},"finish_reason":null}]}`)
		}
		return body + frame("", `{"error":`+harkBudgetForwardError+`}`) + frame("", "[DONE]")
	case "/v1/responses":
		body := frame("response.created", `{"type":"response.created","response":{"id":"resp_fixture","object":"response","status":"in_progress","output":[]}}`)
		if partial {
			body += frame("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_fixture","output_index":0,"content_index":0,"delta":"checkpoint visible"}`)
		}
		return body + frame("response.failed", `{"type":"response.failed","response":{"id":"resp_fixture","object":"response","status":"failed","output":[],"error":`+harkBudgetForwardError+`}}`)
	default:
		body := frame("message_start", `{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`)
		if partial {
			body += frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			body += frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"checkpoint visible"}}`)
		}
		return body + frame("error", `{"type":"error","error":`+harkBudgetForwardError+`}`)
	}
}
