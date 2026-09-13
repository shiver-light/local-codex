// Package logging writes structured JSON event logs for agent activity.
// Secrets (API keys) are never logged.
package logging

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Event is one structured log/event record. It is used both for the log
// file and for streaming to SSE subscribers.
type Event struct {
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"session_id,omitempty"`
	Iteration int       `json:"iteration,omitempty"`
	Type      string    `json:"type"` // user_message, llm_request, llm_response, llm_delta, tool_call, tool_result, patch, command, approval, agent_message, agent_finished, error
	Tool      string    `json:"tool,omitempty"`
	Success   *bool     `json:"success,omitempty"`
	Duration  string    `json:"duration,omitempty"`
	// Data carries type-specific payloads (command text, patch, message...).
	Data map[string]any `json:"data,omitempty"`
}

// Logger writes events as JSON lines.
type Logger struct {
	mu  sync.Mutex
	out io.Writer
}

// New returns a Logger writing to w (defaults to stderr if nil).
func New(w io.Writer) *Logger {
	if w == nil {
		w = os.Stderr
	}
	return &Logger{out: w}
}

// Log writes one event.
func (l *Logger) Log(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.out.Write(data)
	l.out.Write([]byte("\n"))
}
