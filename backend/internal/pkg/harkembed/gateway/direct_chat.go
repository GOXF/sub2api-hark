package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type upstreamError struct {
	status        int
	code, message string
}

func (e *upstreamError) Error() string { return e.message }

// isAuthError reports a credential problem (HTTP 401/403 other than an
// explicit feature rejection). Only these trigger account failover.
func isAuthError(err error) bool {
	e, ok := err.(*upstreamError)
	return ok && (e.code == "upstream_session_expired" || e.code == "upstream_access_blocked")
}

func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

const defaultJSONLimit = 2 * 1024 * 1024
const historyJSONLimit = 64 * 1024 * 1024

func (s *Server) harkJSON(ctx context.Context, method, path, cookie string, input, output any) error {
	return s.harkJSONLimit(ctx, method, path, cookie, input, output, defaultJSONLimit)
}

func (s *Server) harkJSONLimit(ctx context.Context, method, path, cookie string, input, output any, limit int64) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.upstream+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", s.upstream)
	}
	timeout := s.client.Timeout
	if limit > defaultJSONLimit {
		timeout = 0 // bounded by ctx; large history reads may take longer
	}
	resp, err := s.clientFor(cookie, timeout, false).Do(req)
	if err != nil {
		return &upstreamError{502, "upstream_connection_failed", "Hark 连接失败"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
		var rejection struct {
			Error string `json:"error"`
		}
		if resp.StatusCode == 403 && json.Unmarshal(body, &rejection) == nil && rejection.Error == "run override not enabled" {
			return &upstreamError{403, "upstream_run_override_denied", "Hark: run override not enabled"}
		}
		code, message := "upstream_http_error", fmt.Sprintf("Hark 返回 HTTP %d", resp.StatusCode)
		status := 502
		switch resp.StatusCode {
		case 400, 422:
			status, code, message = resp.StatusCode, "upstream_request_rejected", fmt.Sprintf("Hark 拒绝转发的请求参数（HTTP %d）", resp.StatusCode)
		case 401:
			code, message = "upstream_session_expired", "Hark 登录会话已失效，请重新导入 Cookie"
		case 403:
			code, message = "upstream_access_blocked", "Hark 返回 HTTP 403，访问受阻（"+upstreamDiag(method, path, resp, body, s.routeDiag(cookie))+"）"
		case 429:
			status, code, message = 429, "upstream_rate_limited", "Hark 请求限流或额度不足，请稍后再试"
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			code, message = "upstream_session_expired", "Hark 重定向了 API 请求，Cookie 可能已失效"
		}
		return &upstreamError{status, code, message}
	}
	if output == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		return &upstreamError{502, "upstream_response_invalid", "Hark 响应读取失败或内容过长"}
	}
	if json.Unmarshal(b, output) != nil {
		return &upstreamError{502, "upstream_response_invalid", "Hark 返回非预期 JSON 响应"}
	}
	return nil
}

type harkFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MediaType   string `json:"mediaType"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"downloadUrl"`
}

type harkMessage struct {
	ID           string          `json:"id"`
	Role         string          `json:"role"`
	Content      string          `json:"content"`
	Status       string          `json:"status"`
	State        string          `json:"state"`
	JobStatus    string          `json:"jobStatus"`
	Streaming    *bool           `json:"isStreaming"`
	TriggeredBy  string          `json:"triggeredByMessageId"`
	Source       string          `json:"source"`
	Error        json.RawMessage `json:"error"`
	Presentation struct {
		Kind string `json:"kind"`
	} `json:"presentation"`
	Onboarding  json.RawMessage  `json:"onboarding"`
	Narration   *statusNarration `json:"narration"`
	InputNeeded bool             `json:"inputNeeded"`
	Attachments []harkFile       `json:"attachments"`
}

// fetchHistory reads /api/messages/history and accepts both shapes seen in
// the wild: a messages array, or a messages object keyed by id with a "log"
// array giving the order (jyao0708/ozuijoy v2 shape).
func (s *Server) fetchHistory(ctx context.Context, cookie, cid string) ([]harkMessage, error) {
	var raw struct {
		Messages json.RawMessage `json:"messages"`
		Log      []struct {
			ID string `json:"id"`
		} `json:"log"`
	}
	if err := s.harkJSONLimit(ctx, http.MethodGet, "/api/messages/history?conversationId="+url.QueryEscape(cid), cookie, nil, &raw, historyJSONLimit); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw.Messages)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var list []harkMessage
		if json.Unmarshal(trimmed, &list) != nil {
			return nil, &upstreamError{502, "upstream_response_invalid", "Hark 历史消息结构无法识别"}
		}
		return list, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, &upstreamError{502, "upstream_response_invalid", "Hark 历史消息结构无法识别"}
	}
	byID := map[string]harkMessage{}
	var order []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, &upstreamError{502, "upstream_response_invalid", "Hark 历史消息结构无法识别"}
		}
		key, _ := tok.(string)
		var m harkMessage
		if dec.Decode(&m) != nil {
			return nil, &upstreamError{502, "upstream_response_invalid", "Hark 历史消息结构无法识别"}
		}
		if m.ID == "" {
			m.ID = key
		}
		if _, dup := byID[m.ID]; !dup {
			order = append(order, m.ID)
		}
		byID[m.ID] = m
	}
	out := make([]harkMessage, 0, len(byID))
	seen := map[string]bool{}
	for _, entry := range raw.Log {
		if m, ok := byID[entry.ID]; ok && !seen[entry.ID] {
			out = append(out, m)
			seen[entry.ID] = true
		}
	}
	for _, id := range order {
		if !seen[id] {
			out = append(out, byID[id])
		}
	}
	return out, nil
}

// Hark uses responseMessageId for a job record, and separate send_bubble IDs
// for text. Correlate both through the submitted user message ID and wait for
// all observed jobs/bubbles in that request to finish, never for stable text.
type projectReply struct {
	Text        string
	Outcome     string
	Attachments []harkFile
}

func completedReply(messages []harkMessage, expected, requestID string) (string, error) {
	reply, err := completedProjectReply(messages, expected, requestID)
	return reply.Text, err
}

func boundToTurn(m harkMessage, expected, requestID string) bool {
	if (m.ID != expected && (requestID == "" || m.TriggeredBy != requestID)) || m.Role != "assistant" || len(m.Onboarding) > 0 && string(m.Onboarding) != "null" {
		return false
	}
	if m.ID == expected && requestID != "" && m.TriggeredBy != requestID {
		return false
	}
	return true
}

func textPart(m harkMessage, expected string) bool {
	return strings.TrimSpace(m.Content) != "" && (m.ID == expected || m.Source == "send_bubble" && (m.Presentation.Kind == "text" || m.Presentation.Kind == "summary"))
}

func messageStatus(m harkMessage) string {
	status := m.JobStatus
	if status == "" {
		status = m.Status
	}
	if status == "" {
		status = m.State
	}
	return status
}

func isDoneStatus(status string) bool {
	return status == "completed" || status == "complete" || status == "finished" || status == "done"
}

func completedProjectReply(messages []harkMessage, expected, requestID string) (projectReply, error) {
	primaryDone, pending := false, false
	var parts []string
	var files []harkFile
	seenFiles := map[string]bool{}
	for _, m := range messages {
		if !boundToTurn(m, expected, requestID) {
			continue
		}
		status := messageStatus(m)
		// A budget-capped job can also carry a generic error payload. Keep the
		// structured status before that payload hides the budget/limit cause.
		if status == "cost_capped" {
			return projectReply{}, &upstreamError{429, "upstream_budget_exhausted", "Hark 回复任务因预算或额度上限终止（cost_capped）；未自动重试，请确认上限后显式继续"}
		}
		if len(m.Error) > 0 && string(m.Error) != "null" && string(m.Error) != "false" && string(m.Error) != `""` {
			return projectReply{}, &upstreamError{502, "upstream_generation_failed", "Hark 回复任务返回错误"}
		}
		switch status {
		case "failed", "error", "cancelled", "canceled", "interrupted":
			return projectReply{}, &upstreamError{502, "upstream_generation_failed", "Hark 回复任务失败或取消"}
		case "completed", "complete", "finished", "done":
			if m.Streaming != nil && *m.Streaming {
				pending = true
			} else if m.ID == expected {
				primaryDone = true
			}
		default:
			if status != "" || m.ID == expected {
				pending = true
			}
		}
		if m.Streaming != nil && *m.Streaming {
			pending = true
		}
		finished := m.Streaming != nil && !*m.Streaming || isDoneStatus(status)
		if textPart(m, expected) {
			if m.Streaming == nil && status == "" {
				return projectReply{}, &upstreamError{502, "upstream_protocol_unverified", "Hark 文字回复缺少已识别的完成字段"}
			}
			if finished {
				parts = append(parts, m.Content)
			}
		}
		if finished {
			for _, f := range m.Attachments {
				key := f.ID + "\x00" + f.DownloadURL + "\x00" + f.Name
				if f.DownloadURL != "" && !seenFiles[key] {
					seenFiles[key] = true
					files = append(files, f)
				}
			}
		}
	}
	if primaryDone && !pending {
		if len(parts) > 0 || len(files) > 0 {
			// Plain-text refusals can accompany completed jobs. Only classify an
			// otherwise empty result with one evidenced, turn-bound text bubble.
			if len(parts) == 1 && len(files) == 0 {
				for _, m := range messages {
					if boundToTurn(m, expected, requestID) && recordedBackendRefusal(m) {
						return projectReply{Text: parts[0], Outcome: "declined"}, nil
					}
				}
			}
			return projectReply{Text: strings.Join(parts, "\n\n"), Outcome: "completed", Attachments: files}, nil
		}
		if notice := declinedNoticeForTurn(messages, expected, requestID); notice != "" {
			return projectReply{Text: notice, Outcome: "declined"}, nil
		}
	}
	return projectReply{}, nil
}

// partialReply is the visible text of the turn so far, including text that
// is still streaming. It drives incremental SSE deltas only; the final answer
// always comes from completedProjectReply.
func partialReply(messages []harkMessage, expected, requestID string) string {
	var parts []string
	for _, m := range messages {
		if boundToTurn(m, expected, requestID) && textPart(m, expected) {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Hark can append a declined_notice without triggeredByMessageId after an
// empty completed job. Bind it by history order inside the submitted user turn,
// never by timestamp or by the notice's text. The caller checks all bound jobs.
func declinedNoticeForTurn(messages []harkMessage, expected, requestID string) string {
	if expected == "" || requestID == "" {
		return ""
	}
	inTurn, primarySeen := false, false
	for _, m := range messages {
		if m.Role == "user" {
			if inTurn {
				break
			}
			inTurn = m.ID == requestID
			continue
		}
		if !inTurn {
			continue
		}
		if m.ID == expected && m.Role == "assistant" && m.TriggeredBy == requestID {
			primarySeen = true
			continue
		}
		if primarySeen && m.Role == "assistant" && m.Source == "declined_notice" && m.Presentation.Kind == "declined" &&
			(m.TriggeredBy == "" || m.TriggeredBy == requestID) && m.Streaming != nil && !*m.Streaming &&
			(len(m.Onboarding) == 0 || string(m.Onboarding) == "null") && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}

// renderTurn converts the messages being forwarded into Hark's single
// "message" string and the list of files to attach. One user message is sent
// verbatim; several messages become ROLE-labelled blocks.
func renderTurn(messages []chatMessage) (string, []inputFile) {
	var files []inputFile
	for _, m := range messages {
		files = append(files, m.Files...)
	}
	if len(messages) == 1 && messages[0].Role == "user" {
		text := messages[0].Content
		if strings.TrimSpace(text) == "" && len(files) > 0 {
			text = "请查看附件。"
		}
		return text, files
	}
	var blocks []string
	for _, m := range messages {
		var lines []string
		if strings.TrimSpace(m.Content) != "" {
			lines = append(lines, m.Content)
		}
		if m.ToolCalls != "" {
			lines = append(lines, "tool_calls: "+m.ToolCalls)
		}
		for _, f := range m.Files {
			lines = append(lines, "[附件: "+f.Name+"]")
		}
		if len(lines) == 0 {
			continue
		}
		blocks = append(blocks, strings.ToUpper(m.Role)+":\n"+strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n"), files
}

func transcript(v chatRequest) string {
	text, _ := renderTurn(v.Messages)
	return text
}

func (s *Server) runDirect(ctx context.Context, cookie, zone string, v chatRequest, interval time.Duration) (string, error) {
	cid, err := s.createProject(ctx, cookie, zone)
	if err != nil {
		return "", err
	}
	payload := map[string]any{"timezone": zone, "conversationId": cid, "message": projectScopedMessage(cid, transcript(v))}
	reply, err := s.sendProjectPayload(ctx, cookie, cid, payload, interval, turnHooks{})
	return reply.Text, err
}

func (s *Server) createProjectDirect(ctx context.Context, cookie, zone string) (string, error) {
	var created struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversationId"`
	}
	if err := s.harkJSON(ctx, "POST", "/api/conversations", cookie, map[string]any{"timezone": zone, "title": "Hark2API Project", "autoTitle": true}, &created); err != nil {
		return "", err
	}
	cid := created.ConversationID
	if cid == "" {
		cid = created.ID
	}
	if cid == "" || len(cid) > 200 {
		return "", &upstreamError{502, "upstream_protocol_unverified", "Hark 建会话响应缺少 conversationId"}
	}
	return cid, nil
}

type turnHooks struct {
	// accepted runs once Hark confirmed the message; streaming starts here.
	accepted func()
	// poll runs after every history read with the visible text so far.
	poll   func(partial string)
	status func(turnStatus)
}

func (s *Server) sendProjectPayload(ctx context.Context, cookie, cid string, payload map[string]any, interval time.Duration, hooks turnHooks) (projectReply, error) {
	expected := uuid()
	requestID := uuid()
	var accepted struct {
		Success        *bool  `json:"success"`
		ConversationID string `json:"conversationId"`
		MessageID      string `json:"messageId"`
		Redirected     *bool  `json:"redirected"`
	}
	payload["idempotencyKey"], payload["responseMessageId"] = requestID, expected
	if err := s.harkJSON(ctx, "POST", "/api/messages/send?cid="+url.QueryEscape(cid), cookie, payload, &accepted); err != nil {
		return projectReply{}, err
	}
	if accepted.Success == nil || !*accepted.Success {
		return projectReply{}, &upstreamError{502, "upstream_protocol_unverified", "Hark 未确认消息受理；未自动重试发送"}
	}
	if accepted.ConversationID != cid || accepted.MessageID != requestID || accepted.Redirected == nil || *accepted.Redirected {
		return projectReply{}, &upstreamError{502, "upstream_project_mismatch", "Hark 未确认消息进入本次独立 Project；未自动重发"}
	}
	if hooks.accepted != nil {
		hooks.accepted()
	}
	var statusTracker *turnStatusTracker
	var statusDone chan struct{}
	if hooks.status != nil {
		statusTracker = newTurnStatusTracker(cid, expected, requestID, hooks.status)
		statusCtx, stopStatus := context.WithCancel(ctx)
		statusDone = make(chan struct{})
		go func() { defer close(statusDone); s.watchTurnStatus(statusCtx, cookie, cid, statusTracker) }()
		defer func() { stopStatus(); <-statusDone }()
	}
	for {
		messages, err := s.fetchHistory(ctx, cookie, cid)
		if err != nil {
			return projectReply{}, err
		}
		if statusTracker != nil {
			statusTracker.history(messages)
		}
		reply, err := completedProjectReply(messages, expected, requestID)
		if err != nil {
			return reply, err
		}
		if reply.Text != "" || len(reply.Attachments) > 0 {
			if statusTracker != nil {
				// A job can finish just before the request-level awaiting-person
				// patch arrives. Give the existing feed a bounded drain, not a
				// replay or an unbounded wait for optional UI metadata.
				timer := time.NewTimer(200 * time.Millisecond)
				select {
				case <-statusTracker.settled:
				case <-statusDone:
				case <-timer.C:
				case <-ctx.Done():
				}
				timer.Stop()
				if reply.Outcome == "declined" {
					statusTracker.finishDeclined()
				}
			}
			return reply, nil
		}
		if hooks.poll != nil {
			hooks.poll(partialReply(messages, expected, requestID))
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return projectReply{}, ctx.Err()
		case <-timer.C:
		}
	}
}
