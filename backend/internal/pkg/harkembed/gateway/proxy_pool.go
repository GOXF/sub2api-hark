package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 代理池（参考 sub2api 的代理管理）：添加 / 测试 / 删除代理，把代理分配给账号。
// 分配的代理优先于账号自带代理（宿主应用模式下即 Hub 的线路），只影响本网关发往 Hark 的请求。
// 特殊值 ProxyDirect 表示强制直连（不走账号代理，也不走全局上游代理）。

const ProxyDirect = "direct"

// ProxyEntry 代理池里的一项（URL 含账号密码，只存盘不下发）。
type ProxyEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Status    string `json:"status,omitempty"` // ok / error / ""
	ExitIP    string `json:"exit_ip,omitempty"`
	Location  string `json:"location,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Checked   string `json:"checked,omitempty"`
	Error     string `json:"error,omitempty"`
}

type proxyView struct {
	ProxyEntry
	URL     string `json:"url,omitempty"` // 永远为空：遮住 ProxyEntry.URL（含密码）
	Display string `json:"display"`
	Auth    bool   `json:"auth"`
	Used    int    `json:"used"`
}

// ParseProxyLine 认这些写法：scheme://user:pass@host:port、host:port、host:port:user:pass、user:pass@host:port（默认 http）。
func ParseProxyLine(line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("空行")
	}
	if !strings.Contains(line, "://") {
		if parts := strings.Split(line, ":"); len(parts) == 4 && !strings.Contains(line, "@") {
			line = "http://" + url.UserPassword(parts[2], parts[3]).String() + "@" + parts[0] + ":" + parts[1]
		} else {
			line = "http://" + line
		}
	}
	u, err := url.Parse(line)
	if err != nil || u.Hostname() == "" || u.Port() == "" {
		return "", errors.New("代理格式不正确：" + maskProxy(line))
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	raw := u.String()
	if err := validAccountProxy(raw); err != nil {
		return "", errors.New("只支持 http / https / socks5 / socks5h 代理")
	}
	return raw, nil
}

func maskProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "（无效地址）"
	}
	return u.Scheme + "://" + u.Host
}

// effectiveProxy 账号实际使用的代理；direct=true 表示强制直连。调用方持 mu。
func (s *Server) effectiveProxyLocked(a Account) (proxy string, direct bool) {
	switch pid := s.acctProxy[a.ID]; pid {
	case "":
		return a.Proxy, false
	case ProxyDirect:
		return "", true
	default:
		for _, p := range s.proxies {
			if p.ID == pid {
				return p.URL, false
			}
		}
		return a.Proxy, false // 代理已删除：退回账号自带
	}
}

func (s *Server) directTransport() http.RoundTripper {
	s.proxyCache.mu.Lock()
	defer s.proxyCache.mu.Unlock()
	if s.proxyCache.m == nil {
		s.proxyCache.m = map[string]*http.Transport{}
	}
	if t := s.proxyCache.m[ProxyDirect]; t != nil {
		return t
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	s.proxyCache.m[ProxyDirect] = t
	return t
}

func (s *Server) proxyViewsLocked() []proxyView {
	used := map[string]int{}
	for _, pid := range s.acctProxy {
		used[pid]++
	}
	out := make([]proxyView, 0, len(s.proxies))
	for _, p := range s.proxies {
		u, _ := url.Parse(p.URL)
		out = append(out, proxyView{ProxyEntry: p, Display: maskProxy(p.URL), Auth: u != nil && u.User != nil, Used: used[p.ID]})
	}
	return out
}

func (s *Server) addProxies(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name  string `json:"name"`
		Lines string `json:"lines"`
	}
	if !decode(w, r, &in) {
		return
	}
	var bad []string
	s.mu.Lock()
	have := map[string]bool{}
	for _, p := range s.proxies {
		have[p.URL] = true
	}
	added := 0
	for _, line := range strings.Split(in.Lines, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		raw, err := ParseProxyLine(line)
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		if have[raw] {
			continue
		}
		have[raw] = true
		name := strings.TrimSpace(in.Name)
		if name == "" || added > 0 {
			u, _ := url.Parse(raw)
			name = u.Host
			if strings.TrimSpace(in.Name) != "" {
				name = strings.TrimSpace(in.Name) + " " + itoa(added+1)
			}
		}
		if r := []rune(name); len(r) > 40 {
			name = string(r[:40])
		}
		s.proxies = append(s.proxies, ProxyEntry{ID: randomID("px_"), Name: name, URL: raw})
		added++
	}
	if added > 0 {
		if err := s.save(); err != nil {
			s.mu.Unlock()
			fail(w, 500, "save_failed", "保存失败")
			return
		}
		s.event("info", "添加代理", "新增 "+itoa(added)+" 个")
	}
	s.mu.Unlock()
	if added == 0 && len(bad) > 0 {
		fail(w, 400, "invalid_proxy", strings.Join(bad, "；"))
		return
	}
	write(w, 200, map[string]any{"added": added, "errors": bad})
}

func (s *Server) updateProxy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.proxies {
		if s.proxies[i].ID != id {
			continue
		}
		if n := strings.TrimSpace(in.Name); n != "" {
			if r := []rune(n); len(r) > 40 {
				n = string(r[:40])
			}
			s.proxies[i].Name = n
		}
		if strings.TrimSpace(in.URL) != "" {
			raw, err := ParseProxyLine(in.URL)
			if err != nil {
				fail(w, 400, "invalid_proxy", err.Error())
				return
			}
			s.proxies[i].URL, s.proxies[i].Status, s.proxies[i].ExitIP, s.proxies[i].Location, s.proxies[i].Error = raw, "", "", "", ""
		}
		if err := s.save(); err != nil {
			fail(w, 500, "save_failed", "保存失败")
			return
		}
		write(w, 200, map[string]bool{"ok": true})
		return
	}
	fail(w, 404, "not_found", "代理不存在")
}

func (s *Server) deleteProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.proxies {
		if p.ID != id {
			continue
		}
		s.proxies = append(s.proxies[:i:i], s.proxies[i+1:]...)
		for aid, pid := range s.acctProxy {
			if pid == id {
				delete(s.acctProxy, aid)
			}
		}
		if err := s.save(); err != nil {
			fail(w, 500, "save_failed", "保存失败")
			return
		}
		s.event("info", "删除代理", p.Name)
		write(w, 200, map[string]bool{"ok": true})
		return
	}
	fail(w, 404, "not_found", "代理不存在")
}

// ProbeProxy 经代理请求 Hark 的 /cdn-cgi/trace，得到出口 IP、地区和延迟。
func ProbeProxy(ctx context.Context, raw, upstream string) (ip, loc string, ms int64, err error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if raw != "" {
		u, e := url.Parse(raw)
		if e != nil {
			return "", "", 0, e
		}
		t.Proxy = http.ProxyURL(u)
	} else {
		t.Proxy = nil
	}
	defer t.CloseIdleConnections()
	if upstream == "" {
		upstream = "https://hark.com"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", upstream+"/cdn-cgi/trace", nil)
	req.Header.Set("User-Agent", DefaultUserAgent)
	start := time.Now()
	resp, err := (&http.Client{Transport: t, Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	ms = time.Since(start).Milliseconds()
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ip":
			ip = v
		case "loc":
			loc = v
		}
	}
	if resp.StatusCode >= 400 && ip == "" {
		return "", "", ms, errors.New("Hark 返回 " + resp.Status)
	}
	return ip, loc, ms, nil
}

func (s *Server) testProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	raw, found := "", false
	for _, p := range s.proxies {
		if p.ID == id {
			raw, found = p.URL, true
		}
	}
	upstream := s.upstream
	s.mu.Unlock()
	if !found {
		fail(w, 404, "not_found", "代理不存在")
		return
	}
	ip, loc, ms, err := ProbeProxy(r.Context(), raw, upstream)
	s.mu.Lock()
	var res ProxyEntry
	for i := range s.proxies {
		if s.proxies[i].ID != id {
			continue
		}
		p := &s.proxies[i]
		p.Checked = time.Now().UTC().Format(time.RFC3339)
		if err != nil {
			p.Status, p.Error, p.LatencyMs = "error", err.Error(), 0
		} else {
			p.Status, p.Error, p.ExitIP, p.Location, p.LatencyMs = "ok", "", ip, loc, ms
		}
		res = *p
		_ = s.save()
	}
	s.mu.Unlock()
	res.URL = ""
	write(w, 200, res)
}

func (s *Server) assignProxy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountIDs []string `json:"account_ids"`
		ProxyID    string   `json:"proxy_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.ProxyID != "" && in.ProxyID != ProxyDirect {
		ok := false
		for _, p := range s.proxies {
			ok = ok || p.ID == in.ProxyID
		}
		if !ok {
			fail(w, 404, "not_found", "代理不存在")
			return
		}
	}
	if s.acctProxy == nil {
		s.acctProxy = map[string]string{}
	}
	n := 0
	for _, id := range in.AccountIDs {
		if id == "" {
			continue
		}
		if in.ProxyID == "" {
			delete(s.acctProxy, id)
		} else {
			s.acctProxy[id] = in.ProxyID
		}
		n++
	}
	if err := s.save(); err != nil {
		fail(w, 500, "save_failed", "保存失败")
		return
	}
	s.event("info", "分配代理", itoa(n)+" 个账号")
	write(w, 200, map[string]int{"updated": n})
}
