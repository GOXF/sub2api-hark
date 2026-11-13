package harkembed

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

func TestRequiredClientToolsContract(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, choice, reply, code string
			}{
				{"missing", "required", "I will inspect the directory, but no command came back.", "tool_call_missing"},
				{"malformed", "required", `<tool_call>{"name":`, "tool_call_missing"},
				{"undeclared", "required", `<tool_call>{"name":"not_declared","arguments":{}}</tool_call>`, "tool_call_missing"},
				{"wrong_named", "read_file", `<tool_call>{"name":"other_file","arguments":{}}</tool_call>`, "tool_call_mismatch"},
				{"required_valid", "required", `<tool_call>{"name":"read_file","arguments":{"path":"fixture.txt"}}</tool_call>`, ""},
				{"named_valid", "read_file", `<tool_call>{"name":"read_file","arguments":{"path":"fixture.txt"}}</tool_call>`, ""},
				{"reserved_auto_missing", "auto", "This is not a tool call.", "tool_call_missing"},
				{"reserved_none_missing", "none", "This is not a tool call.", "tool_call_missing"},
				{"reserved_required_wrong", "required", `<tool_call>{"name":"read_file","arguments":{}}</tool_call>`, "tool_call_mismatch"},
				{"reserved_required_valid", "required", `<tool_call>{"name":"required","arguments":{}}</tool_call>`, ""},
				{"auto_plain", "auto", "This is an ordinary explanation, not a tool execution receipt.", ""},
				{"none_plain", "none", "This is an ordinary explanation, not a tool execution receipt.", ""},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", protocol, stream, tc.name), func(t *testing.T) {
					t.Setenv("HARK_DATA_DIR", t.TempDir())
					t.Setenv("HARK_USAGE_ESTIMATE", "off")
					var mu sync.Mutex
					var sent map[string]string
					submissions, polls := 0, 0
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/api/conversations":
							_ = json.NewEncoder(w).Encode(map[string]string{"conversationId": "client-tools-fixture"})
						case "/api/messages/send":
							submissions++
							var payload map[string]json.RawMessage
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								t.Error(err)
							}
							sent = make(map[string]string)
							for _, key := range []string{"conversationId", "idempotencyKey", "responseMessageId"} {
								var sentValue string
								if err := json.Unmarshal(payload[key], &sentValue); err != nil {
									t.Error(err)
								}
								sent[key] = sentValue
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": sent["conversationId"], "messageId": sent["idempotencyKey"], "redirected": false})
						case "/api/messages/history":
							polls++
							text, status := tc.reply, "completed"
							if stream && polls == 1 {
								text, status = "UNVERIFIED_EXECUTION_PROMISE", "running"
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{
								"id": sent["responseMessageId"], "role": "assistant", "triggeredByMessageId": sent["idempotencyKey"],
								"jobStatus": status, "isStreaming": status != "completed", "content": text,
							}}})
						default:
							http.Error(w, "unexpected fixture request", http.StatusNotFound)
						}
					}))
					t.Cleanup(upstream.Close)
					embedded, err := gateway.NewEmbedded(t.TempDir(), gateway.Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
					if err != nil {
						t.Fatal(err)
					}
					previousServer, err := get()
					if err != nil {
						t.Fatal(err)
					}
					server = embedded
					t.Cleanup(func() { server = previousServer })
					path, body := identityForwardRequest(protocol, "检查 fixture.txt，先调用工具再报告结果。", stream)
					var request map[string]any
					if err := json.Unmarshal([]byte(body), &request); err != nil {
						t.Fatal(err)
					}
					named := tc.choice == "read_file" || strings.HasPrefix(tc.name, "reserved_")
					if protocol == "messages" {
						request["tool_choice"] = map[string]string{"type": "auto"}
						if named {
							request["tool_choice"] = map[string]string{"type": "tool", "name": tc.choice}
						} else if tc.choice == "required" {
							request["tool_choice"] = map[string]string{"type": "any"}
						} else if tc.choice == "none" {
							request["tool_choice"] = map[string]string{"type": "none"}
						}
					} else if named {
						request["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": tc.choice}}
					} else {
						request["tool_choice"] = tc.choice
					}
					tools, ok := request["tools"].([]any)
					if !ok {
						t.Fatal("fixture tools must be an array")
					}
					// A second declared tool makes the wrong-named result parseable.
					switch protocol {
					case "chat":
						request["tools"] = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": "other_file", "parameters": map[string]string{"type": "object"}}})
					case "responses":
						request["tools"] = append(tools, map[string]any{"type": "function", "name": "other_file", "parameters": map[string]string{"type": "object"}})
					default:
						request["tools"] = append(tools, map[string]any{"name": "other_file", "input_schema": map[string]string{"type": "object"}})
					}
					if strings.HasPrefix(tc.name, "reserved_") {
						var declared map[string]any
						switch protocol {
						case "chat":
							declared = map[string]any{"type": "function", "function": map[string]any{"name": tc.choice, "parameters": map[string]string{"type": "object"}}}
						case "responses":
							declared = map[string]any{"type": "function", "name": tc.choice, "parameters": map[string]string{"type": "object"}}
						default:
							declared = map[string]any{"name": tc.choice, "input_schema": map[string]string{"type": "object"}}
						}
						tools, ok = request["tools"].([]any)
						if !ok {
							t.Fatal("fixture tools must be an array")
						}
						request["tools"] = append(tools, declared)
					}
					encoded, err := json.Marshal(request)
					if err != nil {
						t.Fatal(err)
					}
					req := labelRequest(t, path, string(encoded), "fixture-public-model")
					req.Header.Set("Authorization", "Bearer fixture-cookie")
					resp, err := Do(req, "", 42)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = resp.Body.Close() }()
					data, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					wantTool := "read_file"
					if strings.HasPrefix(tc.name, "reserved_") {
						wantTool = tc.choice
					}
					if tc.code != "" {
						if !strings.Contains(string(data), tc.code) || (!stream && resp.StatusCode != http.StatusUnprocessableEntity) {
							t.Fatalf("missing contract failure: status=%d body=%s", resp.StatusCode, data)
						}
						for _, forbidden := range []string{"UNVERIFIED_EXECUTION_PROMISE", tc.reply, `"response.completed"`, `"finish_reason":"stop"`, `"stop_reason":"end_turn"`} {
							if strings.Contains(string(data), forbidden) {
								t.Errorf("failed tool contract leaked a successful text/result: %q body=%s", forbidden, data)
							}
						}
					} else if resp.StatusCode != http.StatusOK {
						t.Fatalf("valid/ordinary request failed: status=%d body=%s", resp.StatusCode, data)
					} else if strings.Contains(tc.name, "valid") && !strings.Contains(string(data), wantTool) {
						t.Fatalf("declared tool call disappeared: %s", data)
					} else if strings.HasSuffix(tc.name, "plain") && !strings.Contains(identityForwardText(protocol, stream, string(data)), tc.reply) {
						t.Fatalf("ordinary non-tool answer changed: %s", data)
					}
					mu.Lock()
					defer mu.Unlock()
					if submissions != 1 {
						t.Fatalf("accepted task replayed: submissions=%d", submissions)
					}
				})
			}
		}
	}
}
