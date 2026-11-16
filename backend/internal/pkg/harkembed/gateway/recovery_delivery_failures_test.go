package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDurableRecoveryFailures(t *testing.T) {
	for _, tc := range []string{"wrong-owner", "lost-wire", "store-failure", "tool-receipt-not-clear", "cleanup-pending", "tail-missing-output", "changed-tools", "parallel-issue"} {
		t.Run(tc, func(t *testing.T) {
			x := newDurableRig(t, "responses", false)
			w := x.post("fixture-key-A", x.path, x.body, false)
			if w.Code != 409 {
				t.Fatal(w.Body)
			}
			if w = x.prepare(); w.Code != 200 {
				t.Fatal(w.Body)
			}
			path := filepath.Join(x.dir, "project-sessions.json")
			before, _ := os.ReadFile(path)
			switch tc {
			case "wrong-owner":
				w = x.post("fixture-key-B", x.path, x.body, false)
				if w.Code != 403 {
					t.Fatal("foreign owner read original tools")
				}
			case "lost-wire":
				r := httptest.NewRequest(http.MethodPost, x.path, strings.NewReader(x.body))
				r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "sub2api-50", Cookie: "fixture-cookie"}))
				broken := &brokenRecoveryWriter{header: http.Header{}}
				durableCaller(t, x.s, "fixture-key-A").ServeHTTP(broken, r)
				x.restart()
				w = x.post("fixture-key-A", x.path, x.body, false)
				if w.Code != 409 || broken.frames != 1 {
					t.Fatal("dropped wire allowed a duplicate tool delivery")
				}
			case "store-failure":
				x.s.data = filepath.Join(x.dir, "missing-parent")
				w = x.post("fixture-key-A", x.path, x.body, false)
				if w.Code != 503 || strings.Contains(w.Body.String(), "call_") {
					t.Fatal("tools emitted before durable issuance")
				}
				x.s.data = x.dir
				x.restart()
				w = x.post("fixture-key-A", x.path, x.body, false)
				if w.Code != 200 {
					t.Fatal("failed save poisoned ready record")
				}
			case "tool-receipt-not-clear":
				w = x.post("fixture-key-A", x.path, x.body, false)
				var ack []byte
				for _, p := range x.s.projects {
					ack, _ = json.Marshal(map[string]any{"owner_id": "fixture-key-A", "delivery_id": p.Recovery.ID, "result_sha256": p.Recovery.ResultHash, "installed": true})
				}
				w = x.post("", "/v1/sessions/original-session/recovery/ack", string(ack), true)
				if w.Code != 409 {
					t.Fatal("boolean receipt cleared unexecuted tools")
				}
			case "cleanup-pending":
				x.s.cleanupProjects(time.Now().Add(48*time.Hour), time.Minute)
				if len(x.s.projects) != 1 {
					t.Fatal("pending Project archived by cleanup")
				}
			case "tail-missing-output", "changed-tools":
				w = x.post("fixture-key-A", x.path, x.body, false)
				body := strings.Replace(x.body, "Inspect mock fixture only", "Different user task", 1)
				if tc == "changed-tools" {
					body = strings.Replace(x.body, `"fixture_probe"`, `"other_tool"`, 1)
				}
				w = x.post("fixture-key-A", x.path, body, false)
				if w.Code != 409 {
					t.Fatal("invalid continuation receipt accepted")
				}
			case "parallel-issue":
				var wg sync.WaitGroup
				codes := make(chan int, 8)
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); w := x.post("fixture-key-A", x.path, x.body, false); codes <- w.Code }()
				}
				wg.Wait()
				close(codes)
				ok, conflicts := 0, 0
				for code := range codes {
					if code == 200 {
						ok++
					} else if code == 409 {
						conflicts++
					}
				}
				if ok != 1 || conflicts != 7 {
					t.Fatalf("concurrent issuance: ok=%d conflicts=%d", ok, conflicts)
				}
			}
			x.mu.Lock()
			sends, archives := x.sends, x.archives
			x.mu.Unlock()
			if sends != 1 || archives != 0 {
				t.Fatal("guard submitted or archived original task")
			}
			if tc == "wrong-owner" || tc == "cleanup-pending" {
				after, _ := os.ReadFile(path)
				if !bytes.Equal(before, after) {
					t.Fatal("non-mutating check changed original session")
				}
			}
		})
	}
}

type brokenRecoveryWriter struct {
	header http.Header
	frames int
}

func (w *brokenRecoveryWriter) Header() http.Header { return w.header }
func (w *brokenRecoveryWriter) WriteHeader(int)     {}
func (w *brokenRecoveryWriter) Write([]byte) (int, error) {
	w.frames++
	return 0, fmt.Errorf("synthetic client disconnected")
}

func TestDurableRecoveryPlainAck(t *testing.T) {
	x := newDurableRig(t, "responses", false)
	x.replyText = "Saved plain result"
	x.body = strings.Replace(x.body, `"tool_choice":"required"`, `"tool_choice":"auto"`, 1)
	if w := x.post("fixture-key-A", x.path, x.body, false); w.Code != 409 {
		t.Fatal(w.Body)
	}
	if w := x.prepare(); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := x.post("fixture-key-A", x.path, x.body, false); w.Code != 200 || !strings.Contains(w.Body.String(), "Saved plain result") {
		t.Fatal(w.Body)
	}
	var ack []byte
	for _, p := range x.s.projects {
		ack, _ = json.Marshal(map[string]any{"owner_id": "fixture-key-A", "delivery_id": p.Recovery.ID, "result_sha256": p.Recovery.ResultHash, "installed": true})
	}
	for i := 0; i < 2; i++ {
		x.restart()
		w := x.post("", "/v1/sessions/original-session/recovery/ack", string(ack), true)
		if w.Code != 200 {
			t.Fatalf("explicit ack not idempotent: %d %s", w.Code, w.Body)
		}
	}
	for _, p := range x.s.projects {
		if p.PendingTurn != nil || p.Recovery.State != "acknowledged" {
			t.Fatal("verified plain installation acknowledgment not applied")
		}
	}
}

func durableTail(protocol, body, callID string) string {
	var b map[string]any
	_ = json.Unmarshal([]byte(body), &b)
	switch protocol {
	case "responses":
		b["tool_choice"] = "auto"
		b["input"] = []any{map[string]any{"role": "user", "content": "Inspect mock fixture only"}, map[string]any{"type": "function_call", "call_id": callID, "name": "fixture_probe", "arguments": `{"id":"fixture-only"}`}, map[string]any{"type": "function_call_output", "call_id": callID, "output": "MOCK_RESULT durable fixture"}}
	case "chat":
		b["tool_choice"] = "auto"
		b["messages"] = []any{map[string]any{"role": "user", "content": "Inspect mock fixture only"}, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "fixture_probe", "arguments": `{"id":"fixture-only"}`}}}}, map[string]any{"role": "tool", "tool_call_id": callID, "content": "MOCK_RESULT durable fixture"}}
	case "messages":
		b["tool_choice"] = map[string]string{"type": "auto"}
		b["messages"] = []any{map[string]any{"role": "user", "content": "Inspect mock fixture only"}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": callID, "name": "fixture_probe", "input": map[string]string{"id": "fixture-only"}}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "MOCK_RESULT durable fixture"}}}}
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

func TestDurableRecoveryProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				x := newDurableRig(t, protocol, stream)
				w := x.post("fixture-key-A", x.path, x.body, false)
				if !strings.Contains(w.Body.String(), "upstream_turn_interrupted") {
					t.Fatal("missing accepted interruption")
				}
				if w = x.prepare(); w.Code != 200 {
					t.Fatal(w.Body)
				}
				x.restart()
				w = x.post("fixture-key-A", x.path, x.body, false)
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
				callID := regexp.MustCompile(`(?:call_|toolu_)[a-f0-9]+`).FindString(w.Body.String())
				if callID == "" {
					t.Fatal("missing standard tool call")
				}
				w = x.post("fixture-key-A", x.path, durableTail(protocol, x.body, callID), false)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "Mock continuation completed") {
					t.Fatalf("protocol tail failed: %d %s", w.Code, w.Body)
				}
				x.mu.Lock()
				defer x.mu.Unlock()
				if x.sends != 2 || x.archives != 0 {
					t.Fatal("replayed original request")
				}
				t.Logf("DURABLE_PROTOCOL %s stream=%t original=1 recovery=1 tool_tail=1 archives=0", protocol, stream)
			})
		}
	}
}
