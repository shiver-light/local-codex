package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"local-codex/internal/llm"
)

// Some local models (e.g. qwen3-coder served via certain chat templates)
// intermittently emit tool calls as text markup instead of structured
// tool_calls. extractTextToolCalls recovers those so the loop keeps working.
//
// Supported forms:
//
//	<tool_call>{"name": "git_status", "arguments": {...}}</tool_call>
//	<tool_call><function=git_status>{"arg": 1}</function></tool_call>
//	<function=git_status>
//	</function>
var (
	toolCallBlockRe = regexp.MustCompile("(?s)<tool_call>\\s*(.*?)\\s*</tool_call>")
	funcCallRe      = regexp.MustCompile("(?s)<function=([a-zA-Z_][a-zA-Z0-9_]*)>\\s*(.*?)\\s*</function>")
)

type toolCallJSON struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// extractTextToolCalls parses text-markup tool calls out of an assistant
// message. It returns nil when the content contains no recognizable call.
// The message content is stripped of the markup when extraction succeeds.
func extractTextToolCalls(msg *llm.Message) []llm.ToolCall {
	if msg == nil || len(msg.ToolCalls) > 0 {
		return nil
	}
	content := msg.Content
	if !strings.Contains(content, "<tool_call>") && !strings.Contains(content, "<function=") {
		return nil
	}

	var calls []llm.ToolCall
	rest := content

	// Form 1: <tool_call>{json}</tool_call> or <tool_call><function=…>
	for _, m := range toolCallBlockRe.FindAllStringSubmatchIndex(rest, -1) {
		inner := rest[m[2]:m[3]]
		if fc := funcCallRe.FindStringSubmatch(inner); fc != nil {
			if tc, ok := buildCall(fc[1], fc[2], len(calls)); ok {
				calls = append(calls, tc)
			}
		} else {
			var parsed toolCallJSON
			if err := json.Unmarshal([]byte(inner), &parsed); err == nil && parsed.Name != "" {
				calls = append(calls, llm.ToolCall{
					ID:   fmt.Sprintf("text_call_%d", len(calls)),
					Type: "function",
					Function: llm.FunctionCall{
						Name:      parsed.Name,
						Arguments: string(parsed.Arguments),
					},
				})
			}
		}
	}
	rest = toolCallBlockRe.ReplaceAllString(rest, "")

	// Form 2: bare <function=name>…</function> outside <tool_call>
	for _, fc := range funcCallRe.FindAllStringSubmatch(rest, -1) {
		if tc, ok := buildCall(fc[1], fc[2], len(calls)); ok {
			calls = append(calls, tc)
		}
	}
	rest = funcCallRe.ReplaceAllString(rest, "")

	if len(calls) == 0 {
		return nil
	}
	msg.Content = strings.TrimSpace(rest)
	return calls
}

func buildCall(name, args string, idx int) (llm.ToolCall, bool) {
	args = strings.TrimSpace(args)
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return llm.ToolCall{}, false
	}
	return llm.ToolCall{
		ID:       fmt.Sprintf("text_call_%d", idx),
		Type:     "function",
		Function: llm.FunctionCall{Name: name, Arguments: args},
	}, true
}
