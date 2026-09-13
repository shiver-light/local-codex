package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAIClientChat(t *testing.T) {
	var gotReq ChatRequest
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatResponse{
			Choices: []Choice{{
				Message: Message{
					Role:    "assistant",
					Content: "",
					ToolCalls: []ToolCall{{
						ID:   "call_1",
						Type: "function",
						Function: FunctionCall{
							Name:      "read_file",
							Arguments: `{"path":"a.go"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
			Usage: Usage{TotalTokens: 42},
		})
	}))
	defer srv.Close()

	c := NewOpenAIClient(srv.URL+"/v1", "test-key", "test-model")
	temp := 0.5
	resp, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		},
		Tools: []ToolDef{{
			Type: "function",
			Function: FunctionDef{
				Name:       "read_file",
				Parameters: map[string]any{"type": "object"},
			},
		}},
		Temperature: &temp,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotReq.Model != "test-model" {
		t.Errorf("model = %q", gotReq.Model)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "read_file" {
		t.Errorf("tools not forwarded: %+v", gotReq.Tools)
	}
	msg := resp.First()
	if msg == nil || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("unexpected response %+v", resp)
	}
	if resp.Usage.TotalTokens != 42 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestOpenAIClientErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	c := NewOpenAIClient(srv.URL, "k", "m")
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", httpErr.StatusCode)
	}
	if httpErr.RetryAfter != 3*time.Second {
		t.Errorf("RetryAfter = %v, want 3s", httpErr.RetryAfter)
	}
}

// TestBaseURLV1AutoAppend: a base URL without the API prefix must still hit
// /v1/chat/completions.
func TestBaseURLV1AutoAppend(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(ChatResponse{
			Choices: []Choice{{Message: Message{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer srv.Close()
	c := NewOpenAIClient(srv.URL, "k", "m")
	if _, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
}

func TestProxyBypassForPrivateHosts(t *testing.T) {
	// Even with a proxy configured, loopback/private targets go direct.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1") // unreachable proxy
	t.Setenv("NO_PROXY", "")                     // no exemptions: client must bypass anyway

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ChatResponse{
			Choices: []Choice{{Message: Message{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer srv.Close()

	c := NewOpenAIClient(srv.URL, "k", "m")
	resp, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("private host should bypass proxy: %v", err)
	}
	if resp.First().Content != "ok" {
		t.Errorf("unexpected response %+v", resp.First())
	}
}

func TestIsPrivateHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "localhost": true, "192.168.85.248": true,
		"10.0.0.5": true, "172.16.3.4": true, "169.254.1.1": true,
		"8.8.8.8": false, "api.openai.com": false,
	} {
		if got := isPrivateHost(host); got != want {
			t.Errorf("isPrivateHost(%q) = %v, want %v", host, got, want)
		}
	}
}
