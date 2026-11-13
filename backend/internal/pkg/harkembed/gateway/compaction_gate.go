package gateway

import "strings"

// Match the observed harness command only at the last message, never a quoted
// prompt in history or an ordinary request mentioning summaries. Custom compact
// prompts are intentionally not guessed from keywords.
func isCheckpointCompaction(v chatRequest) bool {
	if len(v.Messages) == 0 {
		return false
	}
	m := v.Messages[len(v.Messages)-1]
	if m.Role != "user" || len(m.Files) != 0 || m.ToolCalls != "" {
		return false
	}
	const prompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

Be concise, structured, and focused on helping the next LLM seamlessly continue the work.`
	return strings.Join(strings.Fields(m.Content), " ") == strings.Join(strings.Fields(prompt), " ")
}

func compactionFailure(compact bool, res *turnResult) error {
	if compact && res.Outcome == "declined" {
		// 422 is an accepted-turn failure, not a reason to replay the request
		// on another account. Keep the original upstream text as diagnostics.
		return &upstreamError{422, "upstream_compaction_declined", "Upstream refused checkpoint compaction; no successful summary returned. Upstream response: " + res.Text}
	}
	return nil
}
