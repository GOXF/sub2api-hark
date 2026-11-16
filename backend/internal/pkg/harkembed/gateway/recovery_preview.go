package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// recoveryPreviewProof must be supplied by a trusted in-process test host,
// using a previously captured client request and an explicitly reviewed native
// result. It is not a network header, API-key scope, or acknowledgment token.
type recoveryPreviewProof struct {
	AccountID         string `json:"account_id"`
	SessionID         string `json:"session_id"`
	ProjectID         string `json:"project_id"`
	RequestID         string `json:"request_id"`
	ResponseMessageID string `json:"response_message_id"`
	Path              string `json:"path"`
	RequestSHA256     string `json:"request_sha256"`
	ResultSHA256      string `json:"result_sha256"`
}

// RecoveryPreviewHandler is an isolated feasibility seam. Deliberately NOT
// registered in Handler, EmbeddedHandler or the sub2api admin/model routes.
// It only renders a reviewed saved result into an existing client wire format;
// it never clears pending, installs context, submits a turn or executes a tool.
// A real host arm/ownership/acknowledgment protocol remains out of scope.
func (s *Server) RecoveryPreviewHandler(raw json.RawMessage) http.Handler {
	var proof recoveryPreviewProof
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&proof)
	valid := err == nil && decoder.Decode(&struct{}{}) == io.EOF &&
		validResultID(proof.AccountID) && validResultID(proof.SessionID) &&
		validResultID(proof.ProjectID) &&
		validResultID(proof.RequestID) && validResultID(proof.ResponseMessageID) &&
		validPreviewSHA(proof.RequestSHA256) && validPreviewSHA(proof.ResultSHA256) &&
		slices.Contains([]string{"/v1/chat/completions", "/v1/responses", "/v1/messages"}, proof.Path)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		a, ok := embeddedAccount(r.Context())
		if !ok || s.embed == nil || a.ID != proof.AccountID {
			fail(w, 403, "recovery_preview_owner_mismatch", "预演需要明确绑定原账号；未换号或重发")
			return
		}
		if !valid || r.Method != http.MethodPost || r.URL.Path != proof.Path || r.URL.RawQuery != "" {
			fail(w, 400, "recovery_preview_invalid_proof", "预演凭据或协议入口不匹配")
			return
		}
		body, readErr := io.ReadAll(io.LimitReader(r.Body, s.maxChatBody()+1))
		if readErr != nil || int64(len(body)) > s.maxChatBody() || previewDigest(body) != proof.RequestSHA256 {
			fail(w, 409, "recovery_preview_request_mismatch", "客户端原请求或工具协议已变化；未安装结果")
			return
		}
		var v chatRequest
		var format outputFormat
		var parseErr error
		switch proof.Path {
		case "/v1/responses":
			v, parseErr = s.normalizeResponses(body)
			format = newResponsesFormat()
		case "/v1/messages":
			var model string
			v, model, parseErr = s.normalizeMessages(body)
			format = newAnthropicFormat(model)
		default:
			v, parseErr = s.normalizeChat(body)
			format = newChatFormat()
		}
		if parseErr == nil {
			parseErr = applySessionHeaders(r, &v)
		}
		if parseErr != nil || v.SessionID != proof.SessionID || v.SessionEnd || v.Bridge || isCheckpointCompaction(v) || s.forwardExtra {
			fail(w, 409, "recovery_preview_context_mismatch", "预演只接受原会话的原执行协议，不接受归档、摘要或额外转发")
			return
		}
		if toolErr := validateClientToolRequest(v, false); toolErr != nil {
			status, code, message := turnError(r.Context(), toolErr)
			fail(w, status, code, message)
			return
		}
		// The exact client request digest covers tools and their schemas; saved
		// client fingerprints independently bind its history to this Project.
		s.projectMu.Lock()
		p := s.projects[projectMapping(r, proof.SessionID)]
		historyMatches := false
		if p != nil && p.AccountID == proof.AccountID && p.ConversationID == proof.ProjectID {
			sent := p.Sent
			if p.LabelHistoryVersion == 2 {
				sent = p.ClientSent
			}
			historyMatches = len(sent) > 0 && slices.Equal(sent, hashes(v.Messages))
		}
		s.projectMu.Unlock()
		if !historyMatches {
			fail(w, 409, "recovery_preview_history_mismatch", "原客户端历史无法与已受理游标精确对应；未修改 Project")
			return
		}
		lookup := r.Clone(r.Context())
		lookup.Method = http.MethodGet
		u := *r.URL
		lookup.URL = &u
		lookup.SetPathValue("id", proof.SessionID)
		lookup.URL.RawQuery = url.Values{"request_id": {proof.RequestID}, "response_message_id": {proof.ResponseMessageID}}.Encode()
		result, resultErr := s.readSessionResult(lookup, proof.ProjectID)
		if resultErr != nil {
			status, code, message := turnError(r.Context(), resultErr)
			fail(w, status, code, message)
			return
		}
		res := &turnResult{Reused: true, Mode: "recovery-preview", Isolation: "key-session", Text: result.Result.Text, Outcome: "completed"}
		if result.Status == "declined" {
			res.Outcome = "declined"
			status, code, message := turnError(r.Context(), declinedTurnFailure(false, res))
			fail(w, status, code, message)
			return
		}
		if result.Status == "failed" {
			status := http.StatusBadGateway
			if result.ErrorCode == "upstream_budget_exhausted" {
				status = http.StatusTooManyRequests
			}
			fail(w, status, result.ErrorCode, result.ErrorMessage)
			return
		}
		if result.Status != "completed" && result.Status != "awaiting_client_tool" || len(result.Result.Attachments) > 0 {
			fail(w, 409, "recovery_preview_not_ready", "原结果未完整完成或包含未验收附件；未应用续接")
			return
		}
		if previewDigest([]byte(res.Text)) != proof.ResultSHA256 {
			fail(w, 409, "recovery_preview_result_mismatch", "上游结果与明确审阅的原文不同；未输出工具调用")
			return
		}
		s.applyToolCalls(v, res)
		if strings.Contains(res.Text, "<tool_call") {
			fail(w, 422, "recovery_preview_tool_mismatch", "原工具调用不能被当前工具协议完整解析；未执行")
			return
		}
		if toolErr := validateClientToolResult(v, res.ToolCalls); toolErr != nil {
			status, code, message := turnError(r.Context(), toolErr)
			fail(w, status, code, message)
			return
		}
		for i := range res.ToolCalls {
			call := &res.ToolCalls[i]
			call.ID = "call_" + previewDigest([]byte(proof.AccountID + "\x00" + proof.SessionID + "\x00" + proof.ResponseMessageID + "\x00" + proof.RequestSHA256 + fmt.Sprintf("\x00%d", i)))[:24]
		}
		setTurnHeaders(w, res)
		w.Header().Set("X-Hark-Recovery-Preview", "true")
		w.Header().Set("X-Hark-Continuation-Applied", "false")
		if !v.Stream {
			write(w, 200, format.json(res))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		emit := func(event string, value any) {
			b, _ := json.Marshal(value)
			if event != "" {
				_, _ = fmt.Fprintf(w, "event: %s\n", event)
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		format.start(emit)
		if res.Text != "" {
			format.delta(emit, res.Text)
		}
		format.finish(emit, res.Text, res.ToolCalls)
		if format.done() {
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
}

func previewDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validPreviewSHA(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && strings.ToLower(value) == value
}
