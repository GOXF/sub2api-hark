package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Usage mirrors Hark's billing meters (GET /api/billing/summary,
// meters.harkTokens), ported from jyao0708/hark2api parse_billing_summary.
type Usage struct {
	Plan          string `json:"plan,omitempty"`
	DailyUsed     int64  `json:"daily_used"`
	DailyLimit    int64  `json:"daily_limit"`
	DailyResetsAt string `json:"daily_resets_at,omitempty"`
	PoolUsed      int64  `json:"pool_used"`
	PoolLimit     int64  `json:"pool_limit"`
	PoolResetsAt  string `json:"pool_resets_at,omitempty"`
	Checked       string `json:"checked,omitempty"`
	Error         string `json:"error,omitempty"`
}

// exhausted reports whether a meter is used up and its reset is still ahead.
func (u *Usage) exhausted(now time.Time) bool {
	if u == nil || u.Error != "" {
		return false
	}
	over := func(used, limit int64, reset string) bool {
		if limit <= 0 || used < limit {
			return false
		}
		if t, err := time.Parse(time.RFC3339, reset); err == nil && !now.Before(t) {
			return false
		}
		return true
	}
	return over(u.DailyUsed, u.DailyLimit, u.DailyResetsAt) || over(u.PoolUsed, u.PoolLimit, u.PoolResetsAt)
}

func (s *Server) queryUsage(ctx context.Context, cookie string) (*Usage, error) {
	var summary struct {
		Plan struct {
			Name string `json:"name"`
		} `json:"plan"`
		Meters struct {
			HarkTokens *struct {
				DailyLimit    *float64 `json:"dailyLimit"`
				DailyUsed     *float64 `json:"dailyUsed"`
				DailyResetsAt string   `json:"dailyResetsAt"`
				PoolLimit     *float64 `json:"poolLimit"`
				PoolUsed      *float64 `json:"poolUsed"`
				PoolResetsAt  string   `json:"poolResetsAt"`
			} `json:"harkTokens"`
		} `json:"meters"`
	}
	if err := s.harkJSON(ctx, http.MethodGet, "/api/billing/summary", cookie, nil, &summary); err != nil {
		return nil, err
	}
	m := summary.Meters.HarkTokens
	if m == nil || m.DailyLimit == nil || m.DailyUsed == nil || m.PoolLimit == nil || m.PoolUsed == nil {
		return nil, &upstreamError{502, "upstream_response_invalid", "额度响应缺少 harkTokens 数据"}
	}
	u := &Usage{Plan: summary.Plan.Name, DailyUsed: int64(*m.DailyUsed), DailyLimit: int64(*m.DailyLimit), DailyResetsAt: normalizeTime(m.DailyResetsAt),
		PoolUsed: int64(*m.PoolUsed), PoolLimit: int64(*m.PoolLimit), PoolResetsAt: normalizeTime(m.PoolResetsAt), Checked: time.Now().UTC().Format(time.RFC3339)}
	return u, nil
}

func normalizeTime(v string) string {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return v
}

// refreshUsage queries and stores an account's quota. Auth failures mark the
// account so the pool stops choosing it.
func (s *Server) refreshUsage(ctx context.Context, id string) (Account, error) {
	s.mu.Lock()
	cookie := ""
	for _, a := range s.accounts {
		if a.ID == id {
			cookie = a.Cookie
		}
	}
	s.mu.Unlock()
	if cookie == "" {
		return Account{}, errors.New("not found")
	}
	usage, err := s.queryUsage(ctx, cookie)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID != id {
			continue
		}
		if err != nil {
			if isAuthError(err) {
				s.markAccountLocked(i, err)
			}
			msg := err.Error()
			s.accounts[i].Usage = &Usage{Error: msg, Checked: time.Now().UTC().Format(time.RFC3339)}
		} else {
			s.accounts[i].Usage = usage
		}
		if saveErr := s.saveAccounts(); saveErr != nil {
			s.event("warning", "额度查询", "额度结果保存失败")
		}
		return s.accounts[i], err
	}
	return Account{}, errors.New("not found")
}

func (s *Server) maybeRefreshUsage(id string) {
	if !s.autoUsage {
		return
	}
	s.mu.Lock()
	stale := true
	for _, a := range s.accounts {
		if a.ID == id && a.Usage != nil {
			if t, err := time.Parse(time.RFC3339, a.Usage.Checked); err == nil && time.Since(t) < 2*time.Minute {
				stale = false
			}
		}
	}
	s.mu.Unlock()
	if stale {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.refreshUsage(ctx, id)
		}()
	}
}

func (s *Server) refreshUsageHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	a, err := s.refreshUsage(ctx, r.PathValue("id"))
	if a.ID == "" {
		fail(w, 404, "not_found", "账号不存在")
		return
	}
	_ = err
	write(w, 200, a)
}

// Caller holds mu.
func (s *Server) markAccountLocked(i int, err error) {
	e, _ := err.(*upstreamError)
	status, detail := "expired", "Hark 返回 401，登录会话已失效"
	if e != nil && e.code == "upstream_access_blocked" {
		status, detail = "blocked", e.message
	}
	s.accounts[i].Status = status
	s.accounts[i].Detail = detail
	s.accounts[i].Checked = time.Now().UTC().Format(time.RFC3339)
	s.event("warning", "账号失效", s.accounts[i].Name+"："+detail)
}

func (s *Server) markAccountFailed(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			s.markAccountLocked(i, err)
			if saveErr := s.saveAccounts(); saveErr != nil {
				s.event("warning", "账号失效", "状态保存失败")
			}
			return
		}
	}
}

// candidates returns usable accounts, least recently used first. Exhausted
// accounts are skipped; if every valid account is exhausted the error says so.
func (s *Server) candidates(exclude map[string]bool) ([]Account, error) {
	return s.candidatesFor(exclude, false)
}

// candidatesFor lists usable accounts; needBridge keeps only accounts whose
// file bridge is on.
func (s *Server) candidatesFor(exclude map[string]bool, needBridge bool) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var valid, usable []Account
	bridgeless := 0
	for _, a := range s.accounts {
		if a.Status != "valid" || exclude[a.ID] {
			continue
		}
		if needBridge && !a.Bridge {
			bridgeless++
			continue
		}
		valid = append(valid, a)
		if !a.Usage.exhausted(now) {
			usable = append(usable, a)
		}
	}
	if len(valid) == 0 && needBridge && bridgeless > 0 {
		return nil, &upstreamError{409, "no_bridge_account", "没有开着文件桥的可用 Hark 账号；请在 Hark Hub 里给账号连接并打开文件桥，或去掉 hark_bridge"}
	}
	if len(valid) == 0 {
		return nil, &upstreamError{409, "no_valid_account", s.noAccountReasonLocked(exclude)}
	}
	if len(usable) == 0 {
		return nil, &upstreamError{429, "quota_exhausted", "所有可用 Hark 账号额度已用尽，请等待额度重置"}
	}
	sort.SliceStable(usable, func(i, j int) bool {
		return usable[i].LastUsed < usable[j].LastUsed
	})
	return usable, nil
}

func (s *Server) accountByID(id string) (Account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

func (s *Server) touchAccount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			s.accounts[i].LastUsed = time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
}

func (s *Server) slot(id string) chan struct{} {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	ch := s.slots[id]
	if ch == nil {
		ch = make(chan struct{}, s.concurrency)
		s.slots[id] = ch
	}
	return ch
}

func (s *Server) inFlight(id string) int {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	return len(s.slots[id])
}

// acquireSlot blocks until the account has a free request slot.
func (s *Server) acquireSlot(ctx context.Context, id string) (func(), error) {
	ch := s.slot(id)
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) tryAcquireSlot(id string) (func(), bool) {
	ch := s.slot(id)
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}

// pickAccount chooses the least recently used account with a free slot; if
// all are busy it waits for the least recently used one.
func (s *Server) pickAccount(ctx context.Context, exclude map[string]bool, needBridge bool) (Account, func(), error) {
	if a, pinned, err := s.pinnedAccount(ctx); pinned {
		// Embedded: only the host's account, and no failover once it failed.
		if err != nil {
			return Account{}, nil, err
		}
		if exclude[a.ID] {
			return Account{}, nil, &upstreamError{409, "no_valid_account", "宿主指定的 Hark 账号本次请求已失败"}
		}
		release, err := s.acquireSlot(ctx, a.ID)
		if err != nil {
			return Account{}, nil, err
		}
		s.touchAccount(a.ID)
		return a, release, nil
	}
	list, err := s.candidatesFor(exclude, needBridge)
	if err != nil {
		return Account{}, nil, err
	}
	for _, a := range list {
		if release, ok := s.tryAcquireSlot(a.ID); ok {
			s.touchAccount(a.ID)
			return a, release, nil
		}
	}
	release, err := s.acquireSlot(ctx, list[0].ID)
	if err != nil {
		return Account{}, nil, err
	}
	s.touchAccount(list[0].ID)
	return list[0], release, nil
}

// noAccountReasonLocked explains why no account can be used. Caller holds mu.
func (s *Server) noAccountReasonLocked(exclude map[string]bool) string {
	if len(s.accounts) == 0 {
		if s.source != nil {
			return "没有可用账号：Hub 还没读到任何已登录账号的 Cookie（请先在 Hub 里打开一个已登录的账号，等几秒后重试）"
		}
		return "没有可用账号：请先导入 Cookie 并检测账号有效"
	}
	var expired, blocked, disabled, skipped, other int
	blockedDetail := ""
	for _, a := range s.accounts {
		switch {
		case exclude[a.ID]:
			skipped++
		case a.Status == "expired":
			expired++
		case a.Status == "blocked":
			blocked++
			if blockedDetail == "" && strings.Contains(a.Detail, "（") {
				blockedDetail = a.Detail
			}
		case a.Status == "disabled":
			disabled++
		default:
			other++
		}
	}
	msg := fmt.Sprintf("没有可用账号（共 %d 个）：", len(s.accounts))
	if blocked > 0 {
		msg += fmt.Sprintf("%d 个 Hark 返回 403 访问受阻（多半是 Cloudflare 拦截，换线路/代理或稍后再试）；", blocked)
		if blockedDetail != "" {
			msg += "其中一个的详情：" + blockedDetail + "；"
		}
	}
	if expired > 0 {
		msg += fmt.Sprintf("%d 个登录已失效（返回 401，请重新登录）；", expired)
	}
	if skipped > 0 {
		msg += fmt.Sprintf("%d 个本次请求已失败被跳过；", skipped)
	}
	if disabled > 0 {
		msg += fmt.Sprintf("%d 个已停用；", disabled)
	}
	if other > 0 {
		msg += fmt.Sprintf("%d 个尚未检测有效；", other)
	}
	return msg + "详情见仪表盘账号页"
}
