package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const attachmentCallPrefix = "call_attachment_"
const attachmentReadTTL = 5 * time.Minute
const attachmentReadBudget int64 = 64 << 20
const AttachmentIntakeHeader = "X-Hark-Attachment-Intake"

type attachmentOriginKey struct{}

// WithAttachmentOrigin accepts only a host-owned administrator setting, never a request Host/header.
func WithAttachmentOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, attachmentOriginKey{}, origin)
}

type attachmentReceipt struct {
	ID     string `json:"read_id"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type attachmentRead struct {
	id, owner, session, history, file string
	call                              toolCall
	polls                             []toolCall
	expires                           time.Time
	uploading, uploaded               bool
	refs                              int
	data                              []byte
	receipt                           attachmentReceipt
}

// A small persistent signing key identifies our transport items even after their file bytes expire.
// Caller holds attachmentMu. File bytes and capabilities themselves are never persisted.
func (s *Server) attachmentKey() ([]byte, error) {
	if len(s.attachmentSigningKey) > 0 {
		return s.attachmentSigningKey, nil
	}
	p := filepath.Join(s.data, "attachment-call.key")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte(randomID(""))
		f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(e, os.ErrExist) {
			b, err = os.ReadFile(p)
		} else if e != nil {
			return nil, e
		} else {
			_, err = f.Write(b)
			if e = f.Close(); err == nil {
				err = e
			}
		}
	}
	if err != nil || len(b) != 48 {
		return nil, errors.New("attachment transport signing key unavailable")
	}
	s.attachmentSigningKey = b
	return b, nil
}

func attachmentSignature(key []byte, owner, session, id string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(owner + "\x00" + session + "\x00" + id))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (s *Server) ownAttachmentCall(id, owner, session string) bool {
	if !strings.HasPrefix(id, attachmentCallPrefix) {
		return false
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(id, attachmentCallPrefix), "_")
	if !ok || len(a) != 48 || len(b) != 32 {
		return false
	}
	key, err := s.attachmentKey()
	return err == nil && hmac.Equal([]byte(b), []byte(attachmentSignature(key, owner, session, a)))
}

func attachmentMessageID(m chatMessage) string {
	if m.ToolCalls != "" {
		var c struct {
			ID string `json:"call_id"`
		}
		json.Unmarshal([]byte(m.ToolCalls), &c)
		return c.ID
	}
	if m.Role == "tool" && strings.HasPrefix(m.Content, "[call_id=") {
		if id, _, ok := strings.Cut(strings.TrimPrefix(m.Content, "[call_id="), "]\n"); ok {
			return id
		}
	}
	return ""
}

func attachmentBase(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" {
		return ""
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())) {
		return ""
	}
	return strings.TrimRight(u.String(), "/")
}

func attachmentPaths(text string) ([]string, error) {
	if !strings.HasPrefix(strings.TrimSpace(text), "# Files mentioned by the user:") || !strings.Contains(text, "## My request:") {
		return nil, nil
	}
	paths := wrapperFilePaths(text)
	if len(paths) > maxFilesPerRequest {
		return nil, inputError("too_many_files", "单轮附件数量超过限制")
	}
	seen := map[string]bool{}
	for _, p := range paths {
		win := len(p) > 3 && (p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z') && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
		if (!win && !strings.HasPrefix(p, "/")) || strings.HasPrefix(p, "//") || len(p) > 4096 || strings.IndexFunc(p, unicode.IsControl) >= 0 || strings.ContainsAny(p, "*?\x00") || strings.HasSuffix(p, "/") || strings.HasSuffix(p, "\\") || seen[p] {
			return nil, inputError("invalid_attachment_path", "附件需要明确、唯一的本机文件绝对路径；不读取目录、网络共享或通配路径")
		}
		seen[p] = true
	}
	return paths, nil
}

func attachmentExec(v chatRequest) *toolSpec {
	if v.ToolChoiceNamed || v.ToolChoice != "auto" && v.ToolChoice != "" {
		return nil
	}
	for i := range v.Tools {
		t := &v.Tools[i]
		if t.Kind != "function" || t.Name != "exec_command" || t.Namespace != "" {
			continue
		}
		var schema struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if json.Unmarshal(t.Parameters, &schema) != nil || schema.Type != "object" || schema.Properties["cmd"].Type != "string" {
			continue
		}
		valid := true
		for _, required := range schema.Required {
			if required != "cmd" {
				valid = false
			}
		}
		if valid {
			return t
		}
	}
	return nil
}

func attachmentPollTool(v chatRequest) *toolSpec {
	if v.ToolChoiceNamed || v.ToolChoice != "auto" && v.ToolChoice != "" {
		return nil
	}
	for i := range v.Tools {
		t := &v.Tools[i]
		if t.Kind != "function" || t.Name != "write_stdin" || t.Namespace != "" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if json.Unmarshal(t.Parameters, &schema) != nil {
			continue
		}
		typ := schema.Properties["session_id"].Type
		if typ != "integer" && typ != "number" {
			continue
		}
		valid := true
		for _, required := range schema.Required {
			if required != "session_id" && required != "chars" {
				valid = false
			}
		}
		if valid {
			return t
		}
	}
	return nil
}

var attachmentRunningSession = regexp.MustCompile(`(?:^|\n)Process running with session ID ([0-9]+)(?:\n|$)`)

func (s *Server) attachmentCall(id string) (*attachmentRead, toolCall, int) {
	for _, a := range s.attachmentReads {
		if a.call.ID == id {
			return a, a.call, 0
		}
		for i, c := range a.polls {
			if c.ID == id {
				return a, c, i + 1
			}
		}
	}
	return nil, toolCall{}, 0
}

func (s *Server) nextAttachmentCall(key []byte, owner, session, name string, args map[string]any) toolCall {
	id := randomID("")
	b, _ := json.Marshal(args)
	return toolCall{ID: attachmentCallPrefix + id + "_" + attachmentSignature(key, owner, session, id), Kind: "function", Name: name, Arguments: string(b)}
}

func writeAttachmentCall(w http.ResponseWriter, v chatRequest, call toolCall) {
	w.Header().Set(AttachmentIntakeHeader, "read-file")
	f := newResponsesFormat()
	if !v.Stream {
		write(w, 200, f.json(&turnResult{ToolCalls: []toolCall{call}}))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	f.start(emit)
	f.finish(emit, "", []toolCall{call})
}

func attachmentCommand(file, destination string, limit int64) string {
	quote := func(s string) string {
		// PowerShell also treats smart single quotes as string delimiters.
		return "'" + strings.NewReplacer("'", "''", "\u2018", "\u2018\u2018", "\u2019", "\u2019\u2019", "\u201a", "\u201a\u201a", "\u201b", "\u201b\u201b").Replace(s) + "'"
	}
	if len(file) > 2 && file[1] == ':' {
		// OpenRead is literal; Invoke-WebRequest -InFile expands brackets in valid filenames.
		return "$ErrorActionPreference='Stop'; $h=[System.Net.Http.HttpClientHandler]::new(); $h.AllowAutoRedirect=$false; $c=[System.Net.Http.HttpClient]::new($h); $c.Timeout=[TimeSpan]::FromSeconds(120); $f=$null; $b=$null; $r=$null; try { $f=[IO.File]::OpenRead(" + quote(file) + "); if($f.Length -gt " + fmt.Sprint(limit) + "){throw 'Attachment exceeds the upload limit'}; $b=[System.Net.Http.StreamContent]::new($f); $b.Headers.ContentType=[System.Net.Http.Headers.MediaTypeHeaderValue]::new('application/octet-stream'); $r=$c.PutAsync(" + quote(destination) + ",$b).GetAwaiter().GetResult(); $r.EnsureSuccessStatusCode()|Out-Null; $r.Content.ReadAsStringAsync().GetAwaiter().GetResult() } finally { if($r){$r.Dispose()}; if($b){$b.Dispose()}; if($f){$f.Dispose()}; $c.Dispose() }"
	}
	quote = func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	p := quote(file)
	return "test -f " + p + " && [ \"$(wc -c < " + p + ")\" -le " + fmt.Sprint(limit) + " ] && curl --disable --fail --silent --show-error --max-redirs 0 --connect-timeout 10 --max-time 120 --upload-file " + p + " --header 'Content-Type: application/octet-stream' " + quote(destination)
}

// Caller holds attachmentMu. In-use buffers remain charged until their request releases them.
func (s *Server) expireAttachmentReads() {
	for id, a := range s.attachmentReads {
		if time.Now().After(a.expires) && a.refs == 0 && !a.uploading {
			s.attachmentBytes -= int64(len(a.data))
			delete(s.attachmentReads, id)
		}
	}
}

func readReceipt(output string) attachmentReceipt {
	if i := strings.LastIndex(output, "\nOutput:"); i >= 0 {
		output = output[i+len("\nOutput:"):]
	}
	var receipt attachmentReceipt
	d := json.NewDecoder(strings.NewReader(strings.TrimSpace(output)))
	d.DisallowUnknownFields()
	if d.Decode(&receipt) != nil || d.Decode(&struct{}{}) != io.EOF {
		return attachmentReceipt{}
	}
	return receipt
}

// prepareAttachmentRead is gateway-owned intake, not an upstream model tool request.
// It returns a release function so expiry cannot uncharge bytes still in a Hark upload.
func (s *Server) prepareAttachmentRead(w http.ResponseWriter, r *http.Request, v *chatRequest) (handled bool, release func()) {
	release = func() {}
	owner := recoveryOwner(r)
	s.attachmentMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.attachmentMu.Unlock()
		}
	}()
	s.expireAttachmentReads()
	last := -1
	for i, m := range v.Messages {
		if m.Role == "user" {
			last = i
		}
	}
	var calls, outputs []chatMessage
	clean := make([]chatMessage, 0, len(v.Messages))
	for i, m := range v.Messages {
		id := attachmentMessageID(m)
		if strings.HasPrefix(id, attachmentCallPrefix) {
			if !s.ownAttachmentCall(id, owner, v.SessionID) {
				fail(w, 403, "attachment_read_owner_mismatch", "附件读取记录不属于当前调用方或会话；未读取、上传或转发")
				return true, release
			}
			if i > last {
				if m.Role == "assistant" {
					calls = append(calls, m)
				} else {
					outputs = append(outputs, m)
				}
			}
			continue
		}
		clean = append(clean, m)
	}
	v.Messages = clean
	last = -1
	for i, m := range clean {
		if m.Role == "user" {
			last = i
		}
	}
	if last < 0 || len(clean[last].Files) > 0 {
		return false, release
	}
	paths, err := attachmentPaths(clean[last].Content)
	if err != nil {
		writeInputError(w, err)
		return true, release
	}
	if len(paths) == 0 {
		return false, release
	}
	if len(calls) != len(outputs) {
		fail(w, 409, "attachment_read_result_missing", "附件读取没有返回完整结果；未发送上游")
		return true, release
	}
	history := strings.Join(hashes(clean[:last+1]), "\x00")
	ready := map[string]*attachmentRead{}
	seenCalls := map[*attachmentRead]int{}
	var poll *attachmentRead
	var processSession int
	for i, m := range calls {
		id := attachmentMessageID(m)
		a, expected, index := s.attachmentCall(id)
		if a == nil || time.Now().After(a.expires) {
			fail(w, 410, "attachment_read_expired", "附件临时读取已过期；未发送上游，请重新附加文件")
			return true, release
		}
		var c struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Type      string `json:"type"`
		}
		json.Unmarshal([]byte(m.ToolCalls), &c)
		output := outputs[i]
		_, receiptText, _ := strings.Cut(output.Content, "]\n")
		if a.owner != owner || a.session != v.SessionID || a.history != history || c.Type != "function_call" || c.Name != expected.Name || c.Arguments != expected.Arguments || attachmentMessageID(output) != id || index != seenCalls[a] || ready[a.file] != nil {
			fail(w, 403, "attachment_read_contract_mismatch", "附件调用、文件或请求历史不匹配；未发送上游")
			return true, release
		}
		seenCalls[a]++
		if index < len(a.polls) {
			poll = a
			continue
		}
		if a.uploaded && readReceipt(receiptText) == a.receipt {
			ready[a.file] = a
			if poll == a {
				poll = nil
			}
			continue
		}
		match := attachmentRunningSession.FindStringSubmatch(receiptText)
		if len(match) == 2 && attachmentPollTool(*v) != nil {
			processSession, _ = strconv.Atoi(match[1])
			poll = a
			continue
		}
		fail(w, 422, "attachment_read_failed", "附件读取或上传未成功；未收到可核验的文件字节，未发送上游")
		return true, release
	}
	if poll != nil {
		tool := attachmentPollTool(*v)
		if tool == nil {
			fail(w, 400, "attachment_read_poll_unavailable", "当前请求未允许等待附件上传所需的工具；未发送上游")
			return true, release
		}
		var call toolCall
		if seenCalls[poll] <= len(poll.polls) {
			call = poll.polls[seenCalls[poll]-1]
		} else {
			if len(poll.polls) >= 8 || processSession <= 0 {
				fail(w, 422, "attachment_read_poll_limit", "附件上传未在限定轮次内完成；未发送上游")
				return true, release
			}
			key, err := s.attachmentKey()
			if err != nil {
				fail(w, 500, "attachment_read_unavailable", "附件读取身份存储不可用")
				return true, release
			}
			args := map[string]any{"session_id": processSession}
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			json.Unmarshal(tool.Parameters, &schema)
			if _, ok := schema.Properties["chars"]; ok {
				args["chars"] = ""
			}
			if _, ok := schema.Properties["max_output_tokens"]; ok {
				args["max_output_tokens"] = 512
			}
			if _, ok := schema.Properties["yield_time_ms"]; ok {
				args["yield_time_ms"] = 30000
			}
			call = s.nextAttachmentCall(key, owner, v.SessionID, "write_stdin", args)
			poll.polls = append(poll.polls, call)
		}
		locked = false
		s.attachmentMu.Unlock()
		writeAttachmentCall(w, *v, call)
		return true, release
	}
	if len(ready) > len(paths) {
		fail(w, 403, "attachment_read_contract_mismatch", "附件列表不匹配")
		return true, release
	}
	var borrowed []*attachmentRead
	for _, p := range paths {
		if a := ready[p]; a != nil {
			a.refs++
			borrowed = append(borrowed, a)
			clean[last].Files = append(clean[last].Files, newInputFile(path.Base(strings.ReplaceAll(p, "\\", "/")), "", a.data, ""))
		}
	}
	// Keep original client message hashes: only the transport projection gains bytes.
	v.Messages = clean
	release = func() {
		s.attachmentMu.Lock()
		defer s.attachmentMu.Unlock()
		for _, a := range borrowed {
			a.refs--
		}
		s.expireAttachmentReads()
	}
	if len(borrowed) == len(paths) {
		return false, release
	}
	tool := attachmentExec(*v)
	if tool == nil {
		fail(w, 400, "missing_file_content", "未收到附件内容，且当前请求未允许可用的本机读取工具；未发送上游")
		return true, release
	}
	origin, ok := r.Context().Value(attachmentOriginKey{}).(string)
	if !ok {
		origin = s.publicBaseURL
	}
	base := attachmentBase(origin)
	if owner == "" || base == "" {
		fail(w, 400, "attachment_read_unavailable", "附件读取需要网关配置可信的 HTTPS API 公网地址及调用方身份；未发送上游")
		return true, release
	}
	key, err := s.attachmentKey()
	if err != nil {
		fail(w, 500, "attachment_read_unavailable", "附件读取身份存储不可用；未发送上游")
		return true, release
	}
	file := ""
	for _, p := range paths {
		if ready[p] == nil {
			file = p
			break
		}
	}
	var a *attachmentRead
	owned := 0
	for _, old := range s.attachmentReads {
		if old.owner == owner {
			owned++
		}
		if time.Now().Before(old.expires) && old.owner == owner && old.session == v.SessionID && old.history == history && old.file == file {
			a = old
		}
	}
	if a == nil {
		if len(s.attachmentReads) >= 64 || owned >= maxFilesPerRequest {
			fail(w, 429, "attachment_read_capacity", "附件临时读取数量已满，请稍后重试")
			return true, release
		}
		id := randomID("")
		callID := attachmentCallPrefix + id + "_" + attachmentSignature(key, owner, v.SessionID, id)
		args := map[string]any{"cmd": attachmentCommand(file, base+"/v1/attachment-reads/"+id, s.maxUpload)}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		json.Unmarshal(tool.Parameters, &schema)
		if _, ok := schema.Properties["login"]; ok {
			args["login"] = false
		}
		if _, ok := schema.Properties["max_output_tokens"]; ok {
			args["max_output_tokens"] = 512
		}
		if _, ok := schema.Properties["yield_time_ms"]; ok {
			args["yield_time_ms"] = 30000
		}
		b, _ := json.Marshal(args)
		a = &attachmentRead{id: id, owner: owner, session: v.SessionID, history: history, file: file, expires: time.Now().Add(attachmentReadTTL), call: toolCall{ID: callID, Kind: "function", Name: "exec_command", Arguments: string(b)}}
		if s.attachmentReads == nil {
			s.attachmentReads = map[string]*attachmentRead{}
		}
		s.attachmentReads[callID] = a
		time.AfterFunc(attachmentReadTTL, func() { s.attachmentMu.Lock(); defer s.attachmentMu.Unlock(); s.expireAttachmentReads() })
	}
	call := a.call
	locked = false
	s.attachmentMu.Unlock()
	writeAttachmentCall(w, *v, call)
	return true, release
}

// AttachmentUploadHandler exposes only a single-use, write-only capability. No account/API credentials.
func (s *Server) AttachmentUploadHandler() http.Handler {
	return http.HandlerFunc(s.receiveAttachmentRead)
}

func (s *Server) receiveAttachmentRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(2 * time.Minute))
	if r.Method != "PUT" || r.Header.Get("Origin") != "" {
		fail(w, 403, "attachment_upload_denied", "附件上传请求不允许")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/attachment-reads/")
	s.attachmentMu.Lock()
	s.expireAttachmentReads()
	var a *attachmentRead
	for _, candidate := range s.attachmentReads {
		if candidate.id == id {
			a = candidate
			break
		}
	}
	if a == nil || time.Now().After(a.expires) {
		s.attachmentMu.Unlock()
		fail(w, 404, "attachment_upload_expired", "附件上传已过期或不存在")
		return
	}
	if a.uploading || a.uploaded {
		s.attachmentMu.Unlock()
		fail(w, 409, "attachment_upload_used", "附件上传地址已使用")
		return
	}
	n := r.ContentLength
	if n < 0 || n > s.maxUpload {
		s.attachmentMu.Unlock()
		fail(w, 413, "file_too_large", "附件需要明确大小且不得超过上传限制")
		return
	}
	if s.attachmentBytes+n > attachmentReadBudget {
		s.attachmentMu.Unlock()
		fail(w, 429, "attachment_upload_capacity", "附件上传内存预算已满，请稍后重试")
		return
	}
	a.uploading = true
	s.attachmentBytes += n
	s.attachmentMu.Unlock()
	data := make([]byte, int(n))
	_, err := io.ReadFull(r.Body, data)
	if err == nil {
		var extra [1]byte
		var count int
		count, err = r.Body.Read(extra[:])
		if count != 0 || err != io.EOF {
			err = errors.New("unexpected attachment length")
		} else {
			err = nil
		}
	}
	s.attachmentMu.Lock()
	a.uploading = false
	if time.Now().After(a.expires) {
		s.attachmentBytes -= n
		s.expireAttachmentReads()
		s.attachmentMu.Unlock()
		fail(w, 410, "attachment_upload_expired", "附件上传已过期；未保留上传")
		return
	}
	if err != nil {
		s.attachmentBytes -= n
		s.attachmentMu.Unlock()
		fail(w, 400, "attachment_upload_incomplete", "附件字节不完整；未保留上传")
		return
	}
	sum := sha256.Sum256(data)
	a.data, a.uploaded = data, true
	a.receipt = attachmentReceipt{ID: a.id, Bytes: n, SHA256: hex.EncodeToString(sum[:])}
	receipt := a.receipt
	s.attachmentMu.Unlock()
	write(w, 200, receipt)
}

// PreflightAttachmentRead runs before host scheduling/billing. Ordinary requests keep their bytes.
// billingBody excludes only authenticated gateway transport items, not arbitrary client tool results.
func (s *Server) PreflightAttachmentRead(w http.ResponseWriter, r *http.Request, owner, origin string) (handled bool, billingBody []byte) {
	if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/responses") {
		return false, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, s.maxChatBody()+1))
	r.Body = io.NopCloser(bytes.NewReader(b))
	if err != nil || int64(len(b)) > s.maxChatBody() {
		fail(w, 413, "request_too_large", "请求体超过限制或读取失败")
		return true, nil
	}
	if !bytes.Contains(b, []byte("# Files mentioned by the user:")) && !bytes.Contains(b, []byte(attachmentCallPrefix)) {
		return false, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil {
		return false, nil
	}
	originalModel := object["model"]
	object["model"] = json.RawMessage(`"hark"`)
	local, _ := json.Marshal(object)
	v, err := s.normalizeResponses(local)
	if err == nil {
		err = applySessionHeaders(r, &v)
	}
	if err != nil {
		writeInputError(w, err)
		return true, nil
	}
	ctx := context.WithValue(WithAttachmentOrigin(r.Context(), origin), recoveryCallerKey{}, owner)
	handled, release := s.prepareAttachmentRead(w, r.WithContext(ctx), &v)
	defer release()
	if handled {
		return true, nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(object["input"], &items) == nil {
		kept := items[:0]
		for _, item := range items {
			var id string
			json.Unmarshal(item["call_id"], &id)
			s.attachmentMu.Lock()
			own := strings.HasPrefix(id, attachmentCallPrefix) && s.ownAttachmentCall(id, owner, v.SessionID)
			s.attachmentMu.Unlock()
			if !own {
				kept = append(kept, item)
			}
		}
		object["input"], _ = json.Marshal(kept)
	}
	object["model"] = originalModel
	billingBody, _ = json.Marshal(object)
	return false, billingBody
}
