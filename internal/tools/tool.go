// Package tools implements the agent's tool system: a uniform Tool
// interface, a registry, and the built-in tools (files, search, patch,
// shell, git).
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"local-codex/internal/llm"
)

// Result is the outcome of a tool execution. Content is fed back to the
// model as a tool message; IsError marks failures so the agent can react.
type Result struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Text builds a successful result.
func Text(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...)}
}

// Error builds a failed result (fed back to the model, not a Go error).
func Error(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// Tool is the uniform interface every agent tool implements.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema of the tool's parameters object.
	Schema() map[string]any
	// Execute runs the tool. args is the raw JSON from the model's tool call.
	Execute(ctx context.Context, args json.RawMessage) (Result, error)
}

// Definition converts a Tool into the wire format sent to the LLM.
func Definition(t Tool) llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		},
	}
}

// decodeArgs unmarshals tool-call arguments, tolerating empty input.
func decodeArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}
