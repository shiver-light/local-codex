package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"local-codex/internal/permission"
	"local-codex/internal/workspace"
)

// ExecResult captures everything about one command execution.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Duration string `json:"duration"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

const maxCommandOutput = 30_000

// Executor runs shell commands inside the workspace under a Policy.
type Executor struct {
	WS             *workspace.Workspace
	Policy         *permission.Policy
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

// Run executes command via `bash -c` in cwd (relative to the workspace root).
// stdout/stderr are captured separately; the exit code is always reported,
// including for failing commands (err is nil unless the process could not
// be started at all).
func (e *Executor) Run(ctx context.Context, command, cwd string, timeout time.Duration) (ExecResult, error) {
	if timeout <= 0 {
		timeout = e.DefaultTimeout
	}
	if e.MaxTimeout > 0 && timeout > e.MaxTimeout {
		timeout = e.MaxTimeout
	}

	dir := e.WS.Root()
	if cwd != "" {
		abs, err := e.WS.Resolve(cwd)
		if err != nil {
			return ExecResult{}, err
		}
		dir = abs
	}

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(tctx, "bash", "-c", command)
	cmd.Dir = dir
	setProcGroup(cmd)
	// The default Cancel kills only the direct child (bash), leaving
	// backgrounded grandchildren alive — and their inherited pipes would
	// block Wait forever. Kill the whole process group instead.
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	// Bound Wait so output-copy goroutines cannot hang on pipes inherited
	// by a process that survived the kill.
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	res := ExecResult{
		Stdout:   truncate(stdout.String(), maxCommandOutput),
		Stderr:   truncate(stderr.String(), maxCommandOutput),
		Duration: time.Since(start).Round(time.Millisecond).String(),
	}
	if tctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("start command: %w", err)
	}
	return res, nil
}

// RunCommandTool exposes the Executor to the agent, enforcing the command
// policy and routing "ask" commands through the Approver.
type RunCommandTool struct {
	Exec     *Executor
	Approver permission.Approver
	// OnApproval is notified when a command is approved/rejected (events).
	OnApproval func(command string, approved bool)
}

func (t *RunCommandTool) Name() string { return "run_command" }
func (t *RunCommandTool) Description() string {
	return "Run a shell command inside the workspace (e.g. read-only commands and builds/tests like `go test ./...`). Returns stdout, stderr and the exit code. Interpreters, package managers, git mutations and unknown commands require user approval; destructive commands are refused."
}
func (t *RunCommandTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "The shell command to execute."},
			"cwd":     map[string]any{"type": "string", "description": "Working directory relative to the workspace root (default: root)."},
			"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds (default from config)."},
		},
		"required": []string{"command"},
	}
}

type runCommandArgs struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	Timeout int    `json:"timeout"`
}

func (t *RunCommandTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a runCommandArgs
	if err := decodeArgs(args, &a); err != nil {
		return Error("%v", err), nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return Error("command is required"), nil
	}

	decision, reason := t.Exec.Policy.Classify(a.Command)
	switch decision {
	case permission.Blocked:
		return Error("command refused by policy: %s", reason), nil
	case permission.Ask:
		if t.Approver == nil {
			return Error("command requires approval (%s) but no approver is configured", reason), nil
		}
		ok, err := t.Approver.Approve(ctx, a.Command, reason)
		if err != nil {
			return Error("approval failed: %v", err), nil
		}
		if t.OnApproval != nil {
			t.OnApproval(a.Command, ok)
		}
		if !ok {
			return Error("command rejected by user"), nil
		}
	}

	res, err := t.Exec.Run(ctx, a.Command, a.Cwd, time.Duration(a.Timeout)*time.Second)
	if err != nil {
		return Error("%v", err), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\nduration: %s\n", res.ExitCode, res.Duration)
	if res.TimedOut {
		b.WriteString("timed_out: true\n")
	}
	b.WriteString("--- stdout ---\n")
	b.WriteString(res.Stdout)
	if res.Stdout == "" {
		b.WriteString("(empty)\n")
	}
	b.WriteString("--- stderr ---\n")
	b.WriteString(res.Stderr)
	if res.Stderr == "" {
		b.WriteString("(empty)\n")
	}
	out := Result{Content: b.String()}
	if res.ExitCode != 0 {
		out.IsError = true // signal failure to the agent so it fixes it
	}
	return out, nil
}
