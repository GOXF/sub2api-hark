package gateway

import (
	"encoding/json"
	"os"
	"testing"
)

func TestTurnStatusLiveTrace(t *testing.T) {
	b, err := os.ReadFile("testdata/turn_status_trace.json")
	if err != nil {
		t.Fatal(err)
	}
	var frames []json.RawMessage
	if err = json.Unmarshal(b, &frames); err != nil {
		t.Fatal(err)
	}
	var got []turnStatus
	tracker := newTurnStatusTracker("fixture-project", "unused-response-id", "eaad53b5-2ac4-4680-a4e3-76027f1fa095", func(v turnStatus) { got = append(got, v) })
	for _, frame := range frames {
		tracker.observe(frame)
	}
	lines := map[string]bool{}
	for i, s := range got {
		if s.Sequence != int64(i+1) {
			t.Fatal("non-monotonic sequence")
		}
		lines[s.Line] = true
		if s.RequestID != tracker.requestID {
			t.Fatal("request mismatch")
		}
	}
	for _, line := range []string{"正在使用我的电脑", "正在查找您的文件。", "正在查看项目日志。"} {
		if !lines[line] {
			t.Fatalf("missing real stage %q: %+v", line, got)
		}
	}
	if len(got) == 0 || got[len(got)-1].Status != "waiting_for_user" {
		t.Fatalf("job completion disguised missing attachment as success: %+v", got)
	}
	if tracker.current().Line != "" {
		t.Fatal("waiting state retained stale working narration")
	}
}

func TestTurnStatusIgnoresOtherTurnsAndReplays(t *testing.T) {
	var got []turnStatus
	s := newTurnStatusTracker("c", "a", "u", func(v turnStatus) { got = append(got, v) })
	s.observe([]byte(`{"type":"patches","seq":1,"data":[{"op":"entry_add","id":"old","value":{"id":"old","role":"assistant","triggeredByMessageId":"other","jobStatus":"running","isStreaming":true}},{"op":"narration_update","value":{"messageId":"old","line":"wrong","live":true}}]}`))
	if len(got) != 0 {
		t.Fatal("other turn leaked")
	}
	frame := []byte(`{"type":"patches","seq":2,"data":[{"op":"entry_add","id":"a","value":{"id":"a","role":"assistant","triggeredByMessageId":"u","jobStatus":"running","isStreaming":true}},{"op":"message_update","id":"a","fields":{"narration":{"line":"Reading fixture","lineKey":"fixture.read"}}}]}`)
	s.observe(frame)
	n := len(got)
	s.observe(frame)
	if len(got) != n || n == 0 {
		t.Fatal("replayed frame changed status")
	}
	s.observe([]byte(`{"type":"patches","data":[{"op":"request_update","id":"u","value":{"messageId":"u","revision":3,"awaitsPerson":true,"state":"open"}}]}`))
	s.observe([]byte(`{"type":"patches","data":[{"op":"request_update","id":"u","value":{"messageId":"u","revision":2,"awaitsPerson":false,"state":"open"}}]}`))
	s.observe([]byte(`{"type":"patches","seq":3,"data":[{"op":"message_update","id":"a","fields":{"narration":{"line":"late"},"isStreaming":true}}]}`))
	if s.current().Status != "waiting_for_user" {
		t.Fatal("late work rolled waiting backward")
	}
}

func TestTurnStatusOptInParsers(t *testing.T) {
	var v chatRequest
	if err := parseEnvelope(map[string]json.RawMessage{"model": json.RawMessage(`"hark"`), "hark_status": json.RawMessage(`true`)}, &v); err != nil || !v.StatusEvents {
		t.Fatalf("flag lost: %+v %v", v, err)
	}
	if _, ok := extraFields(map[string]json.RawMessage{"hark_status": json.RawMessage(`true`)})["hark_status"]; ok {
		t.Fatal("internal status switch forwarded upstream")
	}
	if err := parseEnvelope(map[string]json.RawMessage{"model": json.RawMessage(`"hark"`), "hark_status": json.RawMessage(`"true"`)}, &v); err == nil {
		t.Fatal("non-boolean flag accepted")
	}
}

func TestTurnStatusHistoryResumesAfterSyncDisconnect(t *testing.T) {
	s := newTurnStatusTracker("c", "a", "u", nil)
	s.observe([]byte(`{"type":"patches","seq":1,"data":[{"op":"entry_add","id":"a","value":{"id":"a","role":"assistant","triggeredByMessageId":"u","jobStatus":"running","isStreaming":true,"narration":{"line":"first"}}}]}`))
	s.syncStopped()
	streaming := true
	s.history([]harkMessage{{ID: "a", Role: "assistant", TriggeredBy: "u", JobStatus: "running", Streaming: &streaming, Narration: &statusNarration{Line: "after disconnect"}}})
	if s.current().Line != "after disconnect" {
		t.Fatal("sync disconnect froze fallback status")
	}
}
