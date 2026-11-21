package harkembed

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPublicActivityUsageUnchanged(t *testing.T) {
	activity := `{"type":"reasoning","id":"rs_activity_fixture","summary":[{"type":"summary_text","text":"PUBLIC_STATUS_ONLY"}]}`
	reasoning := `{"type":"reasoning","id":"rs_generated","summary":[{"type":"summary_text","text":"Generated reasoning"}]}`
	message := `{"type":"message","content":[{"type":"output_text","text":"` + reply + `"}]}`
	body := `{"output":[` + activity + `,` + reasoning + `,` + message + `],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`
	want := int64(countTokens("Generated reasoning\n" + reply))
	out, _ := readEstimated(t, estimateRequest("/v1/responses"), responsesReq, "application/json", body)
	if got := gjson.Get(out, "usage.output_tokens").Int(); got != want {
		t.Fatalf("public activity billed in final output: got %d want %d", got, want)
	}
	for _, finalOutput := range []bool{true, false} {
		t.Run(fmt.Sprint(finalOutput), func(t *testing.T) {
			final := body
			if !finalOutput {
				final = `{"output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`
			}
			stream := "data: " + `{"type":"response.reasoning_summary_text.delta","item_id":"rs_activity_fixture","delta":"PUBLIC_STATUS_ONLY"}` + "\n\n" +
				"data: " + `{"type":"response.reasoning_summary_text.delta","item_id":"rs_generated","delta":"Generated reasoning"}` + "\n\n" +
				"data: " + `{"type":"response.output_text.delta","item_id":"msg_1","delta":"` + reply + `"}` + "\n\n" +
				"data: " + `{"type":"response.completed","response":` + final + "}\n\n"
			out, _ := readEstimated(t, estimateRequest("/v1/responses"), responsesReq, "text/event-stream", stream)
			data := sseData(out)
			if got := gjson.Get(data[len(data)-1], "response.usage.output_tokens").Int(); got != want {
				t.Fatalf("public activity billed in stream: got %d want %d", got, want)
			}
			if !strings.Contains(out, "PUBLIC_STATUS_ONLY") {
				t.Fatal("status removed instead of excluded from billing")
			}
		})
	}
	base := `{"input":[{"type":"message","role":"user","content":"hello"}]}`
	withStatus := `{"input":[` + activity + `,{"type":"message","role":"user","content":"hello"}]}`
	if estimateInputTokens(labelResponses, []byte(base)) != estimateInputTokens(labelResponses, []byte(withStatus)) {
		t.Fatal("returned public activity billed as input context")
	}
	t.Log("PUBLIC_ACTIVITY_NONBILLABLE=true GENERATED_REASONING_STILL_COUNTED=true")
}

func TestActualGatewayPublicActivityUsage(t *testing.T) {
	// The external transaction supplies literal SSE emitted by the actual gateway.
	wire := os.Getenv("HARK_ACTIVITY_GATEWAY_WIRE")
	if wire == "" {
		t.Skip("explicit actual-gateway wire proof only")
	}
	data, err := os.ReadFile(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "response.reasoning_summary_text.delta") {
		t.Fatal("not an activity-bearing gateway stream")
	}
	out, _ := readEstimated(t, estimateRequest("/v1/responses"), responsesReq, "text/event-stream", string(data))
	events := sseData(out)
	last := gjson.Parse(events[len(events)-1]).Get("response")
	answer := "Example Domain\nSYNTHETIC_ACTIVITY_FINAL"
	if got := last.Get("usage.output_tokens").Int(); got != int64(countTokens(answer)) {
		t.Fatalf("actual gateway status billed: output tokens=%d want=%d", got, countTokens(answer))
	}
	if last.Get("output_text").String() != answer || !strings.Contains(out, "正在阅读页面") {
		t.Fatal("usage wrapper changed answer or activity")
	}
	t.Logf("ACTUAL_GATEWAY_STATUS_VISIBLE=true OUTPUT_TOKENS=%d ANSWER_ONLY=true", countTokens(answer))
}
