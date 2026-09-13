package agent

import (
	"runtime"
	"strings"
	"testing"
)

func TestBuildSystemPrompt(t *testing.T) {
	p := BuildSystemPrompt("/tmp/ws")
	if !strings.HasPrefix(p, SystemPrompt) {
		t.Error("built prompt must start with the base SystemPrompt")
	}
	for _, want := range []string{"/tmp/ws", runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(p, want) {
			t.Errorf("built prompt missing %q", want)
		}
	}
}

func TestAgentSystemPrompt(t *testing.T) {
	a := &Agent{}
	if got := a.systemPrompt(); got != SystemPrompt {
		t.Error("empty WorkspaceRoot must yield the bare SystemPrompt")
	}
	a.WorkspaceRoot = "/tmp/ws"
	if got := a.systemPrompt(); got != BuildSystemPrompt("/tmp/ws") {
		t.Error("WorkspaceRoot set must yield BuildSystemPrompt output")
	}
}
