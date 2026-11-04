package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed"
)

// HarkUsageMeter 是 Hark 的一个额度计量（今日 / 本期）。
type HarkUsageMeter struct {
	Used             int64      `json:"used"`
	Limit            int64      `json:"limit"`
	Utilization      float64    `json:"utilization"` // 使用率百分比（0-100+）
	ResetsAt         *time.Time `json:"resets_at"`
	RemainingSeconds int        `json:"remaining_seconds"`
}

// HarkUsageInfo 是 Hark 账号的额度（Hark /api/billing/summary 的 meters.harkTokens）。
type HarkUsageInfo struct {
	Plan  string          `json:"plan,omitempty"`
	Daily *HarkUsageMeter `json:"daily,omitempty"`
	Pool  *HarkUsageMeter `json:"pool,omitempty"`
}

const (
	harkUsageCacheTTL      = 2 * time.Minute // 与 hark2api 自动刷新额度的间隔一致
	harkUsageErrorCacheTTL = 1 * time.Minute
)

type harkUsageCacheEntry struct {
	usage *UsageInfo
	at    time.Time
	ttl   time.Duration
}

var (
	harkUsageCache sync.Map // accountID -> *harkUsageCacheEntry
	// harkUsageQuery 便于测试替换。
	harkUsageQuery = harkembed.QueryUsage
)

// getHarkUsage 通过 Hark 账号保存的 Cookie / User-Agent / 代理查询额度。查询失败时
// 降级返回带 Error 的 UsageInfo（不改动账号状态，调度仍由网关请求的结果驱动）。
func (s *AccountUsageService) getHarkUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	if !force {
		if v, ok := harkUsageCache.Load(account.ID); ok {
			if e, ok := v.(*harkUsageCacheEntry); ok && time.Since(e.at) < e.ttl {
				return e.usage, nil
			}
		}
	}
	query := func() (any, error) {
		proxyURL := ""
		if account.ProxyID != nil && account.Proxy != nil {
			proxyURL = account.Proxy.URL()
		}
		now := time.Now()
		u, err := harkUsageQuery(ctx, account.GetCredential("api_key"), proxyURL)
		usage, ttl := buildHarkUsageInfo(u, err, now), harkUsageCacheTTL
		if err != nil {
			ttl = harkUsageErrorCacheTTL
		}
		harkUsageCache.Store(account.ID, &harkUsageCacheEntry{usage: usage, at: now, ttl: ttl})
		return usage, nil
	}
	var v any
	if s.cache != nil {
		v, _, _ = s.cache.apiFlight.Do(fmt.Sprintf("hark-usage:%d", account.ID), query)
	} else {
		v, _ = query()
	}
	usage, _ := v.(*UsageInfo)
	if usage == nil {
		return &UsageInfo{Error: "Hark 额度查询失败"}, nil
	}
	return usage, nil
}

func buildHarkUsageInfo(u *harkembed.Usage, err error, now time.Time) *UsageInfo {
	updated := now
	info := &UsageInfo{Source: "active", UpdatedAt: &updated}
	if err != nil {
		info.Error = err.Error()
		var ue *harkembed.UsageError
		if errors.As(err, &ue) {
			switch {
			case ue.StatusCode == http.StatusUnauthorized, ue.StatusCode >= 300 && ue.StatusCode < 400:
				info.ErrorCode, info.NeedsReauth = "unauthenticated", true
			case ue.StatusCode == http.StatusForbidden:
				info.ErrorCode = "forbidden"
			case ue.StatusCode == http.StatusTooManyRequests:
				info.ErrorCode = "rate_limited"
			case ue.StatusCode == 0:
				info.ErrorCode = "network_error"
			}
		}
		return info
	}
	if u == nil {
		info.Error = "Hark 额度查询失败"
		return info
	}
	info.Hark = &HarkUsageInfo{
		Plan:  u.Plan,
		Daily: newHarkUsageMeter(u.DailyUsed, u.DailyLimit, u.DailyResetsAt, now),
		Pool:  newHarkUsageMeter(u.PoolUsed, u.PoolLimit, u.PoolResetsAt, now),
	}
	return info
}

func newHarkUsageMeter(used, limit int64, resetsAt *time.Time, now time.Time) *HarkUsageMeter {
	m := &HarkUsageMeter{Used: used, Limit: limit, ResetsAt: resetsAt}
	if limit > 0 {
		m.Utilization = math.Round(float64(used)*10000/float64(limit)) / 100
	}
	if resetsAt != nil {
		if remaining := int(resetsAt.Sub(now).Seconds()); remaining > 0 {
			m.RemainingSeconds = remaining
		}
	}
	return m
}
