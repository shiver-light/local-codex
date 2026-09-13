package permission

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApprovalIDsAreRandom(t *testing.T) {
	ids := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newApprovalID()
		if !strings.HasPrefix(id, "approval-") || len(id) != len("approval-")+32 {
			t.Errorf("unexpected id format %q", id)
		}
		if ids[id] {
			t.Fatalf("duplicate id %q", id)
		}
		ids[id] = true
	}
}

func TestBrokerApproverPendingAndResolve(t *testing.T) {
	gotCh := make(chan Request, 1)
	b := NewBrokerApprover(func(r Request) { gotCh <- r })

	done := make(chan bool, 1)
	go func() {
		ok, err := b.Approve(context.Background(), "rm -rf x", "destructive")
		if err != nil {
			t.Errorf("Approve: %v", err)
		}
		done <- ok
	}()

	var got Request
	select {
	case got = <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("onNew callback never fired")
	}
	if got.Command != "rm -rf x" {
		t.Fatalf("onNew request = %+v", got)
	}

	req, ok := b.Pending(got.ID)
	if !ok || req.Command != "rm -rf x" || req.Reason != "destructive" {
		t.Fatalf("Pending = %+v, %v", req, ok)
	}

	if !b.Resolve(got.ID, true) {
		t.Fatal("Resolve returned false")
	}
	if approved := <-done; !approved {
		t.Error("Approve returned false after approval")
	}
	if _, ok := b.Pending(got.ID); ok {
		t.Error("request still pending after resolution")
	}
	if b.Resolve(got.ID, true) {
		t.Error("Resolve succeeded for finished approval")
	}
}

func waitPending(t *testing.T, b *BrokerApprover, n int) []Request {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if l := b.ListPending(); len(l) >= n {
			return l
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending approvals, have %d", n, len(b.ListPending()))
	return nil
}

func TestBrokerApproverTimeout(t *testing.T) {
	b := NewBrokerApprover(nil)
	b.Timeout = 20 * time.Millisecond

	start := time.Now()
	ok, err := b.Approve(context.Background(), "rm -rf x", "destructive")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if ok {
		t.Error("timed-out approval must be auto-rejected")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Approve returned after %v, want ~20ms", elapsed)
	}
	if got := b.ListPending(); len(got) != 0 {
		t.Errorf("pending entries left after timeout: %+v", got)
	}
}

func TestBrokerApproverDoubleResolve(t *testing.T) {
	b := NewBrokerApprover(nil)

	done := make(chan bool, 1)
	go func() {
		ok, err := b.Approve(context.Background(), "make deploy", "risky")
		if err != nil {
			t.Errorf("Approve: %v", err)
		}
		done <- ok
	}()

	id := waitPending(t, b, 1)[0].ID
	if !b.Resolve(id, true) {
		t.Fatal("first Resolve returned false")
	}
	// A duplicate Resolve (double-clicked Approve) must fail fast instead of
	// blocking on the orphaned buffered channel.
	second := make(chan bool, 1)
	go func() { second <- b.Resolve(id, true) }()
	select {
	case ok := <-second:
		if ok {
			t.Error("second Resolve succeeded, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Resolve blocked")
	}

	if approved := <-done; !approved {
		t.Error("Approve returned false after approval")
	}
	if got := b.ListPending(); len(got) != 0 {
		t.Errorf("pending entries left: %+v", got)
	}
}

func TestBrokerApproverListPending(t *testing.T) {
	b := NewBrokerApprover(nil)
	var wg sync.WaitGroup
	for _, cmd := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(cmd string) {
			defer wg.Done()
			// ctx-cancel each wait after the list assertion below
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(500*time.Millisecond, cancel)
			b.Approve(ctx, cmd, "test")
		}(cmd)
	}

	list := waitPending(t, b, 3)
	seen := map[string]bool{}
	for _, r := range list {
		if r.ID == "" || r.Reason != "test" {
			t.Errorf("bad pending entry: %+v", r)
		}
		seen[r.Command] = true
	}
	for _, cmd := range []string{"a", "b", "c"} {
		if !seen[cmd] {
			t.Errorf("ListPending missing command %q: %+v", cmd, list)
		}
	}
	wg.Wait()
	if got := b.ListPending(); len(got) != 0 {
		t.Errorf("pending entries left after ctx cancel: %+v", got)
	}
}
