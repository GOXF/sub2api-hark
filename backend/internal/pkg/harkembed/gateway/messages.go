package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Anthropic Messages API（POST /v1/messages），供 Claude Code 等客户端使用。
// 请求映射到 chatRequest：system → system 消息；messages 的 text/image/document
// 内容块 → 消息文字和附件；assistant 的 tool_use → 工具调用记录；user 的
// tool_result → role "tool" 消息。模型名一律当作 hark。工具调用沿用 tools.go 的模拟。

// anthropicErrorType HTTP 状态码对应的 Anthropic 错误类型。
func anthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	return "api_error"
}

func anthropicError(status int, code, message string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]string{"type": anthropicErrorType(status), "message": message, "code": code}}
}

func anthropicFail(w http.ResponseWriter, status int, code, message string) {
	write(w, status, anthropicError(status, code, message))
}

func writeAnthropicInputError(w http.ResponseWriter, err error) {
	var status *statusInputError
	var detail *chatInputError
	switch {
	case errors.As(err, &status):
		anthropicFail(w, status.status, status.code, status.message)
	case errors.As(err, &detail):
		anthropicFail(w, 400, detail.code, detail.message)
	default:
		anthropicFail(w, 400, "invalid_json", "请求需要一个有效 JSON 对象")
	}
}

func isNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

// anthropicSessionID 从 metadata.user_id 取会话标识。Claude Code 的 user_id 形如
// "user_<hash>_account_<uuid>_session_<uuid>"（新版本可能是 JSON 字符串），每个会话不同。
func anthropicSessionID(object map[string]json.RawMessage) string {
	var meta struct {
		UserID string `json:"user_id"`
	}
	if json.Unmarshal(object["metadata"], &meta) != nil {
		return ""
	}
	id := strings.TrimSpace(meta.UserID)
	if id == "" {
		return ""
	}
	var inner struct {
		SessionID string `json:"session_id"`
	}
	if strings.HasPrefix(id, "{") && json.Unmarshal([]byte(id), &inner) == nil && inner.SessionID != "" {
		id = "session_" + inner.SessionID
	} else if i := strings.Index(id, "session_"); i >= 0 {
		id = id[i:]
	}
	if len(id) > 200 || strings.IndexFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		h := sha256.Sum256([]byte(id))
		id = "anthropic_" + hex.EncodeToString(h[:])
	}
	return "claude:" + id
}

// parseAnthropicTools 解析 [{name, description, input_schema}]。带 type 的服务端工具
// （web_search_20250305、bash_20250124 等）没有 input_schema 时忽略。
func parseAnthropicTools(raw json.RawMessage) ([]toolSpec, error) {
	if isNull(raw) {
		return nil, nil
	}
	var items []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil, inputError("invalid_request", "tools 必须是数组")
	}
	if len(items) > maxTools {
		return nil, inputError("invalid_request", "tools 过多")
	}
	var out []toolSpec
	seen := map[string]bool{}
	for _, it := range items {
		name := strings.TrimSpace(it.Name)
		if name == "" || seen[name] {
			continue
		}
		if it.Type != "" && it.Type != "custom" && isNull(it.InputSchema) {
			continue
		}
		seen[name] = true
		out = append(out, toolSpec{Kind: "function", Name: name, Inner: name, Description: it.Description, Parameters: it.InputSchema})
	}
	return out, nil
}

// parseAnthropicToolChoice {type:auto|any|tool|none, name} → auto / required / 工具名 / none。
func parseAnthropicToolChoice(raw json.RawMessage) string {
	var c struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if isNull(raw) || json.Unmarshal(raw, &c) != nil {
		return "auto"
	}
	switch c.Type {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if c.Name != "" {
			return c.Name
		}
	}
	return "auto"
}

// anthropicBlocks system 或 tool_result 的 content：字符串或内容块数组，取出文字（和图片）。
func (s *Server) anthropicBlocks(raw json.RawMessage) (string, []inputFile, error) {
	if isNull(raw) {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil, inputError("invalid_request", "content 需要字符串或内容块数组")
	}
	var texts []string
	var files []inputFile
	for _, b := range blocks {
		var typ string
		json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			var t string
			json.Unmarshal(b["text"], &t)
			texts = append(texts, t)
		case "image", "document":
			f, note, err := s.anthropicSource(typ, b)
			if err != nil {
				return "", nil, err
			}
			if note != "" {
				texts = append(texts, note)
			}
			if f != nil {
				files = append(files, *f)
			}
		}
		if len(files) > maxFilesPerRequest {
			return "", nil, inputError("too_many_files", "单条消息附件数量过多")
		}
	}
	return strings.Join(texts, "\n"), files, nil
}

// anthropicSource image / document 块的 source：base64、url、text（纯文本文档直接并入文字）。
func (s *Server) anthropicSource(typ string, b map[string]json.RawMessage) (*inputFile, string, error) {
	var src struct {
		Type      string          `json:"type"`
		MediaType string          `json:"media_type"`
		Data      json.RawMessage `json:"data"`
		URL       string          `json:"url"`
		Content   json.RawMessage `json:"content"`
	}
	json.Unmarshal(b["source"], &src)
	var title string
	json.Unmarshal(b["title"], &title)
	kind := "image"
	if typ == "document" {
		kind = "file"
	}
	switch src.Type {
	case "base64":
		var data string
		json.Unmarshal(src.Data, &data)
		raw, err := decodeBase64(data, s.maxUpload)
		if err != nil {
			return nil, "", fileDataError(err, "invalid_"+typ, typ+" 的 base64 数据无效")
		}
		f := newInputFile(title, src.MediaType, raw, kind)
		return &f, "", nil
	case "url":
		if src.URL == "" {
			return nil, "", inputError("invalid_"+typ, typ+" 的 url 不能为空")
		}
		f, err := s.fileFromURL(kind, title, src.URL, "invalid_"+typ)
		if err != nil {
			return nil, "", err
		}
		return &f, "", nil
	case "text":
		var data string
		json.Unmarshal(src.Data, &data)
		if title != "" {
			return nil, "[文档: " + title + "]\n" + data, nil
		}
		return nil, data, nil
	case "content":
		t, _, err := s.anthropicBlocks(src.Content)
		return nil, t, err
	}
	return nil, "", inputError("unsupported_content_part", "暂不支持的 "+typ+" source 类型："+src.Type)
}

func newMessage(role, content string, files []inputFile, toolCalls string) chatMessage {
	m := chatMessage{Role: role, Content: content, Files: files, ToolCalls: toolCalls}
	m.Hash = messageHash(m, nil)
	return m
}

// anthropicMessage 一条 Anthropic 消息可能拆成多条 chatMessage（tool_result 单独成 tool 消息）。
func (s *Server) anthropicMessage(role string, raw json.RawMessage) ([]chatMessage, error) {
	if role != "user" && role != "assistant" {
		return nil, inputError("invalid_request", "messages 的 role 只能是 user 或 assistant")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []chatMessage{newMessage(role, text, nil, "")}, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, inputError("invalid_request", "消息 content 需要字符串或内容块数组")
	}
	var out []chatMessage
	var texts []string
	var files []inputFile
	var calls []map[string]any
	for _, b := range blocks {
		var typ string
		json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			var t string
			json.Unmarshal(b["text"], &t)
			texts = append(texts, t)
		case "image", "document":
			f, note, err := s.anthropicSource(typ, b)
			if err != nil {
				return nil, err
			}
			if note != "" {
				texts = append(texts, note)
			}
			if f != nil {
				files = append(files, *f)
			}
		case "tool_use":
			var id, name string
			json.Unmarshal(b["id"], &id)
			json.Unmarshal(b["name"], &name)
			input := json.RawMessage(compactJSON(b["input"]))
			if isNull(input) {
				input = json.RawMessage("{}")
			}
			calls = append(calls, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
		case "tool_result":
			var id string
			var isErr bool
			json.Unmarshal(b["tool_use_id"], &id)
			json.Unmarshal(b["is_error"], &isErr)
			t, fs, err := s.anthropicBlocks(b["content"])
			if err != nil {
				return nil, err
			}
			prefix := "[call_id=" + id + "]\n"
			if isErr {
				prefix = "[call_id=" + id + "] (tool reported an error)\n"
			}
			out = append(out, newMessage("tool", prefix+t, fs, ""))
		default:
			// thinking / redacted_thinking / server_tool_use 等对 Hark 无意义，忽略。
		}
	}
	content := strings.Join(texts, "\n")
	toolCalls := ""
	if len(calls) > 0 {
		b, _ := json.Marshal(calls)
		toolCalls = string(b)
	}
	if role == "assistant" || strings.TrimSpace(content) != "" || len(files) > 0 || len(out) == 0 {
		out = append(out, newMessage(role, content, files, toolCalls))
	}
	return out, nil
}

func (s *Server) normalizeMessages(b []byte) (chatRequest, string, error) {
	var v chatRequest
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil || object == nil {
		return v, "", inputError("invalid_json", "请求需要一个有效 JSON 对象")
	}
	if len(object) > 64 {
		return v, "", inputError("invalid_request", "请求顶层字段过多")
	}
	var model string
	json.Unmarshal(object["model"], &model)
	if model == "" {
		model = "hark"
	}
	// 任何模型名都当作 hark；tools / tool_choice 是 Anthropic 写法，单独解析。
	env := map[string]json.RawMessage{"model": json.RawMessage(`"hark"`)}
	for _, k := range []string{"stream", "session_id", "session_end", "hark_bridge"} {
		if raw, ok := object[k]; ok {
			env[k] = raw
		}
	}
	if err := parseEnvelope(env, &v); err != nil {
		return v, "", err
	}
	tools, err := parseAnthropicTools(object["tools"])
	if err != nil {
		return v, "", err
	}
	v.Tools, v.ToolChoice, v.ToolsDeclared = tools, parseAnthropicToolChoice(object["tool_choice"]), len(tools)
	if v.ToolChoice == "none" {
		v.Tools = nil
	}
	if v.SessionID == "" {
		v.SessionID = anthropicSessionID(object)
	}
	if !isNull(object["system"]) {
		text, _, err := s.anthropicBlocks(object["system"])
		if err != nil {
			return v, "", inputError("invalid_request", "system 需要字符串或文本块数组")
		}
		if strings.TrimSpace(text) != "" {
			v.Messages = append(v.Messages, newMessage("system", text, nil, ""))
		}
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(object["messages"], &messages) != nil || len(messages) == 0 || len(messages) > maxMessages {
		return v, "", inputError("invalid_request", "messages 需要包含 1–1000 条消息对象")
	}
	for _, m := range messages {
		msgs, err := s.anthropicMessage(m.Role, m.Content)
		if err != nil {
			return v, "", err
		}
		v.Messages = append(v.Messages, msgs...)
	}
	if len(v.Messages) > maxMessages {
		return v, "", inputError("invalid_request", "消息过多")
	}
	v.OriginalBody = append(json.RawMessage(nil), b...)
	v.extra = map[string]json.RawMessage{}
	if err := validateChat(v); err != nil {
		return v, "", inputError("invalid_request", err.Error())
	}
	return v, model, nil
}

type anthropicFormat struct {
	id, model string
	index     int  // 下一个内容块的序号
	textOpen  bool // 流式：文字块已经开始
}

func newAnthropicFormat(model string) *anthropicFormat {
	return &anthropicFormat{id: randomID("msg_"), model: model}
}

// toolUseID 工具调用 ID 用 toolu_ 前缀（客户端在 tool_result 里原样带回）。
func toolUseID(c toolCall) string {
	return "toolu_" + strings.TrimPrefix(c.ID, "call_")
}

func toolInput(c toolCall) json.RawMessage {
	args := strings.TrimSpace(c.Arguments)
	if args == "" || !json.Valid([]byte(args)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

func stopReason(calls []toolCall) string {
	if len(calls) > 0 {
		return "tool_use"
	}
	return "end_turn"
}

func (f *anthropicFormat) message(content []any, stop any) map[string]any {
	return map[string]any{"id": f.id, "type": "message", "role": "assistant", "model": f.model, "content": content,
		"stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}
}

func (f *anthropicFormat) json(res *turnResult) any {
	content := []any{}
	if res.Text != "" || len(res.ToolCalls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": res.Text})
	}
	for _, c := range res.ToolCalls {
		content = append(content, map[string]any{"type": "tool_use", "id": toolUseID(c), "name": c.Name, "input": toolInput(c)})
	}
	return f.message(content, stopReason(res.ToolCalls))
}

func (f *anthropicFormat) start(emit func(string, any)) {
	emit("message_start", map[string]any{"type": "message_start", "message": f.message([]any{}, nil)})
}

func (f *anthropicFormat) openText(emit func(string, any)) {
	if f.textOpen {
		return
	}
	f.textOpen = true
	emit("content_block_start", map[string]any{"type": "content_block_start", "index": f.index, "content_block": map[string]any{"type": "text", "text": ""}})
}

func (f *anthropicFormat) closeText(emit func(string, any)) {
	if !f.textOpen {
		return
	}
	f.textOpen = false
	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": f.index})
	f.index++
}

func (f *anthropicFormat) delta(emit func(string, any), text string) {
	if text == "" {
		return
	}
	f.openText(emit)
	emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": f.index, "delta": map[string]any{"type": "text_delta", "text": text}})
}

func (f *anthropicFormat) finish(emit func(string, any), full string, calls []toolCall) {
	if full == "" && len(calls) == 0 {
		f.openText(emit)
	}
	f.closeText(emit)
	for _, c := range calls {
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": f.index,
			"content_block": map[string]any{"type": "tool_use", "id": toolUseID(c), "name": c.Name, "input": map[string]any{}}})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": f.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(toolInput(c))}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": f.index})
		f.index++
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason(calls), "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": 0}})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

func (f *anthropicFormat) failure(emit func(string, any), code, message string) {
	emit("error", anthropicError(500, code, message))
}

func (f *anthropicFormat) done() bool { return false }

func (f *anthropicFormat) keepalive(emit func(string, any)) {
	emit("ping", map[string]any{"type": "ping"})
}

func (f *anthropicFormat) errorBody(status int, code, message string) any {
	return anthropicError(status, code, message)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		anthropicFail(w, 401, "invalid_api_key", "API Key 无效")
		return
	}
	b, files, ok := s.readBody(w, r)
	if !ok {
		return
	}
	v, model, err := s.normalizeMessages(b)
	if err == nil && len(files) > 0 {
		err = attachToLastUser(&v, files)
	}
	if err == nil {
		if id := strings.TrimSpace(r.Header.Get("X-Claude-Code-Session-Id")); id != "" && len(id) <= 200 {
			v.SessionID = "claude:session_" + id
		}
		err = applySessionHeaders(r, &v)
	}
	if err == nil {
		err = s.checkFileBudget(v.Messages)
	}
	if err != nil {
		writeAnthropicInputError(w, err)
		return
	}
	s.serveTurn(w, r, v, newAnthropicFormat(model))
}

// countTokens POST /v1/messages/count_tokens：粗略估算（约 4 字节一个 token），Hark 不提供真实计数。
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		anthropicFail(w, 401, "invalid_api_key", "API Key 无效")
		return
	}
	b, _, ok := s.readBody(w, r)
	if !ok {
		return
	}
	write(w, 200, map[string]int{"input_tokens": len(b)/4 + 1})
}
