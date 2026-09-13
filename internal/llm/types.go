// Package llm abstracts chat-completion providers behind a small interface.
// The first implementation targets OpenAI-compatible /v1/chat/completions
// endpoints (Ollama, vLLM, llama.cpp, OpenAI, ...).
package llm

import (
	"context"
	"encoding/json"
)

// Message is one chat message. Depending on Role, different fields matter:
//   - system/user: Content
//   - assistant: Content and/or ToolCalls
//   - tool: ToolCallID + Content (the tool result)
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ReasoningContent carries thinking-model reasoning returned by some
	// servers (e.g. vLLM's reasoning parser). It is never sent back: the
	// agent strips it before messages re-enter the request history.
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

// ToolCall is a model-emitted function invocation.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded arguments
}

// Args decodes the JSON arguments.
func (f FunctionCall) Args() json.RawMessage {
	return json.RawMessage(f.Arguments)
}

// ToolDef describes a tool for the API's "tools" parameter.
type ToolDef struct {
	Type     string      `json:"type"` // always "function"
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []ToolDef `json:"tools,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	// StreamOptions asks the server for a final usage chunk when streaming.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions controls streaming behavior (OpenAI-compatible).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Delta is one streamed increment of the assistant reply. Tool-call
// increments are aggregated internally and not reported through Delta.
type Delta struct {
	Content          string
	ReasoningContent string
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatResponse struct {
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// First returns the first choice message, or nil.
func (r *ChatResponse) First() *Message {
	if r == nil || len(r.Choices) == 0 {
		return nil
	}
	return &r.Choices[0].Message
}

// Client is the provider-agnostic LLM interface the agent depends on.
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	Model() string
}

// StreamClient is implemented by clients that support SSE streaming. The
// agent type-asserts for it and falls back to Chat when absent.
//
// onDelta is invoked for every content/reasoning increment as it arrives.
// The returned ChatResponse carries the fully aggregated message and, when
// the server provides it, the token usage.
type StreamClient interface {
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(Delta)) (*ChatResponse, error)
}
