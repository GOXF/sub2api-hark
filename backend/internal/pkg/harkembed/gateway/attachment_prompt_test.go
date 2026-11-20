package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRenderTurnOmitsClientSystemContextAndLocalPaths(t *testing.T) {
	messages := []chatMessage{
		{Role: "system", Content: "You are Codex, a coding agent."},
		{Role: "user", Content: "# AGENTS.md instructions\n<INSTRUCTIONS>Use Codex.</INSTRUCTIONS>"},
		{Role: "user", Content: "# Files mentioned by the user:\n\n## 项目规范对比模板.xlsx: C:/Users/XIA/DeskBox/项目规范对比模板.xlsx\n\n## My request:\n能读取文件吗？", Files: []inputFile{{Name: "项目规范对比模板.xlsx"}}},
	}
	text, files := renderTurn(messages)
	if len(files) != 1 {
		t.Fatalf("files=%d", len(files))
	}
	for _, unwanted := range []string{"You are Codex", "AGENTS.md", "<INSTRUCTIONS>", "C:/Users"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("outbound text leaked %q: %s", unwanted, text)
		}
	}
	for _, want := range []string{"能读取文件吗？"} {
		if !strings.Contains(text, want) {
			t.Errorf("outbound text missing %q: %s", want, text)
		}
	}
}

func TestProjectBoundaryIsNotBackendPrompt(t *testing.T) {
	message := projectScopedMessage("知道这是什么软件吗？")
	for _, unwanted := range []string{"HARK2API", "PROJECT CONTEXT", "instruction-only", "client", "Client", "backend", "API"} {
		if strings.Contains(message, unwanted) {
			t.Errorf("boundary leaked backend wording %q: %s", unwanted, message)
		}
	}
	if !strings.Contains(message, "知道这是什么软件吗？") {
		t.Fatal("boundary dropped the user request")
	}
}

func TestToolPromptOmitsClientWording(t *testing.T) {
	prompt := toolPrompt([]toolSpec{{Kind: "function", Name: "read_file", Description: "The client reads a file from the user's computer."}}, "auto", false)
	if !strings.Contains(prompt, "TOOLS (run on the user's computer)") {
		t.Fatalf("neutral tool header missing: %s", prompt)
	}
	if strings.Contains(strings.ToLower(prompt), "client") {
		t.Errorf("tool prompt still mentions a client: %s", prompt)
	}
	if !strings.Contains(prompt, "read_file") {
		t.Fatal("tool definition missing")
	}
}

func TestAttachmentRequestsSkipOptionalTools(t *testing.T) {
	if includeToolPrompt(chatRequest{}, []inputFile{{Name: "file.xlsx"}}) {
		t.Fatal("optional tools added to an attachment question")
	}
	if !includeToolPrompt(chatRequest{}, nil) {
		t.Fatal("tools missing from an ordinary request")
	}
	if !includeToolPrompt(chatRequest{ToolChoice: "required"}, []inputFile{{Name: "file.xlsx"}}) {
		t.Fatal("required tools dropped from an attachment request")
	}
}

func TestStripLocalPathsKeepsBasename(t *testing.T) {
	for in, want := range map[string]string{
		`C:/Users/XIA/DeskBox/项目规范对比模板.xlsx`:                    "项目规范对比模板.xlsx",
		`C:\Users\XIA\AppData\Local\Temp\shot.png`:              "shot.png",
		`C:\Users\XIA\AppData\Local\Temp\codex-clipboard-1.png`: "[附件]",
		`/home/user/work/report.csv`:                            "report.csv",
		`see C:/Users/XIA/file.txt.`:                            "see file.txt.",
		`https://example.com/C:/Users/XIA/file.txt stays whole`: "https://example.com/C:/Users/XIA/file.txt stays whole",
	} {
		if got := stripLocalPaths(in); got != want {
			t.Errorf("stripLocalPaths(%q)=%q want=%q", in, got, want)
		}
	}
}

func TestExtractClientRequestDropsCodexEnvironment(t *testing.T) {
	in := "# Files mentioned by the user:\n\n## codex-clipboard-1.png: C:/Users/XIA/AppData/Local/Temp/codex-clipboard-1.png\n\n## My request:\n这是什么软件的界面\n\nUSER:\n<environment_context>\nCodex desktop\n</environment_context>\n<image name=[Image #1] path=\"C:\\Users\\XIA\\codex-clipboard-1.png\">\n</image>"
	if got := extractClientRequest(in); got != "这是什么软件的界面" {
		t.Fatalf("extractClientRequest=%q", got)
	}
}

func TestOldPromptVersionResetsProject(t *testing.T) {
	if !(&projectSession{ConversationID: "old"}).needsPromptReset() {
		t.Fatal("old prompt envelope was not reset")
	}
	if (&projectSession{ConversationID: "new", PromptVersion: projectPromptVersion}).needsPromptReset() {
		t.Fatal("current prompt envelope was reset")
	}
	if (&projectSession{ConversationID: "pending", PendingTurn: &pendingProjectTurn{}}).needsPromptReset() {
		t.Fatal("pending turn was reset")
	}
}

func TestRenderMessageKeepsPathsInsideUserRequest(t *testing.T) {
	m := chatMessage{
		Role:  "user",
		Files: []inputFile{{Name: "file.txt"}},
		Content: "## file.txt: C:/Users/XIA/file.txt\n\n## My request:\n" +
			"修复 C:/work/app/main.go 的报错",
	}
	text := renderMessageText(m)
	if strings.Contains(text, "C:/Users/XIA") || !strings.Contains(text, "C:/work/app/main.go") {
		t.Fatalf("wrong path stripped: %s", text)
	}
}

func TestLastUserMessageIsNeverDropped(t *testing.T) {
	text, _ := renderTurn([]chatMessage{{Role: "user", Content: "# AGENTS.md instructions\n请解释这段规则"}})
	if !strings.Contains(text, "请解释这段规则") {
		t.Fatalf("last user message was dropped: %q", text)
	}
}

func TestAttachmentTurnSanitizesOutboundMessage(t *testing.T) {
	var sent map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/conversations":
			_ = json.NewEncoder(w).Encode(map[string]string{"conversationId": "att-project"})
		case "/api/files/upload/att-project/presign":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"upload_url": "http://" + r.Host + "/upload", "attachment_id": "att-1",
				"s3_key": "key-1", "path": "path-1", "content_type": "application/octet-stream",
			})
		case "/upload":
			w.WriteHeader(http.StatusOK)
		case "/api/files/att-project/att-1/finalize":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case "/api/messages/send":
			_ = json.NewDecoder(r.Body).Decode(&sent)
			var userID string
			_ = json.Unmarshal(sent["idempotencyKey"], &userID)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "att-project", "messageId": userID, "redirected": false})
		case "/api/messages/history":
			var userID, responseID string
			_ = json.Unmarshal(sent["idempotencyKey"], &userID)
			_ = json.Unmarshal(sent["responseMessageId"], &responseID)
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{map[string]any{
				"id": responseID, "role": "assistant", "triggeredByMessageId": userID,
				"jobStatus": "completed", "isStreaming": false, "content": "ok",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server, err := NewEmbedded(t.TempDir(), Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"model":  "hark",
		"stream": false,
		"messages": []any{
			map[string]string{"role": "system", "content": "You are Codex, a coding agent."},
			map[string]string{"role": "user", "content": "# AGENTS.md instructions\n<INSTRUCTIONS>Use Codex.</INSTRUCTIONS>"},
			map[string]any{"role": "user", "content": []any{
				map[string]string{"type": "text", "text": "## 项目规范对比模板.xlsx: C:/Users/XIA/DeskBox/项目规范对比模板.xlsx\n\n## My request:\n能读取文件吗？"},
				map[string]any{"type": "file", "file": map[string]string{"filename": "项目规范对比模板.xlsx", "file_data": base64.StdEncoding.EncodeToString([]byte("xlsx-bytes"))}},
			}},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "read_file", "description": "The client reads a file.", "parameters": map[string]string{"type": "object"},
		}}},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "http://hark.internal/v1/chat/completions", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer fixture-cookie")
	req = req.WithContext(WithEmbeddedAccount(context.Background(), SourceAccount{ID: "fixture", Cookie: "fixture-cookie"}))
	rec := httptest.NewRecorder()
	server.EmbeddedHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var message string
	_ = json.Unmarshal(sent["message"], &message)
	for _, unwanted := range []string{"You are Codex", "AGENTS.md", "<INSTRUCTIONS>", "C:/Users", "HARK2API", "client", "TOOLS (run on the user's computer)"} {
		if strings.Contains(message, unwanted) {
			t.Errorf("outbound message leaked %q: %s", unwanted, message)
		}
	}
	for _, want := range []string{"能读取文件吗？"} {
		if !strings.Contains(message, want) {
			t.Errorf("outbound message missing %q: %s", want, message)
		}
	}
	if !strings.Contains(string(sent["attachments"]), "att-1") {
		t.Errorf("attachment not forwarded: %s", sent["attachments"])
	}
}
