package harkembed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed/gateway"
)

// UsageUpstream is the Hark site the quota is read from (tests point it at a
// local server).
var UsageUpstream = "https://hark.com"

// usageTimeout matches the hark2api gateway's JSON client timeout.
const usageTimeout = 20 * time.Second

// Usage is a Hark account's quota: Hark's billing meters (GET
// /api/billing/summary, meters.harkTokens), the same data the standalone
// hark2api admin panel shows as "额度：今日 used/limit · 本期 used/limit".
type Usage struct {
	Plan          string
	DailyUsed     int64
	DailyLimit    int64
	DailyResetsAt *time.Time
	PoolUsed      int64
	PoolLimit     int64
	PoolResetsAt  *time.Time
}

// UsageError is a failed quota query; StatusCode is Hark's HTTP status (0 for
// connection or response errors).
type UsageError struct {
	StatusCode int
	Message    string
}

func (e *UsageError) Error() string { return e.Message }

// QueryUsage reads the quota of the Hark account whose API key (bare login
// cookie or {"cookie","user_agent"} JSON) is apiKey, through proxyURL when set.
// It sends the account's browser User-Agent (gateway default otherwise), like
// the requests the in-process gateway makes for that account.
func QueryUsage(ctx context.Context, apiKey, proxyURL string) (*Usage, error) {
	cred := ParseCredential(apiKey)
	if cred.Cookie == "" {
		return nil, &UsageError{Message: "账号没有 Cookie"}
	}
	ua := cred.UserAgent
	if ua == "" {
		ua = gateway.DefaultUserAgent
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p := strings.TrimSpace(proxyURL); p != "" {
		u, err := url.Parse(p)
		if err != nil {
			return nil, &UsageError{Message: "代理地址无效"}
		}
		transport.Proxy = http.ProxyURL(u)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		Timeout:       usageTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	base := strings.TrimRight(UsageUpstream, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/billing/summary", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", cred.Cookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return nil, &UsageError{Message: "Hark 连接失败"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
		msg := fmt.Sprintf("Hark 返回 HTTP %d", resp.StatusCode)
		switch {
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode >= 300 && resp.StatusCode < 400:
			msg = "Hark 登录会话已失效，请重新导入 Cookie"
		case resp.StatusCode == http.StatusForbidden:
			msg = "Hark 返回 HTTP 403，访问受阻"
		case resp.StatusCode == http.StatusTooManyRequests:
			msg = "Hark 请求限流，请稍后再试"
		}
		return nil, &UsageError{StatusCode: resp.StatusCode, Message: msg}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(b) > 2*1024*1024 {
		return nil, &UsageError{StatusCode: resp.StatusCode, Message: "Hark 响应读取失败或内容过长"}
	}
	return parseUsage(b)
}

func parseUsage(b []byte) (*Usage, error) {
	var summary struct {
		Plan struct {
			Name string `json:"name"`
		} `json:"plan"`
		Meters struct {
			HarkTokens *struct {
				DailyLimit    *float64 `json:"dailyLimit"`
				DailyUsed     *float64 `json:"dailyUsed"`
				DailyResetsAt string   `json:"dailyResetsAt"`
				PoolLimit     *float64 `json:"poolLimit"`
				PoolUsed      *float64 `json:"poolUsed"`
				PoolResetsAt  string   `json:"poolResetsAt"`
			} `json:"harkTokens"`
		} `json:"meters"`
	}
	if json.Unmarshal(b, &summary) != nil {
		return nil, &UsageError{StatusCode: http.StatusOK, Message: "Hark 返回非预期 JSON 响应"}
	}
	m := summary.Meters.HarkTokens
	if m == nil || m.DailyLimit == nil || m.DailyUsed == nil || m.PoolLimit == nil || m.PoolUsed == nil {
		return nil, &UsageError{StatusCode: http.StatusOK, Message: "额度响应缺少 harkTokens 数据"}
	}
	return &Usage{
		Plan:          summary.Plan.Name,
		DailyUsed:     int64(*m.DailyUsed),
		DailyLimit:    int64(*m.DailyLimit),
		DailyResetsAt: parseUsageTime(m.DailyResetsAt),
		PoolUsed:      int64(*m.PoolUsed),
		PoolLimit:     int64(*m.PoolLimit),
		PoolResetsAt:  parseUsageTime(m.PoolResetsAt),
	}, nil
}

func parseUsageTime(v string) *time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}
