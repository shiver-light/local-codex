package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func setup(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	ws, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.Root(), "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws.Root(), "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestResolveInsideWorkspace(t *testing.T) {
	ws := setup(t)
	cases := []string{"main.go", "./main.go", "src/../main.go", ".", "src"}
	for _, c := range cases {
		if _, err := ws.Resolve(c); err != nil {
			t.Errorf("Resolve(%q) should succeed: %v", c, err)
		}
	}
}

func TestResolveDotDotEscape(t *testing.T) {
	ws := setup(t)
	cases := []string{
		"../etc/passwd",
		"../../etc/passwd",
		"src/../../outside",
	}
	for _, c := range cases {
		if _, err := ws.Resolve(c); err == nil {
			t.Errorf("Resolve(%q) should be rejected", c)
		}
	}
}

func TestResolveAbsoluteEscape(t *testing.T) {
	ws := setup(t)
	if _, err := ws.Resolve("/etc/passwd"); err == nil {
		t.Error("absolute path /etc/passwd should be rejected")
	}
	home, _ := os.UserHomeDir()
	if _, err := ws.Resolve(filepath.Join(home, ".ssh", "id_rsa")); err == nil {
		t.Error("absolute path ~/.ssh/id_rsa should be rejected")
	}
	// Absolute path INSIDE the workspace is allowed.
	if _, err := ws.Resolve(filepath.Join(ws.Root(), "main.go")); err != nil {
		t.Errorf("absolute path inside workspace should be allowed: %v", err)
	}
}

func TestResolveSymlinkEscape(t *testing.T) {
	ws := setup(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws.Root(), "evil-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Resolve("evil-link/secret.txt"); err == nil {
		t.Error("symlink escape should be rejected")
	}
}

func TestResolveSymlinkToOutsideFile(t *testing.T) {
	ws := setup(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(ws.Root(), "direct-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Resolve("direct-link"); err == nil {
		t.Error("symlink to outside file should be rejected")
	}
}

func TestResolveNonExistentWriteTarget(t *testing.T) {
	ws := setup(t)
	p, err := ws.Resolve("new/deep/file.go")
	if err != nil {
		t.Fatalf("non-existent write target inside workspace should resolve: %v", err)
	}
	if p != ws.Root()+"/new/deep/file.go" {
		t.Errorf("unexpected resolved path %s", p)
	}
}

func TestOpenRejectsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f); err == nil {
		t.Error("Open on a file should fail")
	}
}
