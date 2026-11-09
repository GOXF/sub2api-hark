package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode"
)

// turnStatus is out-of-band UI metadata, never assistant content or a tool receipt.
type turnStatus struct {
	Type           string `json:"type"`
	Status         string `json:"status"`
	ConversationID string `json:"conversation_id"`
	RequestID      string `json:"request_id"`
	MessageID      string `json:"message_id,omitempty"`
	Line           string `json:"line"`
	LineKey        string `json:"line_key,omitempty"`
	Sequence       int64  `json:"sequence"`
	Reason         string `json:"reason,omitempty"`
}

type statusNarration struct {
	MessageID string `json:"messageId"`
	Line      string `json:"line"`
	LineKey   string `json:"lineKey"`
	Live      bool   `json:"live"`
}

type turnStatusTracker struct {
	mu                       sync.Mutex
	cid, expected, requestID string
	messages                 map[string]harkMessage
	revision                 int64
	upstreamSeq, sequence    int64
	waiting                  bool
	hasSync                  bool
	settled                  chan struct{}
	last                     turnStatus
	notify                   func(turnStatus)
}

func newTurnStatusTracker(cid, expected, requestID string, notify func(turnStatus)) *turnStatusTracker {
	return &turnStatusTracker{cid: cid, expected: expected, requestID: requestID, messages: map[string]harkMessage{}, revision: -1, settled: make(chan struct{}), notify: notify}
}

func (s *turnStatusTracker) waitForPerson(needed bool) {
	if needed && !s.waiting {
		s.waiting = true
		close(s.settled)
	}
}

// Only user-visible narration is decoded. Private thought/reasoning and tool
// arguments/results are intentionally absent from this metadata path.
func statusLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	runes := []rune(s)
	if len(runes) > 2048 {
		runes = runes[:2048]
	}
	return strings.TrimSpace(string(runes))
}

func (s *turnStatusTracker) current() turnStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *turnStatusTracker) syncStopped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasSync = false
}

func (s *turnStatusTracker) publish(m harkMessage) {
	state := "processing"
	if s.waiting {
		state = "waiting_for_user"
	}
	line, key := "", ""
	if state == "processing" && m.Narration != nil {
		line, key = statusLine(m.Narration.Line), statusLine(m.Narration.LineKey)
	}
	if state == "processing" && m.Streaming != nil && !*m.Streaming {
		return
	}
	if state == "processing" && m.JobStatus != "running" && (m.Streaming == nil || !*m.Streaming) && line == "" {
		return
	}
	next := turnStatus{Type: "hark.status", Status: state, ConversationID: s.cid, RequestID: s.requestID, MessageID: m.ID, Line: line, LineKey: key}
	if s.last.Status == next.Status && s.last.MessageID == next.MessageID && s.last.Line == next.Line && s.last.LineKey == next.LineKey {
		return
	}
	s.sequence++
	next.Sequence = s.sequence
	s.last = next
	if s.notify != nil {
		s.notify(next)
	}
}

func (s *turnStatusTracker) history(messages []harkMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range messages {
		if !boundToTurn(m, s.expected, s.requestID) {
			continue
		}
		// The sync channel is authoritative for live text. History is a fallback,
		// but inputNeeded remains a durable waiting-for-person signal.
		s.waitForPerson(m.InputNeeded)
		if _, ok := s.messages[m.ID]; !ok || !s.hasSync {
			s.messages[m.ID] = m
			s.publish(m)
		} else if s.waiting {
			s.publish(s.messages[m.ID])
		}
	}
}

func (s *turnStatusTracker) observe(payload []byte) {
	var frame struct {
		Type string          `json:"type"`
		Seq  int64           `json:"seq"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return
	}
	if frame.Type != "snapshot" && frame.Type != "patches" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if frame.Seq > 0 && frame.Seq <= s.upstreamSeq {
		return
	}
	if frame.Seq > 0 {
		s.upstreamSeq = frame.Seq
	}
	if frame.Type == "snapshot" {
		var snapshot struct {
			Messages  map[string]harkMessage     `json:"messages"`
			Requests  map[string]json.RawMessage `json:"requests"`
			Narration *statusNarration           `json:"narration"`
		}
		if json.Unmarshal(frame.Data, &snapshot) != nil {
			return
		}
		s.hasSync = true
		for id, m := range snapshot.Messages {
			if m.ID == "" {
				m.ID = id
			}
			if boundToTurn(m, s.expected, s.requestID) {
				s.messages[id] = m
				s.waitForPerson(m.InputNeeded)
			}
		}
		if raw := snapshot.Requests[s.requestID]; raw != nil {
			s.request(raw)
		}
		for _, m := range s.messages {
			s.publish(m)
		}
		if n := snapshot.Narration; n != nil {
			if m, ok := s.messages[n.MessageID]; ok {
				m.Narration = n
				s.messages[m.ID] = m
				s.publish(m)
			}
		}
		return
	}
	if frame.Type != "patches" {
		return
	}
	var patches []struct {
		Op     string          `json:"op"`
		ID     string          `json:"id"`
		Value  json.RawMessage `json:"value"`
		Fields json.RawMessage `json:"fields"`
	}
	if json.Unmarshal(frame.Data, &patches) != nil {
		return
	}
	s.hasSync = true
	for _, p := range patches {
		switch p.Op {
		case "entry_add":
			var m harkMessage
			if json.Unmarshal(p.Value, &m) != nil {
				continue
			}
			if m.ID == "" {
				m.ID = p.ID
			}
			if !boundToTurn(m, s.expected, s.requestID) {
				continue
			}
			s.messages[m.ID] = m
			s.waitForPerson(m.InputNeeded)
			s.publish(m)
		case "message_update":
			m, ok := s.messages[p.ID]
			if !ok {
				continue
			}
			if json.Unmarshal(p.Fields, &m) != nil || !boundToTurn(m, s.expected, s.requestID) {
				continue
			}
			// Patch IDs cannot rebind metadata to another message.
			m.ID = p.ID
			s.messages[p.ID] = m
			s.waitForPerson(m.InputNeeded)
			s.publish(m)
		case "narration_update":
			var n statusNarration
			if json.Unmarshal(p.Value, &n) != nil {
				continue
			}
			m, ok := s.messages[n.MessageID]
			if !ok {
				continue
			}
			m.Narration = &n
			s.messages[m.ID] = m
			s.publish(m)
		case "request_update":
			if p.ID == s.requestID {
				s.request(p.Value)
			}
		}
	}
}

func (s *turnStatusTracker) request(raw json.RawMessage) {
	var v struct {
		MessageID    string `json:"messageId"`
		Revision     int64  `json:"revision"`
		AwaitsPerson bool   `json:"awaitsPerson"`
	}
	if json.Unmarshal(raw, &v) != nil || v.MessageID != s.requestID || v.Revision <= s.revision {
		return
	}
	s.revision = v.Revision
	// A new client turn creates a new tracker. Within this accepted request a
	// late stub must not turn waiting-for-person back into working/success.
	s.waitForPerson(v.AwaitsPerson)
	if s.waiting {
		s.publish(harkMessage{ID: s.last.MessageID})
	}
}

// readStatusSSE handles split/multiline SSE, comments and bounded frames.
func readStatusSSE(body io.Reader, apply func([]byte)) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var data strings.Builder
	flush := func() {
		if data.Len() > 0 {
			apply([]byte(strings.TrimSuffix(data.String(), "\n")))
			data.Reset()
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			part := strings.TrimPrefix(line, "data:")
			part = strings.TrimPrefix(part, " ")
			if data.Len()+len(part) > 1024*1024 {
				return
			}
			data.WriteString(part)
			data.WriteByte('\n')
		}
	}
	// An incomplete frame at EOF is not an observed event.
}

func (s *Server) watchTurnStatus(ctx context.Context, cookie, cid string, tracker *turnStatusTracker) {
	defer tracker.syncStopped()
	query := url.Values{"conversationId": {cid}, "v": {"2"}, "manager": {uuid()}, "mode": {"in_tab"}, "ids": {"1"}, "beat": {"5"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.upstream+"/api/sync/conversation?"+query.Encode(), nil)
	if err != nil {
		return
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "text/event-stream")
	response, err := s.clientFor(cookie, 0, false).Do(req)
	if err != nil {
		return
	}
	defer response.Body.Close()
	// Metadata failure is not a failed/retryable model submission.
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return
	}
	readStatusSSE(response.Body, tracker.observe)
}
