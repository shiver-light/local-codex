package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"local-codex/internal/llm"
	"local-codex/internal/logging"
	"local-codex/internal/session"
)

// compactSummaryMaxTokens caps the summary completion; compaction must stay
// cheap relative to the main loop.
const compactSummaryMaxTokens = 1024

// compactMinSavingsBytes is the minimum absolute reduction a summary must
// achieve over the batch it replaces; below this (or below a 2x ratio) the
// compaction is not worth the degraded context and the caller falls back to
// hard truncation.
const compactMinSavingsBytes = 512

// defaultCompactKeepRecent is how many trailing messages are never
// compacted: the model needs its most recent exchanges verbatim.
const defaultCompactKeepRecent = 6

func (a *Agent) keepRecent() int {
	if a.CompactKeepRecent <= 0 {
		return defaultCompactKeepRecent
	}
	return a.CompactKeepRecent
}

// compactHistory replaces the oldest history with an LLM-generated summary
// when the history exceeds the byte budget. The original task message and
// the most recent keepRecent messages are never touched, and the batch
// boundary never splits a tool_call/tool_result group. On any failure
// (summary call error, empty summary, insufficient savings, nothing safe to
// compact) it falls back to hard truncation; compaction must never abort the
// task. At most one summary call is made per invocation: if the compacted
// history still exceeds the budget, hard truncation covers the rest.
func (a *Agent) compactHistory(ctx context.Context, sess *session.Session, history []llm.Message, budget, iter int) []llm.Message {
	units := splitUnits(history)
	cut := findCompactCut(units, a.keepRecent())
	if cut < 2 {
		// Nothing safe to summarize (only the task and/or the protected tail
		// exists); hard truncation is the only option.
		return shrinkHistory(history, budget)
	}

	batch := flattenUnits(units[1:cut])
	batchBytes := contextBytes(batch)
	before := contextBytes(history)

	summary, usage, err := a.summarizeBatch(ctx, batch)
	if err != nil {
		a.logOnly(logging.Event{Type: "error", SessionID: sess.ID, Iteration: iter,
			Success: boolPtr(false),
			Data:    map[string]any{"error": "context compaction failed, falling back to truncation: " + err.Error()}})
		return shrinkHistory(history, budget)
	}
	// The summary call is real LLM work: count it in the session usage, but
	// never broadcast its output as llm_delta (callLLM is bypassed, so no
	// deltas are emitted even for streaming clients).
	sess.AddUsage(usage)

	summaryMsg := llm.Message{Role: "user", Content: "[Earlier work summary]\n" + summary}
	after := contextBytes([]llm.Message{summaryMsg})
	if batchBytes-after < compactMinSavingsBytes || after*2 > batchBytes {
		a.logOnly(logging.Event{Type: "error", SessionID: sess.ID, Iteration: iter,
			Success: boolPtr(false),
			Data: map[string]any{
				"error": fmt.Sprintf("context compaction saved too little (%d -> %d bytes), falling back to truncation", batchBytes, after),
			}})
		return shrinkHistory(history, budget)
	}

	compacted := make([]llm.Message, 0, len(history)-len(batch)+1)
	compacted = append(compacted, units[0]...)
	compacted = append(compacted, summaryMsg)
	compacted = append(compacted, flattenUnits(units[cut:])...)
	// Compaction is not guaranteed to reach the budget (the protected tail
	// may itself be too large): truncate hard rather than summarize again —
	// a second summary in the same round could oscillate.
	if contextBytes(compacted) > budget {
		compacted = shrinkHistory(compacted, budget)
	}

	a.emit(logging.Event{Type: "context_compacted", SessionID: sess.ID, Iteration: iter,
		Data: map[string]any{
			"bytes_before":     before,
			"bytes_after":      contextBytes(compacted),
			"messages_removed": len(batch),
		}})
	return compacted
}

// summarizeBatch asks the LLM for a compact summary of one batch of old
// messages. It uses a plain (non-streaming, tool-less) request with a small
// max_tokens cap.
func (a *Agent) summarizeBatch(ctx context.Context, batch []llm.Message) (string, llm.Usage, error) {
	resp, err := a.LLM.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: CompactSummaryPrompt},
			{Role: "user", Content: renderBatchForSummary(batch)},
		},
		MaxTokens: compactSummaryMaxTokens,
	})
	if err != nil {
		return "", llm.Usage{}, err
	}
	msg := resp.First()
	if msg == nil {
		return "", resp.Usage, errors.New("summary response had no choices")
	}
	summary := strings.TrimSpace(stripThinkBlocks(msg.Content))
	if summary == "" {
		return "", resp.Usage, errors.New("summary response was empty")
	}
	return summary, resp.Usage, nil
}

// logOnly writes an event to the structured log without broadcasting it:
// compaction failures are internal bookkeeping, and a brokered "error" event
// would make the UI believe the task failed.
func (a *Agent) logOnly(e logging.Event) {
	if a.Logger != nil {
		a.Logger.Log(e)
	}
}

// splitUnits groups history into atomic units: an assistant message carrying
// tool_calls plus all its tool results form one unit; every other message is
// a unit of its own. Compaction batch boundaries fall between units so
// tool_call/tool_result pairing stays intact.
func splitUnits(history []llm.Message) [][]llm.Message {
	var units [][]llm.Message
	for i := 0; i < len(history); {
		if history[i].Role == "assistant" && len(history[i].ToolCalls) > 0 {
			j := i + 1
			for j < len(history) && history[j].Role == "tool" {
				j++
			}
			units = append(units, history[i:j])
			i = j
			continue
		}
		units = append(units, history[i:i+1])
		i++
	}
	return units
}

// findCompactCut returns the unit index where the compaction batch ends:
// units[1:cut] is summarized, unit 0 (the original task message) and a
// trailing window of at least keepRecent messages stay verbatim. A cut < 2
// means there is nothing safe to summarize.
func findCompactCut(units [][]llm.Message, keepRecent int) int {
	cut := len(units)
	remaining := 0
	for cut > 0 && remaining < keepRecent {
		cut--
		remaining += len(units[cut])
	}
	return cut
}

func flattenUnits(units [][]llm.Message) []llm.Message {
	var out []llm.Message
	for _, u := range units {
		out = append(out, u...)
	}
	return out
}

// renderBatchForSummary renders a message batch as plain transcript text for
// the summary prompt.
func renderBatchForSummary(batch []llm.Message) string {
	var b strings.Builder
	for _, m := range batch {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant] %s\n", truncateStr(m.Content, 500))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[assistant] called %s with %s\n", tc.Function.Name,
					truncateStr(tc.Function.Arguments, 300))
			}
		case m.Role == "tool":
			fmt.Fprintf(&b, "[tool %s] %s\n", m.Name, m.Content)
		default:
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content)
		}
	}
	return b.String()
}
