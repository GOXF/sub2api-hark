package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// turnResult describes one forwarded turn. Fields before Text are known once
// the message is accepted by Hark (so streaming headers can carry them).
type turnResult struct {
	Reused    bool
	Reset     bool
	Mode      string
	Forwarded int
	Files     int
	Isolation string

	Text      string
	ToolCalls []toolCall // 从回复里解析出的客户端工具调用（tools.go）
	Outcome   string
	Archived  bool

	// For the request log.
	AccountID   string
	AccountName string
	Attempts    int
	Skipped     []string
	Bridge      bool // the account's file bridge is on
}

// runTurn forwards one client request into its Hark Project and waits for the
// correlated, completed reply.
func (s *Server) runTurn(ctx context.Context, r *http.Request, v chatRequest, res *turnResult, hooks turnHooks) error {
	res.Isolation = "key-session"
	if v.SessionID == "" {
		res.Isolation = "per-request"
	}
	if err := s.syncAccounts(ctx); err != nil {
		return err
	}
	p, err := s.lockSession(projectMapping(r, v.SessionID))
	if err != nil {
		return err
	}
	defer s.releaseProject(p)

	plan := planTurn(p, v.Messages)
	if plan.reset {
		oldCID, oldAccount := p.ConversationID, p.AccountID
		if cookie := s.cookieFor(oldAccount); cookie != "" && oldCID != "" {
			go s.archiveConversation(oldCID, cookie)
		}
		s.projectInfo("会话历史不一致，已改用新的 Project：" + plan.reason)
		s.projectMu.Lock()
		p.ConversationID, p.AccountID, p.Sent, p.Systems, p.Turns = "", "", nil, nil, 0
		s.projectMu.Unlock()
		res.Reset = true
	}
	res.Mode, res.Forwarded = plan.mode, len(plan.send)

	if err := s.resolveFiles(ctx, plan.send); err != nil {
		return err
	}
	text, files := renderTurn(plan.send)
	text += toolPrompt(v.Tools, v.ToolChoice, v.ToolChoiceNamed)
	if len(files) > maxFilesPerRequest {
		return &statusInputError{400, "too_many_files", fmt.Sprintf("单轮最多上传 %d 个附件", maxFilesPerRequest)}
	}
	res.Files = len(files)

	var account Account
	var release func()
	var attachments []harkAttachment
	res.Reused = p.ConversationID != ""
	if res.Reused {
		// A Project is pinned to its original account: never fail over a live
		// session to another account.
		a, ok := s.accountByID(p.AccountID)
		if !ok || a.Status != "valid" {
			return &upstreamError{409, "session_account_unavailable", "会话绑定的 Hark 账号不可用；请结束该会话（session_end 或 POST /v1/sessions/{id}/end）后重新开始"}
		}
		res.AccountID, res.AccountName, res.Attempts, res.Bridge = a.ID, a.Name, 1, a.Bridge
		if release, err = s.acquireSlot(ctx, a.ID); err != nil {
			return err
		}
		defer release()
		account = a
		s.touchAccount(a.ID)
		for _, f := range files {
			att, err := s.uploadFile(ctx, a.Cookie, p.ConversationID, f)
			if err != nil {
				return err
			}
			attachments = append(attachments, att)
		}
	} else {
		tried := map[string]bool{}
		var lastAuth error
		for {
			a, rel, err := s.pickAccount(ctx, tried, v.Bridge)
			if err == nil {
				res.Attempts++
				res.AccountID, res.AccountName, res.Bridge = a.ID, a.Name, a.Bridge
			}
			if err != nil {
				if lastAuth != nil {
					return lastAuth
				}
				return err
			}
			cid, atts, err := s.prepareProject(ctx, a, s.zoneFor(a), files)
			if err == nil {
				account, release, attachments = a, rel, atts
				s.projectMu.Lock()
				p.AccountID, p.ConversationID = a.ID, cid
				s.projectMu.Unlock()
				break
			}
			rel()
			if isAuthError(err) && ctx.Err() == nil {
				// Credential failure before anything was sent: mark the
				// account and try the next one.
				s.markAccountFailed(a.ID, err)
				_, code, _ := turnError(ctx, err)
				res.Skipped = append(res.Skipped, a.Name+"："+code)
				tried[a.ID] = true
				lastAuth = err
				continue
			}
			return err
		}
		defer release()
		if err := s.persistProjects(); err != nil {
			return err
		}
	}

	payload := s.nativeProjectPayload(v, s.zoneFor(account), p.ConversationID, text, attachments, account.UserID)
	wrapped := turnHooks{poll: hooks.poll, status: hooks.status, accepted: func() {
		s.projectMu.Lock()
		p.Sent, p.Systems = plan.sent, plan.systems
		p.Turns++
		s.projectMu.Unlock()
		s.persistProjects()
		if hooks.accepted != nil {
			hooks.accepted()
		}
	}}
	reply, err := s.sendProjectPayload(ctx, account.Cookie, p.ConversationID, payload, s.pollInterval, wrapped)
	if err != nil {
		return err
	}
	res.Text, res.Outcome = reply.Text, reply.Outcome
	if len(reply.Attachments) > 0 {
		md := s.replyAttachmentsMarkdown(ctx, r, account.Cookie, reply.Attachments)
		if md != "" {
			if res.Text != "" {
				res.Text += "\n\n"
			}
			res.Text += md
		}
	}
	if v.SessionEnd {
		s.projectMu.Lock()
		p.Ending = true
		s.projectMu.Unlock()
		if s.persistProjects() == nil {
			res.Archived = s.archiveProject(p, account.Cookie)
		}
	}
	s.recordSuccess(account.ID, reply.Outcome)
	return nil
}

// prepareProject creates a Project on the chosen account and uploads the
// turn's files into it. Errors here happen before any message is sent.
func (s *Server) prepareProject(ctx context.Context, a Account, zone string, files []inputFile) (string, []harkAttachment, error) {
	cid, err := s.createProject(ctx, a.Cookie, zone)
	if err != nil {
		return "", nil, err
	}
	var atts []harkAttachment
	for _, f := range files {
		att, err := s.uploadFile(ctx, a.Cookie, cid, f)
		if err != nil {
			return "", nil, err
		}
		atts = append(atts, att)
	}
	return cid, atts, nil
}

func (s *Server) recordSuccess(accountID, outcome string) {
	s.mu.Lock()
	if outcome == "declined" {
		s.event("warning", "聊天请求", "上游拒绝执行请求；已保留原文与拒绝状态，未标记聊天验证通过")
		s.mu.Unlock()
		return
	}
	for i := range s.accounts {
		if s.accounts[i].ID == accountID {
			s.accounts[i].ChatVerified = true
			s.accounts[i].ChatChecked = time.Now().UTC().Format(time.RFC3339)
			s.accounts[i].Detail = "登录会话有效；Project 聊天已实测通过"
			if err := s.saveAccounts(); err != nil {
				s.event("warning", "保存聊天状态", "Project 回复成功，验证状态保存失败")
			}
			break
		}
	}
	s.event("success", "聊天请求", "独立 Project 已返回关联且完成的回复")
	s.mu.Unlock()
	s.maybeRefreshUsage(accountID)
}

// nativeProjectPayload builds the /api/messages/send body. Only the fields
// used by Hark's web client are sent by default; other OpenAI request fields
// are forwarded only with Options.ForwardExtra (they may be rejected by Hark,
// e.g. "run override not enabled").
func (s *Server) nativeProjectPayload(v chatRequest, zone, cid, text string, attachments []harkAttachment, userID string) map[string]any {
	payload := make(map[string]any)
	if s.forwardExtra {
		for name, value := range v.extra {
			payload[name] = value
		}
	}
	payload["conversationId"] = cid
	payload["message"] = projectScopedMessage(cid, text)
	if _, provided := payload["timezone"]; !provided {
		payload["timezone"] = zone
	}
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	if userID != "" {
		payload["expectedUserId"] = userID
	}
	return payload
}

func turnError(ctx context.Context, err error) (int, string, string) {
	status, code, message := 502, "upstream_error", "Hark 聊天请求失败"
	var e *upstreamError
	var in *statusInputError
	var ci *chatInputError
	switch {
	case errors.As(err, &e):
		status, code, message = e.status, e.code, e.message
	case errors.As(err, &in):
		status, code, message = in.status, in.code, in.message
	case errors.As(err, &ci):
		status, code, message = 400, ci.code, ci.message
	}
	if ctx.Err() != nil {
		status, code, message = 504, "upstream_timeout", "等待 Hark 关联且完成的回复超时；请求可能已受理，请勿自动重发"
	}
	if _, ok := embeddedAccount(ctx); ok {
		// The host schedules accounts: give it Hark's own account status.
		switch code {
		case "upstream_session_expired":
			status = 401
		case "upstream_access_blocked":
			status = 403
		}
	}
	return status, code, message
}

// outputFormat renders a turn as chat.completions or responses output.
type outputFormat interface {
	json(res *turnResult) any
	start(emit func(event string, v any))
	delta(emit func(event string, v any), text string)
	finish(emit func(event string, v any), full string, calls []toolCall)
	failure(emit func(event string, v any), code, message string)
	done() bool // whether to send "data: [DONE]"
}

// errorFormat 输出格式自带错误体（Anthropic 的 {"type":"error",...}）时实现。
type errorFormat interface {
	errorBody(status int, code, message string) any
}

func setTurnHeaders(w http.ResponseWriter, res *turnResult) {
	h := w.Header()
	h.Set("X-Hark-Context-Isolation", "instruction-only")
	h.Set("X-Hark-Conversation-Isolation", res.Isolation)
	h.Set("X-Hark-Session-Mode", "project")
	h.Set("X-Hark-Project-Reused", strconv.FormatBool(res.Reused))
	h.Set("X-Hark-Project-Reset", strconv.FormatBool(res.Reset))
	h.Set("X-Hark-Forward-Mode", res.Mode)
	h.Set("X-Hark-Forwarded-Messages", strconv.Itoa(res.Forwarded))
	h.Set("X-Hark-Uploaded-Files", strconv.Itoa(res.Files))
	h.Set("X-Hark-Bridge", strconv.FormatBool(res.Bridge))
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	for n > 0 && n < len(b) && !utf8.RuneStart(b[n]) {
		n--
	}
	return n
}

func (s *Server) serveTurn(w http.ResponseWriter, r *http.Request, v chatRequest, format outputFormat) {
	compact := isCheckpointCompaction(v)
	w.Header().Set("X-Hark-Context-Isolation", "instruction-only")
	toolsNote := ""
	switch {
	case v.ToolsDeclared > 0 && s.forwardExtra:
		toolsNote = "已开启 forward-extra，未做工具调用模拟"
	case v.ToolsDeclared > 0 && len(v.Tools) == 0:
		toolsNote = "tool_choice 为 none"
	}
	if s.forwardExtra {
		v.Tools = nil // 原样转发 tools 给 Hark（ForwardExtra），不做工具调用模拟
	}
	s.mu.Lock()
	timeout := s.settings.Timeout
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeout)*time.Second)
	defer cancel()
	res := &turnResult{}
	rec := reqRecFrom(r.Context())
	rec.update(func(l *RequestLog) {
		l.Model, l.Stream, l.Session = v.Model, v.Stream, v.SessionID != ""
		l.ToolsDeclared, l.ToolsSent, l.ToolsNote = v.ToolsDeclared, len(v.Tools), toolsNote
	})
	defer func() {
		rec.update(func(l *RequestLog) {
			l.Account, l.AccountID, l.Attempts, l.Skipped = res.AccountName, res.AccountID, res.Attempts, res.Skipped
			l.Reused, l.Files, l.Outcome, l.ReplyChars = res.Reused, res.Files, res.Outcome, len([]rune(res.Text))
			l.ToolCalls = len(res.ToolCalls)
		})
	}()
	if err := validateClientToolRequest(v, s.forwardExtra); err != nil {
		s.turnFailed(w, r, ctx, err, format)
		return
	}
	if !v.Stream {
		err := s.runTurn(ctx, r, v, res, turnHooks{})
		if err == nil {
			err = compactionFailure(compact, res)
			w.Header().Set("X-Hark-Upstream-Outcome", res.Outcome)
		}
		if err != nil {
			s.turnFailed(w, r, ctx, err, format)
			return
		}
		s.applyToolCalls(v, res)
		w.Header().Set("X-Hark-Upstream-Outcome", res.Outcome)
		if err := validateClientToolResult(v, res.ToolCalls); err != nil {
			s.turnFailed(w, r, ctx, err, format)
			return
		}
		setTurnHeaders(w, res)
		w.Header().Set("X-Hark-Project-Archived", strconv.FormatBool(res.Archived))
		w.Header().Set("X-Hark-Upstream-Outcome", res.Outcome)
		write(w, 200, format.json(res))
		return
	}

	var mu sync.Mutex
	started := false
	emitted := ""
	lastWrite := time.Now()
	var latestStatus turnStatus
	flusher, _ := w.(http.Flusher)
	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		lastWrite = time.Now()
		if flusher != nil {
			flusher.Flush()
		}
	}
	hooks := turnHooks{
		accepted: func() {
			mu.Lock()
			defer mu.Unlock()
			setTurnHeaders(w, res)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("X-Hark-Stream-Mode", "incremental")
			w.Header().Set("Trailer", "X-Hark-Upstream-Outcome, X-Hark-Project-Archived")
			w.WriteHeader(200)
			started = true
			format.start(emit)
		},
		poll: func(partial string) {
			mu.Lock()
			defer mu.Unlock()
			if !started {
				return
			}
			if len(v.Tools) > 0 {
				partial = toolSafePrefix(partial)
			}
			// A required-tool turn is validated before releasing assistant text.
			// Keepalives still flow, and ordinary auto/none turns stay incremental.
			if !compact && !requiresClientTool(v) && len(partial) > len(emitted) && strings.HasPrefix(partial, emitted) {
				format.delta(emit, partial[len(emitted):])
				emitted = partial
				return
			}
			if time.Since(lastWrite) > 15*time.Second {
				if ka, ok := format.(interface{ keepalive(func(string, any)) }); ok {
					ka.keepalive(emit)
					return
				}
				fmt.Fprint(w, ": keepalive\n\n")
				lastWrite = time.Now()
				if flusher != nil {
					flusher.Flush()
				}
			}
		},
	}
	if v.StatusEvents || r.Header.Get("X-Hark-Status") == "true" {
		hooks.status = func(status turnStatus) {
			mu.Lock()
			defer mu.Unlock()
			if !started || r.Context().Err() != nil {
				return
			}
			latestStatus = status
			emit("hark.status", status)
		}
	}
	err := s.runTurn(ctx, r, v, res, hooks)
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("X-Hark-Upstream-Outcome", res.Outcome)
	if err == nil {
		err = compactionFailure(compact, res)
	}
	if err == nil {
		s.applyToolCalls(v, res)
		err = validateClientToolResult(v, res.ToolCalls)
	}
	if err != nil {
		if !started {
			s.turnFailed(w, r, ctx, err, format)
			return
		}
		status, code, message := turnError(ctx, err)
		if latestStatus.Type != "" && latestStatus.Reason != "upstream_declined" && r.Context().Err() == nil {
			latestStatus.Status, latestStatus.Line, latestStatus.LineKey, latestStatus.Reason = "ended", "", "", "error"
			latestStatus.Sequence++
			emit("hark.status", latestStatus)
		}
		_ = status
		rec.update(func(l *RequestLog) { l.Code = code })
		s.logChatFailure(code)
		if r.Context().Err() == nil {
			format.failure(emit, code, message)
			if format.done() {
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
		}
		return
	}
	final := res.Text
	if len(res.ToolCalls) > 0 && strings.HasPrefix(emitted, final) {
		final = emitted // 已发出的文字里没有工具块，保持一致
	}
	if final != emitted {
		// Normally the final text extends what was streamed. If Hark rewrote
		// earlier text, only the part after the common prefix can be sent.
		rest := final[commonPrefix(emitted, final):]
		if strings.HasPrefix(final, emitted) {
			rest = final[len(emitted):]
		}
		if rest != "" {
			format.delta(emit, rest)
		}
	}
	if latestStatus.Type != "" && latestStatus.Status != "waiting_for_user" && latestStatus.Status != "ended" {
		latestStatus.Status, latestStatus.Line, latestStatus.LineKey, latestStatus.Reason = "ended", "", "", "stream_ended"
		latestStatus.Sequence++
		emit("hark.status", latestStatus)
	}
	format.finish(emit, final, res.ToolCalls)
	w.Header().Set("X-Hark-Upstream-Outcome", res.Outcome)
	w.Header().Set("X-Hark-Project-Archived", strconv.FormatBool(res.Archived))
	if format.done() {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) logChatFailure(code string) {
	s.mu.Lock()
	s.event("warning", "聊天请求", code)
	s.mu.Unlock()
}

func (s *Server) turnFailed(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, format outputFormat) {
	status, code, message := turnError(ctx, err)
	reqRecFrom(r.Context()).update(func(l *RequestLog) { l.Code = code })
	s.logChatFailure(code)
	if r.Context().Err() == nil {
		if ef, ok := format.(errorFormat); ok {
			write(w, status, ef.errorBody(status, code, message))
			return
		}
		fail(w, status, code, message)
	}
}

type chatFormat struct {
	id      string
	created int64
}

func newChatFormat() *chatFormat {
	return &chatFormat{id: randomID("chatcmpl_"), created: time.Now().Unix()}
}

func (f *chatFormat) chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{"id": f.id, "object": "chat.completion.chunk", "created": f.created, "model": "hark", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
}
func (f *chatFormat) json(res *turnResult) any {
	msg := map[string]any{"role": "assistant", "content": res.Text}
	finish := "stop"
	if len(res.ToolCalls) > 0 {
		msg["tool_calls"] = chatToolCalls(res.ToolCalls, false)
		if res.Text == "" {
			msg["content"] = nil
		}
		finish = "tool_calls"
	}
	return map[string]any{"id": f.id, "object": "chat.completion", "created": f.created, "model": "hark",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}
}

// chatToolCalls Chat Completions 的 tool_calls（custom 工具的输入放进 {"input": ...}）。
func chatToolCalls(calls []toolCall, indexed bool) []any {
	out := make([]any, 0, len(calls))
	for i, c := range calls {
		args := c.Arguments
		if c.Kind == "custom" {
			b, _ := json.Marshal(map[string]string{"input": c.Arguments})
			args = string(b)
		}
		tc := map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": args}}
		if indexed {
			tc["index"] = i
		}
		out = append(out, tc)
	}
	return out
}
func (f *chatFormat) start(emit func(string, any)) {
	emit("", f.chunk(map[string]any{"role": "assistant", "content": ""}, nil))
}
func (f *chatFormat) delta(emit func(string, any), text string) {
	emit("", f.chunk(map[string]any{"content": text}, nil))
}
func (f *chatFormat) finish(emit func(string, any), full string, calls []toolCall) {
	if len(calls) > 0 {
		emit("", f.chunk(map[string]any{"tool_calls": chatToolCalls(calls, true)}, nil))
		emit("", f.chunk(map[string]any{}, "tool_calls"))
		return
	}
	emit("", f.chunk(map[string]any{}, "stop"))
}
func (f *chatFormat) failure(emit func(string, any), code, message string) {
	emit("", map[string]any{"error": map[string]string{"code": code, "message": message, "type": "server_error"}})
}
func (f *chatFormat) done() bool { return true }

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		fail(w, 401, "invalid_api_key", "API Key 无效")
		return
	}
	var v chatRequest
	if !s.decodeChat(w, r, &v) {
		return
	}
	s.serveTurn(w, r, v, newChatFormat())
}

// The dashboard is already protected by the administrator session. Keeping
// this relay server-side means the browser never receives or submits an API
// Key; the stored Hark account remains the only upstream credential.
func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request) {
	var v chatRequest
	if !s.decodeChat(w, r, &v) {
		return
	}
	s.serveTurn(w, r, v, newChatFormat())
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var v chatRequest
	if !s.decodeChat(w, r, &v) {
		return
	}
	files := 0
	for _, m := range v.Messages {
		files += len(m.Files)
	}
	s.mu.Lock()
	s.event("success", "校验请求", "请求格式通过；未发送到 Hark")
	s.mu.Unlock()
	write(w, 200, map[string]any{"valid": true, "upstream_sent": false, "messages": len(v.Messages), "files": files, "message": "请求格式正确；此处仅校验，请使用 API Key 调用聊天接口。"})
}

// applyToolCalls 请求带了 tools 时，把回复里的 <tool_call> 块解析成工具调用。
func (s *Server) applyToolCalls(v chatRequest, res *turnResult) {
	if len(v.Tools) == 0 {
		return
	}
	res.Text, res.ToolCalls = extractToolCalls(res.Text, v.Tools)
	if len(res.ToolCalls) > 0 {
		s.mu.Lock()
		s.event("info", "工具调用", toolNames(res.ToolCalls))
		s.mu.Unlock()
	}
}
