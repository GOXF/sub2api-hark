package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type durableRig struct {
	t                         *testing.T
	s                         *Server
	dir, body, path, upstream string
	mu                        sync.Mutex
	ready                     bool
	sends, gets, archives     int
	turns                     []map[string]any
	replyText                 string
	tailFailure               int
}

func durableCaller(t *testing.T, s *Server, owner string) http.Handler {
	t.Helper()
	p, ok := any(s).(interface{ CallerHandler(string) http.Handler })
	if !ok {
		t.Fatal("DURABLE_RECOVERY unavailable: no owner-bound delivery and acknowledgment path")
	}
	return p.CallerHandler(owner)
}

func durableControls(t *testing.T, s *Server) http.Handler {
	t.Helper()
	p, ok := any(s).(interface{ RecoveryControlHandler() http.Handler })
	if !ok {
		t.Fatal("DURABLE_RECOVERY unavailable: no persistent recovery controls")
	}
	return p.RecoveryControlHandler()
}

func newDurableRig(t *testing.T, protocol string, stream bool) *durableRig {
	t.Helper()
	x := &durableRig{t: t, dir: t.TempDir()}
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/conversations":
			_ = json.NewEncoder(w).Encode(map[string]string{"conversationId": "durable-original-project"})
		case "/api/messages/send":
			if r.Method != http.MethodPost {
				t.Error("wrong submission method")
			}
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			x.turns = append(x.turns, p)
			x.sends++
			if x.sends > 1 && x.tailFailure != 0 {
				w.WriteHeader(x.tailFailure)
				_, _ = w.Write([]byte(`{"error":"synthetic acceptance response lost"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "durable-original-project", "messageId": p["idempotencyKey"], "redirected": false})
		case "/api/messages/history":
			x.gets++
			if !x.ready {
				w.Header().Set("Retry-After", "60") // Exceed the live read-retry budget to exercise durable recovery.
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{"error":"fixture polling interrupted"}`))
				return
			}
			var messages []any
			for i, p := range x.turns {
				text := previewToolText
				if x.replyText != "" {
					text = x.replyText
				}
				if i > 0 {
					text = "Mock continuation completed"
				}
				messages = append(messages, map[string]any{"id": p["idempotencyKey"], "role": "user"}, map[string]any{"id": p["responseMessageId"], "role": "assistant", "triggeredByMessageId": p["idempotencyKey"], "status": "completed", "content": text})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": messages})
		default:
			if strings.Contains(r.URL.Path, "archive") {
				x.archives++
			}
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(u.Close)
	x.upstream = u.URL
	x.restart()
	x.path, x.body = previewRequest(protocol, stream)
	return x
}

func (x *durableRig) restart() {
	x.t.Helper()
	var err error
	x.s, err = NewEmbedded(x.dir, Options{Upstream: x.upstream, PollInterval: time.Millisecond})
	if err != nil {
		x.t.Fatal(err)
	}
}
func (x *durableRig) post(owner, path, body string, control bool) *httptest.ResponseRecorder {
	x.t.Helper()
	h := durableCaller(x.t, x.s, owner)
	if control {
		h = durableControls(x.t, x.s)
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if !control {
		r.Header.Set("X-Hark-Session-Id", "original-session")
	}
	r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "sub2api-50", Cookie: "fixture-cookie"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func (x *durableRig) prepare() *httptest.ResponseRecorder {
	x.t.Helper()
	x.mu.Lock()
	turn := x.turns[0]
	x.ready = true
	x.mu.Unlock()
	var obj any
	_ = json.Unmarshal([]byte(x.body), &obj)
	canonical, _ := json.Marshal(obj)
	text := previewToolText
	if x.replyText != "" {
		text = x.replyText
	}
	b, _ := json.Marshal(map[string]any{"owner_id": "fixture-key-A", "request_id": turn["idempotencyKey"], "response_message_id": turn["responseMessageId"], "request_sha256": previewSHA(canonical), "result_sha256": previewSHA([]byte(text))})
	return x.post("", "/v1/sessions/original-session/recovery/prepare", string(b), true)
}

func TestDurableRecoveryLifecycle(t *testing.T) {
	x := newDurableRig(t, "responses", false)
	w := x.post("fixture-key-A", x.path, x.body, false)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "upstream_turn_interrupted") {
		t.Fatalf("initial interruption not preserved: %d %s", w.Code, w.Body)
	}
	x.restart()
	w = x.prepare()
	if w.Code != 200 {
		t.Fatalf("prepare failed: %d %s", w.Code, w.Body)
	}
	x.restart()
	w = x.post("fixture-key-A", x.path, x.body, false)
	if w.Code != 200 {
		t.Fatalf("persisted delivery unavailable: %d %s", w.Code, w.Body)
	}
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &envelope)
	var items []map[string]any
	_ = json.Unmarshal(envelope["output"], &items)
	if len(items) != 1 || items[0]["name"] != "fixture_probe" {
		t.Fatalf("missing recovered mock call: %s", w.Body)
	}
	callID, _ := items[0]["call_id"].(string)
	if !strings.HasPrefix(callID, "call_") {
		t.Fatal("invalid stable call ID")
	}
	before, _ := os.ReadFile(filepath.Join(x.dir, "project-sessions.json"))
	x.restart()
	again := x.post("fixture-key-A", x.path, x.body, false)
	if again.Code != 409 || !strings.Contains(again.Body.String(), "recovery_delivery_uncertain") {
		t.Fatalf("duplicate tool execution was possible: %d %s", again.Code, again.Body)
	}
	var b map[string]any
	_ = json.Unmarshal([]byte(x.body), &b)
	b["tool_choice"] = "auto"
	b["input"] = []any{map[string]any{"role": "user", "content": "Inspect mock fixture only"}, map[string]any{"type": "function_call", "call_id": callID, "name": "fixture_probe", "arguments": `{"id":"fixture-only"}`}, map[string]any{"type": "function_call_output", "call_id": callID, "output": "MOCK_RESULT durable fixture"}}
	tail, _ := json.Marshal(b)
	w = x.post("fixture-key-A", x.path, string(tail), false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Mock continuation completed") {
		t.Fatalf("tail-only continuation failed: %d %s", w.Code, w.Body)
	}
	x.restart()
	again = x.post("fixture-key-A", x.path, string(tail), false)
	if again.Code != 409 {
		t.Fatalf("ack/tail replay was not idempotent: %d %s", again.Code, again.Body)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.sends != 2 || x.archives != 0 || len(x.turns) != 2 {
		t.Fatalf("replayed original task: sends=%d archives=%d", x.sends, x.archives)
	}
	sent, _ := x.turns[1]["message"].(string)
	if strings.Contains(sent, "Inspect mock fixture only") || !strings.Contains(sent, "MOCK_RESULT durable fixture") {
		t.Fatal("original task or assistant call was resent instead of only tool output")
	}
	after, _ := os.ReadFile(filepath.Join(x.dir, "project-sessions.json"))
	if bytes.Equal(before, after) {
		t.Fatal("verified acknowledgment/continuation was not persisted")
	}
	t.Log(fmt.Sprintf("DURABLE_RECOVERY initial_send=1 recovered_delivery=1 duplicate_delivery=409 tool_output_tail=1 duplicate_tail=409 archives=0 restart_preserved=true"))
}
