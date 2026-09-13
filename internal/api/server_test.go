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

const testToken = "test-token"

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Server.Token = testToken
	ag := &agent.Agent{
		LLM:    &mockLLM{},
		Broker: agent.NewEventBroker(),
	}
	srv := NewServer(cfg, ws, ag, session.NewMemoryStore())
	return httptest.NewServer(srv.http.Handler), srv
}

func authed(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()
	resp := authed(t, "GET", ts.URL+"/api/health", "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()
	for _, path := range []string{"/api/health", "/api/sessions", "/api/events"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("GET %s without token: status = %d, want 401", path, resp.StatusCode)
		}
	}
	resp, err := http.Post(ts.URL+"/api/tasks", "application/json", strings.NewReader(`{"task":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("POST /api/tasks without token: status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthWrongToken(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAuthQueryToken(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/health?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCORSOrigins(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()
	for _, origin := range []string{"http://127.0.0.1:5173", "http://localhost:3000"} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/health", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("origin %s: ACAO = %q, want echo", origin, got)
		}
	}
	for _, origin := range []string{"https://evil.example", "http://127.0.0.1.evil.example"} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/health", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %s: ACAO = %q, want empty", origin, got)
		}
	}
}

func TestTaskConcurrencyLimit(t *testing.T) {
	ts, srv := newTestServer(t)
	defer ts.Close()
	// Fill the semaphore so the next submission hits the concurrency limit.
	for i := 0; i < cap(srv.taskSem); i++ {
		srv.taskSem <- struct{}{}
	}
	resp := authed(t, "POST", ts.URL+"/api/tasks", `{"task":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
}

func TestTaskLifecycle(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()

	resp := authed(t, "POST", ts.URL+"/api/tasks", `{"task":"explore"}`)
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
		r := authed(t, "GET", ts.URL+"/api/sessions/"+created.SessionID, "")
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
	ts, _ := newTestServer(t)
	defer ts.Close()
	resp := authed(t, "POST", ts.URL+"/api/approvals", `{"id":"nope","approved":true}`)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEventsSSE(t *testing.T) {
	ts, _ := newTestServer(t)
	defer ts.Close()

	resp := authed(t, "GET", ts.URL+"/api/events", "")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}

	// trigger an event and expect to see it on the stream
	post := authed(t, "POST", ts.URL+"/api/tasks", `{"task":"x"}`)
	post.Body.Close()
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

func TestListApprovalsEndpoint(t *testing.T) {
	ts, srv := newTestServer(t)
	defer ts.Close()

	// Open a pending approval directly on the broker approver.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = srv.approver.Approve(ctx, "rm -rf build", "destructive") }()

	var list []map[string]string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := authed(t, "GET", ts.URL+"/api/approvals", "")
		list = nil
		json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if len(list) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(list) != 1 {
		t.Fatalf("GET /api/approvals = %+v, want 1 pending", list)
	}
	if list[0]["command"] != "rm -rf build" || list[0]["reason"] != "destructive" || list[0]["id"] == "" {
		t.Errorf("unexpected pending approval: %+v", list[0])
	}

	// After the waiter goes away, the list must be empty again.
	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := authed(t, "GET", ts.URL+"/api/approvals", "")
		list = nil
		json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if len(list) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("approval still listed after cancellation: %+v", list)
}

// blockingLLM hangs in Chat until its context is cancelled.
type blockingLLM struct{ started chan struct{} }

func (b *blockingLLM) Model() string { return "blocking" }
func (b *blockingLLM) Chat(ctx context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	select {
	case <-b.started:
	default:
		close(b.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func newBlockingServer(t *testing.T) (*httptest.Server, *Server, *blockingLLM) {
	t.Helper()
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Server.Token = testToken
	m := &blockingLLM{started: make(chan struct{})}
	ag := &agent.Agent{
		LLM:    m,
		Broker: agent.NewEventBroker(),
	}
	srv := NewServer(cfg, ws, ag, session.NewMemoryStore())
	return httptest.NewServer(srv.http.Handler), srv, m
}

func TestCancelTask(t *testing.T) {
	ts, srv, m := newBlockingServer(t)
	defer ts.Close()

	resp := authed(t, "POST", ts.URL+"/api/tasks", `{"task":"hang"}`)
	var created struct {
		SessionID string `json:"session_id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.SessionID == "" {
		t.Fatal("no session id returned")
	}

	// Wait until the agent is actually blocked inside the LLM call.
	select {
	case <-m.started:
	case <-time.After(5 * time.Second):
		t.Fatal("agent never reached the LLM call")
	}

	resp = authed(t, "POST", ts.URL+"/api/tasks/"+created.SessionID+"/cancel", "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("cancel status = %d, want 200", resp.StatusCode)
	}

	// Once the task goroutine unwinds, the cancel func is deregistered.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp = authed(t, "POST", ts.URL+"/api/tasks/"+created.SessionID+"/cancel", "")
		resp.Body.Close()
		if resp.StatusCode == 404 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp.StatusCode != 404 {
		t.Errorf("cancel after finish: status = %d, want 404", resp.StatusCode)
	}
	_ = srv
}

func TestErrorEmitsAgentFinished(t *testing.T) {
	ts, srv, m := newBlockingServer(t)
	defer ts.Close()

	events, unsubscribe := srv.broker.Subscribe()
	defer unsubscribe()

	resp := authed(t, "POST", ts.URL+"/api/tasks", `{"task":"hang"}`)
	resp.Body.Close()
	select {
	case <-m.started:
	case <-time.After(5 * time.Second):
		t.Fatal("agent never reached the LLM call")
	}
	resp = authed(t, "POST", ts.URL+"/api/tasks/"+sessionIDOf(t, ts)+"/cancel", "")
	resp.Body.Close()

	// The UI resets its running state only on agent_finished; a task that
	// dies with an error must emit one.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Type == "agent_finished" {
				if e.Success == nil || *e.Success {
					t.Fatalf("agent_finished success = %v, want false", e.Success)
				}
				return
			}
		case <-deadline:
			t.Fatal("no agent_finished after task error")
		}
	}
}

func sessionIDOf(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := authed(t, "GET", ts.URL+"/api/sessions", "")
	defer resp.Body.Close()
	var sessions []session.Session
	json.NewDecoder(resp.Body).Decode(&sessions)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v", sessions)
	}
	return sessions[0].ID
}

func TestEventsSSEHeartbeat(t *testing.T) {
	ts, srv := newTestServer(t)
	defer ts.Close()
	srv.sseHeartbeat = 30 * time.Millisecond

	resp := authed(t, "GET", ts.URL+"/api/events", "")
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	var got string
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(got, ": ping") && time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			got += string(buf[:n])
		}
		if err != nil {
			t.Fatalf("read SSE: %v (got %q)", err, got)
		}
	}
	if !strings.Contains(got, ": ping") {
		t.Errorf("no heartbeat within %v, got %q", 2*time.Second, got)
	}
}
