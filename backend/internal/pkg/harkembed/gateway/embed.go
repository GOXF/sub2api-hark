package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Embedded mode lets another gateway (for example sub2api) schedule Hark
// accounts itself and hand each request to hark2api in-process: the request
// context carries exactly one account, hark2api uses only that account (no
// failover to other accounts) and returns Hark's own status codes so the host
// can mark, cool down or retry the account on its side.
//
// The pinned account never comes from the network: it is set with
// WithEmbeddedAccount by the embedding process, so the /v1 API key check is
// skipped only for requests that carry it.

type embedCtxKey struct{}

// WithEmbeddedAccount pins a request to one account supplied by the embedding
// application. Use it with Server.EmbeddedHandler.
func WithEmbeddedAccount(ctx context.Context, a SourceAccount) context.Context {
	return context.WithValue(ctx, embedCtxKey{}, a)
}

func embeddedAccount(ctx context.Context) (SourceAccount, bool) {
	a, ok := ctx.Value(embedCtxKey{}).(SourceAccount)
	return a, ok && a.ID != ""
}

// embedRegistry is the AccountSource of an embedded server: every account a
// request was pinned to, forgotten after embedIdle without use.
type embedRegistry struct {
	mu   sync.Mutex
	m    map[string]SourceAccount
	seen map[string]time.Time
}

const embedIdle = 24 * time.Hour

func (r *embedRegistry) put(a SourceAccount) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m, r.seen = map[string]SourceAccount{}, map[string]time.Time{}
	}
	r.m[a.ID], r.seen[a.ID] = a, time.Now()
}

func (r *embedRegistry) Accounts(context.Context) ([]SourceAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SourceAccount, 0, len(r.m))
	for id, a := range r.m {
		if time.Since(r.seen[id]) > embedIdle {
			delete(r.m, id)
			delete(r.seen, id)
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// NewEmbedded creates a server for in-process use by another gateway. data
// holds session/project state; accounts are never written to disk.
func NewEmbedded(data string, o Options) (*Server, error) {
	s, err := New(data)
	if err != nil {
		return nil, err
	}
	reg := &embedRegistry{}
	o.AccountSource = reg
	o.AccountPorter = nil
	if err := s.Configure(o); err != nil {
		return nil, err
	}
	s.embed = reg
	return s, nil
}

// EmbeddedHandler serves the OpenAI-compatible /v1 API for requests pinned
// with WithEmbeddedAccount. Requests without a pinned account are refused.
func (s *Server) EmbeddedHandler() http.Handler {
	h := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := embeddedAccount(r.Context())
		if s.embed == nil || !ok || !strings.HasPrefix(r.URL.Path, "/v1/") {
			fail(w, 401, "invalid_api_key", "缺少宿主指定的 Hark 账号")
			return
		}
		if a.Name == "" {
			a.Name = a.ID
		}
		s.embed.put(a)
		h.ServeHTTP(w, r)
	})
}

// pinnedAccount returns the account a request is pinned to, re-armed for this
// attempt: the host decides whether an account is usable, so a status left
// over from an earlier failure does not block it here.
func (s *Server) pinnedAccount(ctx context.Context) (Account, bool, error) {
	src, ok := embeddedAccount(ctx)
	if !ok || s.embed == nil {
		return Account{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == src.ID {
			if s.accounts[i].Cookie == "" {
				return Account{}, true, &upstreamError{401, "upstream_session_expired", "账号没有 Cookie"}
			}
			if s.accounts[i].Status != "valid" {
				s.accounts[i].Status, s.accounts[i].Detail = "valid", "由宿主应用调度"
			}
			return s.accounts[i], true, nil
		}
	}
	return Account{}, true, &upstreamError{409, "no_valid_account", "宿主指定的 Hark 账号不可用"}
}
