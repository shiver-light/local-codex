package permission

import (
	"context"
	"testing"
)

func TestClassifySafe(t *testing.T) {
	p := DefaultPolicy()
	for _, cmd := range []string{
		"go test ./...",
		"git status --short",
		"cmake --build build",
		"npm test",
		"rg foo src/",
		"CGO_ENABLED=0 go build ./...",
		"cat file.txt | grep foo",
	} {
		if d, reason := p.Classify(cmd); d != Safe {
			t.Errorf("Classify(%q) = %s (%s), want safe", cmd, d, reason)
		}
	}
}

func TestClassifyAsk(t *testing.T) {
	p := DefaultPolicy()
	for _, cmd := range []string{
		"docker build .",
		"curl https://example.com",
		"ssh user@host",
		"some-unknown-binary --flag",
	} {
		if d, _ := p.Classify(cmd); d != Ask {
			t.Errorf("Classify(%q) = %s, want ask", cmd, d)
		}
	}
}

func TestClassifyBlocked(t *testing.T) {
	p := DefaultPolicy()
	for _, cmd := range []string{
		"sudo apt install x",
		"shutdown now",
		"reboot",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"/usr/bin/sudo ls", // path prefix must not bypass
	} {
		if d, _ := p.Classify(cmd); d != Blocked {
			t.Errorf("Classify(%q) = %s, want blocked", cmd, d)
		}
	}
}

func TestClassifyDangerousPatterns(t *testing.T) {
	p := DefaultPolicy()
	for _, cmd := range []string{
		"rm -rf /",
		"rm -rf /*",
		"rm -rf / ; echo done",
		"echo x; rm -rf /",
	} {
		if d, reason := p.Classify(cmd); d != Blocked {
			t.Errorf("Classify(%q) = %s (%s), want blocked", cmd, d, reason)
		}
	}
}

func TestCompoundCommandStrictestWins(t *testing.T) {
	p := DefaultPolicy()
	if d, _ := p.Classify("go test ./... && docker build ."); d != Ask {
		t.Errorf("safe && ask should be ask, got %s", d)
	}
	if d, _ := p.Classify("docker build . && sudo rm -rf /tmp/x"); d != Blocked {
		t.Errorf("ask && blocked should be blocked, got %s", d)
	}
}

func TestEmptyCommandBlocked(t *testing.T) {
	p := DefaultPolicy()
	if d, _ := p.Classify("   "); d != Blocked {
		t.Error("empty command should be blocked")
	}
}

func TestBrokerApprover(t *testing.T) {
	reqs := make(chan Request, 1)
	b := NewBrokerApprover(func(r Request) { reqs <- r })
	done := make(chan bool, 1)
	go func() {
		ok, err := b.Approve(context.Background(), "docker build .", "test")
		if err != nil {
			t.Errorf("Approve: %v", err)
		}
		done <- ok
	}()
	req := <-reqs
	if req.Command != "docker build ." {
		t.Errorf("unexpected request %+v", req)
	}
	if !b.Resolve(req.ID, true) {
		t.Error("Resolve should succeed for pending id")
	}
	if !<-done {
		t.Error("expected approval")
	}
	if b.Resolve(req.ID, true) {
		t.Error("Resolve should fail after completion")
	}
}
