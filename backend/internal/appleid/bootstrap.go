// Package appleid implements the independent Apple ID workbench.
// It only initializes the public signup widget and retrieves an image challenge.
package appleid

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	portalURL      = "https://account.apple.com/account#!&page=create"
	bootstrapURL   = "https://account.apple.com/bootstrap/portal"
	widgetURL      = "https://appleid.apple.com/widget/account/"
	captchaURL     = "https://appleid.apple.com/captcha"
	maxBodyBytes   = 4 << 20
	requestTimeout = 25 * time.Second
	userAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
)

// Challenge deliberately excludes Apple's session headers, challenge ID and token.
// This is a readiness probe, not an account-creation or verification endpoint.
type Challenge struct {
	ImageBase64 string `json:"image_base64"`
	MIMEType    string `json:"mime_type"`
	ImageBytes  int    `json:"image_bytes"`
}

// Bootstrap creates an isolated cookie jar for each explicit workbench request.
// Nothing is persisted, and no email, phone or account write is performed.
func Bootstrap(ctx context.Context) (*Challenge, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, errors.New("appleid: session initialization failed")
	}
	client := &http.Client{
		Jar:           jar,
		Timeout:       requestTimeout,
		CheckRedirect: checkRedirect,
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return bootstrap(ctx, client)
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Port() != "" {
		return errors.New("appleid: unexpected redirect")
	}
	switch req.URL.Hostname() {
	case "account.apple.com", "appleid.apple.com":
		return nil
	default:
		return errors.New("appleid: unexpected redirect")
	}
}

func bootstrap(ctx context.Context, client *http.Client) (*Challenge, error) {
	// Keep HTML and JSON Accept headers separate: the portal rejects JSON Accept.
	if _, _, err := request(ctx, client, http.MethodGet, portalURL,
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", nil, nil); err != nil {
		return nil, errors.New("appleid: portal request failed")
	}
	body, _, err := request(ctx, client, http.MethodGet, bootstrapURL, "application/json", nil, nil)
	if err != nil {
		return nil, errors.New("appleid: bootstrap request failed")
	}
	var config struct {
		APIURL     string `json:"apiUrl"`
		ServiceKey string `json:"serviceKey"`
	}
	if json.Unmarshal(body, &config) != nil || config.ServiceKey == "" || config.APIURL != "https://appleid.apple.com" {
		return nil, errors.New("appleid: invalid bootstrap configuration")
	}
	params := url.Values{
		"roleType": {"Agent"}, "lv": {"0.4.3"}, "widgetKey": {config.ServiceKey},
		"v": {"3"}, "appContext": {"account"},
	}
	widget := widgetURL + "?" + params.Encode()
	body, headers, err := request(ctx, client, http.MethodGet, widget, "text/html", nil, nil)
	if err != nil {
		return nil, errors.New("appleid: widget request failed")
	}
	direct, err := parseBootArgs(body)
	if err != nil {
		return nil, err
	}
	scnt := firstNonempty(headers.Get("scnt"), direct.SCNT)
	sessionID := firstNonempty(headers.Get("X-Apple-ID-Session-Id"), direct.SessionID)
	widgetKey := firstNonempty(direct.WidgetKey, config.ServiceKey)
	if scnt == "" || sessionID == "" {
		return nil, errors.New("appleid: incomplete widget session")
	}
	captchaHeaders := http.Header{
		"scnt": {scnt}, "X-Apple-ID-Session-Id": {sessionID}, "X-Apple-Widget-Key": {widgetKey},
		"Origin": {"https://appleid.apple.com"}, "Referer": {widget},
		"Content-Type": {"application/json"},
	}
	if direct.APIKey != "" {
		captchaHeaders.Set("X-Apple-Api-Key", direct.APIKey)
	}
	body, _, err = request(ctx, client, http.MethodPost, captchaURL, "application/json",
		strings.NewReader(`{"type":"IMAGE"}`), captchaHeaders)
	if err != nil {
		return nil, errors.New("appleid: captcha request failed")
	}
	var captcha struct {
		Payload struct {
			Content string `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(body, &captcha) != nil {
		return nil, errors.New("appleid: invalid captcha response")
	}
	image, err := base64.StdEncoding.DecodeString(captcha.Payload.Content)
	if err != nil || len(image) == 0 || len(image) > 1<<20 {
		return nil, errors.New("appleid: invalid captcha image")
	}
	mime := http.DetectContentType(image)
	if mime != "image/jpeg" && mime != "image/png" {
		return nil, errors.New("appleid: unsupported captcha image")
	}
	return &Challenge{ImageBase64: base64.StdEncoding.EncodeToString(image), MIMEType: mime, ImageBytes: len(image)}, nil
}

type bootDirect struct {
	SCNT      string `json:"scnt"`
	SessionID string `json:"sessionId"`
	WidgetKey string `json:"widgetKey"`
	APIKey    string `json:"apiKey"`
}

func parseBootArgs(body []byte) (bootDirect, error) {
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return bootDirect{}, errors.New("appleid: widget boot_args missing")
		case html.StartTagToken:
			tag := tokenizer.Token()
			if tag.Data != "script" {
				continue
			}
			for _, attr := range tag.Attr {
				if attr.Key != "id" || attr.Val != "boot_args" {
					continue
				}
				if tokenizer.Next() != html.TextToken {
					break
				}
				var args struct {
					Direct bootDirect `json:"direct"`
				}
				if json.Unmarshal(tokenizer.Text(), &args) != nil {
					return bootDirect{}, errors.New("appleid: invalid widget boot_args")
				}
				return args.Direct, nil
			}
		}
	}
}

func request(ctx context.Context, client *http.Client, method, target, accept string, body io.Reader, headers http.Header) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, errors.New("appleid: invalid request")
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, errors.New("appleid: upstream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, errors.New("appleid: upstream rejected request")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil || len(data) > maxBodyBytes {
		return nil, nil, errors.New("appleid: invalid upstream body")
	}
	return data, resp.Header, nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
