// Package harkembed runs the Hark gateway (hark2api, copied under ./gateway)
// inside sub2api. Hark accounts are OpenAI-compatible API Key accounts whose
// base URL is BaseURL and whose "API Key" is the Hark login cookie; requests
// to that host never leave the process: the HTTP upstream hands them to Do,
// which pins the request to that one account (sub2api does the scheduling,
// cooldown and failover) and uses the account's proxy for the calls to Hark.
//
// Update ./gateway with tools/sync-hark2api.sh; it records the hark2api
// commit in gateway/UPSTREAM_COMMIT.
package harkembed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Host is the in-process host name of the Hark gateway.
const Host = "hark.internal"

// BaseURL is the default base URL of Hark platform accounts.
const BaseURL = "http://" + Host

// Handles reports whether req goes to the in-process Hark gateway.
func Handles(req *http.Request) bool {
	return req != nil && req.URL != nil && strings.EqualFold(req.URL.Hostname(), Host)
}

var (
	once    sync.Once
	server  *gateway.Server
	initErr error
)

func dataDir() string {
	if d := strings.TrimSpace(os.Getenv("HARK_DATA_DIR")); d != "" {
		return d
	}
	base := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if base == "" {
		if st, err := os.Stat("/app/data"); err == nil && st.IsDir() {
			base = "/app/data"
		} else {
			base = "data"
		}
	}
	return filepath.Join(base, "hark")
}

func get() (*gateway.Server, error) {
	once.Do(func() {
		server, initErr = gateway.NewEmbedded(dataDir(), gateway.Options{})
	})
	return server, initErr
}

// Credential is what a Hark account stores as its API key: the bare login
// cookie, or a JSON object {"cookie": "...", "user_agent": "..."} (the user
// agent the cookie was issued to; Cloudflare ties cf_clearance to it).
type Credential struct {
	Cookie    string `json:"cookie"`
	UserAgent string `json:"user_agent"`
	// Timezone optionally overrides the IANA time zone sent to Hark for this
	// account (see DefaultTimezone).
	Timezone string `json:"timezone"`
}

// DefaultTimezone is the IANA time zone sent to Hark when creating projects
// and messages, unless the account credential or HARK_TIMEZONE sets one.
// Hark sits behind Cloudflare, which answers 403 "Attention Required" to
// POST /api/conversations whose timezone does not fit the egress IP (the
// embedded gateway's own default, Asia/Shanghai, is blocked from US exits;
// hark2api at hark.fanjj.com is configured with America/Los_Angeles).
const DefaultTimezone = "America/Los_Angeles"

// accountTimezone picks the time zone for one account's Hark requests.
func accountTimezone(cred Credential) string {
	for _, tz := range []string{cred.Timezone, os.Getenv("HARK_TIMEZONE")} {
		if tz = strings.TrimSpace(tz); tz != "" && gateway.ValidTimezone(tz) {
			return tz
		}
	}
	return DefaultTimezone
}

// ParseCredential reads a Hark account API key.
func ParseCredential(key string) Credential {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "{") {
		var c Credential
		if json.Unmarshal([]byte(key), &c) == nil {
			c.Cookie, c.UserAgent, c.Timezone = strings.TrimSpace(c.Cookie), strings.TrimSpace(c.UserAgent), strings.TrimSpace(c.Timezone)
			return c
		}
	}
	return Credential{Cookie: key}
}

func apiKeyOf(req *http.Request) string {
	if v := strings.TrimSpace(req.Header.Get("Authorization")); v != "" {
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			return strings.TrimSpace(v[7:])
		}
		return v
	}
	return strings.TrimSpace(req.Header.Get("x-api-key"))
}

// Do serves one upstream request for account accountID in-process.
func Do(req *http.Request, proxyURL string, accountID int64) (*http.Response, error) {
	s, err := get()
	if err != nil {
		return nil, fmt.Errorf("hark gateway: %w", err)
	}
	cred := ParseCredential(apiKeyOf(req))
	acc := gateway.SourceAccount{
		ID:        fmt.Sprintf("sub2api-%d", accountID),
		Name:      fmt.Sprintf("sub2api #%d", accountID),
		Cookie:    cred.Cookie,
		UserAgent: cred.UserAgent,
		Proxy:     strings.TrimSpace(proxyURL),
		Timezone:  accountTimezone(cred),
	}
	in := req.Clone(gateway.WithEmbeddedAccount(req.Context(), acc))
	in.Header.Del("Authorization")
	in.Header.Del("x-api-key")
	in.RequestURI = ""
	if in.Body == nil {
		in.Body = http.NoBody
	}
	if err := forceHarkModel(in); err != nil {
		return nil, fmt.Errorf("hark gateway: read request body: %w", err)
	}
	if cred.Cookie != "" {
		if resp := modelIdentityResponse(in); resp != nil {
			return resp, nil
		}
	}
	// The client's request as sent (after the model rewrite, before the
	// identity note) is what the estimated input usage is counted from.
	var usageBody []byte
	if usageEstimateEnabled(in) && labelFormatOf(in.URL.Path) != labelNone {
		usageBody = requestBodyBytes(in)
	}
	if err := injectModelLabel(in); err != nil {
		return nil, fmt.Errorf("hark gateway: read request body: %w", err)
	}
	resp := serve(s.EmbeddedHandler(), in)
	if usageBody != nil {
		resp = withUsageEstimate(in, usageBody, resp)
	}
	return resp, nil
}

// Model is the only model name the Hark gateway accepts.
const Model = "hark"

// forceHarkModel rewrites the JSON body's "model" to Model for the gateway's
// POST endpoints (Chat Completions, Responses, Anthropic Messages, ...): the
// gateway rejects any other model ("当前模型别名为 hark"), while clients such as
// Codex or Claude Code send their own model names. The account's model mapping
// has already been applied by sub2api; a mapped name other than Model is
// forced as well. Non-JSON bodies are left untouched.
func forceHarkModel(req *http.Request) error {
	if req.Method != http.MethodPost || req.Body == nil || req.Body == http.NoBody {
		return nil
	}
	if ct := strings.ToLower(req.Header.Get("Content-Type")); ct != "" && !strings.Contains(ct, "json") {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return err
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' && gjson.ValidBytes(trimmed) {
		if m := gjson.GetBytes(trimmed, "model"); m.Type != gjson.String || m.Str != Model {
			if out, setErr := sjson.SetBytes(trimmed, "model", Model); setErr == nil {
				body = out
			}
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return nil
}

// serve runs h and returns its response as soon as the headers are written,
// streaming the body (SSE) through a pipe.
func serve(h http.Handler, req *http.Request) *http.Response {
	pr, pw := io.Pipe()
	w := &pipeWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				w.writeHeader(http.StatusBadGateway)
				_ = pw.CloseWithError(fmt.Errorf("hark gateway panic: %v", p))
				return
			}
			w.writeHeader(http.StatusOK)
			_ = pw.Close()
		}()
		h.ServeHTTP(w, req)
	}()
	<-w.ready
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode: w.status,
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        w.snapshot,
		Body:          pr,
		ContentLength: -1,
		Request:       req,
	}
}

type pipeWriter struct {
	header   http.Header
	snapshot http.Header
	pw       *io.PipeWriter
	once     sync.Once
	ready    chan struct{}
	status   int
}

func (w *pipeWriter) Header() http.Header { return w.header }

func (w *pipeWriter) writeHeader(code int) {
	w.once.Do(func() {
		w.status, w.snapshot = code, w.header.Clone()
		close(w.ready)
	})
}

func (w *pipeWriter) WriteHeader(code int) { w.writeHeader(code) }

func (w *pipeWriter) Write(b []byte) (int, error) {
	w.writeHeader(http.StatusOK)
	return w.pw.Write(b)
}

// Flush is a no-op: every Write already reaches the reader.
func (w *pipeWriter) Flush() {}
