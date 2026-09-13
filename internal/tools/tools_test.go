package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"local-codex/internal/permission"
	"local-codex/internal/workspace"
)

func testWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.Open: %v", err)
	}
	return ws
}

func write(t *testing.T, ws *workspace.Workspace, rel, content string) {
	t.Helper()
	abs := filepath.Join(ws.Root(), rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- registry ---

type fakeTool struct{ name string }

func (f fakeTool) Name() string         { return f.name }
func (f fakeTool) Description() string  { return "fake" }
func (f fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (f fakeTool) Execute(context.Context, json.RawMessage) (Result, error) {
	return Text("ok"), nil
}

func TestRegistryRegisterAndExecute(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(fakeTool{name: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(fakeTool{name: "a"}); err == nil {
		t.Error("duplicate registration should fail")
	}
	res := r.Execute(context.Background(), "a", nil)
	if res.IsError || res.Content != "ok" {
		t.Errorf("unexpected result %+v", res)
	}
	res = r.Execute(context.Background(), "missing", nil)
	if !res.IsError || !strings.Contains(res.Content, "unknown tool") {
		t.Errorf("unknown tool should yield error result, got %+v", res)
	}
	defs := r.Definitions()
	if len(defs) != 1 || defs[0].Function.Name != "a" || defs[0].Type != "function" {
		t.Errorf("bad definitions %+v", defs)
	}
}

// --- read_file ---

func TestReadFile(t *testing.T) {
	ws := testWorkspace(t)
	write(t, ws, "a.txt", "line1\nline2\nline3\n")
	tool := &ReadFileTool{WS: ws}

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"a.txt"}`))
	if err != nil || res.IsError {
		t.Fatalf("read failed: %v %+v", err, res)
	}
	if !strings.Contains(res.Content, "1\tline1") || !strings.Contains(res.Content, "3\tline3") {
		t.Errorf("unexpected content:\n%s", res.Content)
	}
}

func TestReadFileRange(t *testing.T) {
	ws := testWorkspace(t)
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		sb.WriteString("line\n")
	}
	write(t, ws, "big.txt", sb.String())
	tool := &ReadFileTool{WS: ws}

	// default: capped at 300 lines
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"path":"big.txt"}`))
	if res.IsError {
		t.Fatalf("unexpected error %s", res.Content)
	}
	if !strings.Contains(res.Content, "more lines") {
		t.Error("expected truncation notice for large file")
	}
	// explicit range
	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"path":"big.txt","start_line":400,"end_line":410}`))
	if strings.Count(res.Content, "\tline\n") != 11 { // lines 400..410 inclusive
		t.Errorf("expected 10 lines, got:\n%s", res.Content)
	}
	// out of range
	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"path":"big.txt","start_line":9999}`))
	if !res.IsError {
		t.Error("start beyond EOF should be an error result")
	}
	// escape attempt
	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"path":"../../etc/passwd"}`))
	if !res.IsError {
		t.Error("path escape should be rejected")
	}
}

// --- list_files ---

func TestListFiles(t *testing.T) {
	ws := testWorkspace(t)
	write(t, ws, "go.mod", "module x")
	write(t, ws, "src/main.go", "package main")
	write(t, ws, ".git/config", "x")
	tool := &ListFilesTool{WS: ws}

	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"path":".","max_depth":3}`))
	if res.IsError {
		t.Fatalf("list failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "src/") || !strings.Contains(res.Content, "main.go") {
		t.Errorf("missing entries:\n%s", res.Content)
	}
	if strings.Contains(res.Content, ".git") {
		t.Error(".git should be skipped")
	}
}

// --- run_command ---

func TestRunCommand(t *testing.T) {
	ws := testWorkspace(t)
	exec := &Executor{WS: ws, Policy: permission.DefaultPolicy(), DefaultTimeout: 10_000_000_000, MaxTimeout: 60_000_000_000}
	tool := &RunCommandTool{Exec: exec, Approver: permission.DenyAll{}}

	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"command":"echo hello"}`))
	if res.IsError || !strings.Contains(res.Content, "hello") || !strings.Contains(res.Content, "exit_code: 0") {
		t.Errorf("unexpected result:\n%s", res.Content)
	}

	// failing command: exit code captured, marked as error for the agent
	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"command":"echo oops >&2; grep -q oops /dev/null"}`))
	if !res.IsError || !strings.Contains(res.Content, "exit_code: 1") || !strings.Contains(res.Content, "oops") {
		t.Errorf("expected failure capture:\n%s", res.Content)
	}

	// blocked command
	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"command":"sudo shutdown now"}`))
	if !res.IsError || !strings.Contains(res.Content, "refused") {
		t.Errorf("blocked command should be refused:\n%s", res.Content)
	}

	// ask command denied by approver
	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"command":"docker build ."}`))
	if !res.IsError || !strings.Contains(res.Content, "rejected") {
		t.Errorf("denied ask command should report rejection:\n%s", res.Content)
	}

	// timeout
	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"command":"tail -f /dev/null","timeout":1}`))
	if !strings.Contains(res.Content, "timed_out: true") {
		t.Errorf("expected timeout:\n%s", res.Content)
	}
}

// --- search_code ---

func TestSearchCode(t *testing.T) {
	ws := testWorkspace(t)
	write(t, ws, "main.go", "package main\n\nfunc reconnect() {}\n")
	write(t, ws, "README.md", "nothing here\n")
	tool := &SearchCodeTool{WS: ws}

	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"query":"reconnect"}`))
	if res.IsError || !strings.Contains(res.Content, "main.go:3") {
		t.Errorf("unexpected result:\n%s", res.Content)
	}
	if strings.Contains(res.Content, ws.Root()) {
		t.Error("paths should be workspace-relative")
	}

	res, _ = tool.Execute(context.Background(), json.RawMessage(`{"query":"definitely-not-present"}`))
	if res.IsError || res.Content != "(no matches)" {
		t.Errorf("unexpected no-match result: %+v", res)
	}
}

// --- git tools ---

func TestGitTools(t *testing.T) {
	ws := testWorkspace(t)
	git := func(args ...string) {
		t.Helper()
		tool := &RunCommandTool{Exec: &Executor{WS: ws, Policy: permission.DefaultPolicy(), DefaultTimeout: 10_000_000_000}, Approver: permission.ApproveAll{}}
		res, _ := tool.Execute(context.Background(), json.RawMessage(
			`{"command":"git `+strings.Join(args, " ")+`"}`))
		if res.IsError {
			t.Fatalf("git %v failed: %s", args, res.Content)
		}
	}
	git("init")
	write(t, ws, "a.txt", "hello\n")
	git("add", ".")
	git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")

	write(t, ws, "a.txt", "hello changed\n")

	status, _ := (&GitStatusTool{WS: ws}).Execute(context.Background(), nil)
	if !strings.Contains(status.Content, "M a.txt") {
		t.Errorf("git_status should show modified file:\n%s", status.Content)
	}
	diff, _ := (&GitDiffTool{WS: ws}).Execute(context.Background(), json.RawMessage(`{}`))
	if !strings.Contains(diff.Content, "+hello changed") {
		t.Errorf("git_diff should show the change:\n%s", diff.Content)
	}
}
