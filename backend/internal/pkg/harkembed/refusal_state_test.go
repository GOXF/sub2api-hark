package harkembed

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

// Real completed web jobs and a non-streaming send_bubble refusal. Only the
// per-request correlation IDs are rebound to the local mock submission.
func TestObservedRefusal1526TerminalFailure(t *testing.T) {
	raw, err := os.ReadFile("gateway/testdata/refusal_event_20261115_1526.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		RequestID string           `json:"request_id"`
		Expected  string           `json:"expected"`
		Messages  []map[string]any `json:"messages"`
	}
	if err = json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	refusal := ""
	for _, m := range captured.Messages {
		if m["source"] == "send_bubble" {
			refusal, _ = m["content"].(string)
		}
	}
	const observed = "Hark 不能被用作其他应用背后的模型。如果你直接在这里告诉我想做什么，比如直接发截图问这是什么软件，我很乐意帮你。"
	if refusal != observed || len(captured.Messages) != 3 {
		t.Fatal("captured native event fixture changed")
	}
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, compact := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/compact=%t", protocol, stream, compact), func(t *testing.T) {
					t.Setenv("HARK_DATA_DIR", t.TempDir())
					t.Setenv("HARK_USAGE_ESTIMATE", "off")
					var mu sync.Mutex
					var sent map[string]string
					submissions, creates := 0, 0
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/api/conversations":
							creates++
							_ = json.NewEncoder(w).Encode(map[string]string{"conversationId": "refusal-1526-fixture"})
						case "/api/messages/send":
							submissions++
							var payload map[string]json.RawMessage
							if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
								t.Error(e)
							}
							sent = make(map[string]string)
							for _, key := range []string{"conversationId", "idempotencyKey", "responseMessageId"} {
								var value string
								if e := json.Unmarshal(payload[key], &value); e != nil {
									t.Error(e)
								}
								sent[key] = value
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": sent["conversationId"], "messageId": sent["idempotencyKey"], "redirected": false})
						case "/api/messages/history":
							var messages []map[string]any
							if e := json.Unmarshal(raw, &struct {
								Messages *[]map[string]any `json:"messages"`
							}{&messages}); e != nil {
								t.Error(e)
							}
							for _, m := range messages {
								if m["id"] == captured.Expected {
									m["id"] = sent["responseMessageId"]
								}
								if m["triggeredByMessageId"] == captured.RequestID {
									m["triggeredByMessageId"] = sent["idempotencyKey"]
								}
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"messages": messages})
						default:
							http.Error(w, "unexpected local fixture request", http.StatusNotFound)
						}
					}))
					t.Cleanup(upstream.Close)
					previous, e := get()
					if e != nil {
						t.Fatal(e)
					}
					embedded, e := gateway.NewEmbedded(t.TempDir(), gateway.Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
					if e != nil {
						t.Fatal(e)
					}
					server = embedded
					t.Cleanup(func() { server = previous })
					question, code := "知道这个是什么软件吗", "upstream_declined"
					if compact {
						question, code = scopeCompactionCommand, "upstream_compaction_declined"
					}
					// Synthetic local request, actual captured reply metadata.
					// No real image is sent and no acceptance capability is tested.
					path, body := identityForwardRequest(protocol, question, stream)
					req := labelRequest(t, path, body, "claude-opus-5-5")
					req.Header.Set("Authorization", "Bearer fixture-cookie")
					resp, e := Do(req, "", 42)
					if e != nil {
						t.Fatal(e)
					}
					defer func() { _ = resp.Body.Close() }()
					data, e := io.ReadAll(resp.Body)
					if e != nil {
						t.Fatal(e)
					}
					expectedStatus := http.StatusUnprocessableEntity
					if stream {
						expectedStatus = http.StatusOK
					} // Already-started SSE carries the failure event.
					if resp.StatusCode != expectedStatus || !strings.Contains(string(data), code) {
						t.Fatalf("refusal was reported as success: http=%d want=%d code=%s body=%s", resp.StatusCode, expectedStatus, code, data)
					}
					if !strings.Contains(string(data), observed) {
						t.Fatal("original refusal was replaced or dropped from the error diagnostic")
					}
					for _, success := range []string{`"response.completed"`, `"finish_reason":"stop"`, `"stop_reason":"end_turn"`} {
						if strings.Contains(string(data), success) {
							t.Errorf("refusal emitted successful terminal event %s", success)
						}
					}
					if stream {
						failure := map[string]string{"chat": `"error"`, "responses": `"response.failed"`, "messages": `"type":"error"`}[protocol]
						if !strings.Contains(string(data), failure) {
							t.Errorf("stream lacks explicit failure event: %s", data)
						}
					}
					mu.Lock()
					defer mu.Unlock()
					if creates != 1 || submissions != 1 {
						t.Fatalf("accepted refusal replayed: projects=%d submissions=%d", creates, submissions)
					}
					t.Logf("ACTUAL_REFUSAL protocol=%s stream=%t compact=%t http=%d code=%s original_text=true submissions=1", protocol, stream, compact, resp.StatusCode, code)
				})
			}
		}
	}
}
