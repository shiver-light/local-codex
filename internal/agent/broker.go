package agent

import (
	"sync"

	"local-codex/internal/logging"
)

// EventBroker fans agent events out to subscribers (CLI printer, SSE
// clients, the structured logger).
type EventBroker struct {
	mu   sync.Mutex
	subs map[chan logging.Event]struct{}
}

func NewEventBroker() *EventBroker {
	return &EventBroker{subs: map[chan logging.Event]struct{}{}}
}

// Publish sends an event to all current subscribers (non-blocking; slow
// subscribers drop events rather than stalling the agent).
func (b *EventBroker) Publish(e logging.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns a buffered channel of events and an unsubscribe func.
func (b *EventBroker) Subscribe() (<-chan logging.Event, func()) {
	ch := make(chan logging.Event, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		close(ch)
		b.mu.Unlock()
	}
}
