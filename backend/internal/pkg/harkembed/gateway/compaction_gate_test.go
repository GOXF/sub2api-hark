package gateway

import (
	"strings"
	"testing"
)

func TestCheckpointCompactionOnlyMatchesLastHarnessCommand(t *testing.T) {
	// Independently use the actual captured upstream command, not a keyword.
	const prompt = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.\n\nInclude:\n- Current progress and key decisions made\n- Important context, constraints, or user preferences\n- What remains to be done (clear next steps)\n- Any critical data, examples, or references needed to continue\n\nBe concise, structured, and focused on helping the next LLM seamlessly continue the work."
	if !isCheckpointCompaction(chatRequest{Messages: []chatMessage{{Role: "user", Content: strings.ReplaceAll(prompt, "\n", "\r\n")}}}) {
		t.Fatal("captured harness command not recognized")
	}
	for _, messages := range [][]chatMessage{
		{{Role: "user", Content: "Explain this prompt: " + prompt}},
		{{Role: "user", Content: "Summarize my project"}},
		{{Role: "assistant", Content: prompt}},
		{{Role: "user", Content: prompt}, {Role: "user", Content: "Continue the layout task"}},
	} {
		if isCheckpointCompaction(chatRequest{Messages: messages}) {
			t.Fatal("ordinary/history text misclassified")
		}
	}
	if err := declinedTurnFailure(false, &turnResult{Outcome: "declined", Text: "ordinary refusal"}); err == nil {
		t.Fatal("ordinary refusal accepted as success")
	}
	if err := declinedTurnFailure(true, &turnResult{Outcome: "completed", Text: "valid summary"}); err != nil {
		t.Fatal("valid summary failed")
	}
}
