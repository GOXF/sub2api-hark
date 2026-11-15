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

// sessionResult is an embedded, administrator-facing diagnostic, not a model
// response. It never submits, acknowledges, or installs a turn or tool call.
func (s *Server) sessionResult(w http.ResponseWriter, r *http.Request) {
	a, ok := embeddedAccount(r.Context())
	if !ok || s.embed == nil {
		fail(w, 401, "embedded_account_required", "只读恢复需要宿主管理员指定原账号")
		return
	}
	sessionID := r.PathValue("id")
	if sessionID == "" || validateSession(chatRequest{SessionID: sessionID}) != nil {
		fail(w, 400, "invalid_session", "会话 ID 无效")
		return
	}
	// Do not accept a Project supplied by the caller, duplicate selectors, or
	// silently ignored malformed query parameters.
	q, err := parseResultQuery(r)
	if err != nil {
		fail(w, 400, "invalid_result_selector", "只支持成对的 request_id 和 response_message_id")
		return
	}
	requestID, expected := q.Get("request_id"), q.Get("response_message_id")
	id := projectMapping(r, sessionID)
	s.projectMu.Lock()
	p := s.projects[id]
	if p == nil || p.AccountID != a.ID || p.ConversationID == "" || p.Archived || p.Ending {
		s.projectMu.Unlock()
		fail(w, 404, "session_not_found", "原账号下没有可读取的会话映射")
		return
	}
	if p.Busy {
		s.projectMu.Unlock()
		fail(w, 409, "session_busy", "原会话仍在处理中；未重发请求")
		return
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
		fail(w, 400, "result_correlation_required", "旧会话需要明确提供原始请求及响应关联 ID")
		return
	}
	if hasPending && (pending.RequestID != requestID || pending.Expected != expected) {
		fail(w, 409, "result_correlation_mismatch", "关联 ID 与原会话待恢复请求不一致")
		return
	}
	if a.Cookie == "" || validAccountProxy(a.Proxy) != nil {
		fail(w, 400, "invalid_original_account", "原账号凭据或代理配置不可用；未换号")
		return
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
		fail(w, status, code, detail)
		return
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
		fail(w, 409, "result_correlation_unverified", "原始事件未证实请求与响应属于同一轮；未返回其他轮结果")
		return
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
		fail(w, 409, "session_changed", "读取期间原会话状态已变化；未应用恢复结果")
		return
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
	write(w, 200, map[string]any{
		"read_only": true, "continuation_applied": false, "session_id": sessionID,
		"request_id": requestID, "response_message_id": expected, "status": status,
		"error_code": code, "error_message": detail,
		"result": map[string]any{"text": reply.Text, "attachments": reply.Attachments},
	})
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
