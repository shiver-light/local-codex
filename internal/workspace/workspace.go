// Package workspace confines every file access to a single root directory.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace represents a sandboxed project root. All paths resolved through
// it are guaranteed to live inside Root (after symlink evaluation).
type Workspace struct {
	root string // absolute, symlink-resolved
}

// Open creates a Workspace rooted at root. The root must exist.
func Open(root string) (*Workspace, error) {
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root %q: %w", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root %q: %w", root, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat workspace root %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", resolved)
	}
	return &Workspace{root: resolved}, nil
}

// Root returns the absolute, symlink-resolved workspace root.
func (w *Workspace) Root() string { return w.root }

// Resolve turns a user/agent-supplied path into an absolute path that is
// guaranteed to be inside the workspace. It rejects:
//   - paths that escape via ".."
//   - absolute paths outside the root
//   - symlinks pointing outside the root
//
// Non-existent paths (write targets) are checked against their nearest
// existing ancestor.
func (w *Workspace) Resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(w.root, p)
	}

	if !within(w.root, abs) {
		return "", fmt.Errorf("path %q escapes workspace root %q", p, w.root)
	}

	// Evaluate symlinks on the deepest existing ancestor to catch
	// symlink-based escapes, including for not-yet-created files.
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if !within(w.root, resolved) {
				return "", fmt.Errorf("path %q resolves through a symlink outside workspace root %q", p, w.root)
			}
			// Re-anchor any non-existent suffix onto the resolved ancestor.
			if probe != abs {
				suffix, relErr := filepath.Rel(probe, abs)
				if relErr != nil {
					return "", fmt.Errorf("resolve path %q: %w", p, relErr)
				}
				abs = filepath.Join(resolved, suffix)
			} else {
				abs = resolved
			}
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("resolve path %q: %w", p, err)
		}
		probe = parent
	}

	if !within(w.root, abs) {
		return "", fmt.Errorf("path %q escapes workspace root %q", p, w.root)
	}
	return abs, nil
}

// Rel returns p relative to the workspace root (for display).
func (w *Workspace) Rel(p string) string {
	if rel, err := filepath.Rel(w.root, p); err == nil {
		return rel
	}
	return p
}

// within reports whether path is root itself or lives under root.
// Both must be absolute and clean.
func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
