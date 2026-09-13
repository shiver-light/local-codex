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
	if err := ctx.Err(); err != nil {
		return Error("read cancelled: %v", err), nil
	}

	// Stat before reading: Resolve already evaluates symlinks for existing
	// paths, so Stat observes the final target. This rejects non-regular
	// files (a FIFO or device would block ReadFile forever) and oversized
	// files before they are loaded into memory.
	info, err := os.Stat(abs)
	if err != nil {
		return Error("read %s: %v", a.Path, err), nil
	}
	if !info.Mode().IsRegular() {
		return Error("%s is not a regular file (%s); only regular files can be read", a.Path, fileKind(info.Mode())), nil
	}
	const maxFileBytes = 4 << 20
	if info.Size() > maxFileBytes {
		return Error("file %s is too large (%d bytes); read it with start_line/end_line via search_code instead", a.Path, info.Size()), nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return Error("read %s: %v", a.Path, err), nil
	}
	if looksBinary(data) {
		return Error("file %s looks like a binary file; refusing to load it into context", a.Path), nil
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

// fileKind describes a non-regular file type for error messages.
func fileKind(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&os.ModeNamedPipe != 0:
		return "FIFO/named pipe"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device file"
	case m&os.ModeSymlink != 0:
		return "symlink"
	default:
		return m.Type().String()
	}
}

// looksBinary reports whether data appears to be binary: a NUL byte in the
// first 8KB, or a high proportion of non-printable control bytes.
func looksBinary(data []byte) bool {
	const sniff = 8192
	if len(data) > sniff {
		data = data[:sniff]
	}
	if len(data) == 0 {
		return false
	}
	bad := 0
	for _, b := range data {
		if b == 0 {
			return true
		}
		if (b < 0x20 && b != '\t' && b != '\n' && b != '\r') || b == 0x7f {
			bad++
		}
	}
	return float64(bad)/float64(len(data)) > 0.3
}
