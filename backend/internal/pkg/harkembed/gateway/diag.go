package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Diagnostics for blocked upstream calls. A Hark 403 can come from Cloudflare
// (bot management / challenge, HTML page, cf-mitigated header) or from Hark
// itself (JSON {"error": ...}); the summary below tells them apart without
// exposing cookies or account data.

var (
	diagTitleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	diagTagRE   = regexp.MustCompile(`(?s)<[^>]*>`)
	diagUARE    = regexp.MustCompile(`(Edg|Chrome|Firefox|Safari)/(\d+)`)
)

// upstreamDiag summarizes a failed Hark response in one line.
func upstreamDiag(method, path string, resp *http.Response, body []byte, route string) string {
	parts := []string{}
	if method != "" || path != "" {
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		parts = append(parts, strings.TrimSpace(method+" "+path))
	}
	if resp != nil {
		h := resp.Header
		if v := h.Get("Cf-Mitigated"); v != "" {
			parts = append(parts, "cf-mitigated="+diagClean(v, 40))
		}
		if v := h.Get("Server"); v != "" {
			parts = append(parts, "server="+diagClean(v, 40))
		}
		if v := h.Get("Cf-Ray"); v != "" {
			parts = append(parts, "cf-ray="+diagClean(v, 40))
		}
		if v := h.Get("Content-Type"); v != "" {
			if i := strings.IndexByte(v, ';'); i >= 0 {
				v = v[:i]
			}
			parts = append(parts, diagClean(v, 40))
		}
	}
	if s := diagBody(body); s != "" {
		parts = append(parts, "「"+s+"」")
	}
	if route != "" {
		parts = append(parts, route)
	}
	return strings.Join(parts, " · ")
}

// diagBody extracts the useful part of a response body: a JSON error field,
// an HTML page title, or the first characters of plain text.
func diagBody(body []byte) string {
	b := strings.TrimSpace(string(body))
	if b == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(b), &obj) == nil && obj != nil {
		for _, k := range []string{"error", "message", "code", "reason"} {
			switch v := obj[k].(type) {
			case string:
				if v != "" {
					return diagClean(k+": "+v, 160)
				}
			case map[string]any:
				if m, ok := v["message"].(string); ok && m != "" {
					return diagClean(k+": "+m, 160)
				}
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return diagClean("JSON 字段: "+strings.Join(keys, ","), 160)
	}
	if m := diagTitleRE.FindStringSubmatch(b); m != nil {
		t := diagClean(diagTagRE.ReplaceAllString(m[1], " "), 120)
		if strings.Contains(b, "cf-error-code") || strings.Contains(b, "cf-browser-verification") || strings.Contains(b, "challenge-platform") {
			t += "（Cloudflare 页面）"
		}
		return t
	}
	// Arbitrary text is not echoed (it may carry private data); its size is enough
	// to tell an empty block page from a real response.
	return fmt.Sprintf("非 JSON/HTML 正文 %d 字节", len(body))
}

func diagClean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// routeDiag describes the egress and browser identity used for an account,
// without credentials: "经代理 127.0.0.1:7890 · UA Edg/142" or "直连 · UA …".
func (s *Server) routeDiag(cookie string) string {
	proxy, ua := "", ""
	var global *url.URL
	s.mu.Lock()
	for _, a := range s.accounts {
		if cookie != "" && a.Cookie == cookie {
			proxy, _ = s.effectiveProxyLocked(a)
			ua = a.UserAgent
			break
		}
	}
	if t, ok := s.client.Transport.(*http.Transport); ok && t != nil && t.Proxy != nil {
		if req, err := http.NewRequest("GET", s.upstream+"/", nil); err == nil {
			global, _ = t.Proxy(req)
		}
	}
	s.mu.Unlock()
	route := "直连"
	if proxy == "" {
		if global != nil {
			route = "经全局代理 " + global.Scheme + "://" + global.Host
		}
	} else if u, err := url.Parse(proxy); err == nil && u.Host != "" {
		route = "经代理 " + u.Scheme + "://" + u.Host
	} else {
		route = "经代理"
	}
	uaTag := "默认 Firefox"
	if ua != "" {
		uaTag = "?"
		if m := diagUARE.FindAllStringSubmatch(ua, -1); len(m) > 0 {
			// Prefer the most specific browser token (Edg over Chrome over Safari).
			best := m[0]
			for _, x := range m {
				if x[1] == "Edg" || (x[1] == "Chrome" && best[1] == "Safari") || (x[1] == "Firefox") {
					best = x
				}
			}
			uaTag = fmt.Sprintf("%s/%s", best[1], best[2])
		}
	}
	return route + " · UA " + uaTag
}
