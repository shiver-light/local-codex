package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer serves the given raw SSE payload from /v1/chat/completions and
// records the request body.
func sseServer(t *testing.T, payload string, gotReq *ChatRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotReq != nil {
			_ = json.NewDecoder(r.Body).Decode(gotReq)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, payload)
	}))
}

func TestChatStreamContentAggregation(t *testing.T) {
	var gotReq ChatRequest
	srv := sseServer(t, ""+
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello, \"}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"content\":\"wor\"}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"content\":\"ld!\"},\"finish_reason\":\"stop\"}]}\n\n"+
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"total_tokens\":14}}\n\n"+
		"data: [DONE]\n\n"+
		"data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n", // after [DONE]: must be ignored
		&gotReq)
	defer srv.Close()

	c := NewOpenAIClient(srv.URL, "k", "m")
	var deltas []string
	resp, err := c.ChatStream(context.Background(),
		ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(d Delta) { deltas = append(deltas, d.Content) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	msg := resp.First()
	if msg == nil || msg.Content != "Hello, world!" {
		t.Fatalf("aggregated content = %+v", msg)
	}
	if msg.Role != "assistant" {
		t.Errorf("role = %q", msg.Role)
	}
	if strings.Join(deltas, "") != "Hello, world!" {
		t.Errorf("deltas = %q", deltas)
	}
	if resp.Usage.TotalTokens != 14 || resp.Usage.PromptTokens != 11 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
	if !gotReq.Stream {
		t.Error("request must force stream: true")
	}
	if gotReq.StreamOptions == nil || !gotReq.StreamOptions.IncludeUsage {
		t.Errorf("stream_options.include_usage not set: %+v", gotReq.StreamOptions)
	}
	if gotReq.Model != "m" {
		t.Errorf("model = %q", gotReq.Model)
	}
}

func TestChatStreamToolCallMerge(t *testing.T) {
	srv := sseServer(t, ""+
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_\",\"arguments\":\"\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"file\",\"arguments\":\"{\\\"path\\\"\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"list_files\",\"arguments\":\"{}\"}}]}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"a.go\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"+
		"data: [DONE]\n\n", nil)
	defer srv.Close()

	c := NewOpenAIClient(srv.URL, "k", "m")
	resp, err := c.ChatStream(context.Background(),
		ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	msg := resp.First()
	if msg == nil || len(msg.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", msg)
	}
	tc0, tc1 := msg.ToolCalls[0], msg.ToolCalls[1]
	if tc0.ID != "call_1" || tc0.Type != "function" ||
		tc0.Function.Name != "read_file" || tc0.Function.Arguments != `{"path":"a.go"}` {
		t.Errorf("merged tool call 0 = %+v", tc0)
	}
	if tc1.ID != "call_2" || tc1.Function.Name != "list_files" || tc1.Function.Arguments != "{}" {
		t.Errorf("merged tool call 1 = %+v", tc1)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

func TestChatStreamReasoningContent(t *testing.T) {
	srv := sseServer(t, ""+
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think \"}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"hard\"}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"+
		"data: [DONE]\n\n", nil)
	defer srv.Close()

	c := NewOpenAIClient(srv.URL, "k", "m")
	var reasoning []string
	resp, err := c.ChatStream(context.Background(),
		ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(d Delta) { reasoning = append(reasoning, d.ReasoningContent) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	msg := resp.First()
	if msg.ReasoningContent != "think hard" {
		t.Errorf("reasoning = %q", msg.ReasoningContent)
	}
	if strings.Join(reasoning, "") != "think hard" {
		t.Errorf("reasoning deltas = %q", reasoning)
	}
}

// TestChatStreamInterrupted: the connection drops mid-stream; the client
// must report an error instead of silently returning a partial message.
func TestChatStreamInterrupted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijack unsupported")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// A declared Content-Length larger than the body makes the client
		// hit unexpected EOF once we close.
		rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 1000\r\n\r\n")
		rw.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		rw.Flush()
	}))
	defer srv.Close()

	c := NewOpenAIClient(srv.URL, "k", "m")
	var deltas []string
	_, err := c.ChatStream(context.Background(),
		ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
		func(d Delta) { deltas = append(deltas, d.Content) })
	if err == nil || !strings.Contains(err.Error(), "stream interrupted") {
		t.Fatalf("expected stream interruption error, got %v", err)
	}
	if strings.Join(deltas, "") != "partial" {
		t.Errorf("deltas delivered before the failure = %q", deltas)
	}
}

// TestChatStreamCleanEOF: some servers close the stream without a [DONE]
// line; a clean EOF still terminates the stream normally.
func TestChatStreamCleanEOF(t *testing.T) {
	srv := sseServer(t,
		"data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n", nil)
	defer srv.Close()

	c := NewOpenAIClient(srv.URL, "k", "m")
	resp, err := c.ChatStream(context.Background(),
		ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.First().Content != "done" {
		t.Errorf("content = %q", resp.First().Content)
	}
}

func TestChatStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"busy"}`))
	}))
	defer srv.Close()

	c := NewOpenAIClient(srv.URL, "k", "m")
	_, err := c.ChatStream(context.Background(),
		ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != 429 {
		t.Errorf("status = %d", httpErr.StatusCode)
	}
}

// TestChatStreamBlankLinesAndComments: SSE keep-alive comments, event:
// lines and blank separators must be skipped without breaking parsing.
func TestChatStreamBlankLinesAndComments(t *testing.T) {
	payload := ": ping\n\nevent: message\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\r\n\r\n" +
		"data: [DONE]\n\n"
	srv := sseServer(t, payload, nil)
	defer srv.Close()

	c := NewOpenAIClient(srv.URL, "k", "m")
	resp, err := c.ChatStream(context.Background(),
		ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.First().Content != "ab" {
		t.Errorf("content = %q", resp.First().Content)
	}
}
