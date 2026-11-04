package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxRequestLogs bounds the in-memory request log (newest kept).
const maxRequestLogs = 500

// RequestLog is one client call to /v1/chat/completions or /v1/responses
// (or the dashboard playground). It never holds message content, cookies or
// API key material: only which key and account handled it and how it ended.
type RequestLog struct {
	ID         string   `json:"id"`
	Time       string   `json:"time"`
	Path       string   `json:"path"`
	Key        string   `json:"key"`
	KeyPrefix  string   `json:"key_prefix,omitempty"`
	Model      string   `json:"model,omitempty"`
	Stream     bool     `json:"stream"`
	Session    bool     `json:"session"`
	Account    string   `json:"account,omitempty"`
	AccountID  string   `json:"account_id,omitempty"`
	Reused     bool     `json:"reused"`
	Attempts   int      `json:"attempts"`
	Skipped    []string `json:"skipped,omitempty"`
	Files      int      `json:"files"`
	Status     int      `json:"status"`
	Code       string   `json:"code,omitempty"`
	Outcome    string   `json:"outcome,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	ReplyChars int      `json:"reply_chars"`
}

// RequestStats summarises the kept request logs.
type RequestStats struct {
	Total      int   `json:"total"`
	OK         int   `json:"ok"`
	Failed     int   `json:"failed"`
	LastHour   int   `json:"last_hour"`
	AvgMS      int64 `json:"avg_ms"`
	InProgress int   `json:"in_progress"`
}

type reqRecord struct {
	mu    sync.Mutex
	start time.Time
	log   RequestLog
}

type reqCtxKey struct{}

func reqRecFrom(ctx context.Context) *reqRecord {
	r, _ := ctx.Value(reqCtxKey{}).(*reqRecord)
	return r
}

func (r *reqRecord) update(f func(l *RequestLog)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	f(&r.log)
	r.mu.Unlock()
}

// statusRecorder captures the status and the start of an error body while
// still passing Flush through for streaming replies.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	errBody []byte
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.status >= 400 && len(w.errBody) < 2048 {
		n := 2048 - len(w.errBody)
		if n > len(b) {
			n = len(b)
		}
		w.errBody = append(w.errBody, b[:n]...)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// trackRequest records one request log entry around a chat handler.
func (s *Server) trackRequest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &reqRecord{start: time.Now(), log: RequestLog{
			ID:   randomID("req_"),
			Time: time.Now().UTC().Format(time.RFC3339Nano),
			Path: r.URL.Path,
		}}
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			rec.log.Key = "控制台测试"
		}
		s.reqMu.Lock()
		s.reqInFlight++
		s.reqMu.Unlock()
		sw := &statusRecorder{ResponseWriter: w}
		defer func() {
			rec.mu.Lock()
			l := rec.log
			rec.mu.Unlock()
			l.DurationMS = time.Since(rec.start).Milliseconds()
			l.Status = sw.status
			if l.Status == 0 {
				l.Status = 499 // client went away before any reply
				if l.Code == "" {
					l.Code = "client_closed"
				}
			}
			if l.Code == "" && l.Status >= 400 {
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if json.Unmarshal(sw.errBody, &body) == nil {
					l.Code = body.Error.Code
				}
			}
			s.reqMu.Lock()
			s.reqInFlight--
			s.reqLogs = append(s.reqLogs, l)
			if len(s.reqLogs) > maxRequestLogs {
				s.reqLogs = s.reqLogs[len(s.reqLogs)-maxRequestLogs:]
			}
			s.reqMu.Unlock()
		}()
		next(sw, r.WithContext(context.WithValue(r.Context(), reqCtxKey{}, rec)))
	}
}

func requestFailed(l RequestLog) bool { return l.Status >= 400 || l.Code != "" }

func (s *Server) requestStats() RequestStats {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	st := RequestStats{Total: len(s.reqLogs), InProgress: s.reqInFlight}
	hour := time.Now().Add(-time.Hour)
	var sum int64
	for _, l := range s.reqLogs {
		if requestFailed(l) {
			st.Failed++
		} else {
			st.OK++
		}
		if t, err := time.Parse(time.RFC3339Nano, l.Time); err == nil && t.After(hour) {
			st.LastHour++
		}
		sum += l.DurationMS
	}
	if st.Total > 0 {
		st.AvgMS = sum / int64(st.Total)
	}
	return st
}

// GET /admin/requests?limit=N&failed=1&account=<id>&key=<name>
func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > maxRequestLogs {
		limit = 200
	}
	onlyFailed := q.Get("failed") == "1"
	account, key := q.Get("account"), q.Get("key")
	s.reqMu.Lock()
	out := make([]RequestLog, 0, limit)
	for i := len(s.reqLogs) - 1; i >= 0 && len(out) < limit; i-- {
		l := s.reqLogs[i]
		if onlyFailed && !requestFailed(l) {
			continue
		}
		if account != "" && l.AccountID != account {
			continue
		}
		if key != "" && l.Key != key {
			continue
		}
		out = append(out, l)
	}
	s.reqMu.Unlock()
	write(w, 200, map[string]any{"requests": out, "stats": s.requestStats()})
}

// DELETE /admin/requests
func (s *Server) clearRequests(w http.ResponseWriter, r *http.Request) {
	s.reqMu.Lock()
	s.reqLogs = nil
	s.reqMu.Unlock()
	s.mu.Lock()
	s.event("success", "请求记录", "已清空")
	s.mu.Unlock()
	write(w, 200, map[string]any{"ok": true})
}
