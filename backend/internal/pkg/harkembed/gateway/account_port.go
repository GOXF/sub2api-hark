package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// 账号导出 / 导入。导出文件格式（导入也认它）：
//
//	{"format":"hark2api-accounts","version":1,"exported_at":"...","accounts":[
//	  {"name":"...","cookie":"a=1; b=2","cookies":[{"name":"a","value":"1","domain":".hark.com",...}],
//	   "user_agent":"...","proxy":"http://..."}]}
//
// 导入还接受直接给数组 [...]，或单个 {"name","cookie"} 对象。cookies（完整 cookie，带域名/HttpOnly）
// 可选；没有时由 cookie 请求头生成。文件里有登录 Cookie，等同密码，注意保管。

const PortFormat = "hark2api-accounts"

// PortCookie 一个完整 cookie（字段名同 Chrome DevTools Protocol）。
type PortCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
}

// PortAccount 导出文件里的一个账号。
type PortAccount struct {
	Name      string       `json:"name"`
	Cookie    string       `json:"cookie,omitempty"`
	Cookies   []PortCookie `json:"cookies,omitempty"`
	UserAgent string       `json:"user_agent,omitempty"`
	Proxy     string       `json:"proxy,omitempty"`
	// Timezone 可选：账号专用 IANA 时区。
	Timezone string `json:"timezone,omitempty"`
}

// PortFile 导出文件。
type PortFile struct {
	Format     string        `json:"format"`
	Version    int           `json:"version"`
	ExportedAt string        `json:"exported_at"`
	Accounts   []PortAccount `json:"accounts"`
}

// ImportResult 导入结果。
type ImportResult struct {
	Added   int      `json:"added"`
	Updated int      `json:"updated"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors,omitempty"`
}

// AccountPorter 宿主应用（账号由它管理时）实现导出、导入。
type AccountPorter interface {
	ExportAccounts(ctx context.Context) ([]PortAccount, error)
	ImportAccounts(ctx context.Context, list []PortAccount) (ImportResult, error)
}

// CookieHeader 由完整 cookie 生成请求头。
func CookieHeader(cs []PortCookie) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.Name != "" {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// CookiesFromHeader 把 "a=1; b=2" 拆成 .hark.com 下的 cookie（导入只有请求头时用）。
func CookiesFromHeader(h string) []PortCookie {
	var out []PortCookie
	for _, p := range strings.Split(h, ";") {
		p = strings.TrimSpace(p)
		i := strings.IndexByte(p, '=')
		if i <= 0 {
			continue
		}
		out = append(out, PortCookie{Name: p[:i], Value: p[i+1:], Domain: ".hark.com", Path: "/", Secure: true})
	}
	return out
}

// ParsePortFile 解析导入内容：导出文件、数组或单个账号。会补齐 Cookie / Cookies、去掉无效项。
func ParsePortFile(b []byte) ([]PortAccount, error) {
	b = []byte(strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff")))
	var list []PortAccount
	switch {
	case len(b) > 0 && b[0] == '[':
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, errors.New("文件不是有效的账号列表")
		}
	case len(b) > 0 && b[0] == '{':
		var f struct {
			Format   string          `json:"format"`
			Accounts json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, errors.New("文件不是有效的 JSON")
		}
		if len(f.Accounts) > 0 {
			if err := json.Unmarshal(f.Accounts, &list); err != nil {
				return nil, errors.New("accounts 格式不正确")
			}
		} else {
			var one PortAccount
			if err := json.Unmarshal(b, &one); err != nil {
				return nil, errors.New("文件不是有效的账号")
			}
			list = []PortAccount{one}
		}
	default:
		return nil, errors.New("请选择导出的 JSON 文件")
	}
	out := list[:0]
	for _, a := range list {
		a.Name = strings.TrimSpace(a.Name)
		a.Cookie = strings.TrimSpace(a.Cookie)
		a.Proxy = strings.TrimSpace(a.Proxy)
		if a.Timezone = strings.TrimSpace(a.Timezone); !ValidTimezone(a.Timezone) {
			a.Timezone = ""
		}
		if a.Cookie == "" && len(a.Cookies) > 0 {
			a.Cookie = CookieHeader(a.Cookies)
		}
		if len(a.Cookies) == 0 && a.Cookie != "" {
			a.Cookies = CookiesFromHeader(a.Cookie)
		}
		if !strings.Contains(a.Cookie, "=") {
			continue
		}
		if r := []rune(a.Name); len(r) > 40 {
			a.Name = string(r[:40])
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, errors.New("文件里没有带 Cookie 的账号")
	}
	return out, nil
}

func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request) {
	var list []PortAccount
	if s.porter != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		var err error
		if list, err = s.porter.ExportAccounts(ctx); err != nil {
			fail(w, 500, "export_failed", "导出失败："+err.Error())
			return
		}
	} else if s.source != nil {
		fail(w, 409, "accounts_external", "账号由宿主应用管理，宿主未提供导出")
		return
	} else {
		s.mu.Lock()
		for _, a := range s.accounts {
			if a.Cookie != "" {
				list = append(list, PortAccount{Name: a.Name, Cookie: a.Cookie, UserAgent: a.UserAgent, Proxy: a.Proxy, Timezone: a.Timezone})
			}
		}
		s.mu.Unlock()
	}
	if list == nil {
		list = []PortAccount{}
	}
	s.mu.Lock()
	s.event("info", "导出账号", "共 "+itoa(len(list))+" 个")
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("format") == "sub2api" {
		w.Header().Set("Content-Disposition", `attachment; filename="sub2api-hark-`+time.Now().Format("20060102-1504")+`.json"`)
		write(w, 200, Sub2APIFile(list, time.Now()))
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="hark-accounts-`+time.Now().Format("20060102-1504")+`.json"`)
	write(w, 200, PortFile{Format: PortFormat, Version: 1, ExportedAt: time.Now().Format(time.RFC3339), Accounts: list})
}

func (s *Server) importAccounts(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		fail(w, 413, "too_large", "文件太大")
		return
	}
	list, err := ParsePortFile(b)
	if err != nil {
		fail(w, 400, "invalid_file", err.Error())
		return
	}
	var res ImportResult
	if s.porter != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		if res, err = s.porter.ImportAccounts(ctx, list); err != nil {
			fail(w, 500, "import_failed", "导入失败："+err.Error())
			return
		}
		if s.source != nil {
			sctx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
			_ = s.syncAccounts(sctx)
			c2()
		}
	} else if s.source != nil {
		fail(w, 409, "accounts_external", "账号由宿主应用管理，宿主未提供导入")
		return
	} else {
		s.mu.Lock()
		before := len(s.accounts)
		for _, a := range list {
			if validAccountProxy(a.Proxy) != nil {
				a.Proxy = ""
			}
			if strings.ContainsAny(a.Cookie, "\r\n") || len(a.Cookie) > 16000 {
				res.Skipped++
				continue
			}
			name := a.Name
			if name == "" {
				name = "导入账号 " + itoa(len(s.accounts)+1)
			}
			dup := false
			for i := range s.accounts {
				if s.accounts[i].Name == name {
					s.accounts[i].Cookie, s.accounts[i].Status, s.accounts[i].Detail = a.Cookie, "unchecked", "已从文件更新"
					if a.Proxy != "" {
						s.accounts[i].Proxy = a.Proxy
					}
					if a.UserAgent != "" {
						s.accounts[i].UserAgent = a.UserAgent
					}
					if a.Timezone != "" {
						s.accounts[i].Timezone = a.Timezone
					}
					dup = true
					res.Updated++
					break
				}
			}
			if !dup {
				s.accounts = append(s.accounts, Account{ID: randomID("acct_"), Name: name, Status: "unchecked", Detail: "已从文件导入", Cookie: a.Cookie, Proxy: a.Proxy, UserAgent: a.UserAgent, Timezone: a.Timezone})
				res.Added++
			}
		}
		if err := s.saveAccounts(); err != nil {
			s.accounts = s.accounts[:before]
			s.mu.Unlock()
			fail(w, 500, "save_failed", "保存失败，请检查数据目录")
			return
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.event("info", "导入账号", "新增 "+itoa(res.Added)+"，更新 "+itoa(res.Updated)+"，跳过 "+itoa(res.Skipped))
	s.mu.Unlock()
	write(w, 200, res)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
