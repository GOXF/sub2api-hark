package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type adminSession struct{ expires time.Time }
type authThrottle struct {
	count int
	until time.Time
}

func (s *Server) configureAccess() error {
	s.publicOrigin = strings.TrimRight(os.Getenv("HARK_PUBLIC_ORIGIN"), "/")
	if s.publicOrigin != "" {
		u, e := url.Parse(s.publicOrigin)
		if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return errors.New("HARK_PUBLIC_ORIGIN must be an HTTP or HTTPS origin")
		}
	}
	encoded := os.Getenv("HARK_ADMIN_KEY_SHA256")
	if plain := os.Getenv("HARK2API_ADMIN_KEY"); plain != "" && encoded == "" {
		if len(plain) < 12 {
			return errors.New("HARK2API_ADMIN_KEY must be at least 12 characters")
		}
		h := sha256.Sum256([]byte(plain))
		encoded = hex.EncodeToString(h[:])
	}
	if encoded != "" {
		b, e := hex.DecodeString(encoded)
		if e != nil || len(b) != 32 {
			return errors.New("invalid administrator key hash")
		}
		s.adminHash = b
	}
	if s.publicOrigin != "" && len(s.adminHash) == 0 {
		return errors.New("remote access requires administrator key hash")
	}
	s.adminSessions = make(map[string]adminSession)
	s.authAttempts = make(map[string]authThrottle)
	return nil
}
func (s *Server) allowedHost(host string) bool {
	if s.publicOrigin != "" {
		u, _ := url.Parse(s.publicOrigin)
		if host == u.Host {
			return true
		}
	}
	// Server mode without HARK_PUBLIC_ORIGIN: the administrator key (enforced
	// at startup) and SameSite=Strict HttpOnly session cookie protect the
	// dashboard, so any Host is accepted.
	if s.remote && len(s.adminHash) > 0 && s.publicOrigin == "" {
		return true
	}
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	return name == "127.0.0.1" || name == "localhost" || name == "[::1]" || name == "::1"
}

// sameOrigin compares the Origin header with the request host (or the
// configured public origin), ignoring the scheme so TLS-terminating reverse
// proxies keep working.
func (s *Server) sameOrigin(r *http.Request, origin string) bool {
	if s.publicOrigin != "" && origin == s.publicOrigin {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if s.publicOrigin != "" {
		return origin == requestOrigin(r) && u.Host == r.Host
	}
	return u.Host == r.Host
}
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
func adminCookieName(r *http.Request) string {
	if r.TLS != nil {
		return "hark_admin"
	}
	// HTTP cannot overwrite a Secure cookie left by an earlier HTTPS login.
	return "hark_admin_http"
}
func (s *Server) isAdmin(r *http.Request) bool {
	if len(s.adminHash) == 0 {
		return true
	}
	cookie, err := r.Cookie(adminCookieName(r))
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.adminSessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(session.expires) {
		delete(s.adminSessions, cookie.Value)
		return false
	}
	return true
}
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(s.adminHash) == 0 {
		write(w, 200, map[string]bool{"ok": true})
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	hash := sha256.Sum256([]byte(input.Key))
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range s.authAttempts {
		if now.After(value.until) {
			delete(s.authAttempts, key)
		}
	}
	attempt := s.authAttempts[ip]
	if attempt.count >= 5 && now.Before(attempt.until) {
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "auth_limited", "尝试次数较多，请一分钟后再试")
		return
	}
	if subtle.ConstantTimeCompare(hash[:], s.adminHash) != 1 {
		if len(s.authAttempts) >= 2048 {
			fail(w, 429, "auth_limited", "请稍后重试")
			return
		}
		if attempt.count == 0 {
			attempt.until = now.Add(time.Minute)
		}
		attempt.count++
		s.authAttempts[ip] = attempt
		fail(w, 401, "invalid_admin_key", "管理员密钥不正确")
		return
	}
	delete(s.authAttempts, ip)
	for key, session := range s.adminSessions {
		if now.After(session.expires) {
			delete(s.adminSessions, key)
		}
	}
	if len(s.adminSessions) >= 100 {
		fail(w, 429, "session_limit", "管理员会话数量已达上限")
		return
	}
	token := randomID("admin_")
	s.adminSessions[token] = adminSession{expires: now.Add(12 * time.Hour)}
	http.SetCookie(w, &http.Cookie{Name: adminCookieName(r), Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	write(w, 200, map[string]bool{"ok": true})
}
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	cookie, e := r.Cookie(adminCookieName(r))
	if e == nil {
		s.mu.Lock()
		delete(s.adminSessions, cookie.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookieName(r), Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	write(w, 200, map[string]bool{"ok": true})
}
