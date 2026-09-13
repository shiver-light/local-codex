package agent

import (
	"strings"
	"testing"

	"local-codex/internal/llm"
)

func TestExtractToolCallJSONForm(t *testing.T) {
	msg := &llm.Message{
		Role:    "assistant",
		Content: "Let me check.\n<tool_call>\n{\"name\": \"git_status\", \"arguments\": {}}\n</tool_call>",
	}
	calls := extractTextToolCalls(msg)
	if len(calls) != 1 || calls[0].Function.Name != "git_status" {
		t.Fatalf("calls = %+v", calls)
	}
	if strings.Contains(msg.Content, "<tool_call>") {
		t.Error("markup should be stripped from content")
	}
}

func TestExtractFunctionForm(t *testing.T) {
	msg := &llm.Message{
		Role: "assistant",
		Content: "checking\n<function=read_file>\n{\"path\": \"main.go\"}\n</function>\n",
	}
	calls := extractTextToolCalls(msg)
	if len(calls) != 1 || calls[0].Function.Name != "read_file" {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, "main.go") {
		t.Errorf("args = %s", calls[0].Function.Arguments)
	}
}

func TestExtractNestedFunctionForm(t *testing.T) {
	msg := &llm.Message{
		Role:    "assistant",
		Content: "<tool_call>\n<function=git_status>\n</function>\n</tool_call>",
	}
	calls := extractTextToolCalls(msg)
	if len(calls) != 1 || calls[0].Function.Name != "git_status" || calls[0].Function.Arguments != "{}" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestExtractIgnoresPlainText(t *testing.T) {
	msg := &llm.Message{Role: "assistant", Content: "just a normal answer"}
	if calls := extractTextToolCalls(msg); calls != nil {
		t.Errorf("plain text should yield no calls, got %+v", calls)
	}
	if msg.Content != "just a normal answer" {
		t.Error("content should be untouched")
	}
}

func TestExtractSkipsInvalidJSON(t *testing.T) {
	msg := &llm.Message{Role: "assistant", Content: "<function=read_file>not json</function>"}
	if calls := extractTextToolCalls(msg); calls != nil {
		t.Errorf("invalid args should not produce a call, got %+v", calls)
	}
}

func TestExtractSkipsWhenStructuredCallsExist(t *testing.T) {
	msg := &llm.Message{
		Role:    "assistant",
		Content: "<function=git_status></function>",
		ToolCalls: []llm.ToolCall{{
			ID: "x", Type: "function",
			Function: llm.FunctionCall{Name: "list_files", Arguments: "{}"},
		}},
	}
	if calls := extractTextToolCalls(msg); calls != nil {
		t.Errorf("structured tool_calls take precedence, got %+v", calls)
	}
}
