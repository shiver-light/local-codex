//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"local-codex/internal/permission"
)

func TestReadFileRejectsFIFO(t *testing.T) {
	ws := testWorkspace(t)
	if err := syscall.Mkfifo(filepath.Join(ws.Root(), "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFileTool{WS: ws}

	// Reading a FIFO would block forever; the stat check must reject it.
	done := make(chan Result, 1)
	go func() {
		res, _ := tool.Execute(context.Background(), json.RawMessage(`{"path":"pipe"}`))
		done <- res
	}()
	select {
	case res := <-done:
		if !res.IsError || !strings.Contains(res.Content, "FIFO") {
			t.Errorf("FIFO should be rejected:\n%s", res.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read_file blocked on a FIFO")
	}
}

func TestRunCommandKillsProcessGroup(t *testing.T) {
	ws := testWorkspace(t)
	exec := &Executor{WS: ws, Policy: permission.DefaultPolicy(), DefaultTimeout: 10 * time.Second, MaxTimeout: 60 * time.Second}
	tool := &RunCommandTool{Exec: exec, Approver: permission.ApproveAll{}}

	// The backgrounded grandchild would outlive a plain kill of bash.
	res, _ := tool.Execute(context.Background(), json.RawMessage(
		`{"command":"sleep 2 && touch marker.txt & wait","timeout":1}`))
	if !strings.Contains(res.Content, "timed_out: true") {
		t.Fatalf("expected timeout:\n%s", res.Content)
	}
	// If the grandchild survived, it creates the marker after the timeout.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(ws.Root(), "marker.txt")); !os.IsNotExist(err) {
		t.Error("background grandchild survived the timeout; process group was not killed")
	}
}
