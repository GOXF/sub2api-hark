package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type recoveryCallerKey struct{}

// CallerHandler binds an authenticated host-owned caller identity. Never
// expose owner selection as an HTTP header or derive it from the upstream cookie.
func (s *Server) CallerHandler(owner string) http.Handler {
	h := s.EmbeddedHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if validResultID(owner) {
			r = r.WithContext(context.WithValue(r.Context(), recoveryCallerKey{}, owner))
		}
		h.ServeHTTP(w, r)
	})
}
func recoveryOwner(r *http.Request) string {
	value, _ := r.Context().Value(recoveryCallerKey{}).(string)
	return value
}

type recoveryContract struct {
	Path        string     `json:"path"`
	RequestHash string     `json:"request_sha256"`
	ToolHash    string     `json:"tool_sha256"`
	History     []string   `json:"history"`
	Tools       []toolSpec `json:"tools"`
	Choice      string     `json:"choice"`
	Named       bool       `json:"named"`
	Stream      bool       `json:"stream"`
	Model       string     `json:"model"`
}

type recoveryDelivery struct {
	ID          string           `json:"id"`
	State       string           `json:"state"` // ready -> issued -> continuing/acknowledged
	OwnerID     string           `json:"owner_id"`
	ProjectID   string           `json:"project_id"`
	RequestID   string           `json:"request_id"`
	Expected    string           `json:"response_message_id"`
	RequestHash string           `json:"request_sha256"`
	ResultHash  string           `json:"result_sha256"`
	Contract    recoveryContract `json:"contract"`
	Calls       []toolCall       `json:"calls"`
	Text        string           `json:"text"`
	Wire        []byte           `json:"wire"`
	WireHash    string           `json:"wire_sha256"`
	ContentType string           `json:"content_type"`
	IssuedAt    time.Time        `json:"issued_at,omitempty"`
	AckHash     string           `json:"ack_sha256,omitempty"`
	TailHash    string           `json:"tail_sha256,omitempty"`
}

func canonicalRequestHash(raw []byte) string {
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&value) != nil || d.Decode(&struct{}{}) != io.EOF {
		return ""
	}
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return previewDigest(b)
}
func recoveryToolHash(tools []toolSpec) string {
	b, _ := json.Marshal(tools)
	return canonicalRequestHash(b)
}

func (s *Server) newRecoveryContract(r *http.Request, v chatRequest) *recoveryContract {
	if recoveryOwner(r) == "" || v.SessionID == "" || v.SessionEnd || v.Bridge || s.forwardExtra || isCheckpointCompaction(v) {
		return nil
	}
	hash := canonicalRequestHash(v.OriginalBody)
	if hash == "" {
		return nil
	}
	return &recoveryContract{Path: r.URL.Path, RequestHash: hash, ToolHash: recoveryToolHash(v.Tools), History: hashes(v.Messages), Tools: append([]toolSpec(nil), v.Tools...), Choice: v.ToolChoice, Named: v.ToolChoiceNamed, Stream: v.Stream, Model: v.Model}
}

// Caller holds projectMu. On failed persistence restore the entire prior value;
// no wire frame or native submission is allowed until the intent is durable.
func (s *Server) commitRecoveryLocked(p *projectSession, next projectSession) error {
	old := *p
	*p = next
	if err := s.saveProjectsLocked(); err != nil {
		*p = old
		return &upstreamError{503, "recovery_store_failed", "恢复状态持久化失败；未投递或重发"}
	}
	return nil
}

// RecoveryControlHandler is trusted in-process administrator access. It is NOT
// registered in the public model router; the host supplies the original account.
func (s *Server) RecoveryControlHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/sessions/{id}/recovery/prepare", s.prepareRecovery)
	m.HandleFunc("POST /v1/sessions/{id}/recovery/ack", s.ackRecovery)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if _, ok := embeddedAccount(r.Context()); !ok || s.embed == nil {
			fail(w, 403, "recovery_operator_required", "缺少宿主指定原账号的管理员操作")
			return
		}
		m.ServeHTTP(w, r)
	})
}

type recoveryPrepareInput struct {
	OwnerID     string `json:"owner_id"`
	RequestID   string `json:"request_id"`
	Expected    string `json:"response_message_id"`
	RequestHash string `json:"request_sha256"`
	ResultHash  string `json:"result_sha256"`
}

func (s *Server) prepareRecovery(w http.ResponseWriter, r *http.Request) {
	var in recoveryPrepareInput
	if !decode(w, r, &in) {
		return
	}
	sid := r.PathValue("id")
	a, _ := embeddedAccount(r.Context())
	id := projectMapping(r, sid)
	s.projectMu.Lock()
	p := s.projects[id]
	if p == nil || p.AccountID != a.ID || p.OwnerID == "" || p.OwnerID != in.OwnerID || p.Archived || p.Ending || p.PendingTurn == nil || p.PendingTurn.Contract == nil || !p.PendingTurn.Accepted {
		s.projectMu.Unlock()
		fail(w, 409, "recovery_contract_unverified", "缺少原调用方、原请求契约或已受理证明；旧记录只允许诊断读取")
		return
	}
	if p.Busy {
		s.projectMu.Unlock()
		fail(w, 409, "session_busy", "会话仍在处理")
		return
	}
	pendingPtr := p.PendingTurn
	pending := *pendingPtr
	contract := *pending.Contract
	project := p.ConversationID
	owner := p.OwnerID
	if in.RequestID != pending.RequestID || in.Expected != pending.Expected || in.RequestHash != contract.RequestHash || !validPreviewSHA(in.ResultHash) {
		s.projectMu.Unlock()
		fail(w, 409, "recovery_proof_mismatch", "恢复证明与原始请求不一致")
		return
	}
	if p.Recovery != nil && p.Recovery.RequestID == in.RequestID {
		d := p.Recovery
		if d.ResultHash != in.ResultHash || d.RequestHash != in.RequestHash {
			s.projectMu.Unlock()
			fail(w, 409, "recovery_proof_mismatch", "同一请求的已保存投递与证明冲突")
			return
		}
		result := recoveryReceipt(d)
		s.projectMu.Unlock()
		write(w, 200, result)
		return
	}
	s.projectMu.Unlock()
	lookup := r.Clone(r.Context())
	u := *r.URL
	lookup.URL = &u
	lookup.URL.RawQuery = url.Values{"request_id": {in.RequestID}, "response_message_id": {in.Expected}}.Encode()
	result, err := s.readSessionResult(lookup, project)
	if err != nil {
		status, code, message := turnError(r.Context(), err)
		fail(w, status, code, message)
		return
	}
	if result.Status != "completed" && result.Status != "awaiting_client_tool" || len(result.Result.Attachments) > 0 {
		code := "recovery_result_not_ready"
		if result.Status == "declined" {
			fail(w, 409, "upstream_declined", result.Result.Text)
			return
		}
		if result.Status == "failed" {
			fail(w, 409, result.ErrorCode, result.ErrorMessage)
			return
		}
		fail(w, 409, code, "原结果不是可验收完成结果；未替换拒绝或安装上下文")
		return
	}
	if previewDigest([]byte(result.Result.Text)) != in.ResultHash {
		fail(w, 409, "recovery_result_mismatch", "原文与明确审阅的结果哈希不同")
		return
	}
	v := chatRequest{Tools: contract.Tools, ToolChoice: contract.Choice, ToolChoiceNamed: contract.Named}
	res := &turnResult{Text: result.Result.Text, Outcome: "completed"}
	s.applyToolCalls(v, res)
	if strings.Contains(res.Text, "<tool_call") {
		fail(w, 422, "recovery_tool_mismatch", "无法完整解析原工具契约；未执行")
		return
	}
	if err := validateClientToolResult(v, res.ToolCalls); err != nil {
		status, code, message := turnError(r.Context(), err)
		fail(w, status, code, message)
		return
	}
	d := &recoveryDelivery{ID: "recovery_" + previewDigest([]byte(id + pending.RequestID + in.RequestHash + in.ResultHash))[:24], State: "ready", OwnerID: owner, ProjectID: project, RequestID: in.RequestID, Expected: in.Expected, RequestHash: in.RequestHash, ResultHash: in.ResultHash, Contract: contract, Text: res.Text, Calls: res.ToolCalls}
	for i := range d.Calls {
		d.Calls[i].ID = "call_" + previewDigest([]byte(d.ID + fmt.Sprint(i)))[:24]
	}
	d.Wire, d.ContentType = renderRecoveryWire(d)
	d.WireHash = previewDigest(d.Wire)
	if len(d.Wire) > maxJSONOverhead {
		fail(w, 413, "recovery_result_too_large", "恢复投递超过持久化预算")
		return
	}
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if s.projects[id] != p || p.Busy || p.Archived || p.Ending || p.OwnerID != in.OwnerID || p.ConversationID != project || p.PendingTurn != pendingPtr {
		fail(w, 409, "session_changed", "准备期间会话变化；未安装投递")
		return
	}
	if p.Recovery != nil && p.Recovery.RequestID == in.RequestID {
		write(w, 200, recoveryReceipt(p.Recovery))
		return
	}
	next := *p
	next.Recovery = d
	next.RecoverySeen = append(append([]string(nil), p.RecoverySeen...), d.RequestHash)
	if len(next.RecoverySeen) > 4096 {
		fail(w, 409, "recovery_journal_full", "会话恢复去重记录达到预算；未丢弃旧去重状态")
		return
	}
	if err := s.commitRecoveryLocked(p, next); err != nil {
		fail(w, 503, "recovery_store_failed", err.Error())
		return
	}
	write(w, 200, recoveryReceipt(d))
}

func validRecoveryRecord(p *projectSession) bool {
	if p.PendingTurn != nil && p.PendingTurn.Contract != nil {
		c := p.PendingTurn.Contract
		if p.OwnerID == "" || !validPreviewSHA(c.RequestHash) || c.ToolHash != recoveryToolHash(c.Tools) || len(c.History) == 0 {
			return false
		}
	}
	if d := p.Recovery; d != nil {
		if p.OwnerID == "" || p.OwnerID != d.OwnerID || p.ConversationID != d.ProjectID || !validPreviewSHA(d.RequestHash) || !validPreviewSHA(d.ResultHash) || len(d.Wire) == 0 || len(d.Wire) > maxJSONOverhead || d.WireHash != previewDigest(d.Wire) || !slices.Contains([]string{"ready", "issued", "continuing", "continued", "acknowledged"}, d.State) {
			return false
		}
		if (d.State == "ready" || d.State == "issued") && (p.PendingTurn == nil || p.PendingTurn.RequestID != d.RequestID || p.PendingTurn.Expected != d.Expected) {
			return false
		}
	}
	return true
}

func recoveryReceipt(d *recoveryDelivery) any {
	return map[string]any{"delivery_id": d.ID, "state": d.State, "result_sha256": d.ResultHash, "request_sha256": d.RequestHash, "tools": len(d.Calls), "continuation_applied": d.State == "continuing", "automatic_reissue": false}
}

func renderRecoveryWire(d *recoveryDelivery) ([]byte, string) {
	var f outputFormat
	stable := strings.TrimPrefix(d.ID, "recovery_")
	switch d.Contract.Path {
	case "/v1/responses":
		f = &responsesFormat{id: "resp_" + stable, messageID: "msg_" + stable, created: 0}
	case "/v1/messages":
		f = &anthropicFormat{id: "msg_" + stable, model: d.Contract.Model}
	default:
		f = &chatFormat{id: "chatcmpl_" + stable, created: 0}
	}
	res := &turnResult{Text: d.Text, ToolCalls: d.Calls, Outcome: "completed"}
	if !d.Contract.Stream {
		b, _ := json.Marshal(f.json(res))
		return append(b, '\n'), "application/json; charset=utf-8"
	}
	var b bytes.Buffer
	emit := func(event string, v any) {
		data, _ := json.Marshal(v)
		if event != "" {
			fmt.Fprintf(&b, "event: %s\n", event)
		}
		fmt.Fprintf(&b, "data: %s\n\n", data)
	}
	f.start(emit)
	if d.Text != "" {
		f.delta(emit, d.Text)
	}
	f.finish(emit, d.Text, d.Calls)
	if f.done() {
		b.WriteString("data: [DONE]\n\n")
	}
	return b.Bytes(), "text/event-stream"
}

// serveRecovery issues a prepared saved result at most once, BEFORE writing any
// executable wire data. A dropped connection leaves issued/uncertain, not ready.
func (s *Server) serveRecovery(w http.ResponseWriter, r *http.Request, v chatRequest) bool {
	if v.SessionID == "" {
		return false
	}
	s.projectMu.Lock()
	p := s.projects[projectMapping(r, v.SessionID)]
	if p == nil {
		s.projectMu.Unlock()
		return false
	}
	if p.OwnerID != "" && p.OwnerID != recoveryOwner(r) {
		s.projectMu.Unlock()
		fail(w, 403, "session_owner_mismatch", "原会话不属于当前调用方")
		return true
	}
	hash := canonicalRequestHash(v.OriginalBody)
	if p.Recovery == nil {
		s.projectMu.Unlock()
		return false
	}
	d := p.Recovery
	if hash != d.RequestHash && hash != d.TailHash {
		if slices.Contains(p.RecoverySeen, hash) {
			s.projectMu.Unlock()
			fail(w, 409, "recovery_delivery_uncertain", "旧恢复请求已处理；不再发行旧工具")
			return true
		}
		s.projectMu.Unlock()
		return false
	}
	if hash != d.RequestHash || d.State != "ready" {
		s.projectMu.Unlock()
		fail(w, 409, "recovery_delivery_uncertain", "原投递已发行或续接已处理；不会重复下发工具，请核对显式回执")
		return true
	}
	if p.Busy || p.PendingTurn == nil || p.PendingTurn.RequestID != d.RequestID || p.ConversationID != d.ProjectID || v.SessionEnd || r.URL.Path != d.Contract.Path {
		s.projectMu.Unlock()
		fail(w, 409, "session_changed", "原待恢复请求已变化")
		return true
	}
	copy := *d
	copy.State = "issued"
	copy.IssuedAt = time.Now().UTC()
	next := *p
	next.Recovery = &copy
	if err := s.commitRecoveryLocked(p, next); err != nil {
		s.projectMu.Unlock()
		fail(w, 503, "recovery_store_failed", err.Error())
		return true
	}
	wire := append([]byte(nil), d.Wire...)
	ct := d.ContentType
	deliveryID := d.ID
	s.projectMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Hark-Recovery-Delivery", deliveryID)
	w.WriteHeader(200)
	_, _ = w.Write(wire)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return true
}

func (s *Server) ackRecovery(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OwnerID    string `json:"owner_id"`
		DeliveryID string `json:"delivery_id"`
		ResultHash string `json:"result_sha256"`
		Installed  bool   `json:"installed"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, _ := embeddedAccount(r.Context())
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	p := s.projects[projectMapping(r, r.PathValue("id"))]
	if p == nil || p.AccountID != a.ID || p.Recovery == nil || p.OwnerID != in.OwnerID || !in.Installed {
		fail(w, 409, "recovery_receipt_invalid", "缺少原调用方和明确安装回执")
		return
	}
	d := p.Recovery
	if d.ID != in.DeliveryID || d.ResultHash != in.ResultHash || len(d.Calls) > 0 {
		fail(w, 409, "recovery_receipt_invalid", "工具结果必须由绑定call ID的工具输出确认，不能只清 pending")
		return
	}
	if d.State == "acknowledged" {
		write(w, 200, recoveryReceipt(d))
		return
	}
	if d.State != "issued" || p.Busy || p.PendingTurn == nil || p.PendingTurn.RequestID != d.RequestID {
		fail(w, 409, "recovery_receipt_conflict", "回执与当前状态不匹配")
		return
	}
	copy := *d
	copy.State = "acknowledged"
	copy.AckHash = previewDigest([]byte(in.DeliveryID + in.ResultHash))
	next := *p
	next.Recovery = &copy
	next.PendingTurn = nil
	if err := s.commitRecoveryLocked(p, next); err != nil {
		fail(w, 503, "recovery_store_failed", err.Error())
		return
	}
	write(w, 200, recoveryReceipt(&copy))
}

func (s *Server) recoveryTailPlan(r *http.Request, p *projectSession, v chatRequest) (turnPlan, error) {
	d := p.Recovery
	bad := func() (turnPlan, error) {
		return turnPlan{}, &upstreamError{409, "recovery_receipt_invalid", "工具输出回执、原历史或工具契约不匹配；未清 pending、未重发"}
	}
	if d == nil || d.State != "issued" || p.OwnerID != recoveryOwner(r) || d.OwnerID != p.OwnerID || p.ConversationID != d.ProjectID || p.PendingTurn == nil || p.PendingTurn.RequestID != d.RequestID || r.URL.Path != d.Contract.Path || v.SessionEnd || v.Bridge || len(d.Calls) == 0 || recoveryToolHash(v.Tools) != d.Contract.ToolHash {
		return bad()
	}
	n := len(d.Contract.History)
	if len(v.Messages) <= n || !slices.Equal(hashes(v.Messages[:n]), d.Contract.History) {
		return bad()
	}
	seenCalls := map[string]bool{}
	seenOutputs := map[string]bool{}
	var outputs []chatMessage
	for _, m := range v.Messages[n:] {
		if len(m.Files) > 0 {
			return bad()
		}
		switch m.Role {
		case "assistant":
			if m.Content != "" && m.Content != d.Text {
				return bad()
			}
			calls, err := recoveryEchoCalls(m.ToolCalls)
			if err != nil {
				return bad()
			}
			for _, c := range calls {
				matched := false
				for _, want := range d.Calls {
					if c.ID == recoveryWireCallID(d, want) && c.Name == want.Name && c.Namespace == want.Namespace && sameRecoveryArguments(c.Arguments, want.Arguments) {
						matched = true
					}
				}
				if !matched || seenCalls[c.ID] {
					return bad()
				}
				seenCalls[c.ID] = true
			}
		case "tool":
			id := recoveryOutputID(m.Content)
			if id == "" || seenOutputs[id] || !seenCalls[id] {
				return bad()
			}
			seenOutputs[id] = true
			outputs = append(outputs, m)
		default:
			return bad()
		}
	}
	if len(seenCalls) != len(d.Calls) || len(seenOutputs) != len(d.Calls) {
		return bad()
	}
	plan := turnPlan{send: outputs, mode: "recovery-tail", sent: hashes(v.Messages), systems: systemHashes(v.Messages, nil)}
	if label, ok := r.Context().Value(modelLabelKey{}).(modelLabelContext); ok {
		legacy, err := s.decodeRecoveryBody(r.URL.Path, label.legacyBody)
		if err != nil {
			return bad()
		}
		plan.legacySent = hashes(legacy.Messages)
		plan.legacySystems = systemHashes(legacy.Messages, nil)
	}
	return plan, nil
}

func recoveryWireCallID(d *recoveryDelivery, c toolCall) string {
	if d.Contract.Path == "/v1/messages" {
		return toolUseID(c)
	}
	return c.ID
}
func sameRecoveryArguments(a, b string) bool {
	ha, hb := canonicalRequestHash([]byte(a)), canonicalRequestHash([]byte(b))
	if ha != "" && hb != "" {
		return ha == hb
	}
	return a == b
}
func recoveryOutputID(content string) string {
	for _, prefix := range []string{"[call_id=", "[tool_call_id="} {
		if strings.HasPrefix(content, prefix) {
			rest := strings.TrimPrefix(content, prefix)
			i := strings.Index(rest, "]")
			if i > 0 && validResultID(rest[:i]) {
				return rest[:i]
			}
		}
	}
	return ""
}
func recoveryEchoCalls(raw string) ([]toolCall, error) {
	var rows []map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &rows) != nil {
		var row map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &row) != nil || row == nil {
			return nil, fmt.Errorf("missing echoed calls")
		}
		rows = []map[string]json.RawMessage{row}
	}
	var out []toolCall
	for _, row := range rows {
		var c toolCall
		_ = json.Unmarshal(row["call_id"], &c.ID)
		if c.ID == "" {
			_ = json.Unmarshal(row["id"], &c.ID)
		}
		_ = json.Unmarshal(row["name"], &c.Name)
		_ = json.Unmarshal(row["namespace"], &c.Namespace)
		if fn := row["function"]; len(fn) > 0 {
			var f map[string]json.RawMessage
			if json.Unmarshal(fn, &f) != nil {
				return nil, fmt.Errorf("bad echo")
			}
			_ = json.Unmarshal(f["name"], &c.Name)
			row = f
		}
		if json.Unmarshal(row["arguments"], &c.Arguments) != nil {
			c.Arguments = string(row["input"])
			var input string
			if json.Unmarshal(row["input"], &input) == nil {
				c.Arguments = input
			}
		}
		out = append(out, c)
	}
	return out, nil
}
func (s *Server) decodeRecoveryBody(path string, b []byte) (chatRequest, error) {
	switch path {
	case "/v1/responses":
		return s.normalizeResponses(b)
	case "/v1/messages":
		v, _, err := s.normalizeMessages(b)
		return v, err
	default:
		return s.normalizeChat(b)
	}
}
