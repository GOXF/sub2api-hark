package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Since 2026-11 Hark answers POST /api/conversations (creating a Project
// directly, also what the web app's /projects/new does) with 403. Projects
// that Hark's own agent creates when asked in a chat still work. So when the
// direct call is refused, the gateway asks Hark to create the Project:
//
//   - the first time, in the account's main chat, it creates one dedicated
//     "creator" Project (agentCreatorTitle);
//   - every later Project is requested inside that creator Project, which keeps
//     these requests out of the user's main chat;
//   - two spare Projects per account are created in the background so a new
//     session usually does not wait for creation.
//
// So each account keeps three Projects ready: the creator plus two spares.
// Spares are handed to sessions and archived after use like any session
// Project; the creator is never archived.
//
// The new Project is found by diffing GET /api/conversations before and after.

const (
	agentCreatorTitle    = "Hark2API 建项目专用"
	agentProjectTitle    = "Hark2API 会话"
	agentCreateTimeout   = 2 * time.Minute
	directBlockedRetry   = 30 * time.Minute
	agentSparePerAccount = 2
	agentPrewarmParallel = 2
)

type agentCreator struct {
	mu            sync.Mutex
	creator       string
	spare         []string
	ready         atomic.Int32 // len(spare), readable without mu
	saved         atomic.Value // agentSnapshot, readable without mu
	directBlocked time.Time
	directOK      bool        // the last direct create worked: spares are not needed
	directWorks   atomic.Bool // directOK, readable without mu
	refilling     atomic.Bool
	verified      atomic.Bool // restored spares checked against Hark this run
}

// agentSnapshot is what survives a restart: the creator Project and the spare
// Projects not yet handed to a session.
type agentSnapshot struct {
	Creator string   `json:"creator,omitempty"`
	Spare   []string `json:"spare,omitempty"`
}

type convItem struct {
	ID    string
	Title string
}

const agentStoreFile = "agent-projects.json"

// accountKey identifies an account across cookie refreshes: its id when the
// cookie belongs to a known account, else the cookie's fingerprint.
func (s *Server) accountKey(cookie string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.Cookie == cookie && a.ID != "" {
			return a.ID
		}
	}
	return "fp:" + cookieFingerprint(cookie)
}

func (s *Server) agentFor(cookie string) *agentCreator { return s.agentByKey(s.accountKey(cookie)) }

func (s *Server) agentByKey(key string) *agentCreator {
	v, _ := s.agentCreate.LoadOrStore(key, &agentCreator{})
	return v.(*agentCreator)
}

// changedLocked publishes ac's state for readers without mu and persists it.
// Caller holds ac.mu.
func (s *Server) changedLocked(ac *agentCreator) {
	ac.ready.Store(int32(len(ac.spare)))
	ac.saved.Store(agentSnapshot{Creator: ac.creator, Spare: append([]string(nil), ac.spare...)})
	s.saveAgentProjects()
}

func (s *Server) saveAgentProjects() {
	if s.data == "" {
		return
	}
	s.agentFileMu.Lock()
	defer s.agentFileMu.Unlock()
	out := map[string]agentSnapshot{}
	s.agentCreate.Range(func(k, v any) bool {
		if snap, ok := v.(*agentCreator).saved.Load().(agentSnapshot); ok && (snap.Creator != "" || len(snap.Spare) > 0) {
			out[k.(string)] = snap
		}
		return true
	})
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(s.data, agentStoreFile+".tmp")
	if os.WriteFile(tmp, b, 0o600) != nil || os.Rename(tmp, filepath.Join(s.data, agentStoreFile)) != nil {
		s.projectWarning("预建 Project 记录保存失败")
	}
}

// loadAgentProjects restores creator and spare Projects saved before a restart.
func (s *Server) loadAgentProjects() {
	b, err := os.ReadFile(filepath.Join(s.data, agentStoreFile))
	if err != nil {
		return
	}
	var in map[string]agentSnapshot
	if json.Unmarshal(b, &in) != nil {
		return
	}
	for k, snap := range in {
		ac := s.agentByKey(k)
		ac.creator = snap.Creator
		for _, id := range snap.Spare {
			if id != "" && len(id) <= 200 {
				ac.spare = append(ac.spare, id)
			}
		}
		ac.ready.Store(int32(len(ac.spare)))
		ac.saved.Store(agentSnapshot{Creator: ac.creator, Spare: append([]string(nil), ac.spare...)})
	}
}

// createProject gives a session a Project: a spare when one is ready,
// otherwise a new one, directly when Hark allows it, else through Hark's agent.
func (s *Server) createProject(ctx context.Context, cookie, zone string) (string, error) {
	ac := s.agentFor(cookie)
	ac.mu.Lock()
	if n := len(ac.spare); n > 0 {
		cid := ac.spare[n-1]
		ac.spare = ac.spare[:n-1]
		s.changedLocked(ac)
		ac.mu.Unlock()
		s.projectInfo("使用预建的 Project")
		go s.refillSpare(cookie, zone)
		return cid, nil
	}
	cid, err := s.newProjectLocked(ctx, ac, cookie, zone)
	ac.mu.Unlock()
	if err != nil {
		return "", err
	}
	go s.refillSpare(cookie, zone)
	return cid, nil
}

// newProjectLocked creates one Project. Caller holds ac.mu.
func (s *Server) newProjectLocked(ctx context.Context, ac *agentCreator, cookie, zone string) (string, error) {
	if time.Now().After(ac.directBlocked) {
		cid, err := s.createProjectDirect(ctx, cookie, zone)
		if e, ok := err.(*upstreamError); !ok || e.code != "upstream_access_blocked" {
			if err == nil {
				ac.directOK = true
				ac.directWorks.Store(true)
			}
			return cid, err
		}
		ac.directOK = false
		ac.directWorks.Store(false)
		ac.directBlocked = time.Now().Add(directBlockedRetry)
		s.projectInfo("Hark 拒绝直接新建 Project（403），改为让 Hark 代建")
	}
	return s.agentCreateLocked(ctx, ac, cookie, zone)
}

// refillSpare tops the account up to agentSparePerAccount ready Projects. It
// releases the lock between creations so a waiting session can take a spare.
func (s *Server) refillSpare(cookie, zone string) {
	ac := s.agentFor(cookie)
	if !ac.refilling.CompareAndSwap(false, true) {
		return
	}
	defer ac.refilling.Store(false)
	for {
		ac.mu.Lock()
		if len(ac.spare) >= agentSparePerAccount || (ac.directOK && time.Now().After(ac.directBlocked)) {
			ac.mu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), agentCreateTimeout+30*time.Second)
		cid, err := s.newProjectLocked(ctx, ac, cookie, zone)
		cancel()
		if err == nil {
			ac.spare = append(ac.spare, cid)
			s.changedLocked(ac)
		}
		ac.mu.Unlock()
		if err != nil {
			s.projectWarning("预建 Project 失败：" + err.Error())
			return
		}
	}
}

// prewarmSpares runs on startup and every minute: it drops saved spares that
// no longer exist, then fills spares for up to agentPrewarmParallel accounts
// at a time, so every usable account ends up ready and the first session on
// any account does not wait for a Project.
func (s *Server) prewarmSpares() {
	s.mu.Lock()
	now := time.Now()
	var todo []Account
	for _, a := range s.accounts {
		if a.Status == "valid" && !a.Usage.exhausted(now) && a.Cookie != "" {
			todo = append(todo, a)
		}
	}
	zone := s.settings.Timezone
	s.mu.Unlock()
	started := 0
	for _, a := range todo {
		ac := s.agentByKey(a.ID)
		if ac.verified.Load() && (ac.ready.Load() >= agentSparePerAccount || ac.directWorks.Load()) {
			continue
		}
		if ac.refilling.Load() {
			started++
		} else if started < agentPrewarmParallel {
			started++
			go func(cookie string, ac *agentCreator) {
				if !ac.verified.Load() {
					s.verifySpares(cookie, ac)
				}
				s.refillSpare(cookie, zone)
			}(a.Cookie, ac)
		}
		if started >= agentPrewarmParallel {
			return
		}
	}
}

// verifySpares drops restored spares that are gone from the account's list
// (archived or deleted elsewhere), once per run.
func (s *Server) verifySpares(cookie string, ac *agentCreator) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := s.listConversations(ctx, cookie)
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, c := range list {
		have[c.ID] = true
	}
	ac.mu.Lock()
	kept := ac.spare[:0]
	for _, id := range ac.spare {
		if have[id] {
			kept = append(kept, id)
		}
	}
	dropped := len(ac.spare) - len(kept)
	ac.spare = kept
	if ac.creator != "" && !have[ac.creator] {
		ac.creator = ""
	}
	s.changedLocked(ac)
	ac.mu.Unlock()
	ac.verified.Store(true)
	if dropped > 0 {
		s.projectInfo(fmt.Sprintf("丢弃 %d 个已不存在的预建 Project", dropped))
	}
}

// agentCreateLocked creates one session Project through the creator Project,
// creating the creator from the main chat first if needed. Caller holds ac.mu.
func (s *Server) agentCreateLocked(ctx context.Context, ac *agentCreator, cookie, zone string) (string, error) {
	if ac.creator == "" {
		if list, err := s.listConversations(ctx, cookie); err == nil {
			for _, c := range list {
				if c.Title == agentCreatorTitle {
					ac.creator = c.ID
					break
				}
			}
		}
	}
	if ac.creator == "" {
		main, err := s.mainConversation(ctx, cookie, zone)
		if err != nil {
			return "", err
		}
		creator, err := s.askHarkToCreate(ctx, cookie, zone, main, agentCreatorTitle, "主聊天")
		if err != nil {
			return "", err
		}
		ac.creator = creator
	}
	// A unique title per request: with a fixed title Hark sees the earlier
	// identical request in the creator Project's history, decides the Project
	// already exists and only answers "已建好" without creating a new one.
	title := agentProjectTitle + " " + strings.ReplaceAll(uuid(), "-", "")[:6]
	cid, err := s.askHarkToCreate(ctx, cookie, zone, ac.creator, title, "建项目专用 Project")
	if e, ok := err.(*upstreamError); ok && (e.code == "upstream_request_rejected" || e.code == "upstream_http_error") {
		ac.creator = "" // the creator Project may have been archived or deleted; rebuild next time
	}
	return cid, err
}

// askHarkToCreate sends a create request into conversation via and waits for
// a new conversation to appear in the account's list.
func (s *Server) askHarkToCreate(ctx context.Context, cookie, zone, via, title, label string) (string, error) {
	before, err := s.listConversations(ctx, cookie)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{via: true}
	for _, c := range before {
		seen[c.ID] = true
	}
	start := time.Now()
	payload := map[string]any{
		"timezone":          zone,
		"conversationId":    via,
		"message":           fmt.Sprintf("请新建一个新的空项目，标题叫「%s」。这是一个新请求，即使之前建过类似的项目也要再新建一个。不用做任何事，建好只回复「已建好」。", title),
		"idempotencyKey":    uuid(),
		"responseMessageId": uuid(),
	}
	if err := s.harkJSON(ctx, "POST", "/api/messages/send?cid="+url.QueryEscape(via), cookie, payload, nil); err != nil {
		return "", err
	}
	interval := s.agentPoll
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(agentCreateTimeout)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		list, err := s.listConversations(ctx, cookie)
		if err == nil {
			var fresh []convItem
			for _, c := range list {
				if !seen[c.ID] {
					fresh = append(fresh, c)
				}
			}
			pick := ""
			for _, c := range fresh {
				if c.Title == title {
					pick = c.ID
					break
				}
			}
			if pick == "" && len(fresh) == 1 {
				pick = fresh[0].ID
			}
			if pick != "" {
				s.projectInfo(fmt.Sprintf("经%s让 Hark 新建 Project「%s」耗时 %.1f 秒", label, title, time.Since(start).Seconds()))
				return pick, nil
			}
		}
		if time.Now().After(deadline) {
			s.projectWarning(fmt.Sprintf("经%s让 Hark 新建 Project 超时（%.0f 秒未出现）", label, time.Since(start).Seconds()))
			return "", &upstreamError{504, "upstream_project_create_timeout", "Hark 代建 Project 超时，请稍后重试"}
		}
	}
}

// mainConversation returns the id of the account's main chat.
func (s *Server) mainConversation(ctx context.Context, cookie, zone string) (string, error) {
	var out struct {
		ConversationID string `json:"conversationId"`
		ID             string `json:"id"`
	}
	if err := s.harkJSON(ctx, "GET", "/api/conversations/main?timezone="+url.QueryEscape(zone), cookie, nil, &out); err != nil {
		return "", err
	}
	if out.ConversationID != "" {
		return out.ConversationID, nil
	}
	if out.ID != "" {
		return out.ID, nil
	}
	return "", &upstreamError{502, "upstream_protocol_unverified", "Hark 主聊天响应缺少 conversationId"}
}

// listConversations reads GET /api/conversations, accepting a bare array or an
// object holding the array under a common key.
func (s *Server) listConversations(ctx context.Context, cookie string) ([]convItem, error) {
	var raw json.RawMessage
	if err := s.harkJSON(ctx, "GET", "/api/conversations", cookie, nil, &raw); err != nil {
		return nil, err
	}
	return parseConversationList(raw)
}

func parseConversationList(raw json.RawMessage) ([]convItem, error) {
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, &upstreamError{502, "upstream_response_invalid", "Hark 会话列表格式无法识别"}
		}
		found := false
		for _, k := range []string{"conversations", "projects", "items", "data", "results"} {
			if v, ok := obj[k]; ok && json.Unmarshal(v, &arr) == nil {
				found = true
				break
			}
		}
		if !found {
			return nil, &upstreamError{502, "upstream_response_invalid", "Hark 会话列表格式无法识别"}
		}
	}
	out := make([]convItem, 0, len(arr))
	for _, m := range arr {
		id := str(m["conversationId"])
		if id == "" {
			id = str(m["id"])
		}
		if id == "" {
			continue
		}
		title := str(m["title"])
		if title == "" {
			title = str(m["name"])
		}
		out = append(out, convItem{ID: id, Title: strings.TrimSpace(title)})
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// hasSpare reports whether the account has a ready Project. Lock-free so the
// account picker can call it while holding s.mu.
func (s *Server) hasSpare(accountID string) bool {
	v, ok := s.agentCreate.Load(accountID)
	return ok && v.(*agentCreator).ready.Load() > 0
}
