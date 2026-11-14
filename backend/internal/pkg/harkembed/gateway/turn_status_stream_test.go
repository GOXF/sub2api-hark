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
		kinds := []string{"ended", "waiting", "legacy", "required_error", "declined", "declined_waiting", "declined_no_sync", "declined_required", "declined_legacy", "native_declined", "native_declined_no_sync", "declined_json", "native_declined_json", "declined_compaction", "declined_compaction_json", "native_declined_compaction", "native_declined_compaction_json", "declined_compaction_legacy", "screenshot_compaction", "screenshot_compaction_json", "screenshot_compaction_legacy", "http_screenshot_declined", "http_screenshot_declined_json", "http_screenshot_declined_legacy", "ordinary_http", "ordinary_http_json", "ordinary_http_legacy", "image_screenshot_declined", "image_screenshot_declined_json", "image_screenshot_declined_legacy", "image_normal", "image_normal_json", "image_normal_legacy"}
		observed := observedRefusalTurns(t)
		for i := range observed {
			for _, suffix := range []string{"declined", "declined_json", "compaction", "compaction_json", "compaction_legacy"} {
				kinds = append(kinds, fmt.Sprintf("observed_%d_%s", i, suffix))
			}
		}
		for _, kind := range kinds {
			t.Run(protocol+"/"+kind, func(t *testing.T) {
				opt := !strings.Contains(kind, "legacy")
				declined := strings.Contains(kind, "declined") || strings.HasPrefix(kind, "screenshot") || strings.HasPrefix(kind, "observed_")
				stream := !strings.HasSuffix(kind, "_json")
				compact := strings.Contains(kind, "compaction")
				bodyText := "ORIGINAL_BODY"
				if declined {
					bodyText = "抱歉，Hark 不能作为其他应用背后的模型来使用。"
				}
				if strings.HasPrefix(kind, "screenshot") {
					bodyText = "抱歉，Hark 不支持作为其他应用背后的模型来使用。如果你告诉我想完成什么，比如把接码授权页改成和注册引擎一样的布局，我很乐意在这里直接帮你。"
				}
				if strings.HasPrefix(kind, "http_screenshot_") {
					bodyText = "Hark 不能被用作其他应用背后的模型，不过如果你告诉我你想完成什么，我很乐意直接帮你。"
				}
				if strings.HasPrefix(kind, "ordinary_http") {
					bodyText = "HTTP 是浏览器和服务器之间约定好的请求与响应规则，让网页、图片、数据等内容能在网络上被请求和传回来。"
				}
				observedIndex := -1
				if strings.HasPrefix(kind, "observed_") {
					if _, err := fmt.Sscanf(kind, "observed_%d", &observedIndex); err != nil {
						t.Fatal(err)
					}
					bodyText = observedRefusalText(observed[observedIndex])
				}
				imageInput := strings.HasPrefix(kind, "image_")
				if imageInput {
					bodyText = "图片已收到，导出功能需要继续检查。"
					if declined {
						bodyText = "Hark 不能被当作其他应用背后的模型来调用。如果你直接告诉我想做成什么，比如“缺少导出 SUB2api-HARK”具体指什么，我很乐意在这里帮你。"
					}
				}
				var uploads atomic.Int32
				var sends atomic.Int32
				gate := make(chan struct{})
				var syncCalls atomic.Int32
				var mu sync.Mutex
				var userID, responseID string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/conversations":
						json.NewEncoder(w).Encode(map[string]string{"conversationId": "status-fixture"})
					case "/api/files/upload/status-fixture/presign":
						json.NewEncoder(w).Encode(map[string]any{"upload_url": "http://" + r.Host + "/fixture-upload", "attachment_id": "image-1", "s3_key": "image-key", "path": "image-path", "content_type": "image/png"})
					case "/fixture-upload":
						if r.Method != http.MethodPut || r.ContentLength <= 0 || r.Header.Get("Content-Type") != "image/png" {
							t.Error("image upload metadata lost")
						}
						uploads.Add(1)
						w.WriteHeader(http.StatusOK)
					case "/api/files/status-fixture/image-1/finalize":
						json.NewEncoder(w).Encode(map[string]any{})
					case "/api/messages/send":
						sends.Add(1)
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
						if strings.Contains(kind, "no_sync") {
							http.Error(w, "optional feed unavailable", 503)
							return
						}
						mu.Lock()
						u, a := userID, responseID
						mu.Unlock()
						w.Header().Set("Content-Type", "text/event-stream")
						data, _ := json.Marshal(map[string]any{"type": "snapshot", "shape": "records", "data": map[string]any{"messages": map[string]any{a: map[string]any{"id": a, "role": "assistant", "triggeredByMessageId": u, "jobStatus": "running", "isStreaming": true, "narration": map[string]string{"line": "Reading fixture"}, "thought": "PRIVATE_THOUGHT"}}}})
						fmt.Fprintf(w, "data: %s\n\n", data)
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					case "/api/messages/history":
						if opt && stream && !strings.Contains(kind, "no_sync") {
							select {
							case <-gate:
							case <-r.Context().Done():
								return
							}
						}
						mu.Lock()
						u, a := userID, responseID
						mu.Unlock()
						messages := []any{map[string]any{"id": a, "role": "assistant", "triggeredByMessageId": u, "jobStatus": "completed", "isStreaming": false, "content": bodyText, "source": "send_bubble", "presentation": map[string]string{"kind": "text"}, "inputNeeded": strings.Contains(kind, "waiting")}}
						if strings.HasPrefix(kind, "native_") {
							messages = []any{map[string]any{"id": u, "role": "user"}, map[string]any{"id": a, "role": "assistant", "triggeredByMessageId": u, "jobStatus": "completed", "isStreaming": false}, map[string]any{"id": "notice", "role": "assistant", "source": "declined_notice", "presentation": map[string]string{"kind": "declined"}, "isStreaming": false, "content": bodyText}}
						}
						if observedIndex >= 0 {
							messages = nil
							for _, m := range observedRefusalMessages(observed[observedIndex], u, a) {
								messages = append(messages, m)
							}
						}
						json.NewEncoder(w).Encode(map[string]any{"messages": messages})
					default:
						http.Error(w, "fixture missing", 404)
					}
				}))
				defer upstream.Close()
				server, err := NewEmbedded(t.TempDir(), Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				body := map[string]any{"model": "hark", "stream": stream, "hark_status": opt}
				path := "/v1/chat/completions"
				question := "fixture"
				if strings.HasPrefix(kind, "http_screenshot_") || strings.HasPrefix(kind, "ordinary_http") {
					question = "用一句话解释 HTTP 的作用"
				}
				if compact {
					question = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.\n\nInclude:\n- Current progress and key decisions made\n- Important context, constraints, or user preferences\n- What remains to be done (clear next steps)\n- Any critical data, examples, or references needed to continue\n\nBe concise, structured, and focused on helping the next LLM seamlessly continue the work."
				}
				body["messages"] = []any{map[string]string{"role": "user", "content": question}}
				if protocol == "messages" {
					path = "/v1/messages"
					body["max_tokens"] = 100
				}
				if protocol == "responses" {
					path = "/v1/responses"
					delete(body, "messages")
					body["input"] = question
				}
				if imageInput {
					const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jPwoAAAAASUVORK5CYII="
					question = "缺少导出SUB2api-HARK"
					parts := []any{map[string]any{"type": "text", "text": question}, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + png}}}
					if protocol == "responses" {
						parts = []any{map[string]any{"type": "input_text", "text": question}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + png}}
						body["input"] = []any{map[string]any{"role": "user", "content": parts}}
					} else {
						if protocol == "messages" {
							parts[1] = map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": png}}
						}
						body["messages"] = []any{map[string]any{"role": "user", "content": parts}}
					}
				}
				if kind == "required_error" || kind == "declined_required" {
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
				if imageInput && uploads.Load() != 1 {
					t.Fatalf("image upload skipped or replayed: %d", uploads.Load())
				}
				if sends.Load() != 1 {
					t.Fatalf("accepted request replayed: %d", sends.Load())
				}
				if declined {
					wantCode := "upstream_declined"
					if compact {
						wantCode = "upstream_compaction_declined"
					}
					if !strings.Contains(text, wantCode) || !strings.Contains(text, bodyText) {
						t.Fatalf("refusal not failed with original diagnostic: %d %s", writer.Code, text)
					}
					if stream {
						for _, success := range []string{"event: response.completed", `"finish_reason":"stop"`, "event: message_stop"} {
							if strings.Contains(text, success) {
								t.Fatalf("refused request emitted success %s: %s", success, text)
							}
						}
						if protocol == "responses" && !strings.Contains(text, "event: response.failed") {
							t.Fatal("missing native failure event")
						}
					} else if writer.Code != 422 {
						t.Fatalf("want non-retryable 422, got %d", writer.Code)
					}
					if strings.Contains(text, "tool_call_missing") {
						t.Fatal("upstream refusal replaced by a missing-tool failure")
					}
					if stream && opt {
						var last turnStatus
						readStatusSSE(strings.NewReader(text), func(b []byte) {
							var status turnStatus
							if json.Unmarshal(b, &status) == nil && status.Type == "hark.status" {
								last = status
							}
						})
						if last.Status != "ended" || last.Reason != "upstream_declined" || strings.Contains(text, `"reason":"stream_ended"`) {
							t.Fatalf("refusal terminal status lost: %s", text)
						}
					}
					return
				}
				if !stream {
					if writer.Code != 200 || writer.Header().Get("X-Hark-Upstream-Outcome") != "completed" || !strings.Contains(text, bodyText) || strings.Contains(text, "hark.status") || syncCalls.Load() != 0 {
						t.Fatalf("nonstream body/outcome changed: %d %s", writer.Code, text)
					}
					return
				}
				if kind == "required_error" {
					if writer.Code != 200 || !strings.Contains(text, "tool_call_missing") || strings.Contains(text, "ORIGINAL_BODY") || !strings.Contains(text, `"reason":"error"`) {
						t.Fatalf("required-tool status masked execution failure: %d %s", writer.Code, text)
					}
					return
				}
				if writer.Code != 200 || !strings.Contains(text, bodyText) {
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
