package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type activityFixture struct {
	wire           bytes.Buffer
	wireMu         sync.Mutex
	api            *httptest.Server
	gate           chan struct{}
	once           sync.Once
	sends, watches atomic.Int32
	clientFlags    atomic.Bool
	final          string
}

func newActivityFixture(t *testing.T) *activityFixture {
	t.Helper()
	f := &activityFixture{gate: make(chan struct{}), final: "Example Domain\nSYNTHETIC_ACTIVITY_FINAL"}
	tracePath := "testdata/response_activity_trace.json"
	if input := os.Getenv("HARK_ACTIVITY_TRACE"); input != "" {
		tracePath = input
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requestID, responseID string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/conversations":
			fmt.Fprint(w, `{"conversationId":"activity-fixture"}`)
		case "/api/messages/send":
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid upstream request")
				w.WriteHeader(400)
				return
			}
			mu.Lock()
			json.Unmarshal(body["idempotencyKey"], &requestID)
			json.Unmarshal(body["responseMessageId"], &responseID)
			u := requestID
			mu.Unlock()
			f.sends.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "activity-fixture", "messageId": u, "redirected": false})
		case "/api/sync/conversation":
			f.watches.Add(1)
			mu.Lock()
			u, a := requestID, responseID
			mu.Unlock()
			frames := bytes.ReplaceAll(bytes.ReplaceAll(data, []byte("fixture-request"), []byte(u)), []byte("fixture-assistant"), []byte(a))
			var events []json.RawMessage
			json.Unmarshal(frames, &events)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range events {
				compact, _ := json.Marshal(event)
				fmt.Fprintf(w, "data: %s\n\n", compact)
				w.(http.Flusher).Flush()
			}
			<-r.Context().Done()
		case "/api/messages/history":
			select {
			case <-f.gate:
			case <-time.After(150 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			mu.Lock()
			u, a := requestID, responseID
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{"id": a, "role": "assistant", "triggeredByMessageId": u, "jobStatus": "completed", "isStreaming": false, "content": f.final}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)
	s, err := NewEmbedded(t.TempDir(), Options{Upstream: up.URL, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512000))
		if err != nil {
			http.Error(w, "body", 400)
			return
		}
		if bytes.Contains(body, []byte(`"hark_status"`)) || r.Header.Get("X-Hark-Status") != "" {
			f.clientFlags.Store(true)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		ctx := WithEmbeddedAccount(r.Context(), SourceAccount{ID: "activity-account", Cookie: "synthetic-cookie"})
		s.CallerHandler("activity-owner").ServeHTTP(&activityWriter{ResponseWriter: w, fixture: f}, r.WithContext(ctx))
	}))
	t.Cleanup(f.api.Close)
	return f
}

type activityWriter struct {
	http.ResponseWriter
	fixture *activityFixture
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.fixture.wireMu.Lock()
	w.fixture.wire.Write(p)
	w.fixture.wireMu.Unlock()
	if bytes.Contains(p, []byte("response.reasoning_summary_text.delta")) {
		w.fixture.once.Do(func() { close(w.fixture.gate) })
	}
	return w.ResponseWriter.Write(p)
}

func (w *activityWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func TestResponsesPublicActivityWithoutClientChanges(t *testing.T) {
	f := newActivityFixture(t)
	res, err := http.Post(f.api.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"hark","stream":true,"input":"Read the public example page"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var wire bytes.Buffer
	wire.ReadFrom(res.Body)
	text := wire.String()
	if !strings.Contains(text, "response.reasoning_summary_text.delta") || !strings.Contains(text, "正在阅读页面") {
		t.Fatalf("STANDARD_ACTIVITY=false WATCHES=%d CLIENT_FLAGS=%t", f.watches.Load(), f.clientFlags.Load())
	}
	if f.clientFlags.Load() || strings.Contains(text, "hark.status") || f.sends.Load() != 1 {
		t.Fatal("client changes required or upstream replayed")
	}
	var final struct {
		Type     string `json:"type"`
		Response struct {
			OutputText string `json:"output_text"`
			Output     []struct {
				ID, Type string
			}
		}
	}
	added := map[int]string{}
	readStatusSSE(strings.NewReader(text), func(data []byte) {
		var event struct {
			Type        string `json:"type"`
			OutputIndex int    `json:"output_index"`
			Item        struct{ ID string }
		}
		json.Unmarshal(data, &event)
		if event.Type == "response.output_item.added" {
			if _, exists := added[event.OutputIndex]; exists {
				t.Error("output index reused")
			}
			added[event.OutputIndex] = event.Item.ID
		}
		if event.Type == "response.completed" {
			json.Unmarshal(data, &final)
		}
	})
	if final.Response.OutputText != f.final {
		t.Fatalf("final answer changed: %q", final.Response.OutputText)
	}
	for i, item := range final.Response.Output {
		if item.ID != added[i] {
			t.Fatalf("output index mismatch: %d", i)
		}
	}
	t.Logf("STANDARD_ACTIVITY=true CLIENT_FLAGS=false FINAL_UNCHANGED=true SENDS=%d", f.sends.Load())
}

func TestResponsesActivityInterleaving(t *testing.T) {
	for _, textFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(textFirst), func(t *testing.T) {
			f := newResponsesFormat()
			sink, ok := any(f).(interface {
				activity(func(string, any), turnStatus)
			})
			if !ok {
				t.Fatal("standard activity adapter missing")
			}
			var events []map[string]any
			emit := func(_ string, value any) { events = append(events, value.(map[string]any)) }
			f.start(emit)
			if textFirst {
				f.delta(emit, "answer")
			}
			status := turnStatus{Status: "processing", Line: "正在阅读页面"}
			sink.activity(emit, status)
			sink.activity(emit, status)
			sink.activity(emit, turnStatus{Status: "ended", Line: "MUST_NOT_SHOW"})
			if !textFirst {
				f.delta(emit, "answer")
			}
			f.delta(emit, " tail")
			f.finish(emit, "answer tail", []toolCall{{ID: "call_fixture", Kind: "function", Name: "read_file", Arguments: `{}`}})
			added := map[int]string{}
			activeID := ""
			parts := 0
			for _, event := range events {
				if event["type"] == "response.output_item.added" {
					index := event["output_index"].(int)
					if _, seen := added[index]; seen {
						t.Fatal("interleaved index reused")
					}
					added[index] = event["item"].(map[string]any)["id"].(string)
					activeID = added[index]
				}
				if event["type"] == "response.output_text.delta" && event["item_id"] != activeID {
					t.Fatal("activity replaced the client's active answer item")
				}
				if event["type"] == "response.output_item.done" {
					activeID = ""
				}
				if event["type"] == "response.reasoning_summary_text.delta" {
					parts++
				}
			}
			if parts != 1 {
				t.Fatalf("duplicate or terminal activity forwarded: %d", parts)
			}
			output := events[len(events)-1]["response"].(map[string]any)["output"].([]any)
			for i, item := range output {
				if item.(map[string]any)["id"] != added[i] {
					t.Fatal("terminal envelope index drift")
				}
			}
		})
	}
}

func TestResponsesActivityBoundsAndFailure(t *testing.T) {
	f := newResponsesFormat()
	sink, ok := any(f).(interface {
		activity(func(string, any), turnStatus)
	})
	if !ok {
		t.Fatal("standard activity adapter missing")
	}
	var events []map[string]any
	emit := func(_ string, v any) { events = append(events, v.(map[string]any)) }
	f.start(emit)
	for _, status := range []turnStatus{{Status: "processing", Line: "\n\t"}, {Status: "waiting_for_user", Line: "ignored"}, {Status: "ended", Line: "ignored"}} {
		sink.activity(emit, status)
	}
	if len(events) != 2 {
		t.Fatal("blank or non-processing status manufactured content")
	}
	for i := range 40 {
		sink.activity(emit, turnStatus{Status: "processing", Line: fmt.Sprint("public ", i)})
	}
	parts := 0
	for _, e := range events {
		if e["type"] == "response.reasoning_summary_text.delta" {
			parts++
		}
	}
	if parts != 32 {
		t.Fatalf("activity metadata not bounded: %d", parts)
	}
	f.failure(emit, "upstream_unavailable", "fixture failure")
	last := events[len(events)-1]
	if last["type"] != "response.failed" || last["response"].(map[string]any)["status"] != "failed" {
		t.Fatal("activity masked terminal failure")
	}
	for _, e := range events {
		if e["type"] == "response.completed" {
			t.Fatal("failed turn emitted completion")
		}
	}
}

func TestTurnStatusNarrationVisibility(t *testing.T) {
	for _, source := range []string{"snapshot", "snapshot_override", "patch", "history"} {
		for _, visibility := range []string{"absent", "true", "false"} {
			t.Run(source+"/"+visibility, func(t *testing.T) {
				n := map[string]any{"messageId": "a", "line": "VISIBILITY_SENTINEL", "lineKey": "private-if-hidden"}
				if visibility != "absent" {
					n["visible"] = visibility == "true"
				}
				m := map[string]any{"id": "a", "role": "assistant", "triggeredByMessageId": "u", "jobStatus": "running", "isStreaming": true, "narration": n}
				var got []turnStatus
				s := newTurnStatusTracker("c", "a", "u", func(status turnStatus) { got = append(got, status) })
				switch source {
				case "history":
					data, _ := json.Marshal(m)
					var message harkMessage
					json.Unmarshal(data, &message)
					s.history([]harkMessage{message})
				case "snapshot":
					data, _ := json.Marshal(map[string]any{"type": "snapshot", "data": map[string]any{"messages": map[string]any{"a": m}}})
					s.observe(data)
				case "snapshot_override":
					m["narration"] = map[string]any{"line": "STALE_NESTED_LINE", "lineKey": "stale-key"}
					data, _ := json.Marshal(map[string]any{"type": "snapshot", "data": map[string]any{"messages": map[string]any{"a": m}, "narration": n}})
					s.observe(data)
				case "patch":
					delete(m, "narration")
					data, _ := json.Marshal(map[string]any{"type": "patches", "data": []any{map[string]any{"op": "entry_add", "id": "a", "value": m}, map[string]any{"op": "narration_update", "value": n}}})
					s.observe(data)
				}
				status := s.current()
				if visibility == "false" {
					for _, published := range got {
						if published.Line != "" || published.LineKey != "" {
							t.Fatal("hidden narration leaked")
						}
					}
				} else if status.Line != "VISIBILITY_SENTINEL" {
					t.Fatal("public or legacy narration lost")
				}
			})
		}
	}
}
