package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"local-codex/internal/workspace"
)

// ApplyPatchTool applies model-generated patches in the "*** Begin Patch"
// format, so the model edits code with small diffs instead of regenerating
// whole files.
//
//	*** Begin Patch
//	*** Update File: src/main.go
//	@@
//	 context line
//	-old line
//	+new line
//	*** Add File: new.go
//	+package main
//	*** Delete File: old.go
//	*** End Patch
//
// Application failures return a detailed error Result so the model can
// correct and retry — failures are never silent.
type ApplyPatchTool struct {
	WS *workspace.Workspace
	// OnFilesChanged is called with the workspace-relative paths the patch touched.
	OnFilesChanged func(paths []string)
}

func (t *ApplyPatchTool) Name() string { return "apply_patch" }
func (t *ApplyPatchTool) Description() string {
	return "Modify files by applying a patch. Format: lines between '*** Begin Patch' and '*** End Patch', with sections '*** Update File: <path>' (hunks start with '@@'; ' ' = context, '-' = remove, '+' = add), '*** Add File: <path>' (all lines are added, optionally '+'-prefixed) and '*** Delete File: <path>'. Prefer small patches over rewriting whole files."
}
func (t *ApplyPatchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"patch": map[string]any{"type": "string", "description": "The full patch text including '*** Begin Patch' and '*** End Patch'."},
		},
		"required": []string{"patch"},
	}
}

type applyPatchArgs struct {
	Patch string `json:"patch"`
}

type patchOp struct {
	kind  string // "update" | "add" | "delete"
	path  string
	hunks []hunk // update only
	added []string
}

type hunk struct {
	oldLines []string
	newLines []string
}

func (t *ApplyPatchTool) Execute(ctx context.Context, args json.RawMessage) (Result, error) {
	var a applyPatchArgs
	if err := decodeArgs(args, &a); err != nil {
		return Error("%v", err), nil
	}
	ops, err := parsePatch(a.Patch)
	if err != nil {
		return Error("invalid patch: %v", err), nil
	}
	if err := ctx.Err(); err != nil {
		return Error("patch cancelled: %v", err), nil
	}

	var changed []string
	for _, op := range ops {
		abs, err := t.WS.Resolve(op.path)
		if err != nil {
			return Error("patch aborted before applying %s: %v", op.path, err), nil
		}
		switch op.kind {
		case "add":
			if err := applyAdd(abs, op.added); err != nil {
				return Error("add %s failed: %v", op.path, err), nil
			}
		case "delete":
			if err := os.Remove(abs); err != nil {
				return Error("delete %s failed: %v", op.path, err), nil
			}
		case "update":
			if err := applyUpdate(abs, op.hunks); err != nil {
				return Error("update %s failed: %v", op.path, err), nil
			}
		}
		changed = append(changed, op.path)
	}
	if t.OnFilesChanged != nil {
		t.OnFilesChanged(changed)
	}
	return Text("patch applied successfully; files changed: %s", strings.Join(changed, ", ")), nil
}

func parsePatch(patch string) ([]patchOp, error) {
	lines := strings.Split(patch, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "*** Begin Patch" {
		return nil, fmt.Errorf("patch must start with '*** Begin Patch'")
	}
	i++

	var ops []patchOp
	var cur *patchOp
	var curHunk *hunk
	closed := false

	flushHunk := func() {
		if cur != nil && curHunk != nil {
			cur.hunks = append(cur.hunks, *curHunk)
			curHunk = nil
		}
	}

	for ; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "*** End Patch":
			flushHunk()
			closed = true
			i = len(lines)
		case strings.HasPrefix(trimmed, "*** Update File:"):
			flushHunk()
			ops = append(ops, patchOp{kind: "update", path: strings.TrimSpace(trimmed[len("*** Update File:"):])})
			cur = &ops[len(ops)-1]
		case strings.HasPrefix(trimmed, "*** Add File:"):
			flushHunk()
			ops = append(ops, patchOp{kind: "add", path: strings.TrimSpace(trimmed[len("*** Add File:"):])})
			cur = &ops[len(ops)-1]
		case strings.HasPrefix(trimmed, "*** Delete File:"):
			flushHunk()
			ops = append(ops, patchOp{kind: "delete", path: strings.TrimSpace(trimmed[len("*** Delete File:"):])})
			cur = &ops[len(ops)-1]
		case strings.HasPrefix(trimmed, "@@"):
			if cur == nil || cur.kind != "update" {
				return nil, fmt.Errorf("line %d: '@@' hunk outside an Update File section", i+1)
			}
			flushHunk()
			curHunk = &hunk{}
		default:
			if cur == nil {
				return nil, fmt.Errorf("line %d: content outside any file section: %q", i+1, line)
			}
			switch cur.kind {
			case "add":
				cur.added = append(cur.added, strings.TrimPrefix(line, "+"))
			case "update":
				if curHunk == nil {
					return nil, fmt.Errorf("line %d: hunk content before '@@' in %s", i+1, cur.path)
				}
				switch {
				case strings.HasPrefix(line, "-"):
					curHunk.oldLines = append(curHunk.oldLines, line[1:])
				case strings.HasPrefix(line, "+"):
					curHunk.newLines = append(curHunk.newLines, line[1:])
				default: // ' ' prefix or bare context line
					ctx := strings.TrimPrefix(line, " ")
					curHunk.oldLines = append(curHunk.oldLines, ctx)
					curHunk.newLines = append(curHunk.newLines, ctx)
				}
			case "delete":
				if trimmed != "" {
					return nil, fmt.Errorf("line %d: Delete File section must not have content", i+1)
				}
			}
		}
	}
	if !closed {
		return nil, fmt.Errorf("patch missing '*** End Patch'")
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("patch contains no file operations")
	}
	for _, op := range ops {
		if op.path == "" {
			return nil, fmt.Errorf("file operation with empty path")
		}
	}
	return ops, nil
}

func applyAdd(abs string, lines []string) error {
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("file already exists (use Update File instead)")
	}
	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

func applyUpdate(abs string, hunks []hunk) error {
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	hadTrailingNewline := strings.HasSuffix(string(data), "\n")
	lines := strings.Split(string(data), "\n")
	if hadTrailingNewline {
		lines = lines[:len(lines)-1]
	}

	cursor := 0
	for hi, h := range hunks {
		if len(h.oldLines) == 0 {
			return fmt.Errorf("hunk %d has no context/removal lines to locate", hi+1)
		}
		at := findSubslice(lines, h.oldLines, cursor)
		if at < 0 {
			// retry from the top in case hunks arrived out of order
			at = findSubslice(lines, h.oldLines, 0)
			if at < 0 {
				return fmt.Errorf("hunk %d not found; expected lines not present:\n%s",
					hi+1, "- "+strings.Join(h.oldLines, "\n- "))
			}
		}
		replaced := make([]string, 0, len(lines)-len(h.oldLines)+len(h.newLines))
		replaced = append(replaced, lines[:at]...)
		replaced = append(replaced, h.newLines...)
		replaced = append(replaced, lines[at+len(h.oldLines):]...)
		lines = replaced
		cursor = at + len(h.newLines)
	}

	out := strings.Join(lines, "\n")
	if hadTrailingNewline {
		out += "\n"
	}
	// Write back with the file's original mode so updating an executable
	// script does not strip its permission bits.
	return os.WriteFile(abs, []byte(out), info.Mode().Perm())
}

// findSubslice returns the index of the first occurrence of sub in lines at
// or after from, or -1.
func findSubslice(lines, sub []string, from int) int {
	if len(sub) > len(lines) {
		return -1
	}
	for i := from; i+len(sub) <= len(lines); i++ {
		match := true
		for j := range sub {
			if lines[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
