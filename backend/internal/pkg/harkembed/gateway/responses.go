package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

// decodeResponses maps an OpenAI Responses API request onto chatRequest:
// instructions → system message; input string → one user message; input items
// (messages with input_text/input_image/input_file parts, bare parts,
// function_call_output) → messages.
func (s *Server) decodeResponses(w http.ResponseWriter, r *http.Request, out *chatRequest) bool {
	b, files, ok := s.readBody(w, r)
	if !ok {
		return false
	}
	v, err := s.normalizeResponses(b)
	if err == nil && len(files) > 0 {
		err = attachToLastUser(&v, files)
	}
	if err == nil {
		err = applySessionHeaders(r, &v)
	}
	if err == nil {
		err = s.checkFileBudget(v.Messages)
	}
	if err != nil {
		writeInputError(w, err)
		return false
	}
	*out = v
	return true
}

func (s *Server) normalizeResponses(b []byte) (chatRequest, error) {
	var v chatRequest
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil || object == nil {
		return v, inputError("invalid_json", "请求需要一个有效 JSON 对象")
	}
	if len(object) > 64 {
		return v, inputError("invalid_request", "请求顶层字段过多")
	}
	mergeAdditionalTools(object)
	if err := parseEnvelope(object, &v); err != nil {
		return v, err
	}
	if raw, ok := object["instructions"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return v, inputError("invalid_request", "instructions 必须是字符串")
		}
		if text != "" {
			m := chatMessage{Role: "system", Content: text}
			m.Hash = messageHash(m, nil)
			v.Messages = append(v.Messages, m)
		}
	}
	raw := bytes.TrimSpace(object["input"])
	var text string
	switch {
	case len(raw) == 0 || bytes.Equal(raw, []byte("null")):
		return v, inputError("invalid_request", "input 不能为空")
	case json.Unmarshal(raw, &text) == nil:
		m := chatMessage{Role: "user", Content: text}
		m.Hash = messageHash(m, nil)
		v.Messages = append(v.Messages, m)
	default:
		var items []map[string]json.RawMessage
		if json.Unmarshal(raw, &items) != nil || len(items) > maxMessages {
			return v, inputError("invalid_request", "input 需要字符串或数组")
		}
		var loose []json.RawMessage
		flush := func() error {
			if len(loose) == 0 {
				return nil
			}
			content, _ := json.Marshal(loose)
			loose = nil
			msg, err := s.parseMessage(map[string]json.RawMessage{"role": json.RawMessage(`"user"`), "content": content})
			if err != nil {
				return err
			}
			v.Messages = append(v.Messages, msg)
			return nil
		}
		for _, item := range items {
			var typ, role string
			json.Unmarshal(item["type"], &typ)
			json.Unmarshal(item["role"], &role)
			switch {
			case role != "" && (typ == "" || typ == "message"):
				if err := flush(); err != nil {
					return v, err
				}
				msg, err := s.parseMessage(map[string]json.RawMessage{"role": item["role"], "content": item["content"]})
				if err != nil {
					return v, err
				}
				v.Messages = append(v.Messages, msg)
			case typ == "input_text" || typ == "input_image" || typ == "input_file" || typ == "text" || typ == "image_url" || typ == "file":
				b, _ := json.Marshal(item)
				loose = append(loose, b)
			case typ == "function_call_output" || typ == "custom_tool_call_output" || typ == "local_shell_call_output":
				if err := flush(); err != nil {
					return v, err
				}
				var output, callID string
				if json.Unmarshal(item["output"], &output) != nil {
					if t, _, err := s.parseContent(item["output"]); err == nil && t != "" {
						output = t
					} else {
						output = string(item["output"])
					}
				}
				json.Unmarshal(item["call_id"], &callID)
				m := chatMessage{Role: "tool", Content: "[call_id=" + callID + "]\n" + output}
				m.Hash = messageHash(m, nil)
				v.Messages = append(v.Messages, m)
			case typ == "function_call" || typ == "custom_tool_call" || typ == "local_shell_call":
				if err := flush(); err != nil {
					return v, err
				}
				b, _ := json.Marshal(item)
				m := chatMessage{Role: "assistant", ToolCalls: string(b)}
				m.Hash = messageHash(m, nil)
				v.Messages = append(v.Messages, m)
			default:
				// 客户端附带的非对话项（additional_tools、reasoning、item_reference、
				// *_call 等）对 Hark 无意义，直接忽略，不再报错。
				continue
			}
		}
		if err := flush(); err != nil {
			return v, err
		}
	}
	if len(v.Messages) > maxMessages {
		return v, inputError("invalid_request", "input 消息过多")
	}
	v.OriginalBody = append(json.RawMessage(nil), b...)
	v.extra = extraFields(object)
	delete(v.extra, "previous_response_id")
	if err := validateChat(v); err != nil {
		return v, inputError("invalid_request", err.Error())
	}
	return v, nil
}

type responsesFormat struct {
	id, messageID string
	created       int64
	msgOpen       bool // 流式：message 项已经开始
}

func newResponsesFormat() *responsesFormat {
	return &responsesFormat{id: randomID("resp_"), messageID: randomID("msg_"), created: time.Now().Unix()}
}

func (f *responsesFormat) messageItem(status, text string) map[string]any {
	content := []any{}
	if text != "" || status == "completed" {
		content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
	}
	return map[string]any{"type": "message", "id": f.messageID, "role": "assistant", "status": status, "content": content}
}

// toolItem 一次工具调用对应的 Responses 输出项。
func toolItem(c toolCall) map[string]any {
	switch c.Kind {
	case "custom":
		return map[string]any{"type": "custom_tool_call", "id": "ctc_" + c.ID[5:], "call_id": c.ID, "name": c.Name, "input": c.Arguments, "status": "completed"}
	case "local_shell":
		return map[string]any{"type": "local_shell_call", "id": "lsh_" + c.ID[5:], "call_id": c.ID, "status": "completed", "action": localShellAction(c.Arguments)}
	default:
		item := map[string]any{"type": "function_call", "id": "fc_" + c.ID[5:], "call_id": c.ID, "name": c.Name, "arguments": c.Arguments, "status": "completed"}
		if c.Namespace != "" {
			item["namespace"] = c.Namespace
		}
		return item
	}
}

func (f *responsesFormat) outputItems(status, text string, calls []toolCall) []any {
	items := []any{}
	itemStatus := "in_progress"
	if status == "completed" {
		itemStatus = "completed"
	}
	if text != "" || len(calls) == 0 {
		items = append(items, f.messageItem(itemStatus, text))
	}
	for _, c := range calls {
		items = append(items, toolItem(c))
	}
	return items
}

func (f *responsesFormat) envelope(status, text string, calls []toolCall) map[string]any {
	output := []any{}
	if status != "in_progress" || text != "" {
		output = f.outputItems(status, text, calls)
	}
	return map[string]any{"id": f.id, "object": "response", "created_at": f.created, "status": status, "model": "hark",
		"output":      output,
		"output_text": text,
		"usage":       map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}
}
func (f *responsesFormat) json(res *turnResult) any {
	return f.envelope("completed", res.Text, res.ToolCalls)
}
func (f *responsesFormat) start(emit func(string, any)) {
	emit("response.created", map[string]any{"type": "response.created", "response": f.envelope("in_progress", "", nil)})
	emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": f.envelope("in_progress", "", nil)})
}
func (f *responsesFormat) openMessage(emit func(string, any)) {
	if f.msgOpen {
		return
	}
	f.msgOpen = true
	emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": f.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	emit("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": f.messageID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
}
func (f *responsesFormat) delta(emit func(string, any), text string) {
	if text == "" {
		return
	}
	f.openMessage(emit)
	emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": f.messageID, "output_index": 0, "content_index": 0, "delta": text})
}
func (f *responsesFormat) finish(emit func(string, any), full string, calls []toolCall) {
	idx := 0
	if full != "" || len(calls) == 0 {
		f.openMessage(emit)
		emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": f.messageID, "output_index": 0, "content_index": 0, "text": full})
		emit("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": f.messageID, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}}})
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": f.messageItem("completed", full)})
		idx = 1
	}
	for i, c := range calls {
		item := toolItem(c)
		added := map[string]any{}
		for k, v := range item {
			added[k] = v
		}
		added["status"] = "in_progress"
		switch c.Kind {
		case "custom":
			added["input"] = ""
		case "function":
			added["arguments"] = ""
		}
		emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": idx + i, "item": added})
		switch c.Kind {
		case "function":
			emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": item["id"], "output_index": idx + i, "delta": c.Arguments})
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": item["id"], "output_index": idx + i, "arguments": c.Arguments})
		case "custom":
			emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "item_id": item["id"], "output_index": idx + i, "delta": c.Arguments})
			emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "item_id": item["id"], "output_index": idx + i, "input": c.Arguments})
		}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": idx + i, "item": item})
	}
	emit("response.completed", map[string]any{"type": "response.completed", "response": f.envelope("completed", full, calls)})
}
func (f *responsesFormat) failure(emit func(string, any), code, message string) {
	env := f.envelope("failed", "", nil)
	env["output"] = []any{}
	env["error"] = map[string]string{"code": code, "message": message}
	emit("response.failed", map[string]any{"type": "response.failed", "response": env})
}
func (f *responsesFormat) done() bool { return false }

// keepalive Codex 的空闲超时只认真正的事件（SSE 注释行不算），所以发 response.in_progress。
func (f *responsesFormat) keepalive(emit func(string, any)) {
	emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": f.envelope("in_progress", "", nil)})
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		fail(w, 401, "invalid_api_key", "API Key 无效")
		return
	}
	var v chatRequest
	if !s.decodeResponses(w, r, &v) {
		return
	}
	s.serveTurn(w, r, v, newResponsesFormat())
}

// mergeAdditionalTools 把 input[] 里 additional_tools 项声明的工具并入顶层 tools。
// Codex 桌面版的 Responses Lite 会发 "tools": null，把工具定义放在
// {"type":"additional_tools","tools":[...]} 里；不合并的话网关会当成没带工具。
func mergeAdditionalTools(object map[string]json.RawMessage) {
	var items []map[string]json.RawMessage
	if json.Unmarshal(object["input"], &items) != nil {
		return
	}
	var extra []json.RawMessage
	for _, it := range items {
		var typ string
		json.Unmarshal(it["type"], &typ)
		if typ != "additional_tools" {
			continue
		}
		var tools []json.RawMessage
		if json.Unmarshal(it["tools"], &tools) == nil {
			extra = append(extra, tools...)
		}
	}
	if len(extra) == 0 {
		return
	}
	var top []json.RawMessage
	if raw := bytes.TrimSpace(object["tools"]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		if json.Unmarshal(raw, &top) != nil {
			return // 顶层 tools 格式不对，交给 parseTools 报错
		}
	}
	merged, err := json.Marshal(append(top, extra...))
	if err == nil {
		object["tools"] = merged
	}
}
