// Package session tracks per-task agent state: message history, tool
// history and modified files. The in-memory store sits behind an interface
// so it can later be swapped for SQLite.
package session

import (
	"fmt"
	"sync"
	"time"

	"local-codex/internal/llm"
)

type ToolRecord struct {
	Name      string    `json:"name"`
	Args      string    `json:"args"`
	IsError   bool      `json:"is_error"`
	StartedAt time.Time `json:"started_at"`
	Duration  string    `json:"duration"`
}

// Session is one coding task.
type Session struct {
	ID            string           `json:"id"`
	Task          string           `json:"task"`
	CreatedAt     time.Time        `json:"created_at"`
	Messages      []llm.Message    `json:"messages"`
	ToolHistory   []ToolRecord     `json:"tool_history"`
	ModifiedFiles []string         `json:"modified_files"`
	Commands      []string         `json:"commands"`
	FinalAnswer   string           `json:"final_answer,omitempty"`
	Done          bool             `json:"done"`
	mu            sync.Mutex
}

func (s *Session) AppendMessage(m llm.Message) {
	s.mu.Lock()
	s.Messages = append(s.Messages, m)
	s.mu.Unlock()
}

func (s *Session) SnapshotMessages() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Message, len(s.Messages))
	copy(out, s.Messages)
	return out
}

func (s *Session) ReplaceMessages(msgs []llm.Message) {
	s.mu.Lock()
	s.Messages = msgs
	s.mu.Unlock()
}

func (s *Session) AddToolRecord(r ToolRecord) {
	s.mu.Lock()
	s.ToolHistory = append(s.ToolHistory, r)
	if r.Name == "run_command" {
		s.Commands = append(s.Commands, r.Args)
	}
	s.mu.Unlock()
}

func (s *Session) AddModifiedFiles(paths []string) {
	s.mu.Lock()
	seen := map[string]bool{}
	for _, p := range s.ModifiedFiles {
		seen[p] = true
	}
	for _, p := range paths {
		if !seen[p] {
			s.ModifiedFiles = append(s.ModifiedFiles, p)
			seen[p] = true
		}
	}
	s.mu.Unlock()
}

func (s *Session) Finish(answer string) {
	s.mu.Lock()
	s.FinalAnswer = answer
	s.Done = true
	s.mu.Unlock()
}

// Snapshot returns a deep copy of the session taken under the lock. Readers
// (e.g. HTTP handlers marshaling to JSON) must use it instead of touching
// the live session, which the agent goroutine mutates concurrently.
func (s *Session) Snapshot() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &Session{
		ID:          s.ID,
		Task:        s.Task,
		CreatedAt:   s.CreatedAt,
		FinalAnswer: s.FinalAnswer,
		Done:        s.Done,
	}
	if s.Messages != nil {
		out.Messages = make([]llm.Message, len(s.Messages))
		for i, m := range s.Messages {
			out.Messages[i] = m
			if m.ToolCalls != nil {
				out.Messages[i].ToolCalls = append([]llm.ToolCall(nil), m.ToolCalls...)
			}
		}
	}
	if s.ToolHistory != nil {
		out.ToolHistory = append([]ToolRecord(nil), s.ToolHistory...)
	}
	if s.ModifiedFiles != nil {
		out.ModifiedFiles = append([]string(nil), s.ModifiedFiles...)
	}
	if s.Commands != nil {
		out.Commands = append([]string(nil), s.Commands...)
	}
	return out
}

// Store persists sessions.
type Store interface {
	Create(task string) *Session
	Get(id string) (*Session, bool)
	List() []*Session
}

// MemoryStore is the default in-memory Store.
type MemoryStore struct {
	mu       sync.Mutex
	seq      int
	sessions map[string]*Session
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: map[string]*Session{}}
}

func (m *MemoryStore) Create(task string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	s := &Session{
		ID:        fmt.Sprintf("session-%d-%d", time.Now().Unix(), m.seq),
		Task:      task,
		CreatedAt: time.Now(),
	}
	m.sessions[s.ID] = s
	return s
}

func (m *MemoryStore) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *MemoryStore) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}
