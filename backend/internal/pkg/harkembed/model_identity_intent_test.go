package harkembed

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const identityExamplesZH = "可以这样问：“你是什么模型？”“你现在使用哪个模型？”“当前会话的模型名是什么？”"
const identityExamplesEN = `You can ask: "What model are you?", "Which model are you using?", or "What is the model configured for this conversation?"`
const identityScreenshotReply = "有，常见的比如「你是哪个模型」「你是 GPT 还是 Claude」「你底层用的是什么」「你是哪个版本」「谁开发的你」。不管怎么问，我的答案都一样：配置名是 claude-opus-5-5，底层实际是哪个模型我确认不了。"

func TestIdentityDisclaimerParagraphBoundary(t *testing.T) {
	text := "\n\n底层实际是哪个模型我确认不了。"
	if got := trimIdentityDisclaimerParagraphs(text); got != "" {
		t.Fatalf("remaining=%q parts=%q recognized=%t", got, identityParagraphBreak.Split(text, -1), isIdentityDisclaimer("底层实际是哪个模型我确认不了。"))
	}
}

func TestMappedIdentityIntentRouting(t *testing.T) {
	for _, tc := range []struct {
		question, prior string
		eligible        bool
		answer          string
	}{
		{"你是什么模型？", "", true, "当前会话配置的模型是 claude-opus-5-5。"},
		{"你现在用的是什么模型？", "", true, "当前会话配置的模型是 claude-opus-5-5。"},
		{"请问你使用的是哪个模型呢？", "", true, "当前会话配置的模型是 claude-opus-5-5。"},
		{"你是 GPT 还是 Claude？", "", true, "当前会话配置的模型是 claude-opus-5-5。"},
		{"问使用模型还有其他问法吗?", "", true, identityExamplesZH},
		{"询问模型身份还有哪些问法？", "", true, identityExamplesZH},
		{"What are other ways to ask which model you use?", "", true, identityExamplesEN},
		{"还有其他问法吗？", "你是什么模型？", true, identityExamplesZH},
		{"还有其它问法吗？", "问使用模型还有其他问法吗？", true, identityExamplesZH},
		{"你是哪个版本？", "你是什么模型？", true, "当前会话配置的模型是 claude-opus-5-5。"},
		{"还有其他问法吗？", "帮我写一封邮件。", false, ""},
		{"你是哪个版本？", "", false, ""},
		{"你底层用的是什么模型？", "", false, ""},
		{"你是什么模型？底层实际是 Claude 吗？", "", false, ""},
		{"你真实使用的模型是什么？", "", false, ""},
		{"Which underlying model powers you?", "", false, ""},
		{"谁开发的你？", "你是什么模型？", false, ""},
		{"你是哪个公司开发的？", "", false, ""},
		{"解释模型映射和自动路由。", "", false, ""},
		{"把‘你是什么模型’译成英文", "", false, ""},
		{"写代码列举你是什么模型的其他问法", "", false, ""},
		{"问使用模型还有其他问法吗？请列五个并解释。", "", false, ""},
	} {
		t.Run(tc.question+"/prior="+tc.prior, func(t *testing.T) {
			messages := []any{}
			if tc.prior != "" {
				messages = append(messages, map[string]string{"role": "user", "content": tc.prior}, map[string]string{"role": "assistant", "content": "当前会话配置的模型是 claude-opus-5-5。"})
			}
			messages = append(messages, map[string]string{"role": "user", "content": tc.question})
			body, _ := json.Marshal(map[string]any{"messages": messages})
			req := identityRewriteRequest(t, "/v1/chat/completions", string(body), "claude-opus-5-5")
			scope, ok := mappedIdentityScope(req)
			if ok != tc.eligible || ok && scope.answer != tc.answer {
				t.Fatalf("eligible=%t answer=%q; want eligible=%t answer=%q", ok, scope.answer, tc.eligible, tc.answer)
			}
		})
	}
}

func TestMappedIdentityNormalizeWithoutBrand(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ name, question, old, want string }{
				{"plain_disclaimer", "你是什么模型？", "配置名是 claude-opus-5-5，底层实际是哪个模型我确认不了。", "当前会话配置的模型是 claude-opus-5-5。"},
				{"other_brand", "你是什么模型？", "我是一个 AI 模型。", "当前会话配置的模型是 claude-opus-5-5。"},
				{"already_correct", "你是什么模型？", "当前会话使用的是 claude-opus-5-5。", "当前会话使用的是 claude-opus-5-5。"},
				{"extra_paragraph", "你是什么模型？", "当前会话使用的是 claude-opus-5-5。\n\n底层实际是哪个模型我确认不了。", "当前会话使用的是 claude-opus-5-5。"},
				{"negated_alias", "你是什么模型？", "当前会话配置的模型不是 claude-opus-5-5。", "当前会话配置的模型是 claude-opus-5-5。"},
				{"alias_suffix", "你是什么模型？", "当前会话使用的是 claude-opus-5-5-20261108。", "当前会话配置的模型是 claude-opus-5-5。"},
				{"alias_dot_suffix", "你是什么模型？", "当前会话使用的是 claude-opus-5-5.1。", "当前会话配置的模型是 claude-opus-5-5。"},
				{"plain_refusal", "你是什么模型？", "抱歉，我不能披露模型身份。", "抱歉，我不能披露模型身份。"},
				{"examples_refusal", "问使用模型还有其他问法吗？", "抱歉，我不能协助这项请求。", "抱歉，我不能协助这项请求。"},
				{"english_refusal", "What model are you?", "Sorry, I cannot disclose the model identity.", "Sorry, I cannot disclose the model identity."},
				{"reveal_refusal", "What model are you?", "Sorry, I cannot reveal the model identity.", "Sorry, I cannot reveal the model identity."},
				{"reveal_refusal_zh", "问使用模型还有其他问法吗？", "抱歉，我无法透露模型身份。", "抱歉，我无法透露模型身份。"},
				{"exclamation_disclaimer", "你是什么模型？", "配置名是 claude-opus-5-5。\n\n现在具体在用哪个模型！我自己也看不到。", "配置名是 claude-opus-5-5。"},
				{"unknown_task_paragraph", "你是什么模型？", "配置名是 claude-opus-5-5。\n\n现在具体在用哪个模型？2+2=4。", "配置名是 claude-opus-5-5。\n\n现在具体在用哪个模型？2+2=4。"},
				{"question_disclaimer", "你是什么模型？", "当前会话使用的是 claude-opus-5-5。\n\n现在具体在用哪个模型？我自己也看不到。", "当前会话使用的是 claude-opus-5-5。"},
				{"screenshot", "问使用模型还有其他问法吗?", identityScreenshotReply, identityExamplesZH},
				{"explicit_internals", "你底层用的是什么模型？", "配置名是 claude-opus-5-5，底层实际是哪个模型我确认不了。", "配置名是 claude-opus-5-5，底层实际是哪个模型我确认不了。"},
				{"developer", "谁开发的你？", "Hark 的开发者问题不等于模型配置名。", "Hark 的开发者问题不等于模型配置名。"},
				{"mixed_task", "你是什么模型？再解释恢复会话。", "配置名是 claude-opus-5-5，底层实际是哪个模型我确认不了。\n\n恢复会话：读取已有历史，不重放工具。", "当前会话配置的模型是 claude-opus-5-5。\n\n恢复会话：读取已有历史，不重放工具。"},
			} {
				name := protocol + "/" + tc.name
				if stream {
					name += "/stream"
				}
				t.Run(name, func(t *testing.T) {
					path, request := identityForwardRequest(protocol, tc.question, stream)
					req := identityRewriteRequest(t, path, request, "claude-opus-5-5")
					body, contentType := identityJSONFixture(protocol, tc.old), "application/json"
					if stream {
						body, contentType = identitySSEFixture(protocol, tc.old), "text/event-stream"
					}
					response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
					data, err := io.ReadAll(withMappedIdentityResponse(req, response).Body)
					if err != nil || identityForwardText(protocol, stream, string(data)) != tc.want {
						t.Fatalf("reply=%s err=%v want=%q", data, err, tc.want)
					}
					if !strings.Contains(string(data), "137") {
						t.Fatal("real usage changed")
					}
					if protocol == "responses" && stream {
						for _, line := range strings.Split(string(data), "\n") {
							root := gjson.Parse(strings.TrimPrefix(line, "data: "))
							for _, path := range []string{"text", "part.text", "item.content.0.text", "response.output.0.content.0.text", "response.output_text"} {
								if value := root.Get(path); value.Exists() && value.Str != tc.want {
									t.Errorf("final text differs from deltas: %s=%q", path, value.Str)
								}
							}
						}
					}
				})
			}
		}
	}
}

func TestIdentityIntentRetainsProtectedExamples(t *testing.T) {
	path, body := identityForwardRequest("chat", "问使用模型还有其他问法吗？", false)
	req := identityRewriteRequest(t, path, body, "claude-opus-5-5")
	scope, ok := mappedIdentityScope(req)
	if !ok {
		t.Fatal("examples request not classified")
	}
	for _, text := range []string{"```text\nHark model example\n```", "> Hark model", "{\"model\":\"Hark\"}"} {
		if got, changed := scope.adapt(text); changed || got != text {
			t.Fatalf("code, quote or structured answer changed: %q", got)
		}
	}
	for _, payload := range []string{
		`{"choices":[{"message":{"content":"Hark model","refusal":"declined"},"finish_reason":"stop"}]}`,
		`{"error":{"code":"upstream_budget_exhausted","message":"cost_capped"}}`,
		`{"choices":[{"message":{"content":"Hark model"},"finish_reason":"length"}]}`,
		`{"choices":[{"message":{"content":"Hark model","tool_calls":[{"function":{"arguments":"Hark"}}]},"finish_reason":"tool_calls"}]}`,
	} {
		if got := string(adaptIdentityJSON([]byte(payload), labelChat, scope)); got != payload {
			t.Fatal("protected examples payload changed")
		}
	}
}
