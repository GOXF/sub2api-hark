package harkembed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"
	"go.uber.org/zap"
)

// Estimated token usage. Hark does not report token counts, so the gateway
// answers every request with zero usage and sub2api would bill nothing. For
// Chat Completions, Responses and Anthropic Messages responses of the
// in-process gateway, usage that is missing or all zero is replaced by an
// estimate: input from the request's text (messages, system, instructions,
// input, tool definitions) and output from the generated text (streamed
// deltas included), counted with the o200k_base tokenizer (cl100k_base, then
// ~4 bytes per token, as fallbacks). Non-zero usage is never touched.
//
// The admin setting enable_hark_usage_estimate turns it off for a request by
// setting ctxkey.HarkUsageEstimateDisabled (service layer, Hark accounts
// only); HARK_USAGE_ESTIMATE=off (or 0/false/no/disable/disabled) is a hard
// override that turns it off. Responses carrying the estimator have the
// header UsageEstimateHeader.

// UsageEstimateHeader marks gateway responses whose zero usage is replaced by
// an estimate; its value names the counting method.
const UsageEstimateHeader = "X-Hark-Usage-Estimate"

// maxExactCountBytes caps the text counted with the tokenizer; the rest is
// extrapolated from the counted part's bytes-per-token ratio.
const maxExactCountBytes = 512 << 10

// usageEstimateEnvEnabled reports whether HARK_USAGE_ESTIMATE leaves the
// estimate on.
func usageEstimateEnvEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HARK_USAGE_ESTIMATE"))) {
	case "off", "0", "false", "no", "disable", "disabled":
		return false
	}
	return true
}

func usageEstimateEnabled(req *http.Request) bool {
	if req == nil || !usageEstimateEnvEnabled() {
		return false
	}
	disabled, _ := req.Context().Value(ctxkey.HarkUsageEstimateDisabled).(bool)
	return !disabled
}

var (
	codecOnce   sync.Once
	codec       tokenizer.Codec
	codecMethod = "bytes/4"
)

func tokenCodec() (tokenizer.Codec, string) {
	codecOnce.Do(func() {
		for _, enc := range []tokenizer.Encoding{tokenizer.O200kBase, tokenizer.Cl100kBase} {
			if c, err := tokenizer.Get(enc); err == nil && c != nil {
				codec, codecMethod = c, "tiktoken-"+string(enc)
				return
			}
		}
	})
	return codec, codecMethod
}

// approxTokens is the ~4 bytes per token fallback.
func approxTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + 3) / 4
}

// countTokens estimates the token count of text.
func countTokens(text string) int {
	if text == "" {
		return 0
	}
	c, _ := tokenCodec()
	if c == nil {
		return approxTokens(len(text))
	}
	head := text
	if len(head) > maxExactCountBytes {
		cut := maxExactCountBytes
		for cut > 0 && !utf8RuneStart(head[cut]) {
			cut--
		}
		head = head[:cut]
	}
	n, err := c.Count(head)
	if err != nil {
		return approxTokens(len(text))
	}
	if len(head) < len(text) && len(head) > 0 {
		n += int(float64(n) * float64(len(text)-len(head)) / float64(len(head)))
	}
	return n
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// usageFormat is the API shape of a gateway endpoint (labelFormat values).
type usageFormat = labelFormat

// Structural keys whose string values are not prompt text.
var skipTextKeys = map[string]bool{
	"type": true, "role": true, "id": true, "call_id": true, "tool_call_id": true, "tool_use_id": true,
	"status": true, "cache_control": true, "signature": true, "model": true, "index": true,
	"image_url": true, "file_data": true, "file_id": true, "encrypted_content": true, "detail": true,
}

// Content blocks that carry binary data (images, files) and are not counted.
var skipBlockTypes = map[string]bool{
	"image": true, "image_url": true, "input_image": true, "document": true, "input_file": true,
	"file": true, "input_audio": true, "redacted_thinking": true,
}

// collectText appends the prompt text found in v (strings outside structural
// keys, skipping binary blocks and data URLs) to b.
func collectText(b *strings.Builder, v gjson.Result) {
	switch {
	case v.Type == gjson.String:
		s := v.Str
		if s == "" || strings.HasPrefix(s, "data:") {
			return
		}
		if b.Len() > 0 {
			_ = b.WriteByte('\n')
		}
		_, _ = b.WriteString(s)
	case v.IsArray():
		v.ForEach(func(_, item gjson.Result) bool {
			collectText(b, item)
			return true
		})
	case v.IsObject():
		if skipBlockTypes[v.Get("type").String()] {
			return
		}
		v.ForEach(func(k, item gjson.Result) bool {
			if !skipTextKeys[k.String()] {
				collectText(b, item)
			}
			return true
		})
	}
}

// compactJSON returns raw JSON with insignificant whitespace removed.
func compactJSON(raw string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return raw
	}
	return buf.String()
}

// estimateInputTokens estimates the prompt tokens of a request body.
func estimateInputTokens(format usageFormat, body []byte) int {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0
	}
	var b strings.Builder
	items := 0
	switch format {
	case labelChat:
		msgs := gjson.GetBytes(body, "messages")
		collectText(&b, msgs)
		items = len(msgs.Array())
		for _, k := range []string{"tools", "functions"} {
			if t := gjson.GetBytes(body, k); t.IsArray() {
				_ = b.WriteByte('\n')
				_, _ = b.WriteString(compactJSON(t.Raw))
			}
		}
	case labelResponses:
		collectText(&b, gjson.GetBytes(body, "instructions"))
		in := gjson.GetBytes(body, "input")
		collectText(&b, in)
		if in.IsArray() {
			items = len(in.Array())
		} else if in.Exists() {
			items = 1
		}
		if t := gjson.GetBytes(body, "tools"); t.IsArray() {
			_ = b.WriteByte('\n')
			_, _ = b.WriteString(compactJSON(t.Raw))
		}
	case labelMessages:
		collectText(&b, gjson.GetBytes(body, "system"))
		msgs := gjson.GetBytes(body, "messages")
		collectText(&b, msgs)
		items = len(msgs.Array())
		if t := gjson.GetBytes(body, "tools"); t.IsArray() {
			_ = b.WriteByte('\n')
			_, _ = b.WriteString(compactJSON(t.Raw))
		}
	default:
		return 0
	}
	n := countTokens(b.String())
	if n == 0 && items == 0 {
		return 0
	}
	// Per-message framing (~3 tokens each) plus the reply priming, as in
	// OpenAI's chat token accounting.
	return n + 3*items + 3
}

// usageEstimator rewrites zero usage in one gateway response.
type usageEstimator struct {
	ctx    context.Context
	format usageFormat
	body   []byte
	method string

	inputOnce sync.Once
	input     int

	out          strings.Builder
	includeUsage bool // chat stream: client asked for the usage chunk
	usageSeen    bool // chat stream: a usage chunk was already sent
	failed       bool // stream reported an error
	realInput    bool // messages stream: message_start carried real input
	estimated    bool
}

func (e *usageEstimator) inputTokens() int {
	e.inputOnce.Do(func() { e.input = estimateInputTokens(e.format, e.body) })
	return e.input
}

func (e *usageEstimator) outputTokens() int {
	return countTokens(e.out.String())
}

func (e *usageEstimator) addOut(s string) {
	if s == "" {
		return
	}
	if e.out.Len() > 0 {
		_ = e.out.WriteByte('\n')
	}
	_, _ = e.out.WriteString(s)
}

// zeroUsage reports whether every listed usage field of u is missing or zero.
func zeroUsage(u gjson.Result, fields ...string) bool {
	if !u.Exists() || u.Type == gjson.Null {
		return true
	}
	if !u.IsObject() {
		return false
	}
	for _, f := range fields {
		if u.Get(f).Int() != 0 {
			return false
		}
	}
	return true
}

func (e *usageEstimator) logEstimate(in, out int) {
	e.estimated = true
	logger.FromContext(e.ctx).Debug("hark usage estimated",
		zap.String("method", e.method), zap.Int("input_tokens", in), zap.Int("output_tokens", out))
}

// --- output text of complete objects ---

func (e *usageEstimator) addChatMessageText(choices gjson.Result) {
	choices.ForEach(func(_, ch gjson.Result) bool {
		m := ch.Get("message")
		e.addOut(m.Get("content").String())
		e.addOut(m.Get("reasoning_content").String())
		m.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			e.addOut(tc.Get("function.name").String())
			e.addOut(tc.Get("function.arguments").String())
			return true
		})
		return true
	})
}

// responsesOutputText is the generated text of a Responses object's output.
func responsesOutputText(resp gjson.Result) string {
	var b strings.Builder
	add := func(s string) {
		if s == "" {
			return
		}
		if b.Len() > 0 {
			_ = b.WriteByte('\n')
		}
		_, _ = b.WriteString(s)
	}
	resp.Get("output").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "message":
			item.Get("content").ForEach(func(_, c gjson.Result) bool {
				add(c.Get("text").String())
				add(c.Get("refusal").String())
				return true
			})
		case "function_call":
			add(item.Get("name").String())
			add(item.Get("arguments").String())
		case "custom_tool_call":
			add(item.Get("name").String())
			add(item.Get("input").String())
		case "local_shell_call":
			add(item.Get("action").Raw)
		case "reasoning":
			item.Get("summary").ForEach(func(_, s gjson.Result) bool {
				add(s.Get("text").String())
				return true
			})
		}
		return true
	})
	if b.Len() == 0 {
		add(resp.Get("output_text").String())
	}
	return b.String()
}

func anthropicContentText(content gjson.Result) string {
	var b strings.Builder
	add := func(s string) {
		if s == "" {
			return
		}
		if b.Len() > 0 {
			_ = b.WriteByte('\n')
		}
		_, _ = b.WriteString(s)
	}
	content.ForEach(func(_, c gjson.Result) bool {
		switch c.Get("type").String() {
		case "text":
			add(c.Get("text").String())
		case "thinking":
			add(c.Get("thinking").String())
		case "tool_use":
			add(c.Get("name").String())
			if in := c.Get("input"); in.Exists() {
				add(compactJSON(in.Raw))
			}
		}
		return true
	})
	return b.String()
}

// patchJSON fills zero usage in a non-streamed response body.
func (e *usageEstimator) patchJSON(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !gjson.ValidBytes(trimmed) {
		return body
	}
	root := gjson.ParseBytes(trimmed)
	if root.Get("error").Exists() {
		return body
	}
	switch e.format {
	case labelChat:
		if !zeroUsage(root.Get("usage"), "prompt_tokens", "completion_tokens", "total_tokens") {
			return body
		}
		e.addChatMessageText(root.Get("choices"))
		in, out := e.inputTokens(), e.outputTokens()
		return e.setUsage(body, trimmed, "usage", map[string]int{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out})
	case labelResponses:
		if !zeroUsage(root.Get("usage"), "input_tokens", "output_tokens", "total_tokens") {
			return body
		}
		e.addOut(responsesOutputText(root))
		in, out := e.inputTokens(), e.outputTokens()
		return e.setUsage(body, trimmed, "usage", map[string]int{"input_tokens": in, "output_tokens": out, "total_tokens": in + out})
	case labelMessages:
		if !zeroUsage(root.Get("usage"), "input_tokens", "output_tokens") {
			return body
		}
		e.addOut(anthropicContentText(root.Get("content")))
		in, out := e.inputTokens(), e.outputTokens()
		return e.setUsage(body, trimmed, "usage", map[string]int{"input_tokens": in, "output_tokens": out})
	}
	return body
}

// setUsage writes the given counts under path (keeping other usage fields).
func (e *usageEstimator) setUsage(orig, body []byte, path string, counts map[string]int) []byte {
	out := body
	if u := gjson.GetBytes(body, path); !u.IsObject() {
		var err error
		if out, err = sjson.SetRawBytes(out, path, []byte("{}")); err != nil {
			return orig
		}
	}
	for _, k := range sortedKeys(counts) {
		var err error
		if out, err = sjson.SetBytes(out, path+"."+k, counts[k]); err != nil {
			return orig
		}
	}
	in, o := counts["input_tokens"]+counts["prompt_tokens"], counts["output_tokens"]+counts["completion_tokens"]
	e.logEstimate(in, o)
	return out
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for _, k := range []string{"input_tokens", "prompt_tokens", "output_tokens", "completion_tokens", "total_tokens"} {
		if _, ok := m[k]; ok {
			keys = append(keys, k)
		}
	}
	return keys
}

// --- streams ---

// streamData handles one SSE data payload; it returns the (possibly
// rewritten) payload and any data payloads to send before it.
func (e *usageEstimator) streamData(payload []byte) (before [][]byte, out []byte) {
	trimmed := bytes.TrimSpace(payload)
	if e.format == labelChat && string(trimmed) == "[DONE]" {
		if e.includeUsage && !e.usageSeen && !e.failed {
			if chunk := e.chatUsageChunk(); chunk != nil {
				before = append(before, chunk)
			}
		}
		return before, payload
	}
	if len(trimmed) == 0 || trimmed[0] != '{' || !gjson.ValidBytes(trimmed) {
		return nil, payload
	}
	ev := gjson.ParseBytes(trimmed)
	switch e.format {
	case labelChat:
		return nil, e.chatChunk(payload, trimmed, ev)
	case labelResponses:
		return nil, e.responsesEvent(payload, trimmed, ev)
	case labelMessages:
		return nil, e.messagesEvent(payload, trimmed, ev)
	}
	return nil, payload
}

func (e *usageEstimator) chatChunk(orig, body []byte, ev gjson.Result) []byte {
	if ev.Get("error").Exists() {
		e.failed = true
		return orig
	}
	ev.Get("choices").ForEach(func(_, ch gjson.Result) bool {
		d := ch.Get("delta")
		e.addOut(d.Get("content").String())
		e.addOut(d.Get("reasoning_content").String())
		d.Get("tool_calls").ForEach(func(_, tc gjson.Result) bool {
			e.addOut(tc.Get("function.name").String())
			e.addOut(tc.Get("function.arguments").String())
			return true
		})
		return true
	})
	u := ev.Get("usage")
	if !u.Exists() || u.Type == gjson.Null {
		return orig
	}
	e.usageSeen = true
	if !zeroUsage(u, "prompt_tokens", "completion_tokens", "total_tokens") {
		return orig
	}
	in, out := e.inputTokens(), e.outputTokens()
	return e.setUsage(orig, body, "usage", map[string]int{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out})
}

// chatUsageChunk is the usage-only final chunk of a Chat Completions stream
// (stream_options.include_usage).
func (e *usageEstimator) chatUsageChunk() []byte {
	in, out := e.inputTokens(), e.outputTokens()
	chunk := map[string]any{
		"id": "chatcmpl_usage", "object": "chat.completion.chunk", "model": Model, "choices": []any{},
		"usage": map[string]int{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	e.usageSeen = true
	e.logEstimate(in, out)
	return b
}

func (e *usageEstimator) responsesEvent(orig, body []byte, ev gjson.Result) []byte {
	switch ev.Get("type").String() {
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
		"response.refusal.delta", "response.reasoning_summary_text.delta":
		e.addOut(ev.Get("delta").String())
		return orig
	case "response.completed", "response.incomplete":
		resp := ev.Get("response")
		if !zeroUsage(resp.Get("usage"), "input_tokens", "output_tokens", "total_tokens") {
			return orig
		}
		if text := responsesOutputText(resp); text != "" {
			e.out.Reset() // the final output supersedes the streamed deltas
			e.addOut(text)
		}
		in, out := e.inputTokens(), e.outputTokens()
		return e.setUsage(orig, body, "response.usage", map[string]int{"input_tokens": in, "output_tokens": out, "total_tokens": in + out})
	}
	return orig
}

func (e *usageEstimator) messagesEvent(orig, body []byte, ev gjson.Result) []byte {
	switch ev.Get("type").String() {
	case "message_start":
		u := ev.Get("message.usage")
		if u.Get("input_tokens").Int() != 0 || u.Get("cache_read_input_tokens").Int() != 0 || u.Get("cache_creation_input_tokens").Int() != 0 {
			e.realInput = true
			return orig
		}
		out, err := sjson.SetBytes(body, "message.usage.input_tokens", e.inputTokens())
		if err != nil {
			return orig
		}
		return out
	case "content_block_start":
		if cb := ev.Get("content_block"); cb.Get("type").String() == "tool_use" {
			e.addOut(cb.Get("name").String())
		}
	case "content_block_delta":
		d := ev.Get("delta")
		e.addOut(d.Get("text").String())
		e.addOut(d.Get("partial_json").String())
		e.addOut(d.Get("thinking").String())
	case "message_delta":
		u := ev.Get("usage")
		if u.Get("output_tokens").Int() != 0 {
			return orig
		}
		counts := map[string]int{"output_tokens": e.outputTokens()}
		if !e.realInput && u.Get("input_tokens").Int() == 0 {
			counts["input_tokens"] = e.inputTokens()
		}
		return e.setUsage(orig, body, "usage", counts)
	case "error":
		e.failed = true
	}
	return orig
}

// --- body wrappers ---

// streamBody rewrites an SSE body line by line.
type streamBody struct {
	src  io.ReadCloser
	r    *bufio.Reader
	e    *usageEstimator
	buf  []byte
	err  error
	done bool
}

func (s *streamBody) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		if s.done {
			return 0, s.err
		}
		line, err := s.r.ReadBytes('\n')
		if len(line) > 0 {
			s.buf = s.process(line)
		}
		if err != nil {
			s.done, s.err = true, err
		}
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *streamBody) process(line []byte) []byte {
	content := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(content, []byte("data:")) {
		return line
	}
	eol := line[len(content):]
	if len(eol) == 0 {
		eol = []byte("\n")
	}
	payload := bytes.TrimPrefix(content[len("data:"):], []byte(" "))
	before, out := s.e.streamData(payload)
	if len(before) == 0 && bytes.Equal(out, payload) {
		return line
	}
	var b bytes.Buffer
	for _, chunk := range before {
		_, _ = b.WriteString("data: ")
		_, _ = b.Write(chunk)
		_, _ = b.Write(eol)
		_, _ = b.Write(eol)
	}
	_, _ = b.WriteString("data: ")
	_, _ = b.Write(out)
	_, _ = b.Write(eol)
	return b.Bytes()
}

func (s *streamBody) Close() error { return s.src.Close() }

// jsonBody reads a non-streamed body on first Read and patches its usage.
type jsonBody struct {
	src  io.ReadCloser
	e    *usageEstimator
	r    *bytes.Reader
	err  error
	read bool
}

func (j *jsonBody) Read(p []byte) (int, error) {
	if !j.read {
		j.read = true
		body, err := io.ReadAll(j.src)
		if err != nil && !errors.Is(err, io.EOF) {
			j.err = err
		}
		if j.err == nil {
			body = j.e.patchJSON(body)
		}
		j.r = bytes.NewReader(body)
	}
	n, err := j.r.Read(p)
	if err == io.EOF && j.err != nil {
		return n, j.err
	}
	return n, err
}

func (j *jsonBody) Close() error { return j.src.Close() }

// withUsageEstimate wraps a successful gateway response of a Chat
// Completions, Responses or Anthropic Messages request (body = the request
// body as the client sent it, after the model rewrite) so that zero usage is
// replaced by an estimate.
func withUsageEstimate(req *http.Request, body []byte, resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK || req == nil || req.URL == nil ||
		req.Method != http.MethodPost || !usageEstimateEnabled(req) {
		return resp
	}
	format := labelFormatOf(req.URL.Path)
	if format == labelNone {
		return resp
	}
	_, method := tokenCodec()
	e := &usageEstimator{ctx: req.Context(), format: format, body: body, method: method}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case strings.Contains(ct, "text/event-stream"):
		e.includeUsage = gjson.GetBytes(body, "stream_options.include_usage").Bool()
		resp.Body = &streamBody{src: resp.Body, r: bufio.NewReaderSize(resp.Body, 64<<10), e: e}
	case strings.Contains(ct, "json"):
		resp.Body = &jsonBody{src: resp.Body, e: e}
	default:
		return resp
	}
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	resp.Header.Set(UsageEstimateHeader, method)
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return resp
}

// requestBodyBytes returns a copy of req's body without consuming it.
func requestBodyBytes(req *http.Request) []byte {
	if req == nil || req.GetBody == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return b
}
