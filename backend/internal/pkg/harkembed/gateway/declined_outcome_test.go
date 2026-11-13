package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDeclinedRecordedBackendRefusal(t *testing.T) {
	b, err := os.ReadFile("testdata/backend_refusal_history.json")
	if err != nil {
		t.Fatal(err)
	}
	var trace struct {
		Turns []struct {
			RequestID string        `json:"request_id"`
			Expected  string        `json:"expected"`
			Messages  []harkMessage `json:"messages"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(b, &trace); err != nil {
		t.Fatal(err)
	}
	for _, turn := range trace.Turns {
		t.Run(turn.RequestID, func(t *testing.T) {
			reply, err := completedProjectReply(turn.Messages, turn.Expected, turn.RequestID)
			if err != nil || reply.Outcome != "declined" || reply.Text != turn.Messages[1].Content {
				t.Fatalf("raw backend refusal treated as success: outcome=%q body_preserved=%v err=%v", reply.Outcome, reply.Text == turn.Messages[1].Content, err)
			}
			// A quote, user text, another turn, summary, or unfinished bubble is
			// not an authoritative self-refusal for this completed turn.
			for _, variant := range []string{"quote", "user", "other_turn", "summary", "streaming", "negation", "with_work"} {
				messages := append([]harkMessage(nil), turn.Messages...)
				m := &messages[1]
				switch variant {
				case "quote":
					m.Content = "> " + m.Content
				case "user":
					m.Role = "user"
				case "other_turn":
					m.TriggeredBy = "other"
				case "summary":
					m.Presentation.Kind = "summary"
				case "streaming":
					yes := true
					m.Streaming = &yes
				case "negation":
					m.Content = "并不是这样：" + m.Content
				case "with_work":
					m.Content += "\n已执行修改。"
				}
				r, _ := completedProjectReply(messages, turn.Expected, turn.RequestID)
				if r.Outcome == "declined" {
					t.Fatalf("false refusal: %s", variant)
				}
			}
			messages := append([]harkMessage(nil), turn.Messages...)
			messages[1].Content = "普通回答：可以修改页面布局。"
			r, err := completedProjectReply(messages, turn.Expected, turn.RequestID)
			if err != nil || r.Outcome != "completed" || !strings.HasPrefix(r.Text, "普通回答") {
				t.Fatal("ordinary body changed")
			}
		})
	}
}

func TestDeclinedStatusIsTerminal(t *testing.T) {
	var got []turnStatus
	s := newTurnStatusTracker("c", "a", "u", func(v turnStatus) { got = append(got, v) })
	s.finishDeclined()
	s.finishDeclined()
	yes := true
	s.history([]harkMessage{{ID: "a", Role: "assistant", TriggeredBy: "u", JobStatus: "running", Streaming: &yes, InputNeeded: true}})
	if len(got) != 1 || got[0].Status != "ended" || got[0].Reason != "upstream_declined" || got[0].RequestID != "u" || got[0].Sequence != 1 {
		t.Fatalf("late event replaced terminal refusal: %+v", got)
	}
}
