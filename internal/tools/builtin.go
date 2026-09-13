package tools

import (
	"time"

	"local-codex/internal/permission"
	"local-codex/internal/workspace"
)

// NewBuiltinRegistry wires all built-in tools into a registry.
func NewBuiltinRegistry(ws *workspace.Workspace, approver permission.Approver,
	defaultTimeout, maxTimeout time.Duration) *Registry {
	r := NewRegistry()
	exec := &Executor{
		WS:             ws,
		Policy:         permission.DefaultPolicy(),
		DefaultTimeout: defaultTimeout,
		MaxTimeout:     maxTimeout,
	}
	must := func(t Tool) {
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
	must(&ListFilesTool{WS: ws})
	must(&ReadFileTool{WS: ws})
	must(&SearchCodeTool{WS: ws})
	must(&ApplyPatchTool{WS: ws})
	must(&RunCommandTool{Exec: exec, Approver: approver})
	must(&GitStatusTool{WS: ws})
	must(&GitDiffTool{WS: ws})
	return r
}
