package harkembed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
)

func identityRewriteRequest(t *testing.T, path, body, model string) *http.Request {
	t.Helper()
	req := labelRequest(t, path, body, model)
	if err := forceHarkModel(req); err != nil {
		t.Fatal(err)
	}
	return req
}

func identityJSONFixture(protocol, text string) string {
	var value any
	switch protocol {
	case "chat":
		value = map[string]any{"id": "fixture-reply", "model": "fixture-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 137}}
	case "responses":
		value = map[string]any{"id": "fixture-reply", "status": "completed", "model": "fixture-model", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}, "output_text": text, "usage": map[string]int{"total_tokens": 137}}
	case "messages":
		value = map[string]any{"id": "fixture-reply", "model": "fixture-model", "content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": map[string]int{"output_tokens": 137}}
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func identitySSEFixture(protocol, text string) string {
	frame := func(kind string, value any) string {
		data, _ := json.Marshal(value)
		return "event: " + kind + "\ndata: " + string(data) + "\n\n"
	}
	var output strings.Builder
	runes := []rune(text)
	chunks := []string{text}
	if len(runes) > 1 {
		chunks = []string{string(runes[:1]), string(runes[1:])}
	}
	for index, chunk := range chunks {
		switch protocol {
		case "chat":
			_, _ = output.WriteString(frame("chunk", map[string]any{"id": "fixture-reply", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": chunk}}}}))
		case "responses":
			_, _ = output.WriteString(frame("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": chunk, "sequence_number": index, "output_index": 0, "content_index": 0}))
		case "messages":
			_, _ = output.WriteString(frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": chunk}}))
		}
	}
	switch protocol {
	case "chat":
		_, _ = output.WriteString(frame("chunk", map[string]any{"id": "fixture-reply", "choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 137}}))
		_, _ = output.WriteString("data: [DONE]\n\n")
	case "responses":
		root := gjson.Parse(identityJSONFixture(protocol, text))
		_, _ = output.WriteString(frame("response.output_text.done", map[string]any{"type": "response.output_text.done", "text": text, "sequence_number": 2}))
		_, _ = output.WriteString(frame("response.content_part.done", map[string]any{"type": "response.content_part.done", "part": map[string]any{"type": "output_text", "text": text}}))
		_, _ = output.WriteString(frame("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": json.RawMessage(root.Get("output.0").Raw)}))
		_, _ = output.WriteString(frame("response.completed", map[string]any{"type": "response.completed", "response": json.RawMessage(root.Raw), "sequence_number": 3}))
	case "messages":
		_, _ = output.WriteString(frame("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 137}}))
		_, _ = output.WriteString(frame("message_stop", map[string]string{"type": "message_stop"}))
	}
	return output.String()
}

func TestMappedIdentityRewriteProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, mixed := range []bool{false, true} {
				name := protocol
				if stream {
					name += "/stream"
				}
				if mixed {
					name += "/mixed"
				}
				t.Run(name, func(t *testing.T) {
					question := "你是什么模型？"
					old := "Hark 会按任务分配模型，我无法确认实际模型。"
					want := "当前会话配置的模型是 claude-opus-5-5。"
					if mixed {
						question += "再解释会话恢复。"
						rest := "\n\n恢复会话：读取保存的工具结果 `Hark.log`，不要重复执行。"
						old, want = old+rest, want+rest
					}
					path, request := identityForwardRequest(protocol, question, stream)
					req := identityRewriteRequest(t, path, request, "claude-opus-5-5")
					body := identityJSONFixture(protocol, old)
					contentType := "application/json"
					if stream {
						body, contentType = identitySSEFixture(protocol, old), "text/event-stream"
					}
					response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
					adapted := withMappedIdentityResponse(req, response)
					data, err := io.ReadAll(adapted.Body)
					_ = adapted.Body.Close()
					if err != nil || identityForwardText(protocol, stream, string(data)) != want {
						t.Fatalf("reply=%s err=%v want=%q", data, err, want)
					}
					if !strings.Contains(string(data), "137") {
						t.Fatal("upstream usage changed")
					}
					if !stream && (gjson.GetBytes(data, "id").Str != "fixture-reply" || gjson.GetBytes(data, "model").Str != "fixture-model") {
						t.Fatal("unrelated response metadata changed")
					}
					if protocol == "responses" && stream {
						for _, line := range strings.Split(string(data), "\n") {
							root := gjson.Parse(strings.TrimPrefix(line, "data: "))
							var got string
							switch root.Get("type").Str {
							case "response.output_text.done":
								got = root.Get("text").Str
							case "response.content_part.done":
								got = root.Get("part.text").Str
							case "response.output_item.done":
								got = root.Get("item.content.0.text").Str
							case "response.completed":
								got = root.Get("response.output.0.content.0.text").Str
							}
							if got != "" && got != want {
								t.Errorf("delta/completion mismatch: %q", got)
							}
						}
					}
				})
			}
		}
	}
}

func TestMappedIdentityRewriteConservativeGuards(t *testing.T) {
	old := identityJSONFixture("chat", "Hark chooses the model.")
	for _, question := range []string{"翻译：你是什么模型？", "解释 Hark 的模型映射", "写一段输出 Hark 的代码", "把‘你是什么模型’译成英文"} {
		req := identityRewriteRequest(t, "/v1/chat/completions", `{"messages":[{"role":"user","content":"`+question+`"}]}`, "claude-opus-5-5")
		original := io.NopCloser(strings.NewReader(old))
		response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: original}
		if withMappedIdentityResponse(req, response).Body != original {
			t.Errorf("nonidentity request wrapped: %q", question)
		}
	}
	for _, model := range []string{"*", "claude-*", "hark", ""} {
		req := identityRewriteRequest(t, "/v1/chat/completions", `{"messages":[{"role":"user","content":"你是什么模型？"}]}`, model)
		if _, ok := mappedIdentityScope(req); ok {
			t.Errorf("invalid model %q enabled adaptation", model)
		}
	}
	for _, suffix := range []string{`,"response_format":{"type":"json_object"}`, `,"tool_choice":"required"`, `,"n":2`, `,"session_end":true`} {
		req := identityRewriteRequest(t, "/v1/chat/completions", `{"messages":[{"role":"user","content":"你是什么模型？"}]`+suffix+`}`, "claude-opus-5-5")
		if _, ok := mappedIdentityScope(req); ok {
			t.Errorf("structured/control request adapted: %s", suffix)
		}
	}
	req := identityRewriteRequest(t, "/v1/chat/completions", `{"messages":[{"role":"user","content":"What model are you?"}]}`, "claude-opus-5-5")
	scope, ok := mappedIdentityScope(req)
	if !ok || scope.answer != "The model configured for this conversation is claude-opus-5-5." {
		t.Fatal("English identity response lost")
	}
	for _, custom := range []bool{false, true} {
		if custom {
			req = req.WithContext(context.WithValue(req.Context(), ctxkey.HarkModelLabelPrompt, "Custom identity note"))
		} else {
			t.Setenv("HARK_MODEL_LABEL", "off")
		}
		body := io.NopCloser(strings.NewReader(old))
		response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}
		if got := withMappedIdentityResponse(req, response); got.Body != body {
			t.Error("custom/disabled note was overridden")
		}
		t.Setenv("HARK_MODEL_LABEL", "on")
	}
}

func TestMappedIdentityRewritePreservesProtectedReplies(t *testing.T) {
	scope := identityReplyScope{answer: "Configured model: claude-opus-5-5"}
	jsonBodies := []string{
		`{"error":{"code":"upstream_budget_exhausted","message":"Hark cost_capped"}}`,
		`{"choices":[{"message":{"content":"Hark model","tool_calls":[{"function":{"arguments":"Hark"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"content":"Hark model","refusal":"declined"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"content":"Hark model"},"finish_reason":"length"}]}`,
		`{"choices":[{"message":{"content":"Hark model","annotations":[{"type":"url_citation","url":"https://example.com"}]}}]}`,
		`{"choices":[{"message":{"content":"Hark model","citations":[{"start_index":0}]}}]}`,
		`{"status":"incomplete","output":[]}`,
		`not json`,
	}
	for _, body := range jsonBodies {
		if got := string(adaptIdentityJSON([]byte(body), labelChat, scope)); got != body {
			t.Errorf("protected JSON changed: %s", got)
		}
	}
	base := "data: {\"choices\":[{\"delta\":{\"content\":\"Hark model\"}}]}\n\n"
	for _, extra := range []string{
		"data: {\"error\":{\"code\":\"upstream_budget_exhausted\"}}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"Hark\"}}]}}]}\n\n",
		"data: broken JSON\n\n", "",
		"data: {\"choices\":[{\"delta\":{\"refusal\":\"declined\"}}]}\n\n",
	} {
		body := base + extra
		if extra != "" {
			body += "data: [DONE]\n\n"
		}
		if got := string(adaptIdentitySSE([]byte(body), labelChat, scope)); got != body {
			t.Errorf("error/tool/truncated stream changed: %s", got)
		}
	}
	for _, protocol := range []string{"responses", "messages"} {
		body := identityJSONFixture(protocol, "Hark model")
		if protocol == "responses" {
			var value map[string]any
			_ = json.Unmarshal([]byte(body), &value)
			items, ok := value["output"].([]any)
			if !ok {
				t.Fatal("fixture output must be an array")
			}
			value["output"] = append(items, map[string]any{"type": "local_shell_call", "action": map[string]any{"command": []string{"echo", "Hark"}}})
			encoded, _ := json.Marshal(value)
			body = string(encoded)
		}
		format := labelResponses
		if protocol == "messages" {
			format = labelMessages
			body = `{"content":[{"type":"text","text":"Hark model"},{"type":"tool_use","id":"call-1","name":"read_file","input":{"path":"Hark.log"}}],"stop_reason":"tool_use"}`
		}
		if got := string(adaptIdentityJSON([]byte(body), format, scope)); got != body {
			t.Errorf("native tool reply changed: %s", got)
		}
	}
	toolStream := identitySSEFixture("responses", "Hark model") + "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"local_shell_call\",\"action\":{\"command\":[\"echo\",\"Hark\"]}}}\n\n"
	if got := string(adaptIdentitySSE([]byte(toolStream), labelResponses, scope)); got != toolStream {
		t.Fatal("Responses local_shell_call stream changed")
	}
	for _, referenceFrame := range []string{
		`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"","citations":[{"type":"char_location","start_char_index":0}]}}`,
	} {
		stream := "data: " + referenceFrame + "\n\n" + identitySSEFixture("messages", "Hark model")
		if got := string(adaptIdentitySSE([]byte(stream), labelMessages, scope)); got != stream {
			t.Fatal("Messages citation stream changed")
		}
	}
	for _, text := range []string{"```python\nprint('Hark model')\n```", "> Hark model", "1. Hark model 配置", "当前会话配置的模型是 claude-opus-5-5。"} {
		if got, changed := scope.adapt(text); changed || got != text {
			t.Errorf("code/quote/correct answer changed: %q", text)
		}
	}
	scope.mixed = true
	if text := "Hark model；恢复会话读取历史。"; func() bool { got, changed := scope.adapt(text); return changed || got != text }() {
		t.Fatal("mixed inline task was discarded without a paragraph boundary")
	}
	for _, text := range []string{"Hark 会按任务选择模型；2+2=4。\n\n以上是计算步骤。", "Hark 会按任务选择模型，首都是北京。\n\n以上是地理答案。"} {
		if got, changed := scope.adapt(text); changed || got != text {
			t.Fatalf("mixed task result discarded: %q", got)
		}
	}
	scope.mixed = false
	if got, changed := scope.adapt("Hark 会按任务选择模型。\n\n详细说明：请保留这个任务说明。"); !changed || got != scope.answer+"\n\n详细说明：请保留这个任务说明。" {
		t.Fatal("later paragraph of a pure identity answer was discarded")
	}
	for _, separator := range []string{"\r\n\r\n", "\n \t\n"} {
		rest := separator + "```\nprint('Hark')\n```"
		if got, changed := scope.adapt("Hark model." + rest); !changed || got != scope.answer+rest {
			t.Fatal("non-LF paragraph boundary lost trailing code")
		}
	}
}

type identityFaultBody struct{ closed bool }

func (*identityFaultBody) Read(p []byte) (int, error) { copy(p, "H"); return 1, io.ErrUnexpectedEOF }
func (b *identityFaultBody) Close() error             { b.closed = true; return nil }

func TestMappedIdentityRewriteBoundedReadAndError(t *testing.T) {
	large := strings.Repeat("x", maxIdentityReplyBytes+30)
	body := &mappedIdentityBody{ReadCloser: io.NopCloser(strings.NewReader(large)), scope: identityReplyScope{answer: "configured"}, format: labelChat}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != large {
		t.Fatal("oversized reply was truncated")
	}
	fault := &identityFaultBody{}
	body = &mappedIdentityBody{ReadCloser: fault, scope: identityReplyScope{}, format: labelChat}
	got, err = io.ReadAll(body)
	_ = body.Close()
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(got) != "H" || !fault.closed {
		t.Fatal("underlying read error or Close lost")
	}
}
