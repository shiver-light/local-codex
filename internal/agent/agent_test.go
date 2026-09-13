package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"local-codex/internal/llm"
	"local-codex/internal/permission"
	"local-codex/internal/session"
	"local-codex/internal/tools"
	"local-codex/internal/workspace"
)

// scriptedLLM is a mock LLM driven by a handler that inspects the
// conversation so far and decides the next assistant message.
type scriptedLLM struct {
	model   string
	handler func(req llm.ChatRequest) llm.Message
	calls   int
}

func (s *scriptedLLM) Model() string { return s.model }
func (s *scriptedLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.calls++
	msg := s.handler(req)
	return &llm.ChatResponse{Choices: []llm.Choice{{Message: msg}}}, nil
}

func toolCall(id, name, args string) llm.Message {
	return llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: llm.FunctionCall{Name: name, Arguments: args},
		}},
	}
}

// lastToolResult returns the content of the most recent tool message.
func lastToolResult(req llm.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" {
			return req.Messages[i].Content
		}
	}
	return ""
}

func newTestAgent(t *testing.T, ws *workspace.Workspace, mock llm.Client, maxIter int) (*Agent, *session.Session) {
	t.Helper()
	reg := tools.NewBuiltinRegistry(ws, permission.ApproveAll{}, 30*time.Second, 60*time.Second)
	a := &Agent{
		LLM:                mock,
		Registry:           reg,
		MaxIterations:      maxIter,
		MaxRepeatToolCalls: 3,
	}
	store := session.NewMemoryStore()
	sess := store.Create("test task")
	return a, sess
}

// TestLoopMaxIterations: an LLM that never stops calling tools must be
// stopped by the iteration cap.
func TestLoopMaxIterations(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	mock := &scriptedLLM{model: "mock", handler: func(req llm.ChatRequest) llm.Message {
		n++
		// vary args each call so the repeat guard does not fire
		return toolCall("c1", "list_files", fmt.Sprintf(`{"path":".","max_depth":%d}`, n))
	}}
	a, sess := newTestAgent(t, ws, mock, 5)
	_, err = a.Run(context.Background(), sess, "never finish")
	if err == nil || !strings.Contains(err.Error(), "max iterations") {
		t.Fatalf("expected max-iteration error, got %v", err)
	}
	if mock.calls != 5 {
		t.Errorf("expected exactly 5 llm calls, got %d", mock.calls)
	}
}

// TestLoopRepeatGuard: an LLM stuck on the same tool call must be aborted.
func TestLoopRepeatGuard(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &scriptedLLM{model: "mock", handler: func(req llm.ChatRequest) llm.Message {
		return toolCall("c1", "search_code", `{"query":"x"}`)
	}}
	a, sess := newTestAgent(t, ws, mock, 30)
	_, err = a.Run(context.Background(), sess, "loop forever")
	if err == nil || !strings.Contains(err.Error(), "repeating") {
		t.Fatalf("expected repeat-guard error, got %v", err)
	}
	if mock.calls >= 30 {
		t.Errorf("repeat guard should abort before max iterations, calls=%d", mock.calls)
	}
}

// TestLoopSimpleToolFlow: one tool call, then a final answer.
func TestLoopSimpleToolFlow(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root(), "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &scriptedLLM{model: "mock", handler: func(req llm.ChatRequest) llm.Message {
		if tr := lastToolResult(req); tr != "" {
			if !strings.Contains(tr, "hello.txt") {
				t.Errorf("tool result missing listing: %s", tr)
			}
			return llm.Message{Role: "assistant", Content: "Done: repo contains hello.txt"}
		}
		return toolCall("c1", "list_files", `{"path":"."}`)
	}}
	a, sess := newTestAgent(t, ws, mock, 10)
	answer, err := a.Run(context.Background(), sess, "what files exist?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "hello.txt") {
		t.Errorf("unexpected answer %q", answer)
	}
	if !sess.Done || sess.FinalAnswer == "" {
		t.Error("session should be marked done with final answer")
	}
}

// TestLoopClosedLoopE2E drives the full coding cycle against a copy of the
// sample repo: search -> read -> patch -> test (fail first, then pass) ->
// git diff -> final summary. It also exercises patch-failure recovery:
// the first patch is intentionally wrong and the agent must fix it after
// seeing the test failure.
func TestLoopClosedLoopE2E(t *testing.T) {
	// copy the fixture into a temp workspace and git-init it
	src := filepath.Join("..", "..", "tests", "fixtures", "sample-repo")
	dir := t.TempDir()
	if out, err := exec.Command("cp", "-r", src+"/.", dir).CombinedOutput(); err != nil {
		t.Fatalf("copy fixture: %v: %s", err, out)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init")
	git("add", ".")
	git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")

	ws, err := workspace.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	step := 0
	mock := &scriptedLLM{model: "mock", handler: func(req llm.ChatRequest) llm.Message {
		step++
		switch step {
		case 1: // explore
			return toolCall("c1", "search_code", `{"query":"Sub"}`)
		case 2: // read the buggy file
			tr := lastToolResult(req)
			if !strings.Contains(tr, "calculator.go") {
				t.Errorf("search should find calculator.go:\n%s", tr)
			}
			return toolCall("c2", "read_file", `{"path":"calculator/calculator.go"}`)
		case 3: // apply a WRONG patch first (still broken) to test recovery
			patch := "*** Begin Patch\n*** Update File: calculator/calculator.go\n@@\n func Sub(a, b int) int {\n-\treturn a + b // BUG: should be a - b\n+\treturn b - a // still wrong\n }\n*** End Patch"
			args, _ := json.Marshal(map[string]string{"patch": patch})
			return toolCall("c3", "apply_patch", string(args))
		case 4: // run tests -> must fail
			return toolCall("c4", "run_command", `{"command":"go test ./..."}`)
		case 5: // see failure, apply correct patch
			tr := lastToolResult(req)
			if !strings.Contains(tr, "FAIL") && !strings.Contains(tr, "exit_code: 1") {
				t.Errorf("expected failing test output:\n%s", tr)
			}
			patch := "*** Begin Patch\n*** Update File: calculator/calculator.go\n@@\n func Sub(a, b int) int {\n-\treturn b - a // still wrong\n+\treturn a - b\n }\n*** End Patch"
			args, _ := json.Marshal(map[string]string{"patch": patch})
			return toolCall("c5", "apply_patch", string(args))
		case 6: // re-run tests -> must pass
			return toolCall("c6", "run_command", `{"command":"go test ./..."}`)
		case 7: // review diff
			tr := lastToolResult(req)
			if !strings.Contains(tr, "exit_code: 0") || !strings.Contains(tr, "ok") {
				t.Errorf("expected passing tests:\n%s", tr)
			}
			return toolCall("c7", "git_diff", `{}`)
		default: // finish
			tr := lastToolResult(req)
			if !strings.Contains(tr, "return a - b") {
				t.Errorf("git diff should show the fix:\n%s", tr)
			}
			return llm.Message{Role: "assistant", Content: "Fixed calculator.Sub: files changed: calculator/calculator.go. Tests: go test ./... PASS."}
		}
	}}

	a, sess := newTestAgent(t, ws, mock, 15)
	answer, err := a.Run(context.Background(), sess, "Fix the Sub bug described in README and run tests")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !strings.Contains(answer, "calculator/calculator.go") {
		t.Errorf("answer should summarize changed files: %q", answer)
	}

	// the real file must contain the fix
	data, err := os.ReadFile(filepath.Join(dir, "calculator", "calculator.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "return a - b") {
		t.Errorf("bug not fixed on disk:\n%s", data)
	}
	if strings.Contains(string(data), "b - a") {
		t.Errorf("intermediate wrong patch must be gone:\n%s", data)
	}

	// session bookkeeping
	found := false
	for _, f := range sess.ModifiedFiles {
		if f == "calculator/calculator.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("ModifiedFiles should include calculator/calculator.go, got %v", sess.ModifiedFiles)
	}
	if len(sess.ToolHistory) < 6 {
		t.Errorf("expected >=6 tool records, got %d", len(sess.ToolHistory))
	}
}

// chatFuncLLM is a mock LLM that can return errors, for retry-path tests.
type chatFuncLLM struct {
	model string
	fn    func(req llm.ChatRequest) (*llm.ChatResponse, error)
	calls int
}

func (s *chatFuncLLM) Model() string { return s.model }
func (s *chatFuncLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.calls++
	return s.fn(req)
}

// assertToolCallPairing fails the test if any assistant tool_call lacks a
// matching tool result, or any tool result lacks a pending tool_call.
func assertToolCallPairing(t *testing.T, msgs []llm.Message) {
	t.Helper()
	pending := map[string]bool{}
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		case "tool":
			if !pending[m.ToolCallID] {
				t.Errorf("tool result for %q without a preceding tool_call", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	for id := range pending {
		t.Errorf("tool_call %q has no tool result", id)
	}
}

// TestToolCallPairingOnRepeatNudge: when the repeat guard fires mid-turn
// with several tool_calls in one response, every unexecuted call must still
// get a (synthetic) tool result so the history stays paired.
func TestToolCallPairingOnRepeatNudge(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &scriptedLLM{model: "mock", handler: func(req llm.ChatRequest) llm.Message {
		assertToolCallPairing(t, req.Messages)
		for _, m := range req.Messages {
			if m.Role == "user" && strings.Contains(m.Content, "Do NOT repeat") {
				return llm.Message{Role: "assistant", Content: "understood, stopping"}
			}
		}
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "search_code", Arguments: `{"query":"x"}`}},
			{ID: "c2", Type: "function", Function: llm.FunctionCall{Name: "search_code", Arguments: `{"query":"x"}`}},
		}}
	}}
	a, sess := newTestAgent(t, ws, mock, 30)
	a.MaxRepeatToolCalls = 2
	answer, err := a.Run(context.Background(), sess, "task")
	if err != nil {
		t.Fatalf("run should recover via nudge, got %v", err)
	}
	if answer != "understood, stopping" {
		t.Errorf("unexpected answer %q", answer)
	}
	assertToolCallPairing(t, sess.SnapshotMessages())
	skipped := 0
	for _, m := range sess.SnapshotMessages() {
		if m.Role == "tool" && strings.Contains(m.Content, "skipped: repeated tool call aborted") {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("expected 2 synthetic skipped tool results, got %d", skipped)
	}
}

// TestRetryDeterministic4xx: a 400 must not be retried.
func TestRetryDeterministic4xx(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &chatFuncLLM{model: "mock", fn: func(req llm.ChatRequest) (*llm.ChatResponse, error) {
		return nil, &llm.HTTPError{StatusCode: 400, Status: "400 Bad Request", Body: "bad request"}
	}}
	a, sess := newTestAgent(t, ws, mock, 30)
	a.retryBackoff = time.Millisecond
	_, err = a.Run(context.Background(), sess, "task")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 error, got %v", err)
	}
	if mock.calls != 1 {
		t.Errorf("deterministic 4xx must not be retried, calls=%d", mock.calls)
	}
}

// TestRetryTransient5xx: 5xx responses are retried until success.
func TestRetryTransient5xx(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &chatFuncLLM{model: "mock"}
	mock.fn = func(req llm.ChatRequest) (*llm.ChatResponse, error) {
		if mock.calls < 3 {
			return nil, &llm.HTTPError{StatusCode: 503, Status: "503 Service Unavailable", Body: "overloaded"}
		}
		return &llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{
			Role: "assistant", Content: "recovered"}}}}, nil
	}
	a, sess := newTestAgent(t, ws, mock, 30)
	a.retryBackoff = time.Millisecond
	answer, err := a.Run(context.Background(), sess, "task")
	if err != nil {
		t.Fatalf("transient 5xx should be retried: %v", err)
	}
	if answer != "recovered" || mock.calls != 3 {
		t.Errorf("answer=%q calls=%d, want recovered/3", answer, mock.calls)
	}
}

// TestRetry429WithRetryAfter: 429 is retried and Retry-After is honored.
func TestRetry429WithRetryAfter(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &chatFuncLLM{model: "mock"}
	mock.fn = func(req llm.ChatRequest) (*llm.ChatResponse, error) {
		if mock.calls == 1 {
			return nil, &llm.HTTPError{StatusCode: 429, Status: "429 Too Many Requests",
				Body: "slow down", RetryAfter: 5 * time.Millisecond}
		}
		return &llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{
			Role: "assistant", Content: "ok"}}}}, nil
	}
	a, sess := newTestAgent(t, ws, mock, 30)
	a.retryBackoff = time.Millisecond
	if _, err := a.Run(context.Background(), sess, "task"); err != nil {
		t.Fatalf("429 should be retried: %v", err)
	}
	if mock.calls != 2 {
		t.Errorf("calls=%d, want 2", mock.calls)
	}
}

// TestRepeatGuardNormalizedArgs: reordered keys / different whitespace must
// not bypass the consecutive repeat guard.
func TestRepeatGuardNormalizedArgs(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	variants := []string{`{"query":"x","limit":1}`, `{ "limit":1, "query":"x" }`}
	mock := &scriptedLLM{model: "mock"}
	mock.handler = func(req llm.ChatRequest) llm.Message {
		return toolCall("c1", "search_code", variants[mock.calls%2])
	}
	a, sess := newTestAgent(t, ws, mock, 30)
	_, err = a.Run(context.Background(), sess, "loop")
	if err == nil || !strings.Contains(err.Error(), "repeating") {
		t.Fatalf("expected repeat-guard abort, got %v", err)
	}
	if mock.calls >= 30 {
		t.Errorf("normalized repeat guard should abort early, calls=%d", mock.calls)
	}
}

// TestRepeatGuardGlobalWindow: alternating between two signatures defeats
// the consecutive guard, but the per-task total cap still aborts.
func TestRepeatGuardGlobalWindow(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	queries := []string{`{"query":"a"}`, `{"query":"b"}`}
	mock := &scriptedLLM{model: "mock"}
	mock.handler = func(req llm.ChatRequest) llm.Message {
		return toolCall("c1", "search_code", queries[mock.calls%2])
	}
	a, sess := newTestAgent(t, ws, mock, 30)
	_, err = a.Run(context.Background(), sess, "loop")
	if err == nil || !strings.Contains(err.Error(), "repeating") {
		t.Fatalf("expected global-window abort, got %v", err)
	}
	if mock.calls >= 30 {
		t.Errorf("global window should abort before max iterations, calls=%d", mock.calls)
	}
	assertToolCallPairing(t, sess.SnapshotMessages())
}

// TestThinkBlocksStripped: <think> blocks and reasoning_content must not
// reach the final answer or the stored history.
func TestThinkBlocksStripped(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &scriptedLLM{model: "mock", handler: func(req llm.ChatRequest) llm.Message {
		return llm.Message{
			Role:             "assistant",
			Content:          "<think>secret reasoning</think>The answer. <think>unterminated",
			ReasoningContent: "secret reasoning",
		}
	}}
	a, sess := newTestAgent(t, ws, mock, 5)
	answer, err := a.Run(context.Background(), sess, "task")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "The answer." {
		t.Errorf("think block leaked into answer: %q", answer)
	}
	for _, m := range sess.SnapshotMessages() {
		if strings.Contains(m.Content, "<think>") || strings.Contains(m.Content, "secret reasoning") {
			t.Errorf("history message still carries reasoning: %+v", m)
		}
		if m.ReasoningContent != "" {
			t.Errorf("history message still carries reasoning_content: %+v", m)
		}
	}
}

// TestAgentSendsLLMParams: configured temperature/max_tokens must be wired
// into every chat request.
func TestAgentSendsLLMParams(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var gotTemp *float64
	var gotMax int
	mock := &chatFuncLLM{model: "mock", fn: func(req llm.ChatRequest) (*llm.ChatResponse, error) {
		gotTemp = req.Temperature
		gotMax = req.MaxTokens
		return &llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{
			Role: "assistant", Content: "done"}}}}, nil
	}}
	a, sess := newTestAgent(t, ws, mock, 5)
	temp := 0.7
	a.Temperature = &temp
	a.MaxTokens = 1234
	if _, err := a.Run(context.Background(), sess, "task"); err != nil {
		t.Fatal(err)
	}
	if gotTemp == nil || *gotTemp != 0.7 {
		t.Errorf("temperature not forwarded: %v", gotTemp)
	}
	if gotMax != 1234 {
		t.Errorf("max_tokens not forwarded: %d", gotMax)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := "ab世界cd"
	if got := truncateUTF8(s, 4); got != "ab" {
		t.Errorf("truncateUTF8 mid-rune = %q, want %q", got, "ab")
	}
	if got := truncateUTF8(s, 5); got != "ab世" {
		t.Errorf("truncateUTF8 at boundary = %q, want %q", got, "ab世")
	}
	if got := truncateUTF8(s, len(s)+10); got != s {
		t.Errorf("truncateUTF8 overlong = %q, want %q", got, s)
	}
}
