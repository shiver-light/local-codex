package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchUpdate(t *testing.T) {
	ws := testWorkspace(t)
	write(t, ws, "main.go", "package main\n\nfunc add(a, b int) int {\n\treturn a - b // BUG\n}\n")
	tool := &ApplyPatchTool{WS: ws}

	patch := `*** Begin Patch
*** Update File: main.go
@@
 func add(a, b int) int {
-	return a - b // BUG
+	return a + b
 }
*** End Patch`
	res, _ := tool.Execute(context.Background(), json.RawMessage(mustJSON(t, map[string]string{"patch": patch})))
	if res.IsError {
		t.Fatalf("patch failed: %s", res.Content)
	}
	data, _ := os.ReadFile(filepath.Join(ws.Root(), "main.go"))
	if !strings.Contains(string(data), "return a + b") {
		t.Errorf("file not updated:\n%s", data)
	}
}

func TestApplyPatchAddAndDelete(t *testing.T) {
	ws := testWorkspace(t)
	write(t, ws, "old.txt", "bye\n")
	tool := &ApplyPatchTool{WS: ws}

	patch := `*** Begin Patch
*** Add File: sub/new.go
+package sub
+
+func Hello() {}
*** Delete File: old.txt
*** End Patch`
	res, _ := tool.Execute(context.Background(), json.RawMessage(mustJSON(t, map[string]string{"patch": patch})))
	if res.IsError {
		t.Fatalf("patch failed: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "sub", "new.go")); err != nil {
		t.Error("new.go should exist")
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "old.txt")); !os.IsNotExist(err) {
		t.Error("old.txt should be deleted")
	}
}

func TestApplyPatchFailureIsReported(t *testing.T) {
	ws := testWorkspace(t)
	write(t, ws, "a.go", "package a\n")
	tool := &ApplyPatchTool{WS: ws}

	patch := `*** Begin Patch
*** Update File: a.go
@@
-this line does not exist
+replacement
*** End Patch`
	res, _ := tool.Execute(context.Background(), json.RawMessage(mustJSON(t, map[string]string{"patch": patch})))
	if !res.IsError {
		t.Fatal("mismatched hunk must be an error result, not silent")
	}
	if !strings.Contains(res.Content, "not found") {
		t.Errorf("error should explain what failed:\n%s", res.Content)
	}
	// file must be untouched
	data, _ := os.ReadFile(filepath.Join(ws.Root(), "a.go"))
	if string(data) != "package a\n" {
		t.Error("failed patch must not modify the file")
	}
}

func TestApplyPatchMalformed(t *testing.T) {
	ws := testWorkspace(t)
	tool := &ApplyPatchTool{WS: ws}
	for _, p := range []string{
		"no markers at all",
		"*** Begin Patch\n*** Update File: a.go\n@@\n-x\n+y\n", // missing End
	} {
		res, _ := tool.Execute(context.Background(), json.RawMessage(mustJSON(t, map[string]string{"patch": p})))
		if !res.IsError {
			t.Errorf("malformed patch %q should fail", p)
		}
	}
}

func TestApplyPatchEscapeRejected(t *testing.T) {
	ws := testWorkspace(t)
	tool := &ApplyPatchTool{WS: ws}
	patch := `*** Begin Patch
*** Update File: ../../victim.txt
@@
-a
+b
*** End Patch`
	res, _ := tool.Execute(context.Background(), json.RawMessage(mustJSON(t, map[string]string{"patch": patch})))
	if !res.IsError {
		t.Error("path escape must be rejected")
	}
}

func TestApplyPatchMultipleHunks(t *testing.T) {
	ws := testWorkspace(t)
	write(t, ws, "m.go", "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\n")
	tool := &ApplyPatchTool{WS: ws}
	patch := `*** Begin Patch
*** Update File: m.go
@@
 l1
-l2
+L2
 l3
@@
 l6
-l7
+L7
 l8
*** End Patch`
	res, _ := tool.Execute(context.Background(), json.RawMessage(mustJSON(t, map[string]string{"patch": patch})))
	if res.IsError {
		t.Fatalf("patch failed: %s", res.Content)
	}
	data, _ := os.ReadFile(filepath.Join(ws.Root(), "m.go"))
	want := "l1\nL2\nl3\nl4\nl5\nl6\nL7\nl8\n"
	if string(data) != want {
		t.Errorf("got %q want %q", data, want)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
