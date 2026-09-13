package permission

import (
	"context"
	"strings"
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
	var got Request
	b := NewBrokerApprover(func(r Request) { got = r })

	done := make(chan bool, 1)
	go func() {
		ok, err := b.Approve(context.Background(), "rm -rf x", "destructive")
		if err != nil {
			t.Errorf("Approve: %v", err)
		}
		done <- ok
	}()

	deadline := time.Now().Add(2 * time.Second)
	for got.ID == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
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
