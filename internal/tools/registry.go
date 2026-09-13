package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"local-codex/internal/llm"
)

// Registry holds all available tools by name. The agent loop executes tools
// exclusively through the registry — it never hardcodes tool names.
type Registry struct {
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register adds a tool. It fails on duplicate names.
func (r *Registry) Register(t Tool) error {
	if t == nil || t.Name() == "" {
		return fmt.Errorf("cannot register tool with empty name")
	}
	if _, exists := r.tools[t.Name()]; exists {
		return fmt.Errorf("tool %q already registered", t.Name())
	}
	r.tools[t.Name()] = t
	return nil
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Execute runs the named tool with raw JSON arguments. Unknown tools return
// an error Result (so the model learns the tool does not exist) rather than
// crashing the loop.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) Result {
	t, ok := r.tools[name]
	if !ok {
		return Error("unknown tool %q; available tools: %v", name, r.Names())
	}
	res, err := t.Execute(ctx, args)
	if err != nil {
		return Error("tool %q failed: %v", name, err)
	}
	return res
}

// Names returns sorted tool names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Definitions returns the wire definitions for all tools (sorted by name).
func (r *Registry) Definitions() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(r.tools))
	for _, name := range r.Names() {
		defs = append(defs, Definition(r.tools[name]))
	}
	return defs
}
