package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Evidence, not a retry/clear instruction. Older intents without this record
// remain unknown; never infer their original HTTP response from accepted=false.
type submissionFailure struct {
	Outcome        string `json:"outcome"` // not_sent, rejected, uncertain
	Phase          string `json:"phase"`
	Code           string `json:"code"`
	Status         int    `json:"status"`
	NativeStatus   int    `json:"native_http_status,omitempty"`
	Message        string `json:"message"`
	ResponseHash   string `json:"response_prefix_sha256,omitempty"`
	ResponseBytes  int    `json:"captured_response_bytes,omitempty"`
	ResponseCut    bool   `json:"response_truncated,omitempty"`
	ResponseDetail string `json:"response_excerpt,omitempty"`
	TransportError string `json:"transport_error,omitempty"`
}

type nativeSubmissionError struct {
	cause   error
	failure submissionFailure
}

func (e *nativeSubmissionError) Error() string { return e.cause.Error() }
func (e *nativeSubmissionError) Unwrap() error { return e.cause }

func isNativeSubmission(method, path string) bool {
	return method == "POST" && (path == "/api/messages/send" || strings.HasPrefix(path, "/api/messages/send?"))
}

func explicitSubmissionRejection(status int, body []byte) bool {
	if len(body) > 65536 || !slices.Contains([]int{400, 401, 403, 413, 415, 422, 429}, status) {
		return false
	}
	var v map[string]json.RawMessage
	if json.Unmarshal(body, &v) != nil || v == nil {
		return false // HTML/proxy errors are not a native rejection proof.
	}
	for _, key := range []string{"success", "accepted", "redirected"} {
		if strings.TrimSpace(string(v[key])) == "true" {
			return false // Contradictory acceptance evidence must stay uncertain.
		}
	}
	var text string
	if json.Unmarshal(v["error"], &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	var detail struct{ Code, Message string }
	return json.Unmarshal(v["error"], &detail) == nil && (detail.Code != "" || detail.Message != "")
}

var submissionSecret = regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]+|("(?:cookie|authorization|api_key|password|token)"\s*:\s*)"(?:[^"\\]|\\.)*"`)
var submissionURLSecret = regexp.MustCompile(`(?i)(https?|socks5h?)://[^/\s@]+@`)

func safeSubmissionText(text, cookie string, limit int) string {
	if cookie != "" {
		text = strings.ReplaceAll(text, cookie, "<REDACTED>")
		for _, item := range strings.Split(cookie, ";") {
			_, value, ok := strings.Cut(item, "=")
			if value = strings.TrimSpace(value); ok && len(value) >= 6 {
				text = strings.ReplaceAll(text, value, "<REDACTED>")
			}
		}
	}
	text = submissionSecret.ReplaceAllString(text, `${1}${2}"<REDACTED>"`)
	text = submissionURLSecret.ReplaceAllString(text, `$1://<REDACTED>@`)
	if len(text) > limit {
		text = text[:limit]
	}
	return strings.ToValidUTF8(text, "")
}

func nativeSubmissionFailure(err error, phase string, status int, body []byte, cookie string) error {
	if err == nil {
		return nil
	}
	f := submissionFailure{Outcome: "uncertain", Phase: phase, Status: 502, NativeStatus: status, Code: "upstream_error"}
	var original *upstreamError
	if errors.As(err, &original) {
		f.Code, f.Status = original.code, original.status
	}
	if phase == "prepare" {
		f.Outcome = "not_sent"
	} else if phase == "response" && explicitSubmissionRejection(status, body) {
		f.Outcome = "rejected"
	}
	f.Message = safeSubmissionText(err.Error(), cookie, 8192)
	if len(body) > 0 {
		f.ResponseCut = len(body) > 65536
		if f.ResponseCut {
			body = body[:65536]
		}
		f.ResponseBytes, f.ResponseHash = len(body), previewDigest(body)
		f.ResponseDetail = safeSubmissionText(string(body), cookie, 8192)
	}
	return &nativeSubmissionError{cause: err, failure: f}
}

func (f *submissionFailure) responseError() error {
	status, code := 409, "recovery_submission_uncertain"
	if f.Outcome == "rejected" {
		status, code = f.Status, "recovery_submission_rejected"
	} else if f.Outcome == "not_sent" {
		status, code = f.Status, "recovery_submission_not_sent"
	}
	return &upstreamError{status, code, fmt.Sprintf("提交状态=%s；原错误 %s（原状态%d，上游HTTP%d，阶段%s）：%s；未重试，pending保留", f.Outcome, f.Code, f.Status, f.NativeStatus, f.Phase, f.Message)}
}

func (s *Server) retainSubmissionFailure(p *projectSession, err error) error {
	var native *nativeSubmissionError
	f := submissionFailure{Outcome: "uncertain", Phase: "acceptance", Code: "upstream_error", Status: 502, Message: safeSubmissionText(err.Error(), "", 8192)}
	if errors.As(err, &native) {
		f = native.failure
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	next := *p
	pending := *p.PendingTurn
	pending.Failure = &f
	next.PendingTurn = &pending
	if saveErr := s.commitRecoveryLocked(p, next); saveErr != nil {
		return &upstreamError{503, "recovery_error_record_failed", fmt.Sprintf("原错误 %s：%s；失败证据持久化未成功，原pending保留，未重试", f.Code, f.Message)}
	}
	return f.responseError()
}

func validSubmissionFailure(f *submissionFailure) bool {
	return f == nil || (slices.Contains([]string{"not_sent", "rejected", "uncertain"}, f.Outcome) && f.Code != "" && f.Phase != "" &&
		f.Status >= 400 && f.Status <= 599 && f.NativeStatus >= 0 && f.NativeStatus <= 599 &&
		len(f.Message) <= 8192 && len(f.ResponseDetail) <= 8192 && f.ResponseBytes >= 0 && f.ResponseBytes <= 65536 &&
		len(f.Code) <= 128 && len(f.Phase) <= 64 && len(f.TransportError) <= 8192 &&
		(f.ResponseHash == "" || validPreviewSHA(f.ResponseHash)))
}
