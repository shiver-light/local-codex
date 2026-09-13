package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	"local-codex/internal/workspace"
)

// SearchCodeTool searches the codebase with ripgrep, falling back to grep.
type SearchCodeTool struct {
	WS *workspace.Workspace
}

func (t *SearchCodeTool) Name() string { return "search_code" }
func (t *SearchCodeTool) Description() string {
	return "Search for a pattern in the workspace source code (ripgrep, falling back to grep). Returns matching lines with file paths and line numbers."
}
func (t *SearchCodeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":        map[string]any{"type": "string", "description": "Search pattern (regular expression)."},
			"path":         map[string]any{"type": "string", "description": "Subdirectory to search in, relative to the workspace root (default: root)."},
			"file_pattern": map[string]any{"type": "string", "description": "Glob limiting which files are searched, e.g. \"*.go\"."},
		},
		"required": []string{"query"},
	}
}

type searchArgs struct {
	Query       string `json:"query"`
	Path        string `json:"path"`
	FilePattern string `json:"file_pattern"`
}

const maxSearchOutput = 20_000

func (t *SearchCodeTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a searchArgs
	if err := decodeArgs(args, &a); err != nil {
		return Error("%v", err), nil
	}
	if a.Query == "" {
		return Error("query is required"), nil
	}

	dir, err := t.WS.Resolve(a.Path)
	if err != nil {
		return Error("%v", err), nil
	}

	var cmd *exec.Cmd
	if rgPath, lookErr := exec.LookPath("rg"); lookErr == nil {
		argv := []string{"-n", "--no-heading", "--color=never", "-S"}
		if a.FilePattern != "" {
			argv = append(argv, "-g", a.FilePattern)
		}
		argv = append(argv, "--", a.Query, dir)
		cmd = exec.CommandContext(ctx, rgPath, argv...)
	} else {
		argv := []string{"-rn"}
		if a.FilePattern != "" {
			argv = append(argv, "--include="+a.FilePattern)
		}
		argv = append(argv, "--", a.Query, dir)
		cmd = exec.CommandContext(ctx, "grep", argv...)
	}
	cmd.Dir = t.WS.Root()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	out := stdout.String()
	// rg/grep exit code 1 means "no matches" — not an error.
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); !ok || exitErr.ExitCode() > 1 {
			return Error("search failed: %v\n%s", runErr, truncate(stderr.String(), 1000)), nil
		}
	}
	out = makePathsRelative(out, t.WS.Root())
	if strings.TrimSpace(out) == "" {
		return Text("(no matches)"), nil
	}
	return Text("%s", truncate(out, maxSearchOutput)), nil
}

func makePathsRelative(out, root string) string {
	return strings.ReplaceAll(out, root+"/", "")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...(truncated)"
}
