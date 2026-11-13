package gateway

import (
	"encoding/json"
	"os"
	"testing"
)

type observedRefusalTurn struct {
	RequestID string        `json:"request_id"`
	Expected  string        `json:"expected"`
	Messages  []harkMessage `json:"messages"`
}

func observedRefusalTurns(t *testing.T) []observedRefusalTurn {
	t.Helper()
	b, err := os.ReadFile("testdata/observed_refusals_20261113.json")
	if err != nil {
		t.Fatal(err)
	}
	var trace struct {
		Turns []observedRefusalTurn `json:"turns"`
	}
	if err := json.Unmarshal(b, &trace); err != nil {
		t.Fatal(err)
	}
	if len(trace.Turns) != 7 {
		t.Fatalf("want 7 observed turns, got %d", len(trace.Turns))
	}
	return trace.Turns
}

func observedRefusalText(turn observedRefusalTurn) string {
	for _, m := range turn.Messages {
		if m.Source == "send_bubble" {
			return m.Content
		}
	}
	return ""
}

// Rebind fixture IDs only; retain the actual four-event order and metadata.
func observedRefusalMessages(turn observedRefusalTurn, user, primary string) []harkMessage {
	messages := append([]harkMessage(nil), turn.Messages...)
	for i := range messages {
		m := &messages[i]
		if m.ID == turn.RequestID {
			m.ID = user
		}
		if m.ID == turn.Expected {
			m.ID = primary
		}
		if m.TriggeredBy == turn.RequestID {
			m.TriggeredBy = user
		}
	}
	return messages
}

func TestObservedRefusalOutcomes(t *testing.T) {
	for _, turn := range observedRefusalTurns(t) {
		t.Run(turn.RequestID, func(t *testing.T) {
			want := observedRefusalText(turn)
			if want == "" {
				t.Fatal("missing observed refusal")
			}
			reply, err := completedProjectReply(turn.Messages, turn.Expected, turn.RequestID)
			if err != nil || reply.Outcome != "declined" || reply.Text != want {
				t.Fatalf("outcome=%q body_preserved=%t err=%v", reply.Outcome, reply.Text == want, err)
			}
			if err := compactionFailure(true, &turnResult{Outcome: reply.Outcome, Text: reply.Text}); err == nil {
				t.Fatal("refusal accepted as checkpoint summary")
			}
			for _, variant := range []string{"quote", "quoted", "code", "user", "other_turn", "summary", "streaming", "unknown_streaming", "wrong_source", "negation", "with_work", "same_line_work", "ordinary"} {
				t.Run(variant, func(t *testing.T) {
					messages := append([]harkMessage(nil), turn.Messages...)
					for i := range messages {
						m := &messages[i]
						if m.Source != "send_bubble" {
							continue
						}
						switch variant {
						case "quote":
							m.Content = "> " + m.Content
						case "quoted":
							m.Content = "“" + m.Content + "”"
						case "code":
							m.Content = "`" + m.Content + "`"
						case "user":
							m.Role = "user"
						case "other_turn":
							m.TriggeredBy = "unrelated-turn"
						case "summary":
							m.Presentation.Kind = "summary"
						case "streaming":
							yes := true
							m.Streaming = &yes
						case "unknown_streaming":
							m.Streaming = nil
						case "wrong_source":
							m.Source = "web"
						case "negation":
							m.Content = "并不是这样：" + m.Content
						case "with_work":
							m.Content += "\n已执行修改。"
						case "same_line_work":
							m.Content += "已执行修改。"
						case "ordinary":
							m.Content = "普通任务已完成，结果是 2。"
						}
					}
					reply, _ := completedProjectReply(messages, turn.Expected, turn.RequestID)
					if reply.Outcome == "declined" {
						t.Fatal("non-authoritative/ordinary reply classified as refusal")
					}
				})
			}
		})
	}
}
