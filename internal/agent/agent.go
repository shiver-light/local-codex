// Package agent implements the autonomous coding loop:
// LLM -> tool calls -> tool results -> LLM -> ... -> final answer.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"local-codex/internal/llm"
	"local-codex/internal/logging"
	"local-codex/internal/session"
	"local-codex/internal/tools"
)

// Agent wires the LLM, the tool registry and session state into the
// autonomous loop.
type Agent struct {
	LLM      llm.Client
	Registry *tools.Registry
	Logger   *logging.Logger
	Broker   *EventBroker

	MaxIterations      int
	MaxRepeatToolCalls int
	MaxContextBytes    int

	// Compact enables LLM-based context compaction: when the history exceeds
	// MaxContextBytes, the oldest messages are summarized by the model and
	// replaced instead of only being hard-truncated. Failures fall back to
	// truncation. CompactKeepRecent trailing messages are never compacted
	// (zero uses a default of 6).
	Compact           bool
	CompactKeepRecent int

	// Temperature and MaxTokens are forwarded to every chat request when
	// set (zero values are omitted, leaving the server defaults).
	Temperature *float64
	MaxTokens   int

	// Stream enables streaming chat completions with llm_delta events when
	// the LLM client supports it (llm.StreamClient). On a connection-phase
	// streaming failure the agent falls back to a plain request.
	Stream bool

	// retryBackoff is the initial retry delay; tests shrink it.
	retryBackoff time.Duration
}

func boolPtr(b bool) *bool { return &b }

func (a *Agent) emit(e logging.Event) {
	if a.Logger != nil {
		a.Logger.Log(e)
	}
	if a.Broker != nil {
		a.Broker.Publish(e)
	}
}

// Run executes one task in a session until the model produces a final
// answer, the iteration cap is hit, or ctx is cancelled. It returns the
// final assistant answer.
func (a *Agent) Run(ctx context.Context, sess *session.Session, task string) (string, error) {
	maxIter := a.MaxIterations
	if maxIter <= 0 {
		maxIter = 30
	}
	maxRepeat := a.MaxRepeatToolCalls
	if maxRepeat <= 0 {
		maxRepeat = 3
	}

	sess.AppendMessage(llm.Message{Role: "user", Content: task})
	a.emit(logging.Event{Type: "user_message", SessionID: sess.ID, Data: map[string]any{"content": task}})

	var lastSig string
	repeatCount := 0
	warned := map[string]bool{}
	sigTotals := map[string]int{}

	for iter := 1; iter <= maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("agent cancelled: %w", err)
		}
		messages := a.buildMessages(ctx, sess, iter)

		a.emit(logging.Event{Type: "llm_request", SessionID: sess.ID, Iteration: iter,
			Data: map[string]any{"messages": len(messages), "model": a.LLM.Model()}})
		resp, elapsed, err := a.chatWithRetry(ctx, messages, iter, sess.ID)
		if err != nil {
			a.emit(logging.Event{Type: "error", SessionID: sess.ID, Iteration: iter,
				Success: boolPtr(false), Data: map[string]any{"error": err.Error()}})
			return "", fmt.Errorf("llm chat (iteration %d): %w", iter, err)
		}
		msg := resp.First()
		if msg == nil {
			return "", fmt.Errorf("llm returned empty response (iteration %d)", iter)
		}
		sess.AddUsage(resp.Usage)
		// Drop thinking-model reasoning so it never bloats the history or
		// leaks into the final answer; also drop server-side reasoning
		// fields before the message is stored.
		msg.Content = stripThinkBlocks(msg.Content)
		msg.ReasoningContent = ""
		// Recover tool calls the model emitted as text markup instead of
		// structured tool_calls (common with local models).
		if recovered := extractTextToolCalls(msg); recovered != nil {
			msg.ToolCalls = recovered
			a.emit(logging.Event{Type: "llm_response", SessionID: sess.ID, Iteration: iter,
				Data: map[string]any{"note": "recovered text-markup tool calls", "count": len(recovered)}})
		}
		a.emit(logging.Event{Type: "llm_response", SessionID: sess.ID, Iteration: iter,
			Duration: elapsed.Round(time.Millisecond).String(),
			Data: map[string]any{
				"tool_calls": len(msg.ToolCalls),
				"tokens":     resp.Usage.TotalTokens,
			}})

		sess.AppendMessage(*msg)

		// Final answer: no tool calls.
		if len(msg.ToolCalls) == 0 {
			sess.Finish(msg.Content)
			a.emit(logging.Event{Type: "agent_message", SessionID: sess.ID, Iteration: iter,
				Data: map[string]any{"content": msg.Content}})
			a.emit(logging.Event{Type: "agent_finished", SessionID: sess.ID, Iteration: iter,
				Success: boolPtr(true), Data: map[string]any{"iterations": iter}})
			return msg.Content, nil
		}

		if msg.Content != "" {
			a.emit(logging.Event{Type: "agent_message", SessionID: sess.ID, Iteration: iter,
				Data: map[string]any{"content": msg.Content}})
		}

		for i, call := range msg.ToolCalls {
			sig := callSignature(call)
			if sig == lastSig {
				repeatCount++
			} else {
				repeatCount = 0
			}
			lastSig = sig
			sigTotals[sig]++

			if repeatCount < maxRepeat && sigTotals[sig] < maxTotalRepeatToolCalls {
				a.executeToolCall(ctx, sess, iter, call)
				continue
			}

			// Pair every unexecuted tool_call with a synthetic tool result;
			// leaving dangling tool_calls in the history makes strict
			// OpenAI-compatible servers (vLLM etc.) reject the next
			// request with a 400.
			appendSkippedToolResults(sess, msg.ToolCalls[i:])

			if warned[sig] {
				err := fmt.Errorf("agent stuck repeating tool %q %d times; aborting", call.Function.Name, sigTotals[sig])
				a.emit(logging.Event{Type: "error", SessionID: sess.ID, Iteration: iter,
					Success: boolPtr(false), Data: map[string]any{"error": err.Error()}})
				return "", err
			}
			warned[sig] = true
			nudge := fmt.Sprintf("You have repeated the exact same tool call %q %d times. Do NOT repeat it again; change your approach or finish the task.",
				call.Function.Name, sigTotals[sig])
			sess.AppendMessage(llm.Message{Role: "user", Content: nudge})
			break
		}
	}

	err := fmt.Errorf("agent reached max iterations (%d) without a final answer", maxIter)
	a.emit(logging.Event{Type: "agent_finished", SessionID: sess.ID,
		Success: boolPtr(false), Data: map[string]any{"error": err.Error()}})
	return "", err
}

// maxTotalRepeatToolCalls caps how often the same (normalized) tool call
// may appear across the whole task, even if not consecutive.
const maxTotalRepeatToolCalls = 5

// callSignature identifies a tool call for repeat detection. Arguments are
// normalized (JSON key order and whitespace canonicalized) so a trivially
// reformatted call cannot bypass the repeat guard.
func callSignature(call llm.ToolCall) string {
	args := call.Function.Arguments
	var v any
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		if norm, err := json.Marshal(v); err == nil {
			args = string(norm)
		}
	}
	return call.Function.Name + "\x00" + args
}

// appendSkippedToolResults appends a synthetic tool result for every call,
// keeping the tool_call/tool_result pairing intact.
func appendSkippedToolResults(sess *session.Session, calls []llm.ToolCall) {
	for _, call := range calls {
		sess.AppendMessage(llm.Message{
			Role:       "tool",
			ToolCallID: call.ID,
			Name:       call.Function.Name,
			Content:    "skipped: repeated tool call aborted",
		})
	}
}

// chatWithRetry calls the LLM, retrying transient failures (connection
// resets, EOF, 429 and 5xx from an overloaded local server) with
// exponential backoff. Deterministic 4xx errors fail immediately, and
// context cancellation is never retried.
func (a *Agent) chatWithRetry(ctx context.Context, messages []llm.Message, iter int, sessionID string) (*llm.ChatResponse, time.Duration, error) {
	const maxAttempts = 4
	backoff := a.retryBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, time.Since(start), err
		}
		resp, err := a.callLLM(ctx, llm.ChatRequest{
			Messages:    messages,
			Tools:       a.Registry.Definitions(),
			Temperature: a.Temperature,
			MaxTokens:   a.MaxTokens,
		}, iter, sessionID)
		if err == nil {
			return resp, time.Since(start), nil
		}
		if ctx.Err() != nil {
			return nil, time.Since(start), err // request was cancelled: don't retry
		}
		lastErr = err
		retry := retryableErr(err)
		a.emit(logging.Event{Type: "error", SessionID: sessionID, Iteration: iter,
			Success: boolPtr(false),
			Data: map[string]any{
				"error":   err.Error(),
				"attempt": attempt,
				"retry":   retry && attempt < maxAttempts,
			}})
		if !retry {
			return nil, time.Since(start), err
		}
		if attempt < maxAttempts {
			delay := backoff
			var httpErr *llm.HTTPError
			if errors.As(err, &httpErr) && httpErr.RetryAfter > 0 {
				delay = httpErr.RetryAfter
			}
			select {
			case <-ctx.Done():
				return nil, time.Since(start), ctx.Err()
			case <-time.After(delay):
			}
			backoff *= 2
		}
	}
	return nil, time.Since(start), fmt.Errorf("llm failed after %d attempts: %w", maxAttempts, lastErr)
}

// streamError wraps a failure that happened after streaming had already
// begun: deltas were already delivered to subscribers, so retrying would
// repeat output the user has seen. It is never retried.
type streamError struct{ err error }

func (e *streamError) Error() string { return e.err.Error() }
func (e *streamError) Unwrap() error { return e.err }

// callLLM performs one chat request, streaming when enabled and supported.
// Each content delta is broadcast as an llm_delta event (think blocks
// filtered out). A streaming failure before the first delta falls back to a
// plain request; a failure mid-stream aborts without retry.
func (a *Agent) callLLM(ctx context.Context, req llm.ChatRequest, iter int, sessionID string) (*llm.ChatResponse, error) {
	sc, ok := a.LLM.(llm.StreamClient)
	if !a.Stream || !ok {
		return a.LLM.Chat(ctx, req)
	}
	streamStarted := false
	filter := &thinkFilter{}
	resp, err := sc.ChatStream(ctx, req, func(d llm.Delta) {
		streamStarted = true
		if d.Content == "" {
			return // reasoning deltas are never broadcast
		}
		if text := filter.feed(d.Content); text != "" {
			a.emit(logging.Event{Type: "llm_delta", SessionID: sessionID, Iteration: iter,
				Data: map[string]any{"content": text}})
		}
	})
	if err == nil {
		return resp, nil
	}
	if streamStarted {
		return nil, &streamError{err}
	}
	// Streaming failed before any output (e.g. the server does not support
	// stream mode): fall back to a plain request for this attempt.
	return a.LLM.Chat(ctx, req)
}

// retryableErr reports whether the failure is worth retrying: network
// errors and 429/5xx are transient; other 4xx are deterministic. Mid-stream
// failures are never retried (partial output was already delivered).
func retryableErr(err error) bool {
	var serr *streamError
	if errors.As(err, &serr) {
		return false
	}
	var httpErr *llm.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= 500
	}
	return true
}

// executeToolCall runs one tool call, appends the result to the session and
// emits tool_call/tool_result events.
func (a *Agent) executeToolCall(ctx context.Context, sess *session.Session, iter int, call llm.ToolCall) {
	name := call.Function.Name
	args := call.Function.Args()

	a.emit(logging.Event{Type: "tool_call", SessionID: sess.ID, Iteration: iter, Tool: name,
		Data: map[string]any{"args": truncateStr(call.Function.Arguments, 2000)}})

	start := time.Now()
	res := a.Registry.Execute(ctx, name, args)
	dur := time.Since(start).Round(time.Millisecond)

	sess.AddToolRecord(session.ToolRecord{
		Name:      name,
		Args:      truncateStr(call.Function.Arguments, 500),
		IsError:   res.IsError,
		StartedAt: start,
		Duration:  dur.String(),
	})
	if name == "apply_patch" && !res.IsError {
		sess.AddModifiedFiles(extractPatchPaths(call.Function.Arguments))
	}

	a.emit(logging.Event{Type: "tool_result", SessionID: sess.ID, Iteration: iter, Tool: name,
		Success:  boolPtr(!res.IsError),
		Duration: dur.String(),
		Data:     map[string]any{"content": truncateStr(res.Content, 4000)}})

	content := res.Content
	if res.IsError {
		content = "ERROR: " + content
	}
	sess.AppendMessage(llm.Message{
		Role:       "tool",
		ToolCallID: call.ID,
		Name:       name,
		Content:    content,
	})
}

// buildMessages assembles the message list sent to the LLM and enforces the
// context byte budget: with Compact enabled it summarizes the oldest history
// via the LLM, otherwise (or on any compaction failure) it hard-truncates
// old tool results.
func (a *Agent) buildMessages(ctx context.Context, sess *session.Session, iter int) []llm.Message {
	history := sess.SnapshotMessages()
	messages := make([]llm.Message, 0, len(history)+1)
	messages = append(messages, llm.Message{Role: "system", Content: SystemPrompt})
	for _, m := range history {
		m.ReasoningContent = "" // never send reasoning back to the server
		messages = append(messages, m)
	}

	budget := a.MaxContextBytes
	if budget <= 0 {
		budget = 200_000
	}
	if contextBytes(messages) > budget {
		historyBudget := budget - len(SystemPrompt)
		if a.Compact {
			sess.ReplaceMessages(a.compactHistory(ctx, sess, history, historyBudget, iter))
		} else {
			sess.ReplaceMessages(shrinkHistory(history, historyBudget))
		}
		history = sess.SnapshotMessages()
		messages = append([]llm.Message{{Role: "system", Content: SystemPrompt}}, history...)
	}
	return messages
}

func contextBytes(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments)
		}
	}
	return total
}

// shrinkHistory truncates the oldest large tool results until the history
// fits the byte budget. The first user message (the task) is never touched.
func shrinkHistory(history []llm.Message, budget int) []llm.Message {
	const keepTail = 2000
	for contextBytes(history) > budget {
		shrank := false
		for i := range history {
			// keep the first two messages (task + first reply) intact; an
			// already-truncated message (keepTail + notice) is skipped so a
			// second pass can never re-truncate it forever.
			if i < 2 || history[i].Role != "tool" ||
				len(history[i].Content) <= keepTail+len(truncNotice) {
				continue
			}
			history[i].Content = truncateUTF8(history[i].Content, keepTail) + truncNotice
			shrank = true
			if contextBytes(history) <= budget {
				break
			}
		}
		if !shrank {
			break
		}
	}
	return history
}

const truncNotice = "\n...(older tool output truncated to bound context size)"

var patchFileRe = regexp.MustCompile(`\*\*\* (?:Update File|Add File|Delete File):\s*([^\n\\"]+)`)

// extractPatchPaths pulls touched file paths out of an apply_patch call's
// raw arguments (used for session bookkeeping).
func extractPatchPaths(argsJSON string) []string {
	var args struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil
	}
	var paths []string
	for _, m := range patchFileRe.FindAllStringSubmatch(args.Patch, -1) {
		paths = append(paths, strings.TrimSpace(m[1]))
	}
	return paths
}

// truncateUTF8 cuts s to at most n bytes without splitting a multi-byte
// UTF-8 character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
