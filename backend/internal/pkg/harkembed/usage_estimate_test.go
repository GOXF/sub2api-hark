package harkembed

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
)

// gatewayResponse is a response shaped like the in-process gateway's.
func gatewayResponse(contentType, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func estimateRequest(path string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "http://hark.internal"+path, nil)
}

func readEstimated(t *testing.T, req *http.Request, reqBody, contentType, respBody string) (string, *http.Response) {
	t.Helper()
	resp := withUsageEstimate(req, []byte(reqBody), gatewayResponse(contentType, respBody))
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return string(b), resp
}

// sseData returns the data payloads of an SSE body ([DONE] included).
func sseData(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

const reply = "Hello! The capital of France is Paris, a city known for the Eiffel Tower."

func TestCountTokensUsesTokenizer(t *testing.T) {
	if _, method := tokenCodec(); !strings.HasPrefix(method, "tiktoken-") {
		t.Fatalf("method = %q", method)
	}
	if n := countTokens(reply); n < 10 || n > 30 {
		t.Fatalf("countTokens = %d", n)
	}
	if countTokens("") != 0 || approxTokens(9) != 3 || approxTokens(0) != 0 {
		t.Fatal("empty / approx")
	}
	// Text over the exact-count cap is extrapolated, not truncated.
	long := strings.Repeat("hello world ", maxExactCountBytes/6)
	if n := countTokens(long); n < maxExactCountBytes/6 {
		t.Fatalf("long text = %d tokens", n)
	}
}

func TestEstimateInputTokens(t *testing.T) {
	plain := estimateInputTokens(labelChat, []byte(`{"model":"hark","messages":[{"role":"user","content":"What is the capital of France?"}]}`))
	if plain < 8 || plain > 30 {
		t.Fatalf("plain = %d", plain)
	}
	withTools := estimateInputTokens(labelChat, []byte(`{"model":"hark","messages":[{"role":"user","content":"What is the capital of France?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`))
	if withTools <= plain+10 {
		t.Fatalf("tools not counted: %d vs %d", withTools, plain)
	}
	// Images (data URLs, image blocks) are not counted as text.
	img := estimateInputTokens(labelChat, []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"What is the capital of France?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,`+strings.Repeat("QUFB", 5000)+`"}}]}]}`))
	if img > plain+5 {
		t.Fatalf("image counted: %d vs %d", img, plain)
	}
	resp := estimateInputTokens(labelResponses, []byte(`{"instructions":"You are terse.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the capital of France?"}]}]}`))
	if resp <= plain {
		t.Fatalf("responses = %d", resp)
	}
	msg := estimateInputTokens(labelMessages, []byte(`{"system":[{"type":"text","text":"You are terse."}],"messages":[{"role":"user","content":"What is the capital of France?"}]}`))
	if msg <= plain {
		t.Fatalf("messages = %d", msg)
	}
	if estimateInputTokens(labelChat, []byte(`not json`)) != 0 || estimateInputTokens(labelNone, []byte(`{}`)) != 0 {
		t.Fatal("invalid input")
	}
}

const chatReq = `{"model":"hark","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"What is the capital of France?"}]}`

// chatStreamReq is chatReq as sub2api sends it for a streamed raw Chat
// Completions request (it forces stream_options.include_usage).
var chatStreamReq = strings.Replace(chatReq, `"model":"hark",`, `"model":"hark","stream":true,"stream_options":{"include_usage":true},`, 1)

func TestUsageEstimateChatCompletionsJSON(t *testing.T) {
	body := `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"hark","choices":[{"index":0,"message":{"role":"assistant","content":"` + reply + `"},"finish_reason":"stop"}],"usage":{"completion_tokens":0,"prompt_tokens":0,"total_tokens":0}}`
	out, resp := readEstimated(t, estimateRequest("/v1/chat/completions"), chatReq, "application/json", body)
	u := gjson.Get(out, "usage")
	in, o := u.Get("prompt_tokens").Int(), u.Get("completion_tokens").Int()
	if in == 0 || in != int64(estimateInputTokens(labelChat, []byte(chatReq))) || o != int64(countTokens(reply)) || u.Get("total_tokens").Int() != in+o {
		t.Fatalf("usage = %s", u.Raw)
	}
	if gjson.Get(out, "choices.0.message.content").String() != reply {
		t.Fatalf("content changed: %s", out)
	}
	if resp.Header.Get(UsageEstimateHeader) == "" || resp.ContentLength != -1 {
		t.Fatalf("header %v / length %d", resp.Header, resp.ContentLength)
	}
}

func TestUsageEstimateChatCompletionsStream(t *testing.T) {
	deltas := []string{"Hello! The capital ", "of France is Paris, ", "a city known for the Eiffel Tower."}
	var sb strings.Builder
	_, _ = sb.WriteString(`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"hark","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n")
	_, _ = sb.WriteString(": keepalive\n\n")
	for _, d := range deltas {
		_, _ = sb.WriteString(`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"hark","choices":[{"index":0,"delta":{"content":"` + d + `"},"finish_reason":null}]}` + "\n\n")
	}
	_, _ = sb.WriteString(`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"hark","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	_, _ = sb.WriteString("data: [DONE]\n\n")
	stream := sb.String()

	out, _ := readEstimated(t, estimateRequest("/v1/chat/completions"), chatStreamReq, "text/event-stream", stream)
	data := sseData(out)
	if len(data) != 7 || data[6] != "[DONE]" {
		t.Fatalf("events = %q", data)
	}
	usage := data[5]
	if !gjson.Get(usage, "choices").IsArray() || gjson.Get(usage, "choices.#").Int() != 0 {
		t.Fatalf("usage chunk = %s", usage)
	}
	if o := gjson.Get(usage, "usage.completion_tokens").Int(); o == 0 || o != int64(countTokens(strings.Join(deltas, "\n"))) {
		t.Fatalf("completion_tokens = %d", o)
	}
	if gjson.Get(usage, "usage.prompt_tokens").Int() == 0 {
		t.Fatalf("prompt_tokens = %s", usage)
	}
	if !strings.Contains(out, ": keepalive\n\n") || !strings.HasSuffix(out, "\n\ndata: [DONE]\n\n") {
		t.Fatalf("stream framing changed:\n%s", out)
	}
	// Without include_usage the stream is passed through unchanged.
	if out, _ = readEstimated(t, estimateRequest("/v1/chat/completions"), chatReq, "text/event-stream", stream); out != stream {
		t.Fatalf("stream without include_usage changed:\n%s", out)
	}
	// A failed stream gets no usage chunk.
	failed := `data: {"error":{"code":"upstream_error","message":"x","type":"server_error"}}` + "\n\ndata: [DONE]\n\n"
	if out, _ = readEstimated(t, estimateRequest("/v1/chat/completions"), chatStreamReq, "text/event-stream", failed); out != failed {
		t.Fatalf("failed stream changed:\n%s", out)
	}
}

const responsesReq = `{"model":"hark","instructions":"You are helpful.","input":"What is the capital of France?"}`

func TestUsageEstimateResponses(t *testing.T) {
	body := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"hark","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + reply + `","annotations":[]}]}],"output_text":"` + reply + `","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`
	out, _ := readEstimated(t, estimateRequest("/v1/responses"), responsesReq, "application/json", body)
	u := gjson.Get(out, "usage")
	if u.Get("input_tokens").Int() == 0 || u.Get("output_tokens").Int() != int64(countTokens(reply)) ||
		u.Get("total_tokens").Int() != u.Get("input_tokens").Int()+u.Get("output_tokens").Int() {
		t.Fatalf("usage = %s", u.Raw)
	}

	env := func(status, text string) string {
		output := `[]`
		if text != "" {
			output = `[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + text + `","annotations":[]}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}]`
		}
		return `{"id":"resp_1","object":"response","created_at":1,"status":"` + status + `","model":"hark","output":` + output + `,"output_text":"` + text + `","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`
	}
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":" + env("in_progress", "") + "}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello! \"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + env("completed", reply) + "}\n\n"
	out, _ = readEstimated(t, estimateRequest("/v1/responses"), responsesReq, "text/event-stream", stream)
	data := sseData(out)
	if len(data) != 3 {
		t.Fatalf("events = %q", data)
	}
	if gjson.Get(data[0], "response.usage.input_tokens").Int() != 0 {
		t.Fatalf("response.created usage changed: %s", data[0])
	}
	cu := gjson.Get(data[2], "response.usage")
	want := int64(countTokens(reply + "\nget_weather\n{\"city\":\"Paris\"}"))
	if cu.Get("output_tokens").Int() != want || cu.Get("input_tokens").Int() == 0 ||
		cu.Get("total_tokens").Int() != cu.Get("input_tokens").Int()+want {
		t.Fatalf("completed usage = %s, want output %d", cu.Raw, want)
	}
	if !strings.Contains(out, "event: response.completed\n") {
		t.Fatalf("event lines lost:\n%s", out)
	}
}

const messagesReq = `{"model":"hark","system":"You are helpful.","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"What is the capital of France?"}]}]}`

func TestUsageEstimateAnthropicMessages(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"` + reply + `"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	out, _ := readEstimated(t, estimateRequest("/v1/messages"), messagesReq, "application/json", body)
	u := gjson.Get(out, "usage")
	in := int64(estimateInputTokens(labelMessages, []byte(messagesReq)))
	if in == 0 || u.Get("input_tokens").Int() != in ||
		u.Get("output_tokens").Int() != int64(countTokens(reply+"\nget_weather\n{\"city\":\"Paris\"}")) {
		t.Fatalf("usage = %s", u.Raw)
	}

	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-x\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello! The capital of France is Paris, \"}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a city known for the Eiffel Tower.\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":0}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out, _ = readEstimated(t, estimateRequest("/v1/messages"), messagesReq, "text/event-stream", stream)
	data := sseData(out)
	if len(data) != 8 {
		t.Fatalf("events = %q", data)
	}
	if gjson.Get(data[0], "message.usage.input_tokens").Int() != in {
		t.Fatalf("message_start = %s", data[0])
	}
	md := gjson.Get(data[6], "usage")
	if md.Get("output_tokens").Int() != int64(countTokens("Hello! The capital of France is Paris, \na city known for the Eiffel Tower.")) || md.Get("input_tokens").Int() != in {
		t.Fatalf("message_delta = %s", data[6])
	}
	if gjson.Get(data[6], "delta.stop_reason").String() != "end_turn" || !strings.Contains(out, "event: message_delta\n") {
		t.Fatalf("message_delta changed: %s", data[6])
	}
}

func TestUsageEstimateKeepsRealUsage(t *testing.T) {
	chat := `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
	if out, _ := readEstimated(t, estimateRequest("/v1/chat/completions"), chatReq, "application/json", chat); out != chat {
		t.Fatalf("chat usage overwritten: %s", out)
	}
	chatStream := `data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\ndata: [DONE]\n\n"
	if out, _ := readEstimated(t, estimateRequest("/v1/chat/completions"), chatStreamReq, "text/event-stream", chatStream); out != chatStream {
		t.Fatalf("chat stream usage overwritten / duplicated:\n%s", out)
	}
	responses := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output_text\":\"hi\",\"usage\":{\"input_tokens\":5,\"output_tokens\":3,\"total_tokens\":8}}}\n\n"
	if out, _ := readEstimated(t, estimateRequest("/v1/responses"), responsesReq, "text/event-stream", responses); out != responses {
		t.Fatalf("responses usage overwritten: %s", out)
	}
	respJSON := `{"object":"response","output_text":"hi","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
	if out, _ := readEstimated(t, estimateRequest("/v1/responses"), responsesReq, "application/json", respJSON); out != respJSON {
		t.Fatalf("responses json usage overwritten: %s", out)
	}
	msgs := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\n\n"
	out, _ := readEstimated(t, estimateRequest("/v1/messages"), messagesReq, "text/event-stream", msgs)
	data := sseData(out)
	if gjson.Get(data[0], "message.usage.input_tokens").Int() != 42 {
		t.Fatalf("real input overwritten: %s", data[0])
	}
	if md := gjson.Get(data[2], "usage"); md.Get("input_tokens").Exists() || md.Get("output_tokens").Int() == 0 {
		t.Fatalf("message_delta = %s (only the zero output may be filled)", data[2])
	}
	full := "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":3,\"output_tokens\":9}}\n\n"
	if out, _ := readEstimated(t, estimateRequest("/v1/messages"), messagesReq, "text/event-stream", full); out != full {
		t.Fatalf("real message_delta overwritten: %s", out)
	}
	anth := `{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":9,"output_tokens":4}}`
	if out, _ := readEstimated(t, estimateRequest("/v1/messages"), messagesReq, "application/json", anth); out != anth {
		t.Fatalf("messages usage overwritten: %s", out)
	}
}

func TestUsageEstimateSkips(t *testing.T) {
	body := `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`

	// Turned off by the admin setting (context flag set by the service layer).
	req := estimateRequest("/v1/chat/completions")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.HarkUsageEstimateDisabled, true))
	if out, resp := readEstimated(t, req, chatReq, "application/json", body); out != body || resp.Header.Get(UsageEstimateHeader) != "" {
		t.Fatalf("disabled by setting: %s", out)
	}
	// Turned off by HARK_USAGE_ESTIMATE=off.
	t.Setenv("HARK_USAGE_ESTIMATE", "off")
	if out, _ := readEstimated(t, estimateRequest("/v1/chat/completions"), chatReq, "application/json", body); out != body {
		t.Fatalf("disabled by env: %s", out)
	}
	t.Setenv("HARK_USAGE_ESTIMATE", "")

	// Error statuses, other endpoints and error bodies are left alone.
	resp := gatewayResponse("application/json", body)
	resp.StatusCode = http.StatusBadGateway
	if got := withUsageEstimate(estimateRequest("/v1/chat/completions"), []byte(chatReq), resp); got.Header.Get(UsageEstimateHeader) != "" {
		t.Fatal("error status wrapped")
	}
	if got := withUsageEstimate(estimateRequest("/v1/messages/count_tokens"), []byte(chatReq), gatewayResponse("application/json", `{"input_tokens":3}`)); got.Header.Get(UsageEstimateHeader) != "" {
		t.Fatal("count_tokens wrapped")
	}
	errBody := `{"error":{"code":"x","message":"y"}}`
	if out, _ := readEstimated(t, estimateRequest("/v1/chat/completions"), chatReq, "application/json", errBody); out != errBody {
		t.Fatalf("error body changed: %s", out)
	}
}

func TestRequestBodyBytesKeepsBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://hark.internal/v1/chat/completions", strings.NewReader(chatReq))
	r.Header.Set("Content-Type", "application/json")
	if err := forceHarkModel(r); err != nil {
		t.Fatal(err)
	}
	if got := string(requestBodyBytes(r)); got != chatReq {
		t.Fatalf("copy = %s", got)
	}
	b, _ := io.ReadAll(r.Body)
	if string(b) != chatReq {
		t.Fatalf("body consumed: %s", b)
	}
	if len(requestBodyBytes(httptest.NewRequest(http.MethodGet, "http://hark.internal/v1/models", nil))) != 0 {
		t.Fatal("no body")
	}
}
