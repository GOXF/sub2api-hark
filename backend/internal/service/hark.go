package service

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// Hark（hark.com）账号：平台 hark、类型 API Key，API Key 填网页登录 Cookie
// （或 {"cookie":"...","user_agent":"..."}），base_url 默认为进程内网关
// DefaultHarkBaseURL，请求不出网，由 internal/pkg/harkembed 内嵌的 hark2api
// 处理并按账号代理访问 hark.com。调度、冷却、换号都由 sub2api 负责：Cookie
// 失效时上游返回 401，被 Cloudflare 拦截时返回 403。

// DefaultHarkTestModel 是 Hark 账号连接测试的模型（hark2api 只有这一个模型）。
const DefaultHarkTestModel = "hark"

// IsHark 报告账号是否为 Hark 平台账号。
func (a *Account) IsHark() bool {
	return a != nil && a.Platform == PlatformHark
}

// isHarkInternalURL 报告 URL 是否指向进程内 Hark 网关（http(s)://hark.internal）。
func isHarkInternalURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Hostname(), "hark.internal") && u.User == nil
}

// harkClientRequestedModel 返回客户端原始请求的模型名（账号模型映射之前）：handler
// 解析请求体后写入的 ctxkey.Model，缺失时取 composite 分组记录的公开模型名。
func harkClientRequestedModel(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	for _, key := range []ctxkey.Key{ctxkey.Model, ctxkey.RequestedPublicModel} {
		if m, ok := ctx.Value(key).(string); ok {
			if m = strings.TrimSpace(m); m != "" {
				return m
			}
		}
	}
	return ""
}

// withHarkRequestedModel 把客户端原始模型名（ctxkey.HarkRequestedModel）与后台
// 设置的说明模板（ctxkey.HarkModelLabelPrompt）挂到发往 Hark 账号的上游请求上，
// harkembed 据此注入如实的模型身份说明。只用于 Hark 账号；后台关闭
// enable_hark_model_label、已设置或取不到原始模型时原样返回。
func withHarkRequestedModel(req *http.Request, settings *SettingService) *http.Request {
	if req == nil {
		return req
	}
	ctx := req.Context()
	if m, ok := ctx.Value(ctxkey.HarkRequestedModel).(string); ok && m != "" {
		return req
	}
	model := harkClientRequestedModel(ctx)
	if model == "" {
		return req
	}
	enabled, prompt := true, ""
	if settings != nil {
		enabled, prompt = settings.GetHarkModelLabelSettings(ctx)
	}
	if !enabled {
		return req
	}
	ctx = context.WithValue(ctx, ctxkey.HarkRequestedModel, model)
	if strings.TrimSpace(prompt) != "" {
		ctx = context.WithValue(ctx, ctxkey.HarkModelLabelPrompt, prompt)
	}
	return req.WithContext(ctx)
}

// withHarkUsageEstimate 在后台关闭 enable_hark_usage_estimate 时给发往 Hark 账号的
// 上游请求挂上 ctxkey.HarkUsageEstimateDisabled，harkembed 据此不估算 Token；
// 默认开启时原样返回（Hark 上游不返回用量，harkembed 只替换为零的用量）。
func withHarkUsageEstimate(req *http.Request, settings *SettingService) *http.Request {
	if req == nil || settings == nil || settings.GetHarkUsageEstimateEnabled(req.Context()) {
		return req
	}
	return req.WithContext(context.WithValue(req.Context(), ctxkey.HarkUsageEstimateDisabled, true))
}
