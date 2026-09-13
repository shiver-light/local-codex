package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"local-codex/internal/workspace"
)

// defaultMaxReadLines caps a single read so large files never flood the
// context; the agent can page with start_line/end_line.
const defaultMaxReadLines = 300

// ReadFileTool reads (a portion of) a file inside the workspace.
type ReadFileTool struct {
	WS *workspace.Workspace
}

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "Read a file from the workspace. Reads at most 300 lines by default; use start_line/end_line to page through large files. Never assume file contents — read them."
}
func (t *ReadFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "File path relative to the workspace root."},
			"start_line": map[string]any{"type": "integer", "description": "1-based first line to read (default 1)."},
			"end_line":   map[string]any{"type": "integer", "description": "1-based last line to read (inclusive)."},
		},
		"required": []string{"path"},
	}
}

type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a readFileArgs
	if err := decodeArgs(args, &a); err != nil {
		return Error("%v", err), nil
	}
	if a.Path == "" {
		return Error("path is required"), nil
	}

	abs, err := t.WS.Resolve(a.Path)
	if err != nil {
		return Error("%v", err), nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Error("read %s: %v", a.Path, err), nil
	}

	const maxFileBytes = 4 << 20
	if len(data) > maxFileBytes {
		return Error("file %s is too large (%d bytes); read it with start_line/end_line via search_code instead", a.Path, len(data)), nil
	}

	lines := strings.Split(string(data), "\n")
	total := len(lines)
	// A trailing newline produces a phantom empty last element.
	if total > 0 && lines[total-1] == "" {
		total--
	}

	start := a.StartLine
	if start <= 0 {
		start = 1
	}
	end := a.EndLine
	if end <= 0 || end-start+1 > defaultMaxReadLines {
		end = start + defaultMaxReadLines - 1
	}
	if end > total {
		end = total
	}
	if start > total {
		return Error("start_line %d is beyond end of file (%d lines)", start, total), nil
	}

	var b strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
	}
	if end < total {
		fmt.Fprintf(&b, "...(%d more lines; use start_line=%d to continue)\n", total-end, end+1)
	}
	return Text("%s", b.String()), nil
}
