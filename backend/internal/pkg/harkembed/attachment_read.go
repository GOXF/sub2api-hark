package harkembed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type attachmentBillingBodyKey struct{}

// PreflightAttachmentRead must run before model scheduling and per-request billing.
// The origin is administrator configuration, never an inbound header.
func PreflightAttachmentRead(w http.ResponseWriter, r *http.Request, origin string) bool {
	s, err := get()
	if err != nil {
		http.Error(w, "Attachment intake unavailable", http.StatusServiceUnavailable)
		return true
	}
	keyID, _ := r.Context().Value(ctxkey.APIKeyID).(int64)
	if keyID <= 0 {
		return false
	}
	handled, billingBody := s.PreflightAttachmentRead(w, r, fmt.Sprintf("sub2api-key-%d", keyID), origin)
	if !handled && billingBody != nil {
		*r = *r.WithContext(context.WithValue(r.Context(), attachmentBillingBodyKey{}, billingBody))
	}
	return handled
}

// AttachmentUploadHandler is a write-only, expiring capability endpoint, not an API-key endpoint.
func AttachmentUploadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := get()
		if err != nil {
			http.Error(w, "Attachment intake unavailable", http.StatusServiceUnavailable)
			return
		}
		s.AttachmentUploadHandler().ServeHTTP(w, r)
	})
}
