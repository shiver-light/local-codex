package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"local-codex/internal/llm"
	"local-codex/internal/logging"
	"local-codex/internal/workspace"
)

// scriptedStreamLLM is a mock llm.StreamClient: ChatStream replays scripted
// deltas and returns the aggregated response; Chat counts plain (fallback)
// calls.
type scriptedStreamLLM struct {
	model      string
	deltas     []llm.Delta
	msg        llm.Message
	usage      llm.Usage
	streamErr  error // returned by ChatStream after replaying deltas
	streamResp func(req llm.ChatRequest) (*llm.ChatResponse, error)
	calls      int
	chatCalls  int
}

func (s *scriptedStreamLLM) Model() string { return s.model }

func (s *scriptedStreamLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.chatCalls++
	if s.streamResp != nil {
		return s.streamResp(req)
	}
	return &llm.ChatResponse{Choices: []llm.Choice{{Message: s.msg}}, Usage: s.usage}, nil
}

func (s *scriptedStreamLLM) ChatStream(_ context.Context, req llm.ChatRequest, onDelta func(llm.Delta)) (*llm.ChatResponse, error) {
	s.calls++
	for _, d := range s.deltas {
		onDelta(d)
	}
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	return &llm.ChatResponse{Choices: []llm.Choice{{Message: s.msg}}, Usage: s.usage}, nil
}

// collectEvents subscribes to the broker and returns all events published
// until the returned stop func is called.
func collectEvents(b *EventBroker) (events *[]logging.Event, stop func()) {
	ch, unsub := b.Subscribe()
	var collected []logging.Event
	done := make(chan struct{})
	go func() {
		for e := range ch {
			collected = append(collected, e)
		}
		close(done)
	}()
	return &collected, func() { unsub(); <-done }
}

// TestStreamDeltaEventsAndUsage: streaming content deltas are broadcast as
// llm_delta events and the response usage accumulates into the session.
func TestStreamDeltaEventsAndUsage(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &scriptedStreamLLM{
		model:  "mock",
		deltas: []llm.Delta{{Content: "Hello"}, {Content: ", "}, {Content: "world"}},
		msg:    llm.Message{Role: "assistant", Content: "Hello, world"},
		usage:  llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	a, sess := newTestAgent(t, ws, mock, 5)
	a.Stream = true
	a.Broker = NewEventBroker()
	events, stop := collectEvents(a.Broker)

	answer, err := a.Run(context.Background(), sess, "task")
	stop()
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Hello, world" {
		t.Errorf("answer = %q", answer)
	}

	var deltaText strings.Builder
	deltaCount := 0
	for _, e := range *events {
		if e.Type != "llm_delta" {
			continue
		}
		deltaCount++
		if e.SessionID != sess.ID || e.Iteration != 1 {
			t.Errorf("delta event missing session/iteration: %+v", e)
		}
		deltaText.WriteString(e.Data["content"].(string))
	}
	if deltaCount != 3 || deltaText.String() != "Hello, world" {
		t.Errorf("deltas = %d / %q", deltaCount, deltaText.String())
	}

	u := sess.Snapshot().Usage
	if u.LLMCalls != 1 || u.TotalTokens != 15 || u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("usage = %+v", u)
	}
}

// TestStreamThinkBlocksFilteredFromDeltas: think blocks must not leak into
// llm_delta events, even when tags are split across deltas.
func TestStreamThinkBlocksFilteredFromDeltas(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &scriptedStreamLLM{
		model: "mock",
		deltas: []llm.Delta{
			{Content: "<thi"}, {Content: "nk>secret"}, {Content: " reasoning</thi"},
			{Content: "nk>"}, {Content: "The ans"}, {Content: "wer."},
		},
		msg: llm.Message{Role: "assistant", Content: "<think>secret reasoning</think>The answer."},
	}
	a, sess := newTestAgent(t, ws, mock, 5)
	a.Stream = true
	a.Broker = NewEventBroker()
	events, stop := collectEvents(a.Broker)

	if _, err := a.Run(context.Background(), sess, "task"); err != nil {
		t.Fatal(err)
	}
	stop()
	var streamed strings.Builder
	for _, e := range *events {
		if e.Type == "llm_delta" {
			streamed.WriteString(e.Data["content"].(string))
		}
	}
	if got := streamed.String(); strings.Contains(got, "secret") || strings.Contains(got, "think") {
		t.Errorf("reasoning leaked into delta events: %q", got)
	} else if got != "The answer." {
		t.Errorf("streamed content = %q, want %q", got, "The answer.")
	}
}

// TestStreamMidStreamFailureNoRetry: once deltas have been delivered the
// call must not be retried — a retry would repeat the streamed output.
func TestStreamMidStreamFailureNoRetry(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &scriptedStreamLLM{
		model:     "mock",
		deltas:    []llm.Delta{{Content: "partial"}},
		streamErr: errors.New("stream interrupted: unexpected EOF"),
	}
	a, sess := newTestAgent(t, ws, mock, 5)
	a.Stream = true
	a.retryBackoff = time.Millisecond

	_, err = a.Run(context.Background(), sess, "task")
	if err == nil || !strings.Contains(err.Error(), "stream interrupted") {
		t.Fatalf("expected stream interruption error, got %v", err)
	}
	if mock.calls != 1 || mock.chatCalls != 0 {
		t.Errorf("mid-stream failure must not retry/fallback: stream=%d chat=%d", mock.calls, mock.chatCalls)
	}
}

// TestStreamFallbackToChat: a streaming failure before any output (e.g. the
// server rejects stream mode) falls back to a plain chat request.
func TestStreamFallbackToChat(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &scriptedStreamLLM{
		model:     "mock",
		streamErr: &llm.HTTPError{StatusCode: 400, Status: "400 Bad Request", Body: "stream unsupported"},
		msg:       llm.Message{Role: "assistant", Content: "plain answer"},
	}
	a, sess := newTestAgent(t, ws, mock, 5)
	a.Stream = true
	a.retryBackoff = time.Millisecond

	answer, err := a.Run(context.Background(), sess, "task")
	if err != nil {
		t.Fatalf("pre-stream failure should fall back to plain chat: %v", err)
	}
	if answer != "plain answer" {
		t.Errorf("answer = %q", answer)
	}
	if mock.calls != 1 || mock.chatCalls != 1 {
		t.Errorf("stream=%d chat=%d, want 1/1", mock.calls, mock.chatCalls)
	}
}

// TestStreamDisabledUsesPlainChat: with Stream off the agent uses Chat even
// when the client supports streaming.
func TestStreamDisabledUsesPlainChat(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := &scriptedStreamLLM{
		model: "mock",
		msg:   llm.Message{Role: "assistant", Content: "ok"},
		usage: llm.Usage{TotalTokens: 7},
	}
	a, sess := newTestAgent(t, ws, mock, 5) // Stream defaults to false

	if _, err := a.Run(context.Background(), sess, "task"); err != nil {
		t.Fatal(err)
	}
	if mock.calls != 0 || mock.chatCalls != 1 {
		t.Errorf("stream=%d chat=%d, want 0/1", mock.calls, mock.chatCalls)
	}
	if sess.Snapshot().Usage.TotalTokens != 7 {
		t.Errorf("usage = %+v", sess.Snapshot().Usage)
	}
}

// TestUsageAccumulatesAcrossIterations: usage from every LLM response is
// accumulated across iterations (tool-call round trips).
func TestUsageAccumulatesAcrossIterations(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	mock := &scriptedLLM{model: "mock", handler: func(req llm.ChatRequest) llm.Message {
		n++
		if n == 1 {
			return toolCall("c1", "list_files", `{"path":"."}`)
		}
		return llm.Message{Role: "assistant", Content: "done"}
	}}
	a, sess := newTestAgent(t, ws, mock, 5)
	if _, err := a.Run(context.Background(), sess, "task"); err != nil {
		t.Fatal(err)
	}
	// scriptedLLM returns zero usage; only the call count accumulates.
	if u := sess.Snapshot().Usage; u.LLMCalls != 2 {
		t.Errorf("usage = %+v, want 2 calls", u)
	}
}

func TestThinkFilter(t *testing.T) {
	cases := []struct {
		chunks []string
		want   string
	}{
		{[]string{"hello"}, "hello"},
		{[]string{"<think>abc</think>hi"}, "hi"},
		{[]string{"<think>", "abc", "</think>", "hi"}, "hi"},
		{[]string{"<thi", "nk>a</t", "hink>b"}, "b"},
		{[]string{"a <think>x</think> b <think>y</think> c"}, "a  b  c"},
		{[]string{"plain <th"}, "plain "},     // dangling partial tag held back
		{[]string{"<think>unterminated"}, ""}, // trailing block dropped
		{[]string{"before <think>gone", " rest"}, "before "},
	}
	for i, tc := range cases {
		f := &thinkFilter{}
		var out strings.Builder
		for _, c := range tc.chunks {
			out.WriteString(f.feed(c))
		}
		if out.String() != tc.want {
			t.Errorf("case %d: feed(%q) = %q, want %q", i, tc.chunks, out.String(), tc.want)
		}
	}
}
