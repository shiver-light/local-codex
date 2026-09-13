package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"local-codex/internal/workspace"
)

// directories that are noise for an agent exploring a repo.
var listSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"target": true, "__pycache__": true, ".idea": true, ".vscode": true,
}

// ListFilesTool lists the directory tree with a depth limit.
type ListFilesTool struct {
	WS *workspace.Workspace
}

func (t *ListFilesTool) Name() string { return "list_files" }
func (t *ListFilesTool) Description() string {
	return "List files and directories under a path in the workspace, up to max_depth levels deep. Use this to explore the repository structure."
}
func (t *ListFilesTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":      map[string]any{"type": "string", "description": "Directory to list, relative to the workspace root. Defaults to the root."},
			"max_depth": map[string]any{"type": "integer", "description": "Maximum directory depth to descend (default 3)."},
		},
	}
}

type listFilesArgs struct {
	Path     string `json:"path"`
	MaxDepth int    `json:"max_depth"`
}

func (t *ListFilesTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a listFilesArgs
	if err := decodeArgs(args, &a); err != nil {
		return Error("%v", err), nil
	}
	if a.MaxDepth <= 0 {
		a.MaxDepth = 3
	}

	base, err := t.WS.Resolve(a.Path)
	if err != nil {
		return Error("%v", err), nil
	}

	var b strings.Builder
	const maxEntries = 500
	count := 0
	truncated := false

	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		name := d.Name()
		if d.IsDir() {
			if skip, ok := listSkipDirs[name]; ok && skip {
				return filepath.SkipDir
			}
			if depth > a.MaxDepth {
				return filepath.SkipDir
			}
		}
		if depth > a.MaxDepth {
			return nil
		}
		if count >= maxEntries {
			truncated = true
			return filepath.SkipAll
		}
		count++
		indent := strings.Repeat("  ", depth-1)
		if d.IsDir() {
			fmt.Fprintf(&b, "%s%s/\n", indent, name)
		} else {
			fmt.Fprintf(&b, "%s%s\n", indent, name)
		}
		return nil
	})
	if err != nil {
		return Error("list files: %v", err), nil
	}
	if count == 0 {
		return Text("(empty directory: %s)", t.WS.Rel(base)), nil
	}
	out := b.String()
	if truncated {
		out += fmt.Sprintf("...(truncated at %d entries; narrow the path or depth)\n", maxEntries)
	}
	return Text("%s", out), nil
}
