package session

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"local-codex/internal/llm"
)

func TestSnapshotIsDeepCopy(t *testing.T) {
	s := &Session{ID: "s1", Task: "task"}
	s.AppendMessage(llm.Message{Role: "assistant", Content: "hi",
		ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function"}}})
	s.AddToolRecord(ToolRecord{Name: "run_command", Args: "ls"})
	s.AddModifiedFiles([]string{"a.go"})

	snap := s.Snapshot()
	if len(snap.Messages) != 1 || len(snap.ToolHistory) != 1 ||
		len(snap.ModifiedFiles) != 1 || len(snap.Commands) != 1 {
		t.Fatalf("snapshot missing data: %+v", snap)
	}

	// Mutating the original after the snapshot must not leak into it.
	s.AppendMessage(llm.Message{Role: "user", Content: "more"})
	s.AddToolRecord(ToolRecord{Name: "list_files", Args: "{}"})
	s.AddModifiedFiles([]string{"b.go"})
	s.Finish("done")
	if len(snap.Messages) != 1 || len(snap.ToolHistory) != 1 ||
		len(snap.ModifiedFiles) != 1 || len(snap.Commands) != 1 || snap.Done {
		t.Fatalf("snapshot changed after further mutations: %+v", snap)
	}

	// Mutating the snapshot (incl. nested slices) must not affect the
	// original.
	snap.Messages[0].ToolCalls[0].ID = "mutated"
	if s.Snapshot().Messages[0].ToolCalls[0].ID != "c1" {
		t.Fatal("nested ToolCalls slice is shared with the snapshot")
	}
}

// TestSnapshotConcurrentAccess exercises the pattern the HTTP handlers use:
// marshal snapshots while the agent goroutine mutates the session. Clean
// under `go test -race`; without Snapshot it data-races on every field.
func TestSnapshotConcurrentAccess(t *testing.T) {
	store := NewMemoryStore()
	s := store.Create("task")

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.AppendMessage(llm.Message{Role: "assistant",
					Content:   fmt.Sprintf("w%d-%d", w, i),
					ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("c%d", i)}}})
				s.AddToolRecord(ToolRecord{Name: "run_command", Args: fmt.Sprintf("cmd %d", i)})
				s.AddModifiedFiles([]string{fmt.Sprintf("f%d.go", i)})
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := json.Marshal(s.Snapshot()); err != nil {
					t.Errorf("marshal snapshot: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	s.Finish("done")
	snap := s.Snapshot()
	if !snap.Done || snap.FinalAnswer != "done" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Messages) != 800 || len(snap.ToolHistory) != 800 {
		t.Errorf("counts: messages=%d tool=%d", len(snap.Messages), len(snap.ToolHistory))
	}
}
