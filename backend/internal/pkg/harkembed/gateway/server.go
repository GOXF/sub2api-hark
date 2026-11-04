package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

type Settings struct {
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
	Timeout  int    `json:"timeout"`
}
type Key struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Prefix  string `json:"prefix"`
	Created string `json:"created"`
	Hash    string `json:"hash,omitempty"`
}
type Account struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Checked      string `json:"checked,omitempty"`
	Detail       string `json:"detail"`
	Cookie       string `json:"-"`
	ChatVerified bool   `json:"chat_verified"`
	ChatChecked  string `json:"chat_checked,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	Proxy        string `json:"-"`
	UserAgent    string `json:"-"`
	ProxyHost    string `json:"proxy,omitempty"`
	LastUsed     string `json:"last_used,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	InFlight     int    `json:"in_flight"`
	Bridge       bool   `json:"bridge"`
	ProxyID      string `json:"proxy_id,omitempty"`
	// Timezone 是该账号专用的 IANA 时区（如按代理出口自动适配）；空则用全局设置。
	Timezone string `json:"timezone,omitempty"`
}
type Event struct {
	Time   string `json:"time"`
	Level  string `json:"level"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}
type persisted struct {
	Settings       Settings          `json:"settings"`
	Keys           []Key             `json:"keys"`
	Proxies        []ProxyEntry      `json:"proxies,omitempty"`
	AccountProxies map[string]string `json:"account_proxies,omitempty"`
}
type Server struct {
	publicOrigin  string
	adminHash     []byte
	adminSessions map[string]adminSession
	authAttempts  map[string]authThrottle
	mu            sync.Mutex
	data          string
	settings      Settings
	keys          []Key
	accounts      []Account
	events        []Event
	started       time.Time
	client        *http.Client
	upstreamProxy string
	loginMu       sync.Mutex
	logins        map[string]*loginAttempt
	projectMu     sync.Mutex
	projects      map[string]*projectSession

	upstream          string
	publicBaseURL     string
	remote            bool
	maxUpload         int64
	concurrency       int
	forwardExtra      bool
	allowPrivateFetch bool
	autoUsage         bool
	pollInterval      time.Duration
	fetchClient       *http.Client
	slotMu            sync.Mutex
	slots             map[string]chan struct{}
	source            AccountSource
	embed             *embedRegistry
	porter            AccountPorter
	proxies           []ProxyEntry
	acctProxy         map[string]string
	proxyCache        proxyTransports

	reqMu       sync.Mutex
	reqLogs     []RequestLog
	reqInFlight int
}

// Options are runtime settings from flags/environment (see main.go).
type Options struct {
	Upstream          string // Hark base URL, default https://hark.com
	PublicBase        string // external base URL used in /v1/media links
	Remote            bool   // listening on a non-loopback address
	MaxUploadBytes    int64  // per attachment, default 20 MiB
	Concurrency       int    // in-flight requests per account, default 1
	ForwardExtra      bool   // forward unknown OpenAI fields to Hark
	AllowPrivateFetch bool   // allow image/file URLs on private networks
	PollInterval      time.Duration
	// AccountSource, when set, supplies the Hark accounts (cookies and
	// optional per-account proxies) instead of the encrypted account store.
	AccountSource AccountSource
	// AccountPorter, when set, exports/imports accounts for the embedding
	// application (used together with AccountSource).
	AccountPorter AccountPorter
	// AdminKey sets the dashboard administrator key (plaintext, hashed here),
	// as an alternative to the HARK2API_ADMIN_KEY environment variable.
	AdminKey string
}

func (s *Server) Configure(o Options) error {
	if o.Upstream != "" {
		u, err := url.Parse(o.Upstream)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" && u.Path != "/" {
			return errors.New("upstream must be an http(s) origin")
		}
		s.upstream = strings.TrimRight(o.Upstream, "/")
	}
	if o.PublicBase != "" {
		u, err := url.Parse(o.PublicBase)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("public base must be an http(s) URL")
		}
		s.publicBaseURL = strings.TrimRight(o.PublicBase, "/")
	}
	if o.AdminKey != "" {
		if len(o.AdminKey) < 12 {
			return errors.New("admin key must be at least 12 characters")
		}
		h := sha256.Sum256([]byte(o.AdminKey))
		s.adminHash = h[:]
	}
	s.remote = o.Remote
	if s.remote && len(s.adminHash) == 0 {
		return errors.New("listening on a non-loopback address requires an administrator key (HARK2API_ADMIN_KEY or HARK_ADMIN_KEY_SHA256)")
	}
	if o.MaxUploadBytes > 0 {
		s.maxUpload = o.MaxUploadBytes
	}
	if o.Concurrency > 0 {
		if o.Concurrency > 64 {
			return errors.New("concurrency must be 1-64")
		}
		s.slotMu.Lock()
		s.concurrency = o.Concurrency
		s.slots = map[string]chan struct{}{}
		s.slotMu.Unlock()
	}
	if o.PollInterval > 0 {
		s.pollInterval = o.PollInterval
	}
	s.forwardExtra = o.ForwardExtra
	s.allowPrivateFetch = o.AllowPrivateFetch
	s.fetchClient = s.newFetchClient()
	s.porter = o.AccountPorter
	if o.AccountSource != nil {
		s.source = o.AccountSource
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.syncAccounts(ctx); err != nil {
			return err
		}
	}
	return nil
}

// SetUpstreamProxy applies one explicit HTTP proxy to all outbound Hark calls.
// Empty keeps the direct route, which preserves the library default for tests.
func (s *Server) SetUpstreamProxy(raw string) error {
	raw = strings.TrimSpace(raw)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if raw == "" {
		transport.Proxy = nil
		s.client.Transport = transport
		s.upstreamProxy = ""
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("upstream proxy must be an HTTP URL without credentials or path")
	}
	transport.Proxy = http.ProxyURL(u)
	s.client.Transport = transport
	s.upstreamProxy = raw
	return nil
}

func New(data string) (*Server, error) {
	s := &Server{data: data, settings: Settings{Name: "Hark Gateway", Timezone: "Asia/Shanghai", Timeout: 180},
		keys: []Key{}, accounts: []Account{}, events: []Event{}, started: time.Now(),
		client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}
	s.logins = make(map[string]*loginAttempt)
	s.upstream, s.maxUpload, s.concurrency, s.autoUsage, s.pollInterval = "https://hark.com", 20*1024*1024, 1, true, time.Second
	s.slots = map[string]chan struct{}{}
	s.fetchClient = s.newFetchClient()
	if err := s.configureAccess(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(data, 0700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(data, "config.json"))
	if err == nil {
		var p persisted
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err = validSettings(p.Settings); err != nil {
			return nil, err
		}
		s.settings = p.Settings
		if p.Keys != nil {
			s.keys = p.Keys
		}
		s.proxies, s.acctProxy = p.Proxies, p.AccountProxies
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := s.loadAccounts(); err != nil {
		return nil, err
	}
	if err := s.loadProjects(); err != nil {
		return nil, err
	}
	s.event("info", "服务启动", "本地管理界面已就绪")
	return s, nil
}

func randomID(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func (s *Server) event(level, action, detail string) {
	s.events = append(s.events, Event{Time: time.Now().UTC().Format(time.RFC3339), Level: level, Action: action, Detail: detail})
	if len(s.events) > 200 {
		s.events = s.events[len(s.events)-200:]
	}
}

// Caller holds mu. Replace the file only after a complete successful write.
func (s *Server) save() error {
	b, err := json.MarshalIndent(persisted{Settings: s.settings, Keys: s.keys, Proxies: s.proxies, AccountProxies: s.acctProxy}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.data, ".config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(s.data, "config.json"))
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	write(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 32768)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid_input", "输入格式不正确或内容过长")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, 400, "invalid_input", "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /admin/auth/login", s.adminLogin)
	m.HandleFunc("POST /admin/auth/logout", s.adminLogout)
	m.HandleFunc("GET /admin/state", s.state)
	m.HandleFunc("POST /admin/keys", s.createKey)
	m.HandleFunc("DELETE /admin/keys/{id}", s.deleteKey)
	m.HandleFunc("POST /admin/accounts", s.addAccount)
	m.HandleFunc("POST /admin/proxies", s.addProxies)
	m.HandleFunc("PUT /admin/proxies/{id}", s.updateProxy)
	m.HandleFunc("DELETE /admin/proxies/{id}", s.deleteProxy)
	m.HandleFunc("POST /admin/proxies/{id}/test", s.testProxy)
	m.HandleFunc("PUT /admin/account-proxies", s.assignProxy)
	m.HandleFunc("GET /admin/accounts/export", s.exportAccounts)
	m.HandleFunc("POST /admin/accounts/import", s.importAccounts)
	m.HandleFunc("DELETE /admin/accounts/{id}", s.deleteAccount)
	m.HandleFunc("POST /admin/accounts/{id}/check", s.checkAccount)
	m.HandleFunc("POST /admin/accounts/{id}/chat-probe", s.chatProbe)
	m.HandleFunc("POST /admin/login/start", s.loginStart)
	m.HandleFunc("POST /admin/login/open", s.loginOpen)
	m.HandleFunc("POST /admin/login/complete", s.loginComplete)
	m.HandleFunc("POST /admin/login/cancel", s.loginCancel)
	m.HandleFunc("GET /admin/login/status", s.loginStatus)
	m.HandleFunc("POST /bridge/login", s.loginBridge)
	m.HandleFunc("PUT /admin/settings", s.updateSettings)
	m.HandleFunc("POST /admin/validate", s.validate)
	m.HandleFunc("POST /admin/playground/chat", s.trackRequest(s.playgroundChat))
	m.HandleFunc("GET /admin/requests", s.listRequests)
	m.HandleFunc("DELETE /admin/requests", s.clearRequests)
	m.HandleFunc("POST /admin/accounts/{id}/usage", s.refreshUsageHandler)
	m.HandleFunc("POST /admin/sessions/{id}/end", s.endSession)
	m.HandleFunc("GET /v1/models", s.models)
	m.HandleFunc("POST /v1/chat/completions", s.trackRequest(s.chat))
	m.HandleFunc("POST /v1/responses", s.trackRequest(s.responses))
	m.HandleFunc("POST /v1/messages", s.trackRequest(s.messages))
	m.HandleFunc("POST /v1/messages/count_tokens", s.countTokens)
	m.HandleFunc("POST /v1/sessions/{id}/end", s.endSession)
	m.HandleFunc("GET /v1/media/{name}", s.serveMedia)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"ok": true, "service": "hark2api"})
	})
	files, _ := fs.Sub(assets, "web")
	m.Handle("GET /", http.FileServer(http.FS(files)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		api := strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/healthz"
		// /v1 is protected by client API keys; the Host allow-list guards the
		// cookie-authenticated dashboard against DNS rebinding.
		if !api && !s.allowedHost(r.Host) {
			fail(w, 403, "local_only", "管理界面仅供本机访问")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
				fail(w, 403, "invalid_origin", "请求来源不正确")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				if !s.sameOrigin(r, origin) {
					fail(w, 403, "invalid_origin", "请求来源不正确")
					return
				}
			}
			if r.Method != "GET" && r.Header.Get("X-Hark-Admin") != "1" {
				fail(w, 403, "invalid_origin", "缺少本地管理请求标记")
				return
			}
			if r.URL.Path != "/admin/auth/login" && !s.isAdmin(r) {
				fail(w, 401, "admin_auth_required", "请先使用管理员密钥登录")
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]Key, len(s.keys))
	copy(keys, s.keys)
	for i := range keys {
		keys[i].Hash = ""
	}
	accounts := make([]Account, len(s.accounts))
	copy(accounts, s.accounts)
	for i := range accounts {
		accounts[i].InFlight = s.inFlight(accounts[i].ID)
		accounts[i].ProxyID = s.acctProxy[accounts[i].ID]
		eff, direct := s.effectiveProxyLocked(accounts[i])
		if u, err := url.Parse(eff); err == nil && eff != "" {
			accounts[i].ProxyHost = u.Scheme + "://" + u.Host
		} else if direct {
			accounts[i].ProxyHost = "直连"
		}
	}
	connected := false
	for _, a := range s.accounts {
		if a.Status == "valid" && a.ChatVerified {
			connected = true
		}
	}
	providerStatus := "Project 聊天待本账号实测"
	if connected {
		providerStatus = "Project 聊天已实测通过"
	}
	write(w, 200, map[string]any{"settings": s.settings, "keys": keys, "accounts": accounts, "max_upload_bytes": s.maxUpload, "account_concurrency": s.concurrency, "remote": s.remote, "accounts_external": s.source != nil, "accounts_portable": s.source == nil || s.porter != nil, "proxies": s.proxyViewsLocked(), "events": s.events, "request_stats": s.requestStats(),
		"endpoint": requestOrigin(r) + "/v1", "started": s.started.UTC().Format(time.RFC3339), "upstream_proxy": s.upstreamProxy,
		"provider": map[string]any{"name": "Hark", "model": "hark", "connected": connected, "status": providerStatus, "mode": "project", "project_protocol": "verified", "context_isolation": "instruction-only", "hard_isolation": false}})
}
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len([]rune(input.Name)) > 40 {
		fail(w, 400, "invalid_name", "名称需要 1–40 个字符")
		return
	}
	plain := randomID("sk-hark-")
	h := sha256.Sum256([]byte(plain))
	k := Key{ID: randomID("key_"), Name: input.Name, Prefix: plain[:16], Hash: hex.EncodeToString(h[:]), Created: time.Now().UTC().Format(time.RFC3339)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	if err := s.save(); err != nil {
		s.keys = s.keys[:len(s.keys)-1]
		fail(w, 500, "save_failed", "保存失败，请检查数据目录")
		return
	}
	s.event("success", "创建 API Key", "已创建一个客户端 Key")
	k.Hash = ""
	write(w, 201, map[string]any{"key": k, "secret": plain})
}
func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.keys {
		if k.ID == r.PathValue("id") {
			old := s.keys
			s.keys = append(append([]Key{}, old[:i]...), old[i+1:]...)
			if err := s.save(); err != nil {
				s.keys = old
				fail(w, 500, "save_failed", "保存失败")
				return
			}
			s.event("info", "删除 API Key", "客户端 Key 已撤销")
			write(w, 200, map[string]bool{"ok": true})
			return
		}
	}
	fail(w, 404, "not_found", "Key 不存在")
}
func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	if s.source != nil {
		fail(w, 409, "accounts_external", "账号由宿主应用管理，不能在此导入")
		return
	}
	var input struct {
		Name   string `json:"name"`
		Cookie string `json:"cookie"`
		Proxy  string `json:"proxy,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Proxy = strings.TrimSpace(input.Proxy)
	if err := validAccountProxy(input.Proxy); err != nil {
		fail(w, 400, "invalid_proxy", "账号代理格式不正确（http/https/socks5）")
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Cookie = strings.TrimSpace(input.Cookie)
	if input.Name == "" || len([]rune(input.Name)) > 40 {
		fail(w, 400, "invalid_name", "名称需要 1–40 个字符")
		return
	}
	if !strings.Contains(input.Cookie, "=") || strings.ContainsAny(input.Cookie, "\r\n") || len(input.Cookie) > 16000 {
		fail(w, 400, "invalid_cookie", "请输入单行 Cookie，格式为 name=value; name=value")
		return
	}
	a := Account{ID: randomID("acct_"), Name: input.Name, Status: "unchecked", Detail: "尚未检测会话", Cookie: input.Cookie, Proxy: input.Proxy}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = append(s.accounts, a)
	if err := s.saveAccounts(); err != nil {
		s.accounts = s.accounts[:len(s.accounts)-1]
		fail(w, 500, "save_failed", "Cookie 保存失败，请检查数据目录")
		return
	}
	s.event("info", "导入账号", "Cookie 已加密保存")
	write(w, 201, a)
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if s.source != nil {
		fail(w, 409, "accounts_external", "账号由宿主应用管理，不能在此移除")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accounts {
		if a.ID == r.PathValue("id") {
			old := s.accounts
			s.accounts = append(append([]Account{}, old[:i]...), old[i+1:]...)
			if err := s.saveAccounts(); err != nil {
				s.accounts = old
				fail(w, 500, "save_failed", "账号移除保存失败")
				return
			}
			s.event("info", "移除账号", "本地会话已移除")
			write(w, 200, map[string]bool{"ok": true})
			return
		}
	}
	fail(w, 404, "not_found", "账号不存在")
}
func (s *Server) checkAccount(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var account Account
	for _, a := range s.accounts {
		if a.ID == r.PathValue("id") {
			account = a
			break
		}
	}
	s.mu.Unlock()
	if account.ID == "" {
		fail(w, 404, "not_found", "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.upstream+"/api/auth/get-session", nil)
	req.Header.Set("Cookie", account.Cookie)
	req.Header.Set("Accept", "application/json")
	resp, err := s.clientFor(account.Cookie, 20*time.Second, false).Do(req)
	status, detail, userID := "error", "检测失败，请稍后重试", ""
	if err == nil {
		defer resp.Body.Close()
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 65537))
		var body struct {
			Session *struct {
				ID string `json:"id"`
			} `json:"session"`
			User *struct {
				ID string `json:"id"`
			} `json:"user"`
		}
		switch {
		case resp.StatusCode == 403:
			status, detail = "blocked", "Hark 返回 HTTP 403，访问受阻（"+upstreamDiag("GET", "/api/auth/get-session", resp, b, s.routeDiag(account.Cookie))+"）"
		case resp.StatusCode == 401:
			status, detail = "expired", "登录会话已失效"
		case resp.StatusCode == 429:
			status, detail = "limited", "检测受到限流，请稍后再试"
		case resp.StatusCode != 200:
			detail = fmt.Sprintf("Hark 返回 HTTP %d", resp.StatusCode)
		case readErr != nil || len(b) > 65536:
			detail = "会话响应读取失败"
		case bytes.Equal(bytes.TrimSpace(b), []byte("null")):
			status, detail = "expired", "未检测到有效登录会话"
		case json.Unmarshal(b, &body) == nil && body.Session != nil && body.Session.ID != "" && body.User != nil && body.User.ID != "":
			status, detail, userID = "valid", "登录会话有效；Project 聊天待本账号实测", body.User.ID
			if account.ChatVerified {
				detail = "登录会话有效；Project 聊天已实测通过"
			}
		default:
			detail = "会话响应结构尚未确认"
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == account.ID {
			old := s.accounts[i]
			s.accounts[i].Status = status
			s.accounts[i].Detail = detail
			s.accounts[i].Checked = time.Now().UTC().Format(time.RFC3339)
			if userID != "" {
				s.accounts[i].UserID = userID
			}
			if err := s.saveAccounts(); err != nil {
				s.accounts[i] = old
				fail(w, 500, "save_failed", "会话状态保存失败")
				return
			}
			level := "info"
			if status == "valid" {
				level = "success"
			} else {
				level = "warning"
			}
			s.event(level, "检测会话", detail)
			write(w, 200, s.accounts[i])
			return
		}
	}
	fail(w, 404, "not_found", "账号已移除")
}
func validSettings(v Settings) error {
	if strings.TrimSpace(v.Name) == "" || len([]rune(v.Name)) > 40 {
		return errors.New("名称需要 1–40 个字符")
	}
	if v.Timeout < 30 || v.Timeout > 600 {
		return errors.New("超时需要在 30–600 秒之间")
	}
	if !ValidTimezone(v.Timezone) {
		return errors.New("请填写有效的 IANA 时区，例如 America/Los_Angeles")
	}
	return nil
}
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var v Settings
	if !decode(w, r, &v) {
		return
	}
	v.Name = strings.TrimSpace(v.Name)
	v.Timezone = strings.TrimSpace(v.Timezone)
	if err := validSettings(v); err != nil {
		fail(w, 400, "invalid_settings", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.settings
	s.settings = v
	if err := s.save(); err != nil {
		s.settings = old
		fail(w, 500, "save_failed", "保存失败")
		return
	}
	s.event("success", "更新配置", "本地配置已保存")
	write(w, 200, v)
}

func (s *Server) authorized(r *http.Request) bool {
	if _, ok := embeddedAccount(r.Context()); ok && s.embed != nil {
		return true
	}
	value := requestAPIKey(r)
	if value == "" {
		return false
	}
	h := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(h[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(hash)) == 1 {
			reqRecFrom(r.Context()).update(func(l *RequestLog) { l.Key, l.KeyPrefix = k.Name, k.Prefix })
			return true
		}
	}
	return false
}

// requestAPIKey 客户端的 API Key：Authorization: Bearer；Anthropic 接口（/v1/messages）
// bearer 不区分大小写；所有接口都另外接受 x-api-key / api-key 头。
func requestAPIKey(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		if value := strings.TrimSpace(auth[7:]); value != "" {
			return value
		}
	}
	// x-api-key / api-key：Claude Code、部分 Codex 账号管理工具和 Azure 风格客户端用这个头。
	for _, h := range []string{"X-Api-Key", "Api-Key"} {
		if value := strings.TrimSpace(r.Header.Get(h)); value != "" {
			return value
		}
	}
	return ""
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		fail(w, 401, "invalid_api_key", "API Key 无效")
		return
	}
	write(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": "hark", "object": "model", "owned_by": "hark", "created": 0}}})
}
