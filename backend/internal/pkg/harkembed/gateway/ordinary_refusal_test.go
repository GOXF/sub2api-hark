package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// The text is transcribed from client screenshots; metadata here is synthetic.
func TestOrdinaryScreenshotRefusalRecognition(t *testing.T) {
	texts := []string{
		"Hark 不能被用作其他应用背后的模型，不过如果你告诉我你想完成什么，我很乐意直接帮你。",
		"Hark 不能被当作其他应用背后的模型来调用。如果你直接告诉我想做成什么，比如“缺少导出 SUB2api-HARK”具体指什么，我很乐意在这里帮你。",
	}
	for _, text := range texts {
		for _, variant := range []string{"exact", "quoted", "example", "code", "multiline", "same_line_work", "other_source", "streaming", "unknown_streaming"} {
			t.Run(text+"/"+variant, func(t *testing.T) {
				raw := map[string]any{"role": "assistant", "source": "send_bubble", "presentation": map[string]string{"kind": "text"}, "isStreaming": false, "content": text}
				switch variant {
				case "quoted":
					raw["content"] = "“" + text + "”"
				case "example":
					raw["content"] = "拒绝示例：" + text
				case "code":
					raw["content"] = "`" + text + "`"
				case "multiline":
					raw["content"] = text + "\n实际任务已完成。"
				case "same_line_work":
					raw["content"] = text + "实际任务已完成。"
				case "other_source":
					raw["source"] = "web"
				case "streaming":
					raw["isStreaming"] = true
				case "unknown_streaming":
					delete(raw, "isStreaming")
				}
				data, err := json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
				var msg harkMessage
				if err = json.Unmarshal(data, &msg); err != nil {
					t.Fatal(err)
				}
				if got := recordedBackendRefusal(msg); got != (variant == "exact") {
					t.Fatalf("variant=%s refusal=%t", variant, got)
				}
			})
		}
	}
	for _, compact := range []bool{false, true} {
		err := declinedTurnFailure(compact, &turnResult{Outcome: "declined", Text: texts[0]})
		u, ok := err.(*upstreamError)
		if !ok || u.status != 422 || !strings.Contains(u.message, texts[0]) {
			t.Fatalf("original refusal lost: %v", err)
		}
	}
}
