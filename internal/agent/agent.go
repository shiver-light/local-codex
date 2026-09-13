// Package agent implements the autonomous coding loop:
// LLM -> tool calls -> tool results -> LLM -> ... -> final answer.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

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
	warned := false

	for iter := 1; iter <= maxIter; iter++ {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("agent cancelled: %w", err)
		}
		messages := a.buildMessages(sess)

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

		for _, call := range msg.ToolCalls {
			sig := call.Function.Name + "\x00" + call.Function.Arguments
			if sig == lastSig {
				repeatCount++
			} else {
				repeatCount = 0
				warned = false
			}
			lastSig = sig

			if repeatCount >= maxRepeat {
				if warned {
					err := fmt.Errorf("agent stuck repeating tool %q %d times; aborting", call.Function.Name, repeatCount)
					a.emit(logging.Event{Type: "error", SessionID: sess.ID, Iteration: iter,
						Success: boolPtr(false), Data: map[string]any{"error": err.Error()}})
					return "", err
				}
				warned = true
				nudge := fmt.Sprintf("You have repeated the exact same tool call %q %d times. Do NOT repeat it again; change your approach or finish the task.",
					call.Function.Name, repeatCount+1)
				sess.AppendMessage(llm.Message{Role: "user", Content: nudge})
				break
			}

			a.executeToolCall(ctx, sess, iter, call)
		}
	}

	err := fmt.Errorf("agent reached max iterations (%d) without a final answer", maxIter)
	a.emit(logging.Event{Type: "agent_finished", SessionID: sess.ID,
		Success: boolPtr(false), Data: map[string]any{"error": err.Error()}})
	return "", err
}

// chatWithRetry calls the LLM, retrying transient failures (connection
// resets, EOF, 5xx from an overloaded local server) with exponential
// backoff. Context cancellation is never retried.
func (a *Agent) chatWithRetry(ctx context.Context, messages []llm.Message, iter int, sessionID string) (*llm.ChatResponse, time.Duration, error) {
	const maxAttempts = 4
	backoff := time.Second
	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, time.Since(start), err
		}
		resp, err := a.LLM.Chat(ctx, llm.ChatRequest{
			Messages: messages,
			Tools:    a.Registry.Definitions(),
		})
		if err == nil {
			return resp, time.Since(start), nil
		}
		if ctx.Err() != nil {
			return nil, time.Since(start), err // request was cancelled: don't retry
		}
		lastErr = err
		a.emit(logging.Event{Type: "error", SessionID: sessionID, Iteration: iter,
			Success: boolPtr(false),
			Data: map[string]any{
				"error":   err.Error(),
				"attempt": attempt,
				"retry":   attempt < maxAttempts,
			}})
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return nil, time.Since(start), ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return nil, time.Since(start), fmt.Errorf("llm failed after %d attempts: %w", maxAttempts, lastErr)
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
// context byte budget by truncating old tool results.
func (a *Agent) buildMessages(sess *session.Session) []llm.Message {
	history := sess.SnapshotMessages()
	messages := make([]llm.Message, 0, len(history)+1)
	messages = append(messages, llm.Message{Role: "system", Content: SystemPrompt})
	messages = append(messages, history...)

	budget := a.MaxContextBytes
	if budget <= 0 {
		budget = 200_000
	}
	if contextBytes(messages) > budget {
		sess.ReplaceMessages(shrinkHistory(history, budget- len(SystemPrompt)))
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
			// keep the first two messages (task + first reply) intact
			if i < 2 || history[i].Role != "tool" || len(history[i].Content) <= keepTail {
				continue
			}
			history[i].Content = history[i].Content[:keepTail] +
				"\n...(older tool output truncated to bound context size)"
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

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
