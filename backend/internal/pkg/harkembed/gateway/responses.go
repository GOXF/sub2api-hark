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
			case typ == "function_call_output":
				if err := flush(); err != nil {
					return v, err
				}
				var output, callID string
				if json.Unmarshal(item["output"], &output) != nil {
					output = string(item["output"])
				}
				json.Unmarshal(item["call_id"], &callID)
				m := chatMessage{Role: "tool", Content: "[call_id=" + callID + "]\n" + output}
				m.Hash = messageHash(m, nil)
				v.Messages = append(v.Messages, m)
			case typ == "function_call":
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
}

func newResponsesFormat() *responsesFormat {
	return &responsesFormat{id: randomID("resp_"), messageID: randomID("msg_"), created: time.Now().Unix()}
}

func (f *responsesFormat) envelope(status, text string) map[string]any {
	itemStatus := "in_progress"
	if status == "completed" {
		itemStatus = "completed"
	}
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
	}
	return map[string]any{"id": f.id, "object": "response", "created_at": f.created, "status": status, "model": "hark",
		"output":      []any{map[string]any{"type": "message", "id": f.messageID, "role": "assistant", "status": itemStatus, "content": content}},
		"output_text": text,
		"usage":       map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}
}
func (f *responsesFormat) json(res *turnResult) any { return f.envelope("completed", res.Text) }
func (f *responsesFormat) start(emit func(string, any)) {
	emit("response.created", map[string]any{"type": "response.created", "response": f.envelope("in_progress", "")})
	emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": f.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	emit("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": f.messageID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
}
func (f *responsesFormat) delta(emit func(string, any), text string) {
	emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": f.messageID, "output_index": 0, "content_index": 0, "delta": text})
}
func (f *responsesFormat) finish(emit func(string, any), full string) {
	emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": f.messageID, "output_index": 0, "content_index": 0, "text": full})
	emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"id": f.messageID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": full, "annotations": []any{}}}}})
	emit("response.completed", map[string]any{"type": "response.completed", "response": f.envelope("completed", full)})
}
func (f *responsesFormat) failure(emit func(string, any), code, message string) {
	env := f.envelope("failed", "")
	env["error"] = map[string]string{"code": code, "message": message}
	emit("response.failed", map[string]any{"type": "response.failed", "response": env})
}
func (f *responsesFormat) done() bool { return false }

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
