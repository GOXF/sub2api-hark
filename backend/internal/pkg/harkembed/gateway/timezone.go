package gateway

import (
	"strings"
	"time"
	// 内嵌时区数据库：Windows 等没有系统 zoneinfo 的环境也能校验 IANA 时区。
	_ "time/tzdata"
)

// ValidTimezone 判断 name 是否为可加载的 IANA 时区（如 America/Los_Angeles、UTC）。
// 拒绝空串和 "Local"，因为它们不是可发给 Hark 的确定时区。
func ValidTimezone(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" || len(name) > 64 {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// zoneFor 返回为账号 a 新建 Project 或发消息时使用的时区：
// 优先账号自己的时区，没有或无效则用全局设置。
func (s *Server) zoneFor(a Account) string {
	if tz := strings.TrimSpace(a.Timezone); ValidTimezone(tz) {
		return tz
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings.Timezone
}
