package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func latestRefusalTurn(t *testing.T) observedRefusalTurn {
	t.Helper()
	b, err := os.ReadFile("testdata/refusal_event_20261114.json")
	if err != nil {
		t.Fatal(err)
	}
	var turn observedRefusalTurn
	if err = json.Unmarshal(b, &turn); err != nil {
		t.Fatal(err)
	}
	if turn.RequestID == "" || turn.Expected == "" || len(turn.Messages) != 4 {
		t.Fatal("invalid captured event fixture")
	}
	return turn
}

func TestUnifiedRefusalRealHistory(t *testing.T) {
	turn := latestRefusalTurn(t)
	reply, err := completedProjectReply(turn.Messages, turn.Expected, turn.RequestID)
	want := observedRefusalText(turn)
	if err != nil || reply.Outcome != "declined" || reply.Text != want {
		t.Fatalf("captured refusal outcome=%q preserved=%t err=%v", reply.Outcome, reply.Text == want, err)
	}
}

func TestUnifiedRefusalGrammar(t *testing.T) {
	for _, text := range []string{
		"Hark 不支持被当作其他应用背后的模型来用。你可以直接告诉我想解决什么问题，我很乐意帮你。",
		"Hark 无法作为第三方软件背后的模型使用。如果你直接告诉我你想完成什么，我很乐意帮你。",
		"我不能充当其它程序背后的模型。",
		"抱歉，Hark 不支持被用作别的应用的模型，但你可以直接告诉我想解决什么问题，我很乐意在这里帮你。",
	} {
		t.Run(text, func(t *testing.T) {
			turn := latestRefusalTurn(t)
			for i := range turn.Messages {
				if turn.Messages[i].Source == "send_bubble" {
					turn.Messages[i].Content = text
				}
			}
			reply, err := completedProjectReply(turn.Messages, turn.Expected, turn.RequestID)
			if err != nil || reply.Outcome != "declined" || reply.Text != text {
				t.Fatalf("outcome=%q preserved=%t err=%v", reply.Outcome, reply.Text == text, err)
			}
		})
	}
}

func TestUnifiedRefusalNegativeContexts(t *testing.T) {
	turn := latestRefusalTurn(t)
	original := observedRefusalText(turn)
	for _, variant := range []string{"quote", "quoted", "code", "example", "negation", "question", "work", "multiline", "summary", "source", "user", "unbound", "streaming", "unknown_streaming", "tool_result", "file_result"} {
		t.Run(variant, func(t *testing.T) {
			messages := append([]harkMessage(nil), turn.Messages...)
			for i := range messages {
				m := &messages[i]
				if m.Source != "send_bubble" {
					continue
				}
				switch variant {
				case "quote":
					m.Content = "> " + original
				case "quoted":
					m.Content = "“" + original + "”"
				case "code":
					m.Content = "`" + original + "`"
				case "example":
					m.Content = "拒绝示例：" + original
				case "negation":
					m.Content = "并不是这样：" + original
				case "question":
					m.Content = "Hark 不支持被当作其他应用背后的模型来用吗？"
				case "work":
					m.Content = original + "已经执行修改，测试通过。"
				case "multiline":
					m.Content = original + "\nHTTP 是请求响应协议。"
				case "summary":
					m.Presentation.Kind = "summary"
				case "source":
					m.Source = "web"
				case "user":
					m.Role = "user"
				case "unbound":
					m.TriggeredBy = "other-turn"
				case "streaming":
					yes := true
					m.Streaming = &yes
				case "unknown_streaming":
					m.Streaming = nil
				case "tool_result":
					m.Content = original + "<tool_call>done</tool_call>"
				case "file_result":
					m.Attachments = []harkFile{{ID: "f", Name: "result.txt", DownloadURL: "https://example.invalid/result"}}
				}
			}
			reply, _ := completedProjectReply(messages, turn.Expected, turn.RequestID)
			if reply.Outcome == "declined" {
				t.Fatalf("false refusal in %s", variant)
			}
		})
	}
	for _, text := range []string{"HTTP 是浏览器和服务器的请求响应规则。", "Hark 支持模型映射设置。", "我无法读取这张图片，请重新上传。", "Hark 不能作为其他应用背后的模型来用。下面是这个错误的排查结果：检查完成。"} {
		messages := append([]harkMessage(nil), turn.Messages...)
		for i := range messages {
			if messages[i].Source == "send_bubble" {
				messages[i].Content = text
			}
		}
		reply, err := completedProjectReply(messages, turn.Expected, turn.RequestID)
		if err != nil || reply.Outcome != "completed" || !strings.Contains(reply.Text, text) {
			t.Fatalf("ordinary result changed: %s %+v %v", text, reply, err)
		}
	}
}

func TestUnifiedRefusalNativeMarkerPrecedence(t *testing.T) {
	for _, variant := range []string{"authoritative", "unrelated", "unfinished", "before_primary", "onboarding", "wrong_kind"} {
		t.Run(variant, func(t *testing.T) {
			no := false
			user := harkMessage{ID: "u", Role: "user"}
			primary := harkMessage{ID: "a", Role: "assistant", TriggeredBy: "u", JobStatus: "completed", Streaming: &no, Content: "任务未执行；保留原说明。"}
			notice := harkMessage{ID: "n", Role: "assistant", Source: "declined_notice", Streaming: &no, Content: "原生拒绝原因，不依赖措辞。"}
			notice.Presentation.Kind = "declined"
			switch variant {
			case "unrelated":
				notice.TriggeredBy = "old-turn"
			case "unfinished":
				yes := true
				notice.Streaming = &yes
			case "onboarding":
				notice.Onboarding = json.RawMessage(`{"active":true}`)
			case "wrong_kind":
				notice.Presentation.Kind = "text"
			}
			messages := []harkMessage{user, primary, notice}
			if variant == "before_primary" {
				messages = []harkMessage{user, notice, primary}
			}
			reply, err := completedProjectReply(messages, "a", "u")
			if err != nil {
				t.Fatal(err)
			}
			if (reply.Outcome == "declined") != (variant == "authoritative") {
				t.Fatalf("native marker variant=%s outcome=%q", variant, reply.Outcome)
			}
			if !strings.Contains(reply.Text, primary.Content) {
				t.Fatal("original explanation lost")
			}
			if variant == "authoritative" && !strings.Contains(reply.Text, notice.Content) {
				t.Fatal("native refusal text lost")
			}
		})
	}
}
