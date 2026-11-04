package service

import (
	"net/url"
	"strings"
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
