package permission

import (
	"context"
	"testing"
)

func TestClassifySafe(t *testing.T) {
	p := DefaultPolicy()
	for _, cmd := range []string{
		"go test ./...",
		"go vet ./...",
		"git status --short",
		"git diff HEAD~1",
		"git log --oneline -5",
		"git -C subdir show HEAD",
		"rg foo src/",
		"find . -name '*.go'",
		"CGO_ENABLED=0 go build ./...",
		"cat file.txt | grep foo",
		"ls -la && wc -l README.md",
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
		// interpreters and build tools execute arbitrary code
		"python3 -c 'print(1)'",
		"node -e 'console.log(1)'",
		"make all",
		"npm test",
		"pip install requests",
		"cargo build",
		"cmake --build build",
		// git write subcommands
		"git push origin main",
		"git clean -fd",
		"git reset --hard HEAD",
		"git add .",
		"git branch -D feature",
		// go subcommands that execute code or modify state
		"go run ./cmd/tool",
		"go generate ./...",
		"go mod tidy",
		"go env -w GOPROXY=off",
		// find with actions
		"find . -name '*.tmp' -delete",
		"find . -exec rm {} ;",
		// wrappers whose inner command is untrusted or non-destructive rm
		"env rm -rf build",
		"echo x | xargs rm -rf",
		"nohup ./server &",
		"mkdir newdir",
		// command substitution: inner command is safe but arg is unresolvable
		"echo $(cat version.txt)",
		"echo `cat version.txt`",
		// parameter expansion cannot be resolved statically
		"cat $CONFIG_FILE",
		// multi-line with an approvable second command
		"echo ok\nrm oldfile",
		// unparseable syntax fails closed
		"echo 'unterminated",
	} {
		if d, reason := p.Classify(cmd); d != Ask {
			t.Errorf("Classify(%q) = %s (%s), want ask", cmd, d, reason)
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
		// destructive rm: whitespace and flag-order variants must not bypass
		"rm -rf /",
		"rm -rf /*",
		"rm  -rf  /",
		"rm -r -f /",
		"rm -fr /",
		"rm --recursive --force /",
		"rm -rf ~",
		"rm -rf *",
		"rm -rf /tmp/x",
		"rm -rf /tmp/x &",
		// command substitution cannot hide a blocked command
		"echo $(rm -rf /tmp/x)",
		"echo `rm -rf /tmp/x`",
		// multi-line scripts cannot hide a blocked command
		"echo ok\nrm -rf /tmp/x",
		"echo ok; rm -rf /",
		// wrappers cannot hide a blocked command
		"env rm -rf /",
		"env FOO=bar rm -rf /tmp/x",
		// writes to raw devices
		"echo x > /dev/sda",
		// fork bomb
		":(){ :|:& };:",
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
	if d, _ := p.Classify("ls | wc -l && git status"); d != Safe {
		t.Errorf("safe pipe && safe should be safe, got %s", d)
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
