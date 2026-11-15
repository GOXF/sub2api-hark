package gateway

import (
	"context"
	"net/http"
	"strings"
)

type modelLabelKey struct{}

type modelLabelContext struct {
	note       string
	legacyBody []byte
}

// WithModelLabel carries gateway-owned instructions separately from client
// history. legacyBody is used only to recognize a pre-upgrade history prefix;
// it is never sent, installed as a summary, or accepted from an HTTP header.
func WithModelLabel(ctx context.Context, note string, legacyBody []byte) context.Context {
	return context.WithValue(ctx, modelLabelKey{}, modelLabelContext{note: note, legacyBody: legacyBody})
}

func (s *Server) planModelLabelTurn(r *http.Request, p *projectSession, v chatRequest) (turnPlan, error) {
	plan := planTurn(p, v.Messages)
	label, owned := r.Context().Value(modelLabelKey{}).(modelLabelContext)
	if !owned || !plan.reset || p.LabelHistoryVersion != 0 {
		return plan, nil
	}
	// Older releases hashed the injected system note. Only an exact legacy
	// prefix proves the same client history; never ignore arbitrary system edits.
	var legacy chatRequest
	var err error
	switch {
	case strings.HasSuffix(r.URL.Path, "/responses"):
		legacy, err = s.normalizeResponses(label.legacyBody)
	case strings.HasSuffix(r.URL.Path, "/messages"):
		legacy, _, err = s.normalizeMessages(label.legacyBody)
	default:
		legacy, err = s.normalizeChat(label.legacyBody)
	}
	if err == nil {
		old := planTurn(p, legacy.Messages)
		if !old.reset && old.mode == "tail" {
			plan.send, plan.reset, plan.mode, plan.reason = old.send, false, "tail", ""
			plan.systems = systemHashes(v.Messages, nil)
			return plan, nil
		}
	}
	// A changed custom template cannot be recovered from a one-way old hash.
	// Preserve the Project rather than archiving it or guessing lost history.
	return turnPlan{}, &upstreamError{http.StatusConflict, "session_label_history_mismatch", "无法核验旧会话的身份说明历史；原 Project 和上下文已保留，未归档、未重放。请核对原模板后显式继续"}
}
