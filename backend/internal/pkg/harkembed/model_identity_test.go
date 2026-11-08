package harkembed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
)

func TestMappedModelIdentityResponse(t *testing.T) {
	for _, tc := range []struct{ path, body, textPath, end string }{
		{"/v1/chat/completions", `{"messages":[{"role":"user","content":"你使用了什么模型？请直接回答当前会话的模型名称。"}]}`, "choices.0.message.content", "data: [DONE]"},
		{"/v1/responses", `{"input":"What model are you using?"}`, "output.0.content.0.text", "event: response.completed"},
		{"/v1/messages", `{"max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"你是什么模型？"}]}]}`, "content.0.text", "event: message_stop"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.path, stream), func(t *testing.T) {
				body := strings.TrimSuffix(tc.body, "}") + fmt.Sprintf(`,"stream":%v}`, stream)
				req := labelRequest(t, tc.path, body, "claude-opus-4-5")
				if err := forceHarkModel(req); err != nil {
					t.Fatal(err)
				}
				resp := modelIdentityResponse(req)
				if resp == nil {
					t.Fatal("pure model identity query was not answered from mapping")
				}
				defer resp.Body.Close()
				b, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d read=%v", resp.StatusCode, err)
				}
				if stream {
					if !strings.Contains(string(b), tc.end) || !strings.Contains(string(b), "claude-opus-4-5") || resp.Header.Get("Content-Type") != "text/event-stream" {
						t.Fatalf("invalid stream: %s", b)
					}
					var text strings.Builder
					for _, line := range strings.Split(string(b), "\n") {
						if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" && !gjson.Valid(strings.TrimPrefix(line, "data: ")) {
							t.Fatalf("invalid SSE JSON: %s", line)
						}
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						data := strings.TrimPrefix(line, "data: ")
						switch tc.path {
						case "/v1/chat/completions":
							text.WriteString(gjson.Get(data, "choices.0.delta.content").Str)
						case "/v1/responses":
							if gjson.Get(data, "type").Str == "response.output_text.delta" {
								text.WriteString(gjson.Get(data, "delta").Str)
							}
						case "/v1/messages":
							text.WriteString(gjson.Get(data, "delta.text").Str)
						}
					}
					if text.String() != "claude-opus-4-5" {
						t.Fatalf("wrong streamed identity %q", text.String())
					}
				} else if gjson.GetBytes(b, tc.textPath).Str != "claude-opus-4-5" || gjson.GetBytes(b, "model").Str != "claude-opus-4-5" {
					t.Fatalf("wrong mapped identity: %s", b)
				}
				if strings.Contains(string(b), "Hark") {
					t.Fatalf("internal transport leaked into model identity: %s", b)
				}
			})
		}
	}
}

func TestDoMappedModelIdentityIsLocal(t *testing.T) {
	t.Setenv("HARK_DATA_DIR", t.TempDir())
	req := labelRequest(t, "/v1/chat/completions", `{"messages":[{"role":"user","content":"你使用了什么模型？"}]}`, "gpt-5.5")
	req.Header.Set("Authorization", "Bearer fixture-cookie")
	resp, err := Do(req, "http://127.0.0.1:1", 987654)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || gjson.GetBytes(body, "choices.0.message.content").Str != "gpt-5.5" {
		t.Fatalf("local reply failed: status=%d body=%s error=%v", resp.StatusCode, body, err)
	}
	if usage := gjson.GetBytes(body, "usage"); !usage.Exists() || usage.Get("total_tokens").Int() != 0 {
		t.Fatalf("local identity query must not consume upstream tokens: %s", body)
	}
}

func TestMappedModelIdentityWhoAreYouWithAgentContext(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6", "claude-sonnet-4-5-20250929"} {
		t.Run(model, func(t *testing.T) {
			req := labelRequest(t, "/v1/chat/completions", `{"messages":[{"role":"system","content":"You are a coding assistant connected through ACP."},{"role":"user","content":"你是谁"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`, model)
			if err := forceHarkModel(req); err != nil {
				t.Fatal(err)
			}
			resp := modelIdentityResponse(req)
			if resp == nil {
				t.Fatal("who-are-you query with ACP context did not use the selected public model")
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || gjson.GetBytes(body, "choices.0.message.content").Str != model {
				t.Fatalf("model=%q response=%s error=%v", model, body, err)
			}
		})
	}
}

func TestMappedModelIdentityDoesNotExposeWildcardAsModel(t *testing.T) {
	for _, model := range []string{"*", "claude-*"} {
		req := labelRequest(t, "/v1/chat/completions", `{"messages":[{"role":"user","content":"你使用了什么模型？"}]}`, model)
		if err := forceHarkModel(req); err != nil {
			t.Fatal(err)
		}
		if resp := modelIdentityResponse(req); resp != nil {
			resp.Body.Close()
			t.Errorf("routing wildcard %q was presented as a concrete model", model)
		}
	}
}

func TestMappedModelIdentityLeavesOtherRequestsUntouched(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"你是谁"}],"session_end":true,"session_id":"existing-conversation"}`,
		`{"messages":[{"role":"user","content":"介绍 Hark 的模型映射"}]}`,
		`{"messages":[{"role":"user","content":"你是什么模型？然后写一个 Python 程序。"}]}`,
		`{"messages":[{"role":"user","content":"翻译：你是什么模型？"}]}`,
		`{"messages":[{"role":"assistant","content":"你是什么模型？"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"你是什么模型？"},{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`,
		`{"messages":[{"role":"user","content":"你是什么模型？"}],"tool_choice":"required"}`,
		`{"messages":[{"role":"user","content":"你是什么模型？"}],"tool_choice":{"type":"tool","name":"check"}}`,
		`{"messages":[{"role":"user","content":"你是什么模型？"}],"response_format":{"type":"json_object"}}`,
		`{"messages":[{"role":"user","content":"你是什么模型？"}],"stream":"yes"}`,
		`not json`,
	} {
		req := labelRequest(t, "/v1/chat/completions", body, "gpt-5.5")
		if err := forceHarkModel(req); err != nil {
			t.Fatal(err)
		}
		before := string(requestBodyBytes(req))
		if resp := modelIdentityResponse(req); resp != nil {
			resp.Body.Close()
			t.Fatalf("unexpected local response for %s", body)
		}
		if after := string(requestBodyBytes(req)); after != before {
			t.Fatalf("request mutated: %s", body)
		}
	}
	body := `{"messages":[{"role":"user","content":"你是什么模型？"}]}`
	for _, model := range []string{"", "hark"} {
		req := labelRequest(t, "/v1/chat/completions", body, model)
		_ = forceHarkModel(req)
		if resp := modelIdentityResponse(req); resp != nil {
			resp.Body.Close()
			t.Fatalf("unexpected response without mapped model: %q", model)
		}
	}
	req := labelRequest(t, "/v1/chat/completions", body, "gpt-5.5")
	_ = forceHarkModel(req)
	t.Setenv("HARK_MODEL_LABEL", "off")
	if resp := modelIdentityResponse(req); resp != nil {
		resp.Body.Close()
		t.Fatal("disabled model label answered locally")
	}
	t.Setenv("HARK_MODEL_LABEL", "on")
	for _, header := range []string{"X-Hark-Session-End", "X-Session-End"} {
		req.Header.Set(header, "true")
		if resp := modelIdentityResponse(req); resp != nil {
			resp.Body.Close()
			t.Errorf("%s was ignored by a local identity reply", header)
		}
		req.Header.Del(header)
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.HarkModelLabelPrompt, "custom {model}"))
	if resp := modelIdentityResponse(req); resp != nil {
		resp.Body.Close()
		t.Fatal("custom model-label template was overridden")
	}
}
