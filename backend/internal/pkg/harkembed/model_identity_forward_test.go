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
	"github.com/tidwall/gjson"
)

func TestIdentityQuestionsUseUpstreamGeneration(t *testing.T) {
	testIdentityUpstreamGeneration(t, "natural")
}

func TestMappedIdentityUpstreamRepliesAreAdapted(t *testing.T) {
	testIdentityUpstreamGeneration(t, "hark")
}

func TestIdentityIntentUsesUpstreamGeneration(t *testing.T) {
	for _, mode := range []string{"nonbrand", "examples", "internals", "developer"} {
		t.Run(mode, func(t *testing.T) { testIdentityUpstreamGeneration(t, mode) })
	}
}

func testIdentityUpstreamGeneration(t *testing.T, mode string) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, mixed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/mixed=%t", protocol, stream, mixed), func(t *testing.T) {
					t.Setenv("HARK_USAGE_ESTIMATE", "off")
					t.Setenv("HARK_DATA_DIR", t.TempDir())
					model := "claude-opus-5-5"
					if protocol == "responses" {
						model = "claude-sonnet-4-6"
					}
					question := "你是什么模型？"
					if mixed {
						question += "再分两点解释会话恢复与继续任务的区别。"
					}
					answer := "当前会话使用的是 " + model + "。这是上游自然生成的完整回答。"
					if mixed {
						answer += "一、恢复会话加载已有上下文。二、继续任务明确启动下一回合。"
					}
					upstreamAnswer := answer
					if mode == "hark" || mode == "nonbrand" {
						upstreamAnswer = "Hark 会根据任务分配不同模型，我无法确认当前实际模型。"
						if mode == "nonbrand" {
							upstreamAnswer = "配置名是 " + model + "，底层实际是哪个模型我确认不了。"
						}
						answer = "当前会话配置的模型是 " + model + "。"
						if mixed {
							remainder := "一、恢复会话加载已有上下文。二、继续任务明确启动下一回合。"
							upstreamAnswer += "\n\n" + remainder
							answer += "\n\n" + remainder
						}
					}
					if mode == "examples" {
						question, upstreamAnswer, answer = "问使用模型还有其他问法吗？", identityScreenshotReply, identityExamplesZH
						if mixed {
							question = "问使用模型还有其他问法吗？再解释恢复会话。"
							upstreamAnswer += "\n\n保留此任务正文：恢复历史，不重放工具。"
							answer = upstreamAnswer
						}
					}
					if mode == "internals" || mode == "developer" {
						question = "你底层实际用的是什么模型？"
						if mode == "developer" {
							question = "谁开发的你？"
						}
						upstreamAnswer = "模型配置是 " + model + "，此问题与公开配置名不同。"
						if mixed {
							question += "再解释会话恢复。"
							upstreamAnswer += "恢复会话保留历史。"
						}
						answer = upstreamAnswer
					}
					var mu sync.Mutex
					var sent map[string]string
					submissions := 0
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/api/conversations":
							_ = json.NewEncoder(w).Encode(map[string]string{"conversationId": "fixture-project"})
						case "/api/messages/send":
							submissions++
							var body map[string]json.RawMessage
							if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
								t.Error(err)
							}
							sent = make(map[string]string)
							for _, key := range []string{"conversationId", "message", "idempotencyKey", "responseMessageId"} {
								var value string
								if err := json.Unmarshal(body[key], &value); err != nil {
									t.Error(err)
								}
								sent[key] = value
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": sent["conversationId"], "messageId": sent["idempotencyKey"], "redirected": false})
						case "/api/messages/history":
							_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{
								"id": sent["responseMessageId"], "role": "assistant", "triggeredByMessageId": sent["idempotencyKey"],
								"jobStatus": "completed", "isStreaming": false, "content": upstreamAnswer,
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
					path, body := identityForwardRequest(protocol, question, stream)
					req := labelRequest(t, path, body, model)
					req.Header.Set("Authorization", "Bearer fixture-cookie")
					resp, err := Do(req, "", 42)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					data, err := io.ReadAll(resp.Body)
					if err != nil || resp.StatusCode != http.StatusOK {
						t.Fatalf("status=%d read=%v body=%s", resp.StatusCode, err, data)
					}
					got := identityForwardText(protocol, stream, string(data))
					if got != answer {
						t.Errorf("upstream answer was replaced: got=%q want=%q", got, answer)
					}
					mu.Lock()
					defer mu.Unlock()
					if submissions != 1 {
						t.Fatalf("upstream submissions=%d; want=1 (no gateway identity shortcut)", submissions)
					}
					for _, want := range []string{modelLabelNote("", model), "Keep original ACP context.", "Prior checkpoint A", "read_file", question} {
						if !strings.Contains(sent["message"], want) {
							t.Errorf("forwarded context missing %q: %s", want, sent["message"])
						}
					}
				})
			}
		}
	}
}

func TestDefaultModelLabelAllowsNaturalAnswers(t *testing.T) {
	note := modelLabelNote("", "claude-opus-5-5")
	for _, want := range []string{"claude-opus-5-5", "naturally", "unless the user requests", "answer every part", "public API configuration", "不要只回答平台品牌"} {
		if !strings.Contains(note, want) {
			t.Errorf("natural-answer guidance missing %q", want)
		}
	}
	if strings.Contains(note, "直接回答“claude-opus-5-5”") || strings.Contains(note, `answer "claude-opus-5-5" directly`) {
		t.Error("identity note still forces a bare model-name reply")
	}
}

func identityForwardRequest(protocol, question string, stream bool) (string, string) {
	messages := []any{map[string]string{"role": "user", "content": "Prior checkpoint A"}, map[string]string{"role": "assistant", "content": "Checkpoint retained"}, map[string]string{"role": "user", "content": question}}
	body := map[string]any{"stream": stream, "max_tokens": 256, "messages": messages}
	path := "/v1/chat/completions"
	switch protocol {
	case "responses":
		path = "/v1/responses"
		delete(body, "messages")
		body["input"], body["instructions"] = messages, "Keep original ACP context."
		body["tools"] = []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]string{"type": "object"}}}
	case "messages":
		path = "/v1/messages"
		body["system"] = "Keep original ACP context."
		body["tools"] = []any{map[string]any{"name": "read_file", "input_schema": map[string]string{"type": "object"}}}
	default:
		body["messages"] = append([]any{map[string]string{"role": "system", "content": "Keep original ACP context."}}, messages...)
		body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "parameters": map[string]string{"type": "object"}}}}
	}
	data, _ := json.Marshal(body)
	return path, string(data)
}

func identityForwardText(protocol string, stream bool, body string) string {
	path := map[string]string{"chat": "choices.0.message.content", "responses": "output.0.content.0.text", "messages": "content.0.text"}[protocol]
	if !stream {
		return gjson.Get(body, path).Str
	}
	var text strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		switch protocol {
		case "chat":
			text.WriteString(gjson.Get(data, "choices.0.delta.content").Str)
		case "responses":
			if gjson.Get(data, "type").Str == "response.output_text.delta" {
				text.WriteString(gjson.Get(data, "delta").Str)
			}
		case "messages":
			text.WriteString(gjson.Get(data, "delta.text").Str)
		}
	}
	return text.String()
}
