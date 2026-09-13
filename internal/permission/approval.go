package permission

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Approver decides whether an "ask" command may run.
type Approver interface {
	Approve(ctx context.Context, command string, reason string) (bool, error)
}

// ApproveAll approves everything (tests / fully autonomous mode).
type ApproveAll struct{}

func (ApproveAll) Approve(context.Context, string, string) (bool, error) { return true, nil }

// DenyAll rejects everything.
type DenyAll struct{}

func (DenyAll) Approve(context.Context, string, string) (bool, error) { return false, nil }

// CLIApprover prompts on the terminal.
type CLIApprover struct {
	reader *bufio.Reader
	out    io.Writer
}

func NewCLIApprover(in io.Reader, out io.Writer) *CLIApprover {
	return &CLIApprover{reader: bufio.NewReader(in), out: out}
}

func (c *CLIApprover) Approve(ctx context.Context, command string, reason string) (bool, error) {
	fmt.Fprintf(c.out, "\n[Approval required] %s\n  command: %s\nApprove? [y/N] ", reason, command)
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := c.reader.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case r := <-ch:
		if r.err != nil && r.line == "" {
			return false, r.err
		}
		ans := strings.ToLower(strings.TrimSpace(r.line))
		return ans == "y" || ans == "yes", nil
	}
}

// Request is a pending approval shown to a remote (web) client.
type Request struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Reason  string `json:"reason"`
}

// DefaultApprovalTimeout bounds how long Approve waits for a resolution
// before auto-rejecting, so a request with no connected client cannot hang
// the task until the hard task timeout.
const DefaultApprovalTimeout = 5 * time.Minute

// BrokerApprover fans approval requests out to subscribers (e.g. over SSE)
// and blocks until someone resolves them via Resolve.
type BrokerApprover struct {
	mu       sync.Mutex
	pending  map[string]chan bool
	requests map[string]Request // full requests, fetchable by id via Pending
	onNewReq func(Request)      // called when a new approval is requested
	// Timeout bounds the wait for a resolution; zero uses
	// DefaultApprovalTimeout. A timed-out request is auto-rejected.
	Timeout time.Duration
}

func NewBrokerApprover(onNew func(Request)) *BrokerApprover {
	return &BrokerApprover{pending: map[string]chan bool{}, requests: map[string]Request{}, onNewReq: onNew}
}

// newApprovalID returns an unguessable random approval id.
func newApprovalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return "approval-" + hex.EncodeToString(b[:])
}

func (b *BrokerApprover) Approve(ctx context.Context, command string, reason string) (bool, error) {
	id := newApprovalID()
	ch := make(chan bool, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.requests[id] = Request{ID: id, Command: command, Reason: reason}
	onNew := b.onNewReq
	b.mu.Unlock()

	if onNew != nil {
		onNew(Request{ID: id, Command: command, Reason: reason})
	}
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		delete(b.requests, id)
		b.mu.Unlock()
	}()

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = DefaultApprovalTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil // nobody resolved in time: auto-reject
	case ok := <-ch:
		return ok, nil
	}
}

// Pending returns the full pending request for an id, if it is still open.
func (b *BrokerApprover) Pending(id string) (Request, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.requests[id]
	return r, ok
}

// ListPending returns every still-open approval request, so clients that
// (re)connect after the broadcast missed the event can recover them.
func (b *BrokerApprover) ListPending() []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Request, 0, len(b.requests))
	for _, r := range b.requests {
		out = append(out, r)
	}
	return out
}

// Resolve answers a pending approval. Returns false if the id is unknown or
// was already resolved: the entry is deleted under the lock before the
// (non-blocking, buffered) send, so a duplicate Resolve observes it as gone
// instead of blocking the caller on a channel nobody drains anymore.
func (b *BrokerApprover) Resolve(id string, approved bool) bool {
	b.mu.Lock()
	ch, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
		delete(b.requests, id)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- approved:
	default:
	}
	return true
}
