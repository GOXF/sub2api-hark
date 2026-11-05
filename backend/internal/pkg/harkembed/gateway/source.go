package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SourceAccount is one Hark account supplied by an embedding application.
type SourceAccount struct {
	// ID must be stable across calls; sessions stay pinned to it.
	ID string
	// Name is shown in the dashboard and logs.
	Name string
	// Cookie is the Cookie request header for hark.com ("name=value; ...").
	Cookie string
	// Proxy optionally routes this account's Hark traffic through an
	// http://, https://, socks5:// or socks5h:// proxy (credentials allowed).
	// Empty uses the gateway-wide upstream proxy (or a direct connection).
	Proxy string
	// UserAgent is the User-Agent of the browser the Cookie came from.
	// Cloudflare binds its clearance cookie to the UA, so upstream requests
	// must send the same one. Empty uses DefaultUserAgent.
	UserAgent string
	// UserID is optional; when known it is sent as expectedUserId.
	UserID string
	// Disabled accounts are kept (sessions remain pinned) but never chosen.
	Disabled bool
	// Bridge says the account's local file bridge is registered in Hark and
	// switched on. Requests that ask for the bridge only start on such accounts.
	Bridge bool
	// Timezone 可选：该账号新建 Project、发消息时使用的 IANA 时区
	// （例如按代理出口 IP 所在地探测）。空或无效则用全局设置。
	Timezone string
}

// AccountSource lets an embedding application (for example a desktop app)
// own the Hark accounts instead of the gateway's encrypted account store.
// Accounts is called before every turn, so it should be cheap; return the
// complete current list.
type AccountSource interface {
	Accounts(ctx context.Context) ([]SourceAccount, error)
}

// AccountSourceFunc adapts a function to AccountSource.
type AccountSourceFunc func(ctx context.Context) ([]SourceAccount, error)

func (f AccountSourceFunc) Accounts(ctx context.Context) ([]SourceAccount, error) { return f(ctx) }

// StaticAccounts is an AccountSource with a fixed list.
type StaticAccounts []SourceAccount

func (s StaticAccounts) Accounts(context.Context) ([]SourceAccount, error) { return s, nil }

var errExternalAccounts = errors.New("accounts are managed by the embedding application")

func validAccountProxy(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("account proxy must be a proxy URL such as http://host:port or socks5://user:pass@host:port")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	}
	return errors.New("account proxy scheme must be http, https, socks5 or socks5h")
}

type proxyTransports struct {
	mu sync.Mutex
	m  map[string]*http.Transport
}

// transportForProxy returns a cached transport for a per-account proxy.
func (s *Server) transportForProxy(raw string) http.RoundTripper {
	s.proxyCache.mu.Lock()
	defer s.proxyCache.mu.Unlock()
	if s.proxyCache.m == nil {
		s.proxyCache.m = map[string]*http.Transport{}
	}
	if t := s.proxyCache.m[raw]; t != nil {
		return t
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	t := browserTransport(u)
	s.proxyCache.m[raw] = t
	return t
}

// DefaultUserAgent is sent to Hark when an account has no browser UA of its
// own. Go's default "Go-http-client/1.1" is routinely blocked by Cloudflare
// with 403, and a clearance cookie only passes with the UA it was issued to.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:140.0) Gecko/20100101 Firefox/140.0"

// uaTransport sets the User-Agent on requests that do not carry one.
type uaTransport struct {
	base http.RoundTripper
	ua   string
}

func (t uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// transportFor picks the transport for requests made with an account's
// cookie: its own proxy if it has one, otherwise the shared upstream client,
// always sending the account's browser User-Agent.
func (s *Server) transportFor(cookie string) http.RoundTripper {
	proxy, ua, direct := "", "", false
	if cookie != "" {
		s.mu.Lock()
		for _, a := range s.accounts {
			if a.Cookie == cookie {
				proxy, direct = s.effectiveProxyLocked(a)
				ua = a.UserAgent
				break
			}
		}
		s.mu.Unlock()
	}
	if ua == "" {
		ua = DefaultUserAgent
	}
	base := s.client.Transport
	if direct || (proxy == "" && s.upstreamProxy == "") {
		base = s.directTransport()
	} else if proxy != "" {
		if t := s.transportForProxy(proxy); t != nil {
			base = t
		}
	}
	return uaTransport{base: base, ua: ua}
}

func (s *Server) clientFor(cookie string, timeout time.Duration, followRedirects bool) *http.Client {
	c := &http.Client{Transport: s.transportFor(cookie), Timeout: timeout}
	if !followRedirects {
		c.CheckRedirect = s.client.CheckRedirect
	}
	return c
}

func cookieFingerprint(cookie string) string {
	h := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(h[:8])
}

// syncAccounts refreshes s.accounts from the AccountSource, keeping runtime
// state (last use, quota, failure status for an unchanged cookie).
func (s *Server) syncAccounts(ctx context.Context) error {
	if s.source == nil {
		return nil
	}
	list, err := s.source.Accounts(ctx)
	if err != nil {
		return &upstreamError{503, "account_source_failed", "账号来源不可用：" + err.Error()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := map[string]Account{}
	for _, a := range s.accounts {
		old[a.ID] = a
	}
	next := make([]Account, 0, len(list))
	seen := map[string]bool{}
	for _, src := range list {
		if src.ID == "" || seen[src.ID] || strings.ContainsAny(src.Cookie, "\r\n") || validAccountProxy(src.Proxy) != nil {
			continue
		}
		seen[src.ID] = true
		a, ok := old[src.ID]
		changed := !ok || cookieFingerprint(a.Cookie) != cookieFingerprint(src.Cookie)
		if !ok {
			a = Account{ID: src.ID}
		}
		a.Name, a.Cookie, a.Proxy, a.UserAgent, a.Bridge = src.Name, src.Cookie, src.Proxy, strings.TrimSpace(src.UserAgent), src.Bridge
		if a.Name == "" {
			a.Name = src.ID
		}
		if src.UserID != "" {
			a.UserID = src.UserID
		}
		a.Timezone = ""
		if tz := strings.TrimSpace(src.Timezone); ValidTimezone(tz) {
			a.Timezone = tz
		}
		switch {
		case src.Disabled || src.Cookie == "":
			a.Status, a.Detail = "disabled", "已由宿主应用停用"
		case changed || a.Status == "disabled" || a.Status == "" || a.Status == "unchecked":
			a.Status, a.Detail = "valid", "由宿主应用提供"
		case a.Status == "blocked" && blockedExpired(a.Checked):
			// 403 多为 Cloudflare 临时拦截：冷却后自动重试，不永久卡死。
			a.Status, a.Detail = "valid", "403 冷却结束，重新尝试"
		}
		next = append(next, a)
	}
	s.accounts = next
	return nil
}

// ReloadAccounts re-reads the AccountSource immediately.
func (s *Server) ReloadAccounts(ctx context.Context) error { return s.syncAccounts(ctx) }

// CreateAPIKey creates a client API key programmatically (for embedding
// applications) and returns its plaintext secret, which is not stored.
func (s *Server) CreateAPIKey(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 40 {
		return "", errors.New("name must be 1-40 characters")
	}
	plain := randomID("sk-hark-")
	h := sha256.Sum256([]byte(plain))
	k := Key{ID: randomID("key_"), Name: name, Prefix: plain[:16], Hash: hex.EncodeToString(h[:]), Created: time.Now().UTC().Format(time.RFC3339)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	if err := s.save(); err != nil {
		s.keys = s.keys[:len(s.keys)-1]
		return "", err
	}
	s.event("success", "创建 API Key", "宿主应用创建了一个客户端 Key")
	return plain, nil
}

const blockedCooldown = 2 * time.Minute

func blockedExpired(checked string) bool {
	t, err := time.Parse(time.RFC3339, checked)
	return err != nil || time.Since(t) > blockedCooldown
}
