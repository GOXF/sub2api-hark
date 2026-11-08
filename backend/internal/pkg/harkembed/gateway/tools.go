package gateway

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// 工具调用模拟：Hark 本身不支持 OpenAI 的 function calling，所以把客户端给的 tools
// 写进发给 Hark 的消息里，让 Hark 用 <tool_call> 块回复；网关再把这些块解析成
// Chat Completions 的 tool_calls 或 Responses 的 function_call / custom_tool_call /
// local_shell_call，交给客户端（如 Codex）在本机执行，结果下一轮再带回来。

// toolSpec 客户端声明的一个工具。
type toolSpec struct {
	Kind        string          // function / custom / local_shell
	Name        string          // 工具名（命名空间里的工具是 "命名空间.工具名"）
	Namespace   string          // Responses 的 namespace 工具组名
	Inner       string          // 命名空间里的原始工具名
	Description string          // 说明
	Parameters  json.RawMessage // function 的 JSON Schema
	Format      json.RawMessage // custom 工具的输入格式（语法等）
}

// toolCall 从 Hark 回复里解析出的一次调用。
type toolCall struct {
	ID        string // call_...
	Kind      string // function / custom / local_shell
	Name      string // 原始工具名（不含命名空间）
	Namespace string
	Arguments string // function：JSON 字符串；custom：原始输入；local_shell：JSON（command/workdir/timeout_ms）
}

const maxTools = 128

// parseTools 解析 Chat Completions 和 Responses 两种写法的 tools。不认识的工具类型（web_search 等）忽略。
func parseTools(raw json.RawMessage) ([]toolSpec, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, inputError("invalid_request", "tools 必须是数组")
	}
	if len(items) > maxTools {
		return nil, inputError("invalid_request", "tools 过多")
	}
	var out []toolSpec
	seen := map[string]bool{}
	var walk func(items []map[string]json.RawMessage, ns string)
	walk = func(items []map[string]json.RawMessage, ns string) {
		for _, it := range items {
			var typ string
			json.Unmarshal(it["type"], &typ)
			var t toolSpec
			switch typ {
			case "namespace":
				var name string
				var inner []map[string]json.RawMessage
				json.Unmarshal(it["name"], &name)
				json.Unmarshal(it["tools"], &inner)
				if name != "" && ns == "" {
					walk(inner, name)
				}
				continue
			case "function", "":
				t.Kind = "function"
				src := it
				if fn, ok := it["function"]; ok { // Chat Completions：{"type":"function","function":{...}}
					var m map[string]json.RawMessage
					if json.Unmarshal(fn, &m) == nil && m != nil {
						src = m
					}
				}
				json.Unmarshal(src["name"], &t.Name)
				json.Unmarshal(src["description"], &t.Description)
				t.Parameters = src["parameters"]
			case "custom":
				t.Kind = "custom"
				src := it
				if c, ok := it["custom"]; ok {
					var m map[string]json.RawMessage
					if json.Unmarshal(c, &m) == nil && m != nil {
						src = m
					}
				}
				json.Unmarshal(src["name"], &t.Name)
				json.Unmarshal(src["description"], &t.Description)
				t.Format = src["format"]
			case "local_shell":
				t.Kind, t.Name = "local_shell", "local_shell"
				t.Description = "在客户端本机执行命令。command 是参数数组（例如 [\"bash\",\"-lc\",\"ls\"]），workdir 可选，timeout_ms 可选。"
				t.Parameters = json.RawMessage(`{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}},"workdir":{"type":"string"},"timeout_ms":{"type":"integer"}},"required":["command"]}`)
			default:
				continue
			}
			t.Name = strings.TrimSpace(t.Name)
			if t.Name == "" {
				continue
			}
			t.Inner = t.Name
			if ns != "" {
				t.Namespace, t.Name = ns, ns+"."+t.Name
			}
			if seen[t.Name] || len(out) >= maxTools {
				continue
			}
			seen[t.Name] = true
			out = append(out, t)
		}
	}
	walk(items, "")
	return out, nil
}

// parseToolChoice 返回 auto / none / required，或指定的工具名。
func parseToolChoice(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "auto"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return "auto"
		}
		return s
	}
	var m struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &m) == nil {
		if m.Function.Name != "" {
			return m.Function.Name
		}
		if m.Name != "" {
			return m.Name
		}
		if m.Type == "required" || m.Type == "auto" || m.Type == "none" {
			return m.Type
		}
	}
	return "auto"
}

// toolPrompt 附在发给 Hark 的消息后面的工具说明。每轮都带：Project 里早先的说明可能被压缩掉。
func toolPrompt(tools []toolSpec, choice string, named bool) string {
	if len(tools) == 0 || choice == "none" && !named {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\nCLIENT TOOLS (the client runs them on the user's own computer)\n")
	b.WriteString("The files, folders, programs and shell the user mentions are on the client's computer, not in your workspace. ")
	b.WriteString("You cannot reach them with your own tools, sandbox, browser or file bridge. To read, search, run or change anything there, call one of the client tools below; ")
	b.WriteString("the client executes the call and sends the result back in a later TOOL message (marked with its call_id).\n\n")
	b.WriteString("To call a tool, write a block exactly like this (JSON object with \"name\" and \"arguments\"):\n")
	b.WriteString("<tool_call>\n{\"name\": \"TOOL_NAME\", \"arguments\": {...}}\n</tool_call>\n")
	hasCustom := false
	for _, t := range tools {
		if t.Kind == "custom" {
			hasCustom = true
		}
	}
	if hasCustom {
		b.WriteString("For a FREEFORM tool, put the raw input between the tags instead of JSON, with the name in the opening tag:\n")
		b.WriteString("<tool_call name=\"TOOL_NAME\">\nraw input here\n</tool_call>\n")
	}
	b.WriteString("\nRules:\n")
	b.WriteString("- When you call tools, reply with only the <tool_call> block(s), optionally after one short sentence. Do not wrap them in code fences. Do not invent results; wait for the TOOL message.\n")
	b.WriteString("- You may put several <tool_call> blocks in one reply when the calls do not depend on each other.\n")
	b.WriteString("- When the task is done or you need nothing more from the computer, answer normally with no <tool_call> block.\n")
	b.WriteString("- Never say you cannot access the user's files or directories: use the tools.\n")
	switch {
	case named:
		b.WriteString("- This turn you MUST call the tool \"" + choice + "\".\n")
	case choice == "auto" || choice == "":
	case choice == "required":
		b.WriteString("- This turn you MUST call at least one tool.\n")
	default:
		b.WriteString("- This turn you MUST call the tool \"" + choice + "\".\n")
	}
	b.WriteString("\nAvailable tools:\n")
	for _, t := range tools {
		b.WriteString("\n### " + t.Name)
		if t.Kind == "custom" {
			b.WriteString(" (FREEFORM: raw input, not JSON)")
		}
		b.WriteString("\n")
		if d := strings.TrimSpace(t.Description); d != "" {
			b.WriteString(d + "\n")
		}
		if len(bytes.TrimSpace(t.Parameters)) > 0 && !bytes.Equal(bytes.TrimSpace(t.Parameters), []byte("null")) {
			b.WriteString("arguments JSON Schema: " + compactJSON(t.Parameters) + "\n")
		}
		if t.Kind == "custom" && len(t.Format) > 0 {
			var f struct {
				Type       string `json:"type"`
				Syntax     string `json:"syntax"`
				Definition string `json:"definition"`
			}
			if json.Unmarshal(t.Format, &f) == nil && f.Definition != "" {
				b.WriteString("input grammar (" + f.Syntax + "):\n" + f.Definition + "\n")
			}
		}
	}
	return b.String()
}

func isNamedToolChoice(raw json.RawMessage) bool {
	var selection struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	return json.Unmarshal(raw, &selection) == nil && (selection.Name != "" || selection.Function.Name != "")
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

var (
	toolCallRe  = regexp.MustCompile(`(?s)<tool_call(\s+name\s*=\s*"([^"]*)")?\s*>(.*?)</tool_call>`)
	emptyFence  = regexp.MustCompile("(?m)^[ \t]*```[a-zA-Z0-9_-]*[ \t]*\n(\\s*\n)*[ \t]*```[ \t]*$")
	innerFence  = regexp.MustCompile("(?s)^\\s*```[a-zA-Z0-9_-]*\\s*\n(.*?)\n?\\s*```\\s*$")
	blankLines3 = regexp.MustCompile(`\n{3,}`)
)

// extractToolCalls 从 Hark 的回复里取出 <tool_call> 块；返回去掉这些块后的文字和调用列表。
// 名字不是已声明工具的块原样留在文字里。
func extractToolCalls(text string, tools []toolSpec) (string, []toolCall) {
	if len(tools) == 0 || !strings.Contains(text, "<tool_call") {
		return text, nil
	}
	byName := map[string]toolSpec{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	var calls []toolCall
	out := toolCallRe.ReplaceAllStringFunc(text, func(block string) string {
		m := toolCallRe.FindStringSubmatch(block)
		tagName, body := strings.TrimSpace(m[2]), m[3]
		if f := innerFence.FindStringSubmatch(body); f != nil {
			body = f[1]
		}
		trimmed := strings.TrimSpace(body)
		name := tagName
		var args json.RawMessage
		if strings.HasPrefix(trimmed, "{") {
			var obj struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Input     *string         `json:"input"`
			}
			if json.Unmarshal([]byte(trimmed), &obj) == nil && (obj.Name != "" || tagName != "") {
				if obj.Name != "" {
					name = obj.Name
				}
				args = obj.Arguments
				if obj.Input != nil && len(args) == 0 {
					b, _ := json.Marshal(map[string]string{"input": *obj.Input})
					args = b
				}
			}
		}
		t, ok := byName[name]
		if !ok {
			return block
		}
		c := toolCall{ID: randomID("call_")[:29], Kind: t.Kind, Name: t.Inner, Namespace: t.Namespace}
		switch t.Kind {
		case "custom":
			if len(args) > 0 {
				var in struct {
					Input string `json:"input"`
				}
				var s string
				if json.Unmarshal(args, &in) == nil && in.Input != "" {
					c.Arguments = in.Input
				} else if json.Unmarshal(args, &s) == nil {
					c.Arguments = s
				} else {
					c.Arguments = string(args)
				}
			} else {
				c.Arguments = strings.Trim(body, "\n")
			}
		default:
			c.Arguments = normalizeArguments(args)
		}
		calls = append(calls, c)
		return ""
	})
	if len(calls) == 0 {
		return text, nil
	}
	out = emptyFence.ReplaceAllString(out, "")
	out = blankLines3.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out), calls
}

// normalizeArguments 把 arguments 统一成 JSON 对象字符串（模型有时给的是字符串化的 JSON）。
func normalizeArguments(args json.RawMessage) string {
	args = bytes.TrimSpace(args)
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		return "{}"
	}
	var s string
	if json.Unmarshal(args, &s) == nil {
		var obj map[string]any
		if json.Unmarshal([]byte(s), &obj) == nil {
			return compactJSON(json.RawMessage(s))
		}
		return "{}"
	}
	return compactJSON(args)
}

// toolSafePrefix 流式输出时可以先发给客户端的部分：到第一个 <tool_call 之前，
// 末尾可能是 "<tool_call" 开头一截的字符先留着。
func toolSafePrefix(partial string) string {
	if i := strings.Index(partial, "<tool_call"); i >= 0 {
		partial = partial[:i]
		// 去掉紧挨着的未闭合代码块开头（```xml 等）
		if j := strings.LastIndex(partial, "```"); j >= 0 && strings.Count(partial, "```")%2 == 1 {
			partial = partial[:j]
		}
		return partial
	}
	const tag = "<tool_call"
	for k := len(tag) - 1; k > 0; k-- {
		if strings.HasSuffix(partial, tag[:k]) {
			return partial[:len(partial)-k]
		}
	}
	if j := strings.LastIndex(partial, "```"); j >= 0 && strings.Count(partial, "```")%2 == 1 && len(partial)-j < 24 {
		return partial[:j] // 刚开的代码块可能包着工具调用，先不发
	}
	return partial
}

// localShellAction local_shell_call 的 action。
func localShellAction(args string) map[string]any {
	var a struct {
		Command   []string `json:"command"`
		Workdir   string   `json:"workdir"`
		TimeoutMS int      `json:"timeout_ms"`
	}
	json.Unmarshal([]byte(args), &a)
	if a.Command == nil {
		a.Command = []string{}
	}
	act := map[string]any{"type": "exec", "command": a.Command, "env": map[string]string{}}
	if a.Workdir != "" {
		act["working_directory"] = a.Workdir
	}
	if a.TimeoutMS > 0 {
		act["timeout_ms"] = a.TimeoutMS
	}
	return act
}

// toolNames 日志用。
func toolNames(calls []toolCall) string {
	n := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.Namespace != "" {
			n = append(n, c.Namespace+"."+c.Name)
			continue
		}
		n = append(n, c.Name)
	}
	sort.Strings(n)
	return strings.Join(n, ",")
}
