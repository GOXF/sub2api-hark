package gateway

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sub2APIHarkBaseURL is the base URL of Hark accounts in sub2api-hark (the
// in-process gateway, see GOXF/sub2api-hark HARK.md).
const Sub2APIHarkBaseURL = "http://hark.internal/v1"

// Sub2APIFile converts exported accounts to sub2api's account import file
// ("sub2api-data" v1): each account becomes a Hark platform API Key account
// whose key is {"cookie","user_agent"}; proxies become sub2api proxies.
func Sub2APIFile(list []PortAccount, now time.Time) map[string]any {
	proxies := []map[string]any{}
	seen := map[string]bool{}
	accounts := []map[string]any{}
	for _, a := range list {
		cookie := a.Cookie
		if cookie == "" {
			cookie = CookieHeader(a.Cookies)
		}
		if cookie == "" {
			continue
		}
		key, _ := json.Marshal(map[string]string{"cookie": cookie, "user_agent": a.UserAgent})
		acc := map[string]any{
			"name":        "hark-" + a.Name,
			"platform":    "hark",
			"type":        "apikey",
			"credentials": map[string]any{"api_key": string(key), "base_url": Sub2APIHarkBaseURL},
			"concurrency": 1,
			"priority":    50,
		}
		if p, pk, ok := sub2apiProxy(a.Proxy); ok {
			acc["proxy_key"] = pk
			if !seen[pk] {
				seen[pk] = true
				proxies = append(proxies, p)
			}
		}
		accounts = append(accounts, acc)
	}
	return map[string]any{"type": "sub2api-data", "version": 1, "exported_at": now.UTC().Format(time.RFC3339), "proxies": proxies, "accounts": accounts}
}

func sub2apiProxy(raw string) (map[string]any, string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return nil, "", false
	}
	proto := strings.ToLower(u.Scheme)
	switch proto {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, "", false
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = map[string]int{"http": 80, "https": 443}[proto]
		if port == 0 {
			port = 1080
		}
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	key := fmt.Sprintf("%s|%s|%d|%s|%s", proto, u.Hostname(), port, user, pass)
	p := map[string]any{"proxy_key": key, "name": fmt.Sprintf("hark-%s-%d", u.Hostname(), port), "protocol": proto, "host": u.Hostname(), "port": port, "status": "active"}
	if user != "" {
		p["username"] = user
	}
	if pass != "" {
		p["password"] = pass
	}
	return p, key, true
}
