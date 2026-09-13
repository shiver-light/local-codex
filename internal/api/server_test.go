package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"local-codex/internal/agent"
	"local-codex/internal/config"
	"local-codex/internal/llm"
	"local-codex/internal/session"
	"local-codex/internal/workspace"
)

type mockLLM struct{ calls int }

func (m *mockLLM) Model() string { return "mock" }
func (m *mockLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls++
	var msg llm.Message
	// first call: list files; second call: final answer
	hasToolResult := false
	for _, mm := range req.Messages {
		if mm.Role == "tool" {
			hasToolResult = true
		}
	}
	if hasToolResult {
		msg = llm.Message{Role: "assistant", Content: "All done."}
	} else {
		msg = llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "list_files", Arguments: `{"path":"."}`},
		}}}
	}
	return &llm.ChatResponse{Choices: []llm.Choice{{Message: msg}}}, nil
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	ag := &agent.Agent{
		LLM:    &mockLLM{},
		Broker: agent.NewEventBroker(),
	}
	srv := NewServer(cfg, ws, ag, session.NewMemoryStore())
	return httptest.NewServer(srv.http.Handler)
}

func TestHealthEndpoint(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestTaskLifecycle(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/tasks", "application/json", strings.NewReader(`{"task":"explore"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.SessionID == "" {
		t.Fatal("no session id returned")
	}

	// poll until the agent finishes
	var sess session.Session
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(ts.URL + "/api/sessions/" + created.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(r.Body).Decode(&sess)
		r.Body.Close()
		if sess.Done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sess.Done {
		t.Fatal("session never finished")
	}
	if sess.FinalAnswer != "All done." {
		t.Errorf("final answer = %q", sess.FinalAnswer)
	}
	if len(sess.ToolHistory) != 1 || sess.ToolHistory[0].Name != "list_files" {
		t.Errorf("tool history = %+v", sess.ToolHistory)
	}
}

func TestApprovalEndpointUnknownID(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/approvals", "application/json",
		strings.NewReader(`{"id":"nope","approved":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEventsSSE(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}

	// trigger an event and expect to see it on the stream
	if _, err := http.Post(ts.URL+"/api/tasks", "application/json", strings.NewReader(`{"task":"x"}`)); err != nil {
		t.Fatal(err)
	}
	type readResult struct {
		data string
		err  error
	}
	readCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 4096)
		var got string
		for !strings.Contains(got, "user_message") {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				got += string(buf[:n])
			}
			if err != nil {
				readCh <- readResult{got, err}
				return
			}
		}
		readCh <- readResult{got, nil}
	}()
	select {
	case r := <-readCh:
		if !strings.Contains(r.data, "user_message") {
			t.Errorf("SSE stream did not carry events, got: %q (err=%v)", r.data, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Error("timed out waiting for SSE events")
	}
}
