//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/harkembed"
	"github.com/stretchr/testify/require"
)

func TestBuildHarkUsageInfo(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reset := now.Add(90 * time.Minute)
	info := buildHarkUsageInfo(&harkembed.Usage{Plan: "Pro", DailyUsed: 250, DailyLimit: 1000, DailyResetsAt: &reset, PoolUsed: 1, PoolLimit: 3}, nil, now)
	require.Empty(t, info.Error)
	require.NotNil(t, info.Hark)
	require.Equal(t, "Pro", info.Hark.Plan)
	require.Equal(t, 25.0, info.Hark.Daily.Utilization)
	require.Equal(t, 5400, info.Hark.Daily.RemainingSeconds)
	require.Equal(t, 33.33, info.Hark.Pool.Utilization)
	require.Nil(t, info.Hark.Pool.ResetsAt)
	require.Nil(t, info.FiveHour)

	info = buildHarkUsageInfo(nil, &harkembed.UsageError{StatusCode: 401, Message: "expired"}, now)
	require.Equal(t, "expired", info.Error)
	require.True(t, info.NeedsReauth)
	require.Equal(t, "unauthenticated", info.ErrorCode)
	require.Nil(t, info.Hark)

	info = buildHarkUsageInfo(nil, &harkembed.UsageError{StatusCode: 429, Message: "slow"}, now)
	require.Equal(t, "rate_limited", info.ErrorCode)
}

func TestGetUsageForHarkAccountUsesStoredCredentialAndProxy(t *testing.T) {
	oldQuery := harkUsageQuery
	t.Cleanup(func() { harkUsageQuery = oldQuery; harkUsageCache.Delete(int64(990001)) })
	calls := 0
	harkUsageQuery = func(_ context.Context, apiKey, proxyURL string) (*harkembed.Usage, error) {
		calls++
		require.Equal(t, `{"cookie":"a=1","user_agent":"UA"}`, apiKey)
		require.Equal(t, "http://127.0.0.1:8080", proxyURL)
		return &harkembed.Usage{DailyUsed: 1, DailyLimit: 10, PoolUsed: 2, PoolLimit: 20}, nil
	}
	proxyID := int64(7)
	account := &Account{
		ID:          990001,
		Platform:    PlatformHark,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": `{"cookie":"a=1","user_agent":"UA"}`},
		ProxyID:     &proxyID,
		Proxy:       &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 8080},
	}
	svc := &AccountUsageService{cache: NewUsageCache()}
	usage, err := svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, usage.Hark)
	require.Equal(t, 10.0, usage.Hark.Daily.Utilization)

	_, err = svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "second call within TTL is served from cache")

	_, err = svc.GetUsageForAccount(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "force bypasses the cache")
}
