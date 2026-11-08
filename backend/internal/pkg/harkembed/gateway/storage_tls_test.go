package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAttachmentStorageUsesStandardTLSWithAccountTransport(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		name := "direct"
		if proxied {
			name = "account-proxy"
		}
		t.Run(name, func(t *testing.T) {
			var puts, apiCalls, dialCalls, tunnels atomic.Int32
			storage := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				puts.Add(1)
				if r.Method != http.MethodPut || r.Header.Get("Cookie") != "" || r.Header.Get("User-Agent") != "fixture-browser" || r.ContentLength != 3 {
					t.Errorf("storage method/headers: %s %v len=%d", r.Method, r.Header, r.ContentLength)
				}
				w.WriteHeader(http.StatusOK)
			}))
			storage.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.CurveP256}}
			storage.StartTLS()
			defer storage.Close()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apiCalls.Add(1)
				if r.Header.Get("Cookie") != "fixture-cookie" || r.Header.Get("User-Agent") != "fixture-browser" {
					t.Error("Hark API account headers changed")
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/presign") {
					_ = json.NewEncoder(w).Encode(map[string]any{"upload_url": storage.URL, "attachment_id": "fixture-file", "s3_key": "fixture-key", "path": "fixture-path"})
				} else {
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer api.Close()
			base := browserTransport(nil)
			proxyURL := ""
			if proxied {
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodConnect {
						t.Error("expected account proxy CONNECT")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					upstream, err := net.Dial("tcp", r.Host)
					if err != nil {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						_ = upstream.Close()
						return
					}
					tunnels.Add(1)
					_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
					go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close() }()
					_, _ = io.Copy(conn, upstream)
					_ = conn.Close()
				}))
				defer proxy.Close()
				proxyURL = proxy.URL
				parsed, _ := url.Parse(proxyURL)
				base = browserTransport(parsed)
			}
			defer base.CloseIdleConnections()
			roots := x509.NewCertPool()
			roots.AddCert(storage.Certificate())
			base.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			dial := base.DialContext
			base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialCalls.Add(1)
				return dial(ctx, network, addr)
			}
			s := &Server{client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, upstream: api.URL,
				accounts: []Account{{Cookie: "fixture-cookie", UserAgent: "fixture-browser"}}}
			s.proxyCache.m = map[string]*http.Transport{ProxyDirect: base}
			if proxied {
				s.accounts[0].Proxy = proxyURL
				s.proxyCache.m[s.accounts[0].Proxy] = base
				direct := browserTransport(nil)
				defer direct.CloseIdleConnections()
				s.proxyCache.m[ProxyDirect] = direct
			}
			got, err := s.uploadFile(context.Background(), "fixture-cookie", "fixture-project", newInputFile("fixture.png", "image/png", []byte("PNG"), "image"))
			if err != nil || got.AttachmentID != "fixture-file" || puts.Load() != 1 || apiCalls.Load() != 2 || dialCalls.Load() < 2 || proxied && tunnels.Load() < 2 {
				t.Fatalf("storage upload: result=%+v err=%v puts=%d api=%d dial=%d tunnels=%d", got, err, puts.Load(), apiCalls.Load(), dialCalls.Load(), tunnels.Load())
			}
			if base.DialTLSContext == nil || base.TLSClientConfig.InsecureSkipVerify {
				t.Fatal("API browser TLS was changed or certificate verification disabled")
			}
		})
	}
}

type idleCloseFixture struct{ closed bool }

func (*idleCloseFixture) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (t *idleCloseFixture) CloseIdleConnections()                         { t.closed = true }

func TestUploadClientClosesOwnedIdleConnections(t *testing.T) {
	base := &idleCloseFixture{}
	client := &http.Client{Transport: uploadTransport{uaTransport{base: base}}}
	client.CloseIdleConnections()
	if !base.closed {
		t.Fatal("storage client's wrapper swallowed idle connection cleanup")
	}
}
