package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"local-codex/internal/workspace"
)

// runGit runs a read-only git command inside the workspace.
func runGit(ctx context.Context, ws *workspace.Workspace, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = ws.Root()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, truncate(stderr.String(), 500))
	}
	return stdout.String(), nil
}

// GitStatusTool runs `git status --short`.
type GitStatusTool struct {
	WS *workspace.Workspace
}

func (t *GitStatusTool) Name() string { return "git_status" }
func (t *GitStatusTool) Description() string {
	return "Show the working tree status (git status --short): modified, added and untracked files."
}
func (t *GitStatusTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *GitStatusTool) Execute(ctx context.Context, _ json.RawMessage) (Result, error) {
	out, err := runGit(ctx, t.WS, "status", "--short")
	if err != nil {
		return Error("%v", err), nil
	}
	if strings.TrimSpace(out) == "" {
		return Text("(clean working tree)"), nil
	}
	return Text("%s", out), nil
}

// GitDiffTool runs `git diff` so the agent can review its own changes.
type GitDiffTool struct {
	WS *workspace.Workspace
}

func (t *GitDiffTool) Name() string { return "git_diff" }
func (t *GitDiffTool) Description() string {
	return "Show uncommitted changes (git diff). Use this to review what you changed before finishing. Optionally include staged changes."
}
func (t *GitDiffTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"staged": map[string]any{"type": "boolean", "description": "Show staged changes (git diff --staged) instead of unstaged."},
			"path":   map[string]any{"type": "string", "description": "Limit the diff to a path relative to the workspace root."},
		},
	}
}

type gitDiffArgs struct {
	Staged bool   `json:"staged"`
	Path   string `json:"path"`
}

func (t *GitDiffTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a gitDiffArgs
	if err := decodeArgs(args, &a); err != nil {
		return Error("%v", err), nil
	}
	argv := []string{"diff"}
	if a.Staged {
		argv = append(argv, "--staged")
	}
	if a.Path != "" {
		abs, err := t.WS.Resolve(a.Path)
		if err != nil {
			return Error("%v", err), nil
		}
		argv = append(argv, "--", t.WS.Rel(abs))
	}
	out, err := runGit(ctx, t.WS, argv...)
	if err != nil {
		return Error("%v", err), nil
	}
	if strings.TrimSpace(out) == "" {
		return Text("(no diff)"), nil
	}
	return Text("%s", truncate(out, 40_000)), nil
}
