//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

type harkUsageEstimateCaptureUpstream struct{ disabled any }

func (u *harkUsageEstimateCaptureUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.disabled = req.Context().Value(ctxkey.HarkUsageEstimateDisabled)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
}

func (u *harkUsageEstimateCaptureUpstream) DoWithTLS(req *http.Request, p string, id int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, c)
}

func TestDoOpenAIUpstreamHarkUsageEstimateSetting(t *testing.T) {
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	settings := NewSettingService(repo, &config.Config{})
	t.Cleanup(func() { gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{}) })

	run := func(account *Account, value string) any {
		t.Helper()
		if value == "" {
			delete(repo.data, SettingKeyEnableHarkUsageEstimate)
		} else {
			repo.data[SettingKeyEnableHarkUsageEstimate] = value
		}
		gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
		up := &harkUsageEstimateCaptureUpstream{}
		s := &OpenAIGatewayService{httpUpstream: up, settingService: settings}
		req := httptest.NewRequest(http.MethodPost, "http://hark.internal/v1/chat/completions", nil)
		resp, err := s.doOpenAIUpstream(req, "", account)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return up.disabled
	}

	hark := &Account{ID: 1, Platform: PlatformHark}
	openai := &Account{ID: 2, Platform: PlatformOpenAI}
	if got := run(hark, ""); got != nil {
		t.Fatalf("default: estimate must stay on, got %v", got)
	}
	if got := run(hark, "true"); got != nil {
		t.Fatalf("enabled: got %v", got)
	}
	if got := run(hark, "false"); got != true {
		t.Fatalf("disabled: Hark request must carry the off flag, got %v", got)
	}
	if got := run(openai, "false"); got != nil {
		t.Fatalf("OpenAI account must be untouched, got %v", got)
	}
}

func TestWithHarkUsageEstimateWithoutSettings(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://hark.internal/v1/responses", nil)
	if out := withHarkUsageEstimate(req, nil); out != req {
		t.Fatal("nil settings must leave the request unchanged (estimate on)")
	}
	if withHarkUsageEstimate(nil, nil) != nil {
		t.Fatal("nil request")
	}
}

func TestParseSettingsHarkUsageEstimateDefault(t *testing.T) {
	// parseSettings 会发布 Grok 全局模型映射选项，测试结束后还原，避免影响其他测试。
	original := xai.RuntimeModelMappingOptions()
	t.Cleanup(func() { xai.SetRuntimeModelMappingOptions(original) })
	s := NewSettingService(&gatewayTTLSettingRepo{data: map[string]string{}}, &config.Config{})
	if !s.parseSettings(map[string]string{}).EnableHarkUsageEstimate {
		t.Fatal("default must be true")
	}
	if s.parseSettings(map[string]string{SettingKeyEnableHarkUsageEstimate: "false"}).EnableHarkUsageEstimate {
		t.Fatal("false must turn it off")
	}
}
