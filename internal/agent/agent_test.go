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
