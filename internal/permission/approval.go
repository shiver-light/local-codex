package permission

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
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

// BrokerApprover fans approval requests out to subscribers (e.g. over SSE)
// and blocks until someone resolves them via Resolve.
type BrokerApprover struct {
	mu       sync.Mutex
	seq      int
	pending  map[string]chan bool
	onNewReq func(Request) // called when a new approval is requested
}

func NewBrokerApprover(onNew func(Request)) *BrokerApprover {
	return &BrokerApprover{pending: map[string]chan bool{}, onNewReq: onNew}
}

func (b *BrokerApprover) Approve(ctx context.Context, command string, reason string) (bool, error) {
	b.mu.Lock()
	b.seq++
	id := fmt.Sprintf("approval-%d", b.seq)
	ch := make(chan bool, 1)
	b.pending[id] = ch
	onNew := b.onNewReq
	b.mu.Unlock()

	if onNew != nil {
		onNew(Request{ID: id, Command: command, Reason: reason})
	}
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case ok := <-ch:
		return ok, nil
	}
}

// Resolve answers a pending approval. Returns false if the id is unknown.
func (b *BrokerApprover) Resolve(id string, approved bool) bool {
	b.mu.Lock()
	ch, ok := b.pending[id]
	b.mu.Unlock()
	if !ok {
		return false
	}
	ch <- approved
	return true
}
