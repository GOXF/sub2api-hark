package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type statusTestRecorder struct {
	*httptest.ResponseRecorder
	gate chan struct{}
	once sync.Once
}

func (w *statusTestRecorder) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte("hark.status")) {
		w.once.Do(func() { close(w.gate) })
	}
	return w.ResponseRecorder.Write(b)
}

func TestTurnStatusStreamingProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		for _, kind := range []string{"ended", "waiting", "legacy", "required_error"} {
			t.Run(protocol+"/"+kind, func(t *testing.T) {
				opt := kind != "legacy"
				gate := make(chan struct{})
				var syncCalls atomic.Int32
				var mu sync.Mutex
				var userID, responseID string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/conversations":
						json.NewEncoder(w).Encode(map[string]string{"conversationId": "status-fixture"})
					case "/api/messages/send":
						var p map[string]json.RawMessage
						json.NewDecoder(r.Body).Decode(&p)
						if _, leaked := p["hark_status"]; leaked {
							t.Error("internal flag leaked to upstream")
						}
						mu.Lock()
						json.Unmarshal(p["idempotencyKey"], &userID)
						json.Unmarshal(p["responseMessageId"], &responseID)
						u := userID
						mu.Unlock()
						json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "status-fixture", "messageId": u, "redirected": false})
					case "/api/sync/conversation":
						syncCalls.Add(1)
						mu.Lock()
						u, a := userID, responseID
						mu.Unlock()
						w.Header().Set("Content-Type", "text/event-stream")
						data, _ := json.Marshal(map[string]any{"type": "snapshot", "shape": "records", "data": map[string]any{"messages": map[string]any{a: map[string]any{"id": a, "role": "assistant", "triggeredByMessageId": u, "jobStatus": "running", "isStreaming": true, "narration": map[string]string{"line": "Reading fixture"}, "thought": "PRIVATE_THOUGHT"}}}})
						fmt.Fprintf(w, "data: %s\n\n", data)
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					case "/api/messages/history":
						if opt {
							select {
							case <-gate:
							case <-r.Context().Done():
								return
							}
						}
						mu.Lock()
						u, a := userID, responseID
						mu.Unlock()
						json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": a, "role": "assistant", "triggeredByMessageId": u, "jobStatus": "completed", "isStreaming": false, "content": "ORIGINAL_BODY", "inputNeeded": kind == "waiting"}}})
					default:
						http.Error(w, "fixture missing", 404)
					}
				}))
				defer upstream.Close()
				server, err := NewEmbedded(t.TempDir(), Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				body := map[string]any{"model": "hark", "stream": true, "hark_status": opt}
				path := "/v1/chat/completions"
				body["messages"] = []any{map[string]string{"role": "user", "content": "fixture"}}
				if protocol == "messages" {
					path = "/v1/messages"
					body["max_tokens"] = 100
				}
				if protocol == "responses" {
					path = "/v1/responses"
					delete(body, "messages")
					body["input"] = "fixture"
				}
				if kind == "required_error" {
					body["tool_choice"] = "required"
					body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "fixture_read", "parameters": map[string]string{"type": "object"}}}}
					if protocol == "responses" {
						body["tools"] = []any{map[string]any{"type": "function", "name": "fixture_read", "parameters": map[string]string{"type": "object"}}}
					}
					if protocol == "messages" {
						body["tool_choice"] = map[string]string{"type": "any"}
						body["tools"] = []any{map[string]any{"name": "fixture_read", "input_schema": map[string]string{"type": "object"}}}
					}
				}
				raw, _ := json.Marshal(body)
				req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				ctx, cancel := context.WithTimeout(WithEmbeddedAccount(req.Context(), SourceAccount{ID: "status-account", Cookie: "fixture-cookie"}), 5*time.Second)
				defer cancel()
				req = req.WithContext(ctx)
				writer := &statusTestRecorder{ResponseRecorder: httptest.NewRecorder(), gate: gate}
				server.EmbeddedHandler().ServeHTTP(writer, req)
				text := writer.Body.String()
				if kind == "required_error" {
					if writer.Code != 200 || !strings.Contains(text, "tool_call_missing") || strings.Contains(text, "ORIGINAL_BODY") || !strings.Contains(text, `"reason":"error"`) {
						t.Fatalf("required-tool status masked execution failure: %d %s", writer.Code, text)
					}
					return
				}
				if writer.Code != 200 || !strings.Contains(text, "ORIGINAL_BODY") {
					t.Fatalf("body changed: %d %s", writer.Code, text)
				}
				if strings.Contains(text, "PRIVATE_THOUGHT") {
					t.Fatal("private reasoning leaked")
				}
				if !opt {
					if syncCalls.Load() != 0 || strings.Contains(text, "hark.status") {
						t.Fatal("legacy client changed")
					}
					return
				}
				if syncCalls.Load() != 1 || !strings.Contains(text, `"line":"Reading fixture"`) {
					t.Fatalf("no live independent status: %s", text)
				}
				want := `"status":"ended"`
				if kind == "waiting" {
					want = `"status":"waiting_for_user"`
					if strings.Contains(text, `"status":"ended"`) {
						t.Fatal("waiting overwritten")
					}
				}
				if !strings.Contains(text, want) {
					t.Fatalf("missing %s: %s", want, text)
				}
			})
		}
	}
}

func TestTurnStatusSSEFraming(t *testing.T) {
	var got []string
	readStatusSSE(strings.NewReader(":ping\r\n\r\ndata: {\r\ndata: \"type\":\"patches\",\"data\":[]\r\ndata: }\r\n\r\ndata: incomplete"), func(b []byte) {
		if !json.Valid(b) {
			t.Fatal("invalid multiline frame")
		}
		got = append(got, string(b))
	})
	if len(got) != 1 {
		t.Fatal("partial or heartbeat became status")
	}
}

func TestTurnStatusLateAwaitingPersonBeforeFinalReply(t *testing.T) {
	var mu sync.Mutex
	var user, assistant string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/messages/send":
			var p map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&p)
			mu.Lock()
			json.Unmarshal(p["idempotencyKey"], &user)
			json.Unmarshal(p["responseMessageId"], &assistant)
			u := user
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "c", "messageId": u, "redirected": false})
		case "/api/messages/history":
			mu.Lock()
			u, a := user, assistant
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": a, "role": "assistant", "triggeredByMessageId": u, "isStreaming": false, "jobStatus": "completed", "content": "UNCHANGED"}}})
		case "/api/sync/conversation":
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			u, a := user, assistant
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			data, _ := json.Marshal(map[string]any{"type": "snapshot", "data": map[string]any{"messages": map[string]any{a: map[string]any{"id": a, "role": "assistant", "triggeredByMessageId": u, "isStreaming": false, "jobStatus": "completed"}}, "requests": map[string]any{u: map[string]any{"messageId": u, "revision": 3, "awaitsPerson": true, "state": "open"}}}})
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	defer upstream.Close()
	server, err := NewEmbedded(t.TempDir(), Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var got []turnStatus
	reply, err := server.sendProjectPayload(context.Background(), "fixture", "c", map[string]any{"conversationId": "c", "message": "fixture"}, time.Millisecond, turnHooks{status: func(s turnStatus) { got = append(got, s) }})
	if err != nil || reply.Text != "UNCHANGED" || len(got) == 0 || got[len(got)-1].Status != "waiting_for_user" {
		t.Fatalf("late request state lost: %s %+v %v", reply.Text, got, err)
	}
}
