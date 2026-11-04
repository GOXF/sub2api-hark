package gateway

import (
	"context"
	"strings"
)

// createProject gives a session a new Project, always created directly with
// POST /api/conversations. A 403 here is almost always the egress IP being
// blocked by Cloudflare in front of Hark (not Hark refusing direct creation),
// so the error is returned as is, with a hint to switch routes; the account
// failover in the caller still applies.
//
// Older versions asked Hark's agent to create Projects when the direct call
// was refused and kept a "creator" Project plus spare Projects per account in
// agent-projects.json. That file is no longer read; the Projects it lists stay
// in the account untouched.
func (s *Server) createProject(ctx context.Context, cookie, zone string) (string, error) {
	cid, err := s.createProjectDirect(ctx, cookie, zone)
	if e, ok := err.(*upstreamError); ok && e.code == "upstream_access_blocked" && !strings.Contains(e.message, "Cloudflare") {
		e.message += "；新建 Project 被拒，出口 IP 可能被 Cloudflare 拦截，请换线路（代理）后重试"
	}
	return cid, err
}
