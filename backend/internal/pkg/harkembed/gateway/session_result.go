package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

type sessionResultBody struct {
	Text        string     `json:"text"`
	Attachments []harkFile `json:"attachments"`
}

type sessionResultData struct {
	ReadOnly            bool               `json:"read_only"`
	ContinuationApplied bool               `json:"continuation_applied"`
	SessionID           string             `json:"session_id"`
	RequestID           string             `json:"request_id"`
	ResponseMessageID   string             `json:"response_message_id"`
	Status              string             `json:"status"`
	ErrorCode           string             `json:"error_code"`
	ErrorMessage        string             `json:"error_message"`
	Result              sessionResultBody  `json:"result"`
	SubmissionFailure   *submissionFailure `json:"submission_failure,omitempty"`
}

// sessionResult is an embedded diagnostic, never a model execution response.
func (s *Server) sessionResult(w http.ResponseWriter, r *http.Request) {
	result, err := s.readSessionResult(r, "")
	if err != nil {
		// readSessionResult has already classified any upstream cancellation.
		// Do not turn local validation errors into timeouts after cancellation.
		if failure, ok := err.(*upstreamError); ok {
			fail(w, failure.status, failure.code, failure.message)
		} else {
			fail(w, 500, "session_result_failed", "只读结果不可用")
		}
		return
	}
	write(w, 200, result)
}

func (s *Server) readSessionResult(r *http.Request, expectedProject string) (sessionResultData, error) {
	a, ok := embeddedAccount(r.Context())
	if !ok || s.embed == nil {
		return sessionResultData{}, &upstreamError{401, "embedded_account_required", "只读恢复需要宿主管理员指定原账号"}
	}
	sessionID := r.PathValue("id")
	if sessionID == "" || validateSession(chatRequest{SessionID: sessionID}) != nil {
		return sessionResultData{}, &upstreamError{400, "invalid_session", "会话 ID 无效"}
	}
	// Do not accept a Project supplied by the caller, duplicate selectors, or
	// silently ignored malformed query parameters.
	q, err := parseResultQuery(r)
	if err != nil {
		return sessionResultData{}, &upstreamError{400, "invalid_result_selector", "只支持成对的 request_id 和 response_message_id"}
	}
	requestID, expected := q.Get("request_id"), q.Get("response_message_id")
	id := projectMapping(r, sessionID)
	s.projectMu.Lock()
	p := s.projects[id]
	if p == nil || p.AccountID != a.ID || p.ConversationID == "" || p.Archived || p.Ending {
		s.projectMu.Unlock()
		return sessionResultData{}, &upstreamError{404, "session_not_found", "原账号下没有可读取的会话映射"}
	}
	if p.Busy {
		s.projectMu.Unlock()
		return sessionResultData{}, &upstreamError{409, "session_busy", "原会话仍在处理中；未重发请求"}
	}
	if expectedProject != "" && p.ConversationID != expectedProject {
		s.projectMu.Unlock()
		return sessionResultData{}, &upstreamError{409, "session_changed", "预演原 Project 已变化；未读取其他 Project"}
	}
	// Copy under the mutex, never use lockSession/releaseProject: those update
	// LastUsed, cursors and the on-disk mapping even on failed requests.
	cid, lastUsed, turns := p.ConversationID, p.LastUsed, p.Turns
	var pending pendingProjectTurn
	hasPending := p.PendingTurn != nil
	if hasPending {
		pending = *p.PendingTurn
		if requestID == "" && expected == "" {
			requestID, expected = pending.RequestID, pending.Expected
		}
	}
	s.projectMu.Unlock()
	if !validResultID(requestID) || !validResultID(expected) || requestID == expected {
		return sessionResultData{}, &upstreamError{400, "result_correlation_required", "旧会话需要明确提供原始请求及响应关联 ID"}
	}
	if hasPending && (pending.RequestID != requestID || pending.Expected != expected) {
		return sessionResultData{}, &upstreamError{409, "result_correlation_mismatch", "关联 ID 与原会话待恢复请求不一致"}
	}
	if hasPending && pending.Failure != nil {
		// This is saved submission evidence, not a fabricated native result.
		// No GET, retry, registry refresh, installation or pending clear occurs.
		return sessionResultData{ReadOnly: true, SessionID: sessionID, RequestID: requestID, ResponseMessageID: expected,
			Status: "submission_" + pending.Failure.Outcome, ErrorCode: pending.Failure.Code,
			ErrorMessage: pending.Failure.Message, SubmissionFailure: pending.Failure}, nil
	}
	if a.Cookie == "" || validAccountProxy(a.Proxy) != nil {
		return sessionResultData{}, &upstreamError{400, "invalid_original_account", "原账号凭据或代理配置不可用；未换号"}
	}
	// A private reader uses exactly the supplied account's current cookie,
	// proxy and UA. No account refresh, scheduler, usage probe or registry write.
	reader := &Server{upstream: s.upstream, client: s.client, upstreamProxy: s.upstreamProxy,
		accounts: []Account{{ID: a.ID, Cookie: a.Cookie, Proxy: a.Proxy, UserAgent: a.UserAgent}}}
	defer func() {
		for _, transport := range reader.proxyCache.m {
			transport.CloseIdleConnections()
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	messages, err := reader.fetchHistory(ctx, a.Cookie, cid)
	if err != nil {
		status, code, detail := turnError(ctx, err)
		return sessionResultData{}, &upstreamError{status, code, detail}
	}
	// A legacy selector is not trusted just because it was supplied by an
	// administrator: require the native user AND its linked primary job.
	users, primaries, correlated := 0, 0, true
	for _, m := range messages {
		if m.ID == requestID {
			users++
			correlated = correlated && m.Role == "user"
		}
		if m.ID == expected {
			primaries++
			correlated = correlated && m.Role == "assistant" && m.TriggeredBy == requestID
		}
	}
	if users != 1 || primaries != 1 || !correlated {
		return sessionResultData{}, &upstreamError{409, "result_correlation_unverified", "原始事件未证实请求与响应属于同一轮；未返回其他轮结果"}
	}
	// If a submission/end happened during the one bounded GET, refuse the
	// stale view without modifying either the old or the new state.
	s.projectMu.Lock()
	current := s.projects[id]
	changed := current != p || p.Busy || p.Archived || p.Ending || p.ConversationID != cid || p.AccountID != a.ID || p.Turns != turns || !p.LastUsed.Equal(lastUsed)
	if p.PendingTurn == nil {
		changed = changed || pending != (pendingProjectTurn{})
	} else {
		changed = changed || *p.PendingTurn != pending
	}
	s.projectMu.Unlock()
	if changed {
		return sessionResultData{}, &upstreamError{409, "session_changed", "读取期间原会话状态已变化；未应用恢复结果"}
	}
	reply, resultErr := completedProjectReply(messages, expected, requestID)
	status, code, detail := reply.Outcome, "", ""
	if resultErr != nil {
		status = "failed"
		_, code, detail = turnError(ctx, resultErr)
		reply.Text = partialReply(messages, expected, requestID)
	} else if status == "" {
		status = "processing"
	}
	if status != "failed" && status != "declined" {
		for _, m := range messages {
			if boundToTurn(m, expected, requestID) && m.InputNeeded {
				status = "awaiting_input"
			}
		}
		if status == "completed" && strings.Contains(reply.Text, "<tool_call") {
			status = "awaiting_client_tool"
		}
	}
	return sessionResultData{ReadOnly: true, SessionID: sessionID, RequestID: requestID, ResponseMessageID: expected,
		Status: status, ErrorCode: code, ErrorMessage: detail, Result: sessionResultBody{Text: reply.Text, Attachments: reply.Attachments}}, nil
}

func validResultID(id string) bool {
	return id != "" && len(id) <= 200 && strings.TrimSpace(id) == id && strings.IndexFunc(id, unicode.IsControl) < 0
}

func parseResultQuery(r *http.Request) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	for k, values := range q {
		if (k != "request_id" && k != "response_message_id") || len(values) != 1 || !validResultID(values[0]) {
			return nil, errors.New("invalid result selector")
		}
	}
	if (q.Get("request_id") == "") != (q.Get("response_message_id") == "") {
		return nil, errors.New("paired result selector required")
	}
	return q, nil
}
