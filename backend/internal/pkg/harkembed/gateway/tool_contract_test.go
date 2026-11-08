package gateway

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClientToolRequestContract(t *testing.T) {
	tools := []toolSpec{{Name: "read_file"}}
	for _, tc := range []struct {
		name, choice string
		tools        []toolSpec
		forwardExtra bool
		code         string
	}{
		{"required", "required", tools, false, ""},
		{"named", "read_file", tools, false, ""},
		{"missing_tools", "required", nil, false, "invalid_tool_choice"},
		{"unknown_name", "not_declared", tools, false, "invalid_tool_choice"},
		{"extra_required", "required", tools, true, "tool_choice_unsupported"},
		{"extra_auto", "auto", nil, true, ""},
		{"ordinary", "auto", nil, false, ""},
		{"none", "none", nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClientToolRequest(chatRequest{Tools: tc.tools, ToolChoice: tc.choice}, tc.forwardExtra)
			var upstream *upstreamError
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &upstream) || upstream.code != tc.code || upstream.status != 400 {
				t.Fatalf("err=%v; want pre-submission 400 %s", err, tc.code)
			}
		})
	}
}

func TestNamedClientToolModeWordsRemainNames(t *testing.T) {
	for _, name := range []string{"auto", "none", "required"} {
		t.Run(name, func(t *testing.T) {
			raw := json.RawMessage(`{"type":"function","function":{"name":"` + name + `"}}`)
			if !isNamedToolChoice(raw) {
				t.Fatal("an explicit function name was treated as a mode")
			}
			v := chatRequest{ToolChoice: parseToolChoice(raw), ToolChoiceNamed: true, Tools: []toolSpec{{Name: name}}}
			if !requiresClientTool(v) {
				t.Fatal("named mode word disabled tool enforcement")
			}
			if err := validateClientToolRequest(v, false); err != nil {
				t.Fatal(err)
			}
			if err := validateClientToolResult(v, []toolCall{{Name: name}}); err != nil {
				t.Fatal(err)
			}
			if err := validateClientToolResult(v, nil); err == nil {
				t.Fatal("missing named tool call was accepted")
			}
			if !strings.Contains(toolPrompt(v.Tools, v.ToolChoice, true), `MUST call the tool "`+name+`"`) {
				t.Fatal("tool instructions lost the explicit name")
			}
		})
	}
}

func TestNamedClientToolResultContract(t *testing.T) {
	for _, tc := range []struct {
		name, choice string
		calls        []toolCall
		code         string
	}{
		{"namespaced", "files.read_file", []toolCall{{Namespace: "files", Name: "read_file"}}, ""},
		{"other_namespace", "files.read_file", []toolCall{{Namespace: "other", Name: "read_file"}}, "tool_call_mismatch"},
		{"mixed_calls", "read_file", []toolCall{{Name: "read_file"}, {Name: "other_file"}}, "tool_call_mismatch"},
		{"required_missing", "required", nil, "tool_call_missing"},
		{"named_missing", "read_file", nil, "tool_call_missing"},
		{"auto_plain", "auto", nil, ""},
		{"none_plain", "none", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClientToolResult(chatRequest{ToolChoice: tc.choice}, tc.calls)
			var upstream *upstreamError
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &upstream) || upstream.code != tc.code || upstream.status != 422 {
				t.Fatalf("err=%v; want accepted non-retryable 422 %s", err, tc.code)
			}
		})
	}
}

func TestToolChoiceObjectsHonorRequiredType(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"type":"required"}`, "required"},
		{`{"type":"auto"}`, "auto"},
		{`{"type":"none"}`, "none"},
		{`{"type":"function","name":"read_file"}`, "read_file"},
		{`{"type":"function","function":{"name":"read_file"}}`, "read_file"},
	} {
		if got := parseToolChoice(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("parseToolChoice(%s)=%q; want=%q", tc.raw, got, tc.want)
		}
	}
}
