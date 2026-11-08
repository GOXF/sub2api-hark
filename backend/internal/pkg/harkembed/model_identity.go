package harkembed

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/tidwall/gjson"
)

// A pure identity question asks for gateway metadata, not provider inference.
// Keep other tasks, structured output and administrator-defined templates on
// their original path; Hark may ignore an injected identity note.
func modelIdentityResponse(req *http.Request) *http.Response {
	if req == nil || req.URL == nil || req.Method != http.MethodPost || !modelLabelEnabled() || strings.TrimSpace(labelTemplate(req)) != "" {
		return nil
	}
	model, format := requestedModel(req), labelFormatOf(req.URL.Path)
	if model == "" || format == labelNone {
		return nil
	}
	if ct := strings.ToLower(req.Header.Get("Content-Type")); ct != "" && !strings.Contains(ct, "json") {
		return nil
	}
	body := requestBodyBytes(req)
	if !gjson.ValidBytes(body) {
		return nil
	}
	if gjson.GetBytes(body, "session_end").Exists() || req.Header.Get("X-Hark-Session-End") != "" || req.Header.Get("X-Session-End") != "" {
		return nil
	}
	stream := gjson.GetBytes(body, "stream")
	if stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
		return nil
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.IsObject() {
		choice = choice.Get("type")
		if !choice.Exists() {
			return nil
		}
	}
	if choice.Exists() && choice.String() != "auto" && choice.String() != "none" {
		return nil
	}
	for _, path := range []string{"response_format.type", "text.format.type"} {
		if typ := gjson.GetBytes(body, path); typ.Exists() && typ.String() != "text" {
			return nil
		}
	}
	if n := gjson.GetBytes(body, "n"); n.Exists() && n.Int() != 1 {
		return nil
	}
	if format == labelMessages && gjson.GetBytes(body, "max_tokens").Int() <= 0 {
		return nil
	}
	input := gjson.GetBytes(body, "messages")
	if format == labelResponses {
		input = gjson.GetBytes(body, "input")
	}
	text := ""
	if format == labelResponses && input.Type == gjson.String {
		text = input.Str
	} else if items := input.Array(); input.IsArray() && len(items) > 0 {
		last := items[len(items)-1]
		if last.Get("role").Str != "user" || last.Get("tool_calls").Exists() {
			return nil
		}
		content := last.Get("content")
		if content.Type == gjson.String {
			text = content.Str
		} else if parts := content.Array(); content.IsArray() && len(parts) == 1 {
			typ := parts[0].Get("type").Str
			if typ == "text" || typ == "input_text" {
				text = parts[0].Get("text").Str
			}
		}
	}
	question := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("?？!！.。", r) {
			return -1
		}
		return unicode.ToLower(r)
	}, text)
	switch question {
	case "你使用了什么模型", "你使用什么模型", "你是什么模型", "你用的是什么模型", "你用的什么模型", "你现在是什么模型", "你用什么模型", "你的模型是什么", "你是哪个模型", "你是什么大模型",
		"你是谁", "请问你是谁", "whoareyou",
		"你使用了什么模型请直接回答当前会话的模型名称",
		"whatmodelareyou", "whatmodelareyouusing", "whichmodelareyou", "whichmodelareyouusing", "whatisyourmodel", "whatmodeldoyouuse":
		return serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeModelIdentity(w, format, model, stream.Bool(), gjson.GetBytes(body, "stream_options.include_usage").Bool())
		}), req)
	}
	return nil
}

func writeModelIdentity(w http.ResponseWriter, format labelFormat, model string, stream, includeUsage bool) {
	id, created := rand.Text(), time.Now().Unix()
	emit := func(event string, value any) {
		if event != "" {
			_, _ = fmt.Fprintf(w, "event: %s\n", event)
		}
		data, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	// These are local metadata replies: no upstream tokens were consumed.
	switch format {
	case labelChat:
		chunk := func(delta any, finish any) map[string]any {
			return map[string]any{"id": "chatcmpl_" + id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		usage := map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
		if stream {
			emit("", chunk(map[string]string{"role": "assistant", "content": ""}, nil))
			emit("", chunk(map[string]string{"content": model}, nil))
			emit("", chunk(map[string]string{}, "stop"))
			if includeUsage {
				last := chunk(map[string]string{}, nil)
				last["choices"], last["usage"] = []any{}, usage
				emit("", last)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl_" + id, "object": "chat.completion", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": model}, "finish_reason": "stop"}}, "usage": usage})
	case labelResponses:
		part := map[string]any{"type": "output_text", "text": model, "annotations": []any{}}
		message := map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
		response := func(status string, output []any) map[string]any {
			return map[string]any{"id": "resp_" + id, "object": "response", "created_at": created, "status": status, "model": model,
				"output": output, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}
		}
		complete := response("completed", []any{message})
		if !stream {
			_ = json.NewEncoder(w).Encode(complete)
			return
		}
		for _, typ := range []string{"response.created", "response.in_progress"} {
			emit(typ, map[string]any{"type": typ, "response": response("in_progress", []any{})})
		}
		emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
		emit("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_" + id, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_" + id, "output_index": 0, "content_index": 0, "delta": model})
		emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_" + id, "output_index": 0, "content_index": 0, "text": model})
		emit("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": "msg_" + id, "output_index": 0, "content_index": 0, "part": part})
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": message})
		emit("response.completed", map[string]any{"type": "response.completed", "response": complete})
	case labelMessages:
		message := func(content []any, stop any) map[string]any {
			return map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant", "model": model, "content": content,
				"stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}
		}
		if !stream {
			_ = json.NewEncoder(w).Encode(message([]any{map[string]string{"type": "text", "text": model}}, "end_turn"))
			return
		}
		emit("message_start", map[string]any{"type": "message_start", "message": message([]any{}, nil)})
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": model}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}})
		emit("message_stop", map[string]any{"type": "message_stop"})
	}
}
