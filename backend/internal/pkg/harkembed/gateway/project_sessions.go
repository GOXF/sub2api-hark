package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// projectSession maps one (API key, session_id) pair to one Hark Project on
// one account. Sent holds fingerprints of the client messages already
// forwarded, so later turns forward only the new tail.
type projectSession struct {
	MappingID      string    `json:"mapping_id"`
	AccountID      string    `json:"account_id"`
	ConversationID string    `json:"conversation_id"`
	LastUsed       time.Time `json:"last_used"`
	Ending         bool      `json:"ending,omitempty"`
	Archived       bool      `json:"archived,omitempty"`
	Sent           []string  `json:"sent,omitempty"`
	Systems        []string  `json:"systems,omitempty"`
	Turns          int       `json:"turns,omitempty"`
	Busy           bool      `json:"-"`
}

func validateSession(v chatRequest) error {
	if len(v.SessionID) > 256 || strings.TrimSpace(v.SessionID) != v.SessionID || strings.IndexFunc(v.SessionID, unicode.IsControl) >= 0 {
		return inputError("invalid_session", "会话 ID 需要不超过 256 字节，且不含首尾空格或控制字符")
	}
	if v.SessionEnd && v.SessionID == "" {
		return inputError("invalid_session", "结束会话需要提供 session_id 或 X-Session-Id")
	}
	return nil
}

func sessionNamespace(r *http.Request) string {
	if a, ok := embeddedAccount(r.Context()); ok {
		return "embed:" + a.ID
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		key := requestAPIKey(r)
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		h := sha256.Sum256([]byte(key))
		return "api:" + hex.EncodeToString(h[:])
	}
	return "admin-playground"
}

// projectMapping never derives a session from the OpenAI "user" field or any
// other implicit value: without session_id every request is one-shot.
func projectMapping(r *http.Request, sessionID string) string {
	if sessionID == "" {
		return randomID("ephemeral_")
	}
	h := sha256.Sum256([]byte(sessionNamespace(r) + "\x00" + sessionID))
	return hex.EncodeToString(h[:])
}

// lockSession marks the mapping busy. Concurrent requests into one Project are
// refused rather than interleaved.
func (s *Server) lockSession(id string) (*projectSession, error) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	p := s.projects[id]
	if p != nil {
		if p.Busy {
			return nil, &upstreamError{409, "session_busy", "同一会话正在处理请求，请等待完成后再发送"}
		}
		if p.Ending && !p.Archived {
			return nil, &upstreamError{409, "session_ending", "会话正在归档，请稍后再开启新会话"}
		}
		if p.Archived {
			p = nil
		}
	}
	if p == nil {
		p = &projectSession{MappingID: id}
		s.projects[id] = p
	}
	p.Busy, p.LastUsed = true, time.Now().UTC()
	return p, nil
}

func (s *Server) releaseProject(p *projectSession) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	p.Busy, p.LastUsed = false, time.Now().UTC()
	if p.ConversationID == "" || p.Archived {
		if s.projects[p.MappingID] == p {
			delete(s.projects, p.MappingID)
		}
	}
	if err := s.saveProjectsLocked(); err != nil {
		s.projectWarning("会话映射保存失败")
	}
}

func (s *Server) projectWarning(detail string) {
	s.mu.Lock()
	s.event("warning", "Project 会话", detail)
	s.mu.Unlock()
}

func (s *Server) projectInfo(detail string) {
	s.mu.Lock()
	s.event("info", "Project 会话", detail)
	s.mu.Unlock()
}

func (s *Server) persistProjects() error {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if err := s.saveProjectsLocked(); err != nil {
		s.projectWarning("会话映射保存失败")
		return &upstreamError{500, "session_store_failed", "Project 会话映射保存失败；未自动重发消息"}
	}
	return nil
}

func (s *Server) saveProjectsLocked() error {
	items := make(map[string]*projectSession)
	for id, p := range s.projects {
		if p.ConversationID != "" {
			items[id] = p
		}
	}
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return atomicPrivate(filepath.Join(s.data, "project-sessions.json"), b)
}

func (s *Server) loadProjects() error {
	s.projects = make(map[string]*projectSession)
	b, err := os.ReadFile(filepath.Join(s.data, "project-sessions.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(b) > 64*1024*1024 {
		return errors.New("project session store unreadable or too large")
	}
	if json.Unmarshal(b, &s.projects) != nil || s.projects == nil {
		return errors.New("project session store invalid")
	}
	for id, p := range s.projects {
		if p == nil || p.MappingID != id || p.AccountID == "" || p.ConversationID == "" || len(p.ConversationID) > 200 || p.LastUsed.IsZero() {
			return errors.New("project session record invalid")
		}
	}
	return nil
}

// turnPlan says which client messages to forward on this turn.
type turnPlan struct {
	send    []chatMessage
	reset   bool   // the existing Project cannot continue; start a new one
	reason  string // why reset (logged)
	mode    string // new | tail | append
	sent    []string
	systems []string
}

func hashes(messages []chatMessage) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		out[i] = m.Hash
	}
	return out
}

func isSystemRole(role string) bool { return role == "system" || role == "developer" }

func hasContent(messages []chatMessage) bool {
	for _, m := range messages {
		if m.Role != "assistant" && (strings.TrimSpace(m.Content) != "" || len(m.Files) > 0) {
			return true
		}
	}
	return false
}

func systemHashes(messages []chatMessage, prev []string) []string {
	out := append([]string(nil), prev...)
	seen := map[string]bool{}
	for _, h := range out {
		seen[h] = true
	}
	for _, m := range messages {
		if isSystemRole(m.Role) && !seen[m.Hash] {
			seen[m.Hash] = true
			out = append(out, m.Hash)
		}
	}
	if len(out) > 64 {
		out = out[len(out)-64:]
	}
	return out
}

// planTurn decides what to forward into an existing Project:
//   - full history whose prefix matches what was already sent → only the tail
//     (leading assistant messages, i.e. Hark's own replies echoed back, are
//     skipped);
//   - requests without any assistant message (clients that send only the new
//     turn) → appended as-is, minus system prompts already sent;
//   - full history that diverges (edited/regenerated history) → new Project.
func planTurn(p *projectSession, messages []chatMessage) turnPlan {
	all := hashes(messages)
	if p == nil || p.ConversationID == "" {
		return turnPlan{send: messages, mode: "new", sent: all, systems: systemHashes(messages, nil)}
	}
	n := len(p.Sent)
	if n > 0 && len(messages) > n {
		match := true
		for i := 0; i < n; i++ {
			if messages[i].Hash != p.Sent[i] {
				match = false
				break
			}
		}
		if match {
			tail := messages[n:]
			for len(tail) > 0 && tail[0].Role == "assistant" {
				tail = tail[1:]
			}
			if hasContent(tail) {
				return turnPlan{send: tail, mode: "tail", sent: all, systems: systemHashes(tail, p.Systems)}
			}
			return turnPlan{send: messages, reset: true, reason: "历史前缀匹配但没有新的用户内容", mode: "new", sent: all, systems: systemHashes(messages, nil)}
		}
	}
	hasAssistant := false
	for _, m := range messages {
		if m.Role == "assistant" {
			hasAssistant = true
		}
	}
	if !hasAssistant {
		seen := map[string]bool{}
		for _, h := range p.Systems {
			seen[h] = true
		}
		var send []chatMessage
		for _, m := range messages {
			if isSystemRole(m.Role) && seen[m.Hash] {
				continue
			}
			send = append(send, m)
		}
		if hasContent(send) {
			return turnPlan{send: send, mode: "append", sent: all, systems: systemHashes(send, p.Systems)}
		}
	}
	return turnPlan{send: messages, reset: true, reason: "客户端历史与已发送内容不一致", mode: "new", sent: all, systems: systemHashes(messages, nil)}
}

// Archiving uses a separate bounded context so a disconnected client cannot
// cancel cleanup after a completed answer. Only this gateway's Projects enter
// this function, and archive failure keeps their mapping for retry.
func (s *Server) archiveProject(p *projectSession, cookie string) bool {
	if !s.archiveConversation(p.ConversationID, cookie) {
		s.projectWarning("Project 归档失败，保留映射并等待重试")
		return false
	}
	s.projectMu.Lock()
	p.Archived = true
	err := s.saveProjectsLocked()
	s.projectMu.Unlock()
	if err != nil {
		s.projectWarning("Project 已归档，归档状态保存失败")
	}
	return true
}

func (s *Server) archiveConversation(cid, cookie string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return s.harkJSON(ctx, http.MethodPost, "/api/conversations/"+url.PathEscape(cid)+"/archive", cookie, struct{}{}, nil) == nil
}

func (s *Server) cookieFor(accountID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == accountID {
			return a.Cookie
		}
	}
	return ""
}

func (s *Server) cleanupProjects(now time.Time, idle time.Duration) {
	s.projectMu.Lock()
	var pending []*projectSession
	for _, p := range s.projects {
		if !p.Busy && (p.Ending || p.Archived || now.Sub(p.LastUsed) >= idle) {
			p.Busy = true
			pending = append(pending, p)
		}
	}
	s.projectMu.Unlock()
	for _, p := range pending {
		cookie := s.cookieFor(p.AccountID)
		if !p.Archived && cookie != "" {
			s.archiveProject(p, cookie)
		}
		s.projectMu.Lock()
		p.Busy = false
		if p.Archived && s.projects[p.MappingID] == p {
			delete(s.projects, p.MappingID)
		}
		if err := s.saveProjectsLocked(); err != nil {
			s.projectWarning("归档清理状态保存失败")
		}
		s.projectMu.Unlock()
	}
}

func (s *Server) RunProjectCleanup(ctx context.Context, idle time.Duration) {
	s.cleanupProjects(time.Now(), idle)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.cleanupProjects(now, idle)
		}
	}
}

// endSession handles POST /v1/sessions/{id}/end and the admin playground's
// "新开对话": the Project is archived and the next request with the same
// session_id starts a new Project.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") && !s.authorized(r) {
		fail(w, 401, "invalid_api_key", "API Key 无效")
		return
	}
	v := chatRequest{SessionID: r.PathValue("id")}
	if v.SessionID == "" || validateSession(v) != nil {
		fail(w, 400, "invalid_session", "会话 ID 无效")
		return
	}
	id := projectMapping(r, v.SessionID)
	s.projectMu.Lock()
	p := s.projects[id]
	if p == nil || p.Archived {
		s.projectMu.Unlock()
		write(w, 200, map[string]any{"ok": true, "session_id": v.SessionID, "existed": false, "archived": false})
		return
	}
	if p.Busy {
		s.projectMu.Unlock()
		fail(w, 409, "session_busy", "同一会话正在处理请求，请等待完成后再结束")
		return
	}
	p.Ending, p.Busy = true, true
	s.projectMu.Unlock()
	archived := false
	if s.persistProjects() == nil {
		if cookie := s.cookieFor(p.AccountID); cookie != "" {
			archived = s.archiveProject(p, cookie)
		}
	}
	// The mapping is dropped even when archiving failed: an explicit end must
	// always lead to a fresh Project. A failed archive only leaves the old
	// Project visible in the Hark account.
	s.projectMu.Lock()
	p.Busy = false
	if s.projects[id] == p {
		delete(s.projects, id)
	}
	s.saveProjectsLocked()
	s.projectMu.Unlock()
	if !archived {
		s.projectWarning("会话已结束，但旧 Project 归档失败（仍保留在 Hark 账号中）")
	}
	s.projectInfo("会话已结束；下次请求将新建 Project")
	write(w, 200, map[string]any{"ok": true, "session_id": v.SessionID, "existed": true, "archived": archived})
}
