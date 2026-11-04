package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

// maxJSONOverhead is the JSON/text budget on top of the base64-encoded
// attachments a chat body may carry.
const maxJSONOverhead = 2 * 1024 * 1024

// maxMessages bounds one request; long conversations should rely on the
// session's tail-only forwarding instead of resending everything.
const maxMessages = 1000

type chatInputError struct{ code, message string }

func (e *chatInputError) Error() string     { return e.message }
func inputError(code, message string) error { return &chatInputError{code, message} }

type statusInputError struct {
	status        int
	code, message string
}

func (e *statusInputError) Error() string { return e.message }

// inputFile is one image or document supplied by the client. Remote URLs are
// resolved lazily, only when the message is actually forwarded to Hark, so a
// history prefix that was already sent is never fetched or uploaded again.
type inputFile struct {
	Kind      string // image or file
	Name      string
	MediaType string
	Data      []byte
	URL       string
}

type chatMessage struct {
	Role      string      `json:"role"`
	Content   string      `json:"content"`
	Files     []inputFile `json:"-"`
	ToolCalls string      `json:"-"`
	Hash      string      `json:"-"`
}

type chatRequest struct {
	Model      string        `json:"model"`
	Messages   []chatMessage `json:"messages"`
	Stream     bool          `json:"stream"`
	SessionID  string        `json:"session_id,omitempty"`
	SessionEnd bool          `json:"session_end,omitempty"`
	// Bridge asks for an account whose local file bridge is on (hark_bridge
	// in the body or X-Hark-Bridge: true). It only affects new sessions.
	Bridge       bool            `json:"hark_bridge,omitempty"`
	OriginalBody json.RawMessage `json:"-"`
	extra        map[string]json.RawMessage
}

// maxChatBody allows roughly three maximum-size base64 attachments plus text.
func (s *Server) maxChatBody() int64 {
	return s.maxUpload/3*4*3 + maxJSONOverhead
}

func writeInputError(w http.ResponseWriter, err error) {
	var status *statusInputError
	var detail *chatInputError
	switch {
	case errors.As(err, &status):
		fail(w, status.status, status.code, status.message)
	case errors.As(err, &detail):
		fail(w, 400, detail.code, detail.message)
	default:
		fail(w, 400, "invalid_json", "聊天请求需要一个有效 JSON 对象")
	}
}

// readBody reads a JSON body or a multipart/form-data body. Multipart uses
// the field "payload" (or "request") for the JSON request and every file part
// becomes an attachment of the last user message.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, []inputFile, bool) {
	limit := s.maxChatBody()
	ct := r.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(ct)
	if mediaType == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxFilesPerRequest)*s.maxUpload+maxJSONOverhead)
		reader := multipart.NewReader(r.Body, params["boundary"])
		var payload []byte
		var files []inputFile
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				s.bodyReadError(w, err)
				return nil, nil, false
			}
			if part.FileName() == "" {
				name := part.FormName()
				b, err := io.ReadAll(io.LimitReader(part, maxJSONOverhead+1))
				if err != nil || len(b) > maxJSONOverhead {
					fail(w, 413, "request_too_large", "multipart 文本字段过长")
					return nil, nil, false
				}
				if name == "payload" || name == "request" {
					payload = b
				}
				continue
			}
			if len(files) >= maxFilesPerRequest {
				fail(w, 400, "too_many_files", "单次请求附件数量过多")
				return nil, nil, false
			}
			b, err := io.ReadAll(io.LimitReader(part, s.maxUpload+1))
			if err != nil {
				s.bodyReadError(w, err)
				return nil, nil, false
			}
			if int64(len(b)) > s.maxUpload {
				fail(w, 413, "file_too_large", "附件超过大小限制")
				return nil, nil, false
			}
			files = append(files, newInputFile(part.FileName(), part.Header.Get("Content-Type"), b, ""))
		}
		if payload == nil {
			fail(w, 400, "invalid_request", "multipart 请求需要 payload 字段（JSON）")
			return nil, nil, false
		}
		return payload, files, true
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		s.bodyReadError(w, err)
		return nil, nil, false
	}
	return b, nil, true
}

func (s *Server) bodyReadError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		fail(w, 413, "request_too_large", "请求体超过大小限制，请减少历史内容或附件")
	} else {
		fail(w, 400, "invalid_json", "请求体读取失败")
	}
}

func applySessionHeaders(r *http.Request, v *chatRequest) error {
	for _, name := range []string{"X-Hark-Session-Id", "X-Session-Id"} {
		if values, ok := r.Header[name]; ok {
			if len(values) != 1 {
				return inputError("invalid_session", name+" 只能设置一次")
			}
			v.SessionID = values[0]
			break
		}
	}
	if values, ok := r.Header["X-Hark-Bridge"]; ok {
		if len(values) != 1 || values[0] != "true" && values[0] != "false" {
			return inputError("invalid_bridge", "X-Hark-Bridge 必须是 true 或 false")
		}
		v.Bridge = values[0] == "true"
	}
	for _, name := range []string{"X-Hark-Session-End", "X-Session-End"} {
		if values, ok := r.Header[name]; ok {
			if len(values) != 1 || values[0] != "true" && values[0] != "false" {
				return inputError("invalid_session", name+" 必须是 true 或 false")
			}
			v.SessionEnd = values[0] == "true"
			break
		}
	}
	return validateSession(*v)
}

// decodeChat parses an OpenAI chat-completions request (JSON or multipart).
func (s *Server) decodeChat(w http.ResponseWriter, r *http.Request, out *chatRequest) bool {
	b, files, ok := s.readBody(w, r)
	if !ok {
		return false
	}
	v, err := s.normalizeChat(b)
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
	w.Header().Set("X-Hark-Input-Adapter", "native-project-v2")
	*out = v
	return true
}

func attachToLastUser(v *chatRequest, files []inputFile) error {
	for i := len(v.Messages) - 1; i >= 0; i-- {
		if v.Messages[i].Role == "user" {
			v.Messages[i].Files = append(v.Messages[i].Files, files...)
			v.Messages[i].Hash = messageHash(v.Messages[i], nil)
			return nil
		}
	}
	return inputError("invalid_request", "multipart 附件需要至少一条 user 消息")
}

func (s *Server) checkFileBudget(messages []chatMessage) error {
	count := 0
	for _, m := range messages {
		for _, f := range m.Files {
			count++
			if int64(len(f.Data)) > s.maxUpload {
				return &statusInputError{413, "file_too_large", "附件超过大小限制"}
			}
		}
	}
	if count > maxFilesPerRequest*10 {
		return inputError("too_many_files", "请求中附件数量过多")
	}
	return nil
}

func (s *Server) normalizeChat(b []byte) (chatRequest, error) {
	var v chatRequest
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil || object == nil {
		return v, inputError("invalid_json", "聊天请求需要一个有效 JSON 对象")
	}
	if len(object) > 64 {
		return v, inputError("invalid_request", "聊天请求顶层字段过多")
	}
	if err := parseEnvelope(object, &v); err != nil {
		return v, err
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(object["messages"], &messages) != nil || len(messages) == 0 || len(messages) > maxMessages {
		return v, inputError("invalid_request", "messages 需要包含 1–1000 条消息对象")
	}
	for _, m := range messages {
		msg, err := s.parseMessage(m)
		if err != nil {
			return v, err
		}
		v.Messages = append(v.Messages, msg)
	}
	v.OriginalBody = append(json.RawMessage(nil), b...)
	v.extra = extraFields(object)
	if err := validateChat(v); err != nil {
		return v, inputError("invalid_request", err.Error())
	}
	return v, nil
}

func parseEnvelope(object map[string]json.RawMessage, v *chatRequest) error {
	if json.Unmarshal(object["model"], &v.Model) != nil || v.Model != "hark" {
		return inputError("invalid_request", "当前模型别名为 hark")
	}
	if raw, ok := object["stream"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &v.Stream) != nil {
			return inputError("invalid_request", "stream 必须是布尔值")
		}
	}
	if raw, ok := object["session_id"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &v.SessionID) != nil {
			return inputError("invalid_session", "session_id 必须是字符串")
		}
	}
	if raw, ok := object["session_end"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &v.SessionEnd) != nil {
			return inputError("invalid_session", "session_end 必须是布尔值")
		}
	}
	if raw, ok := object["hark_bridge"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &v.Bridge) != nil {
			return inputError("invalid_bridge", "hark_bridge 必须是布尔值")
		}
	}
	for _, name := range []string{"conversationId", "idempotencyKey", "responseMessageId", "message", "attachments", "expectedUserId"} {
		if _, ok := object[name]; ok {
			return inputError("reserved_gateway_field", name+" 由网关管理，请使用 messages 和 session_id")
		}
	}
	return nil
}

// Fields that are not part of the gateway envelope. They are forwarded to Hark
// only when forwarding is explicitly enabled (see Options.ForwardExtra).
func extraFields(object map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for name, value := range object {
		switch name {
		case "model", "stream", "messages", "session_id", "session_end", "hark_bridge", "input", "instructions", "user":
		default:
			out[name] = value
		}
	}
	return out
}

func (s *Server) parseMessage(m map[string]json.RawMessage) (chatMessage, error) {
	var msg chatMessage
	if json.Unmarshal(m["role"], &msg.Role) != nil || msg.Role == "" {
		return msg, inputError("invalid_request", "消息需要 role 字符串")
	}
	text, files, err := s.parseContent(m["content"])
	if err != nil {
		return msg, err
	}
	msg.Content, msg.Files = text, files
	if raw, ok := m["tool_calls"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		msg.ToolCalls = string(raw)
	}
	if raw, ok := m["tool_call_id"]; ok {
		var id string
		json.Unmarshal(raw, &id)
		if id != "" {
			msg.Content = "[tool_call_id=" + id + "]\n" + msg.Content
		}
	}
	msg.Hash = messageHash(msg, m["content"])
	return msg, nil
}

// messageHash fingerprints a message for history prefix matching. It covers
// the role, text, tool calls and the bytes or URL of each attachment.
func messageHash(m chatMessage, raw json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(m.Role))
	h.Write([]byte{0})
	h.Write([]byte(m.Content))
	h.Write([]byte{0})
	h.Write([]byte(m.ToolCalls))
	for _, f := range m.Files {
		h.Write([]byte{1})
		h.Write([]byte(f.Kind + "\x00" + f.Name + "\x00" + f.URL + "\x00"))
		sum := sha256.Sum256(f.Data)
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validateChat(v chatRequest) error {
	if v.Model != "hark" {
		return errors.New("当前模型别名为 hark")
	}
	if len(v.Messages) == 0 || len(v.Messages) > maxMessages {
		return errors.New("请输入 1–1000 条消息")
	}
	for _, m := range v.Messages {
		if m.Role == "user" && strings.TrimSpace(m.Content) == "" && len(m.Files) == 0 {
			return errors.New("user 消息内容不能为空")
		}
	}
	return nil
}

// parseContent accepts a string, null, or an array of OpenAI content parts:
// text/input_text, image_url/input_image, file/input_file. Other part types are
// rejected explicitly instead of being silently dropped.
func (s *Server) parseContent(raw json.RawMessage) (string, []inputFile, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || parts == nil {
		return "", nil, inputError("invalid_request", "消息 content 需要字符串、内容数组或 null")
	}
	var texts []string
	var files []inputFile
	for _, p := range parts {
		var typ string
		json.Unmarshal(p["type"], &typ)
		switch typ {
		case "text", "input_text", "output_text", "refusal":
			var t string
			key := "text"
			if typ == "refusal" {
				key = "refusal"
			}
			if json.Unmarshal(p[key], &t) != nil {
				return "", nil, inputError("invalid_request", typ+" 内容需要 text 字符串")
			}
			texts = append(texts, t)
		case "image_url", "input_image":
			f, err := s.parseImagePart(p)
			if err != nil {
				return "", nil, err
			}
			files = append(files, f)
		case "file", "input_file":
			f, err := s.parseFilePart(typ, p)
			if err != nil {
				return "", nil, err
			}
			files = append(files, f)
		case "":
			return "", nil, inputError("invalid_request", "内容数组中的每一项都需要 type")
		default:
			return "", nil, inputError("unsupported_content_part", "暂不支持的内容类型："+typ)
		}
		if len(files) > maxFilesPerRequest {
			return "", nil, inputError("too_many_files", "单条消息附件数量过多")
		}
	}
	return strings.Join(texts, "\n"), files, nil
}

func (s *Server) parseImagePart(p map[string]json.RawMessage) (inputFile, error) {
	var url string
	raw := p["image_url"]
	if json.Unmarshal(raw, &url) != nil {
		var obj struct {
			URL string `json:"url"`
		}
		json.Unmarshal(raw, &obj)
		url = obj.URL
	}
	if url == "" {
		var fileID string
		json.Unmarshal(p["file_id"], &fileID)
		if fileID != "" {
			return inputFile{}, inputError("unsupported_file_id", "不支持 file_id，请使用 data: URL 或 http(s) URL")
		}
		return inputFile{}, inputError("invalid_image_url", "image_url 需要 url 字段")
	}
	return s.fileFromURL("image", "", url, "invalid_image_url")
}

func (s *Server) parseFilePart(typ string, p map[string]json.RawMessage) (inputFile, error) {
	var spec struct {
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
		FileID   string `json:"file_id"`
		FileURL  string `json:"file_url"`
	}
	if typ == "file" {
		json.Unmarshal(p["file"], &spec)
	} else {
		b, _ := json.Marshal(p)
		json.Unmarshal(b, &spec)
	}
	switch {
	case spec.FileData != "":
		if strings.HasPrefix(spec.FileData, "data:") {
			return s.fileFromURL("file", spec.Filename, spec.FileData, "invalid_file_data")
		}
		data, err := decodeBase64(spec.FileData, s.maxUpload)
		if err != nil {
			return inputFile{}, fileDataError(err, "invalid_file_data", "file_data 需要 base64 或 data: URL")
		}
		return newInputFile(spec.Filename, "", data, "file"), nil
	case spec.FileURL != "":
		return s.fileFromURL("file", spec.Filename, spec.FileURL, "invalid_file_url")
	case spec.FileID != "":
		return inputFile{}, inputError("unsupported_file_id", "不支持 file_id，请直接提供 file_data（base64）")
	}
	return inputFile{}, inputError("invalid_file_data", "file 内容需要 file_data")
}

func fileDataError(err error, code, message string) error {
	if errors.Is(err, errTooLarge) {
		return &statusInputError{413, "file_too_large", "附件超过大小限制"}
	}
	return inputError(code, message)
}
