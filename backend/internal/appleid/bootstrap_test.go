package appleid

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var jpegBytes = []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0}

func mockResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func fixtureClient(t *testing.T, change func(*http.Request, *http.Response)) (*http.Client, *[]string) {
	t.Helper()
	calls := new([]string)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		var resp *http.Response
		switch r.URL.Path {
		case "/account":
			if strings.Contains(r.Header.Get("Accept"), "application/json") {
				t.Error("portal must request HTML")
			}
			resp = mockResponse(200, "<html></html>")
		case "/bootstrap/portal":
			if r.Header.Get("Accept") != "application/json" {
				t.Error("bootstrap must request JSON")
			}
			resp = mockResponse(200, `{"apiUrl":"https://appleid.apple.com","serviceKey":"public-fixture-widget-key-longer-than-32-characters"}`)
		case "/widget/account/":
			if r.URL.Query().Get("appContext") != "account" || r.URL.Query().Get("widgetKey") == "" {
				t.Error("missing widget parameters")
			}
			resp = mockResponse(200, `<html><script id='boot_args' type='application/json'>{"direct":{"scnt":"fixture-scnt","sessionId":"fixture-session","widgetKey":"fixture-key"}}</script></html>`)
		case "/captcha":
			if r.Method != http.MethodPost {
				t.Error("captcha must POST")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"type":"IMAGE"}` {
				t.Errorf("unexpected captcha input: %s", body)
			}
			if r.Header.Get("scnt") == "" || r.Header.Get("X-Apple-ID-Session-Id") == "" {
				t.Error("missing session headers")
			}
			if r.Header.Get("X-Apple-Api-Key") != "" {
				t.Error("optional API key must not be invented")
			}
			resp = mockResponse(201, `{"id":"private-id","token":"private-token","payload":{"content":"`+base64.StdEncoding.EncodeToString(jpegBytes)+`"}}`)
		default:
			t.Fatalf("unexpected upstream endpoint: %s %s", r.Method, r.URL.Path)
		}
		if change != nil {
			change(r, resp)
		}
		return resp, nil
	})}
	return client, calls
}

func TestBootstrapFlow(t *testing.T) {
	client, calls := fixtureClient(t, nil)
	result, err := bootstrap(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if result.MIMEType != "image/jpeg" || result.ImageBytes != len(jpegBytes) {
		t.Fatalf("invalid image metadata: %+v", result)
	}
	want := []string{"GET /account", "GET /bootstrap/portal", "GET /widget/account/", "POST /captcha"}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("unexpected requests: %v", *calls)
	}
	data, _ := json.Marshal(result)
	for _, secret := range []string{"private-token", "private-id", "fixture-session", "fixture-scnt", "fixture-key"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("response exposed %s", secret)
		}
	}
}

func TestWidgetResponseHeadersTakePrecedence(t *testing.T) {
	client, _ := fixtureClient(t, func(r *http.Request, resp *http.Response) {
		if r.URL.Path == "/widget/account/" {
			resp.Header.Set("scnt", "updated-scnt")
			resp.Header.Set("X-Apple-ID-Session-Id", "updated-session")
		}
		if r.URL.Path == "/captcha" && (r.Header.Get("scnt") != "updated-scnt" || r.Header.Get("X-Apple-ID-Session-Id") != "updated-session") {
			t.Error("stale widget session used")
		}
	})
	if _, err := bootstrap(context.Background(), client); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapRejectsInvalidResponses(t *testing.T) {
	cases := []struct {
		name, path, body string
		status           int
	}{
		{"portal error", "/account", "upstream-private-value", 500},
		{"invalid config", "/bootstrap/portal", `{}`, 200},
		{"untrusted api URL", "/bootstrap/portal", `{"apiUrl":"https://example.com","serviceKey":"x"}`, 200},
		{"missing boot_args", "/widget/account/", `<html></html>`, 200},
		{"invalid JSON", "/widget/account/", `<script id="boot_args">not-json</script>`, 200},
		{"missing session", "/widget/account/", `<script id="boot_args">{"direct":{}}</script>`, 200},
		{"captcha rejected", "/captcha", `upstream-private-value`, 429},
		{"invalid captcha JSON", "/captcha", `not-json`, 201},
		{"invalid base64", "/captcha", `{"payload":{"content":"?!"}}`, 201},
		{"missing image", "/captcha", `{"payload":{}}`, 201},
		{"non-image payload", "/captcha", `{"payload":{"content":"PHN2Zz48L3N2Zz4="}}`, 201},
		{"oversize response", "/widget/account/", strings.Repeat("x", maxBodyBytes+1), 200},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := fixtureClient(t, func(r *http.Request, resp *http.Response) {
				if r.URL.Path == tt.path {
					resp.StatusCode = tt.status
					resp.Body = io.NopCloser(strings.NewReader(tt.body))
				}
			})
			if result, err := bootstrap(context.Background(), client); err == nil || result != nil {
				t.Fatal("invalid response accepted")
			} else if strings.Contains(err.Error(), "upstream-private-value") {
				t.Fatal("upstream data leaked")
			}
		})
	}
}

func TestRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	if _, err := bootstrap(ctx, client); err == nil {
		t.Fatal("canceled request succeeded")
	}
}

func TestNetworkErrorRedacted(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret-proxy-password") })}
	_, err := bootstrap(context.Background(), client)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("network error was not redacted")
	}
}

func TestRedirectPolicy(t *testing.T) {
	for _, target := range []string{"http://account.apple.com/path", "https://example.com/path", "https://appleid.apple.com:8443/path", "https://user@appleid.apple.com/path"} {
		u, _ := url.Parse(target)
		if checkRedirect(&http.Request{URL: u}, nil) == nil {
			t.Errorf("accepted redirect: %s", target)
		}
	}
	u, _ := url.Parse("https://appleid.apple.com/path")
	if err := checkRedirect(&http.Request{URL: u}, nil); err != nil {
		t.Fatal(err)
	}
	if checkRedirect(&http.Request{URL: u}, make([]*http.Request, 5)) == nil {
		t.Fatal("unbounded redirects")
	}
}

// Opt-in integration probe: only public bootstrap GETs and one captcha POST.
func TestLiveBootstrap(t *testing.T) {
	if os.Getenv("APPLEID_LIVE_PROBE") != "1" {
		t.Skip("set APPLEID_LIVE_PROBE=1 to exercise the official image endpoint")
	}
	result, err := Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CAPTCHA_READY mime=%s bytes=%d account_writes=0", result.MIMEType, result.ImageBytes)
}
