package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// HTTPError is a non-200 response from the chat completions endpoint. It
// lets callers distinguish deterministic 4xx failures (do not retry) from
// transient 429/5xx ones.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	// RetryAfter is parsed from the Retry-After header when present
	// (typically on 429 responses).
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("chat completions returned %s: %s", e.Status, e.Body)
}

// OpenAIClient talks to any OpenAI-compatible /v1/chat/completions endpoint.
type OpenAIClient struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// NewOpenAIClient builds a client. baseURL may include or omit a trailing
// "/v1"; when omitted it is appended before the chat completions path.
//
// LLM endpoints are typically local (localhost / LAN), so requests to
// loopback or private addresses bypass any configured HTTP proxy — a proxy
// that cannot reach the LAN would otherwise break the client with 502s.
func NewOpenAIClient(baseURL, apiKey, model string) *OpenAIClient {
	base := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return &OpenAIClient{
		baseURL: base,
		apiKey:  apiKey,
		model:   model,
		http: &http.Client{
			Timeout:   10 * time.Minute,
			Transport: &http.Transport{Proxy: proxyExceptPrivate},
		},
	}
}

// proxyExceptPrivate is http.ProxyFromEnvironment except that loopback and
// private (RFC 1918 / link-local) hosts are always reached directly.
func proxyExceptPrivate(req *http.Request) (*url.URL, error) {
	if isPrivateHost(req.URL.Hostname()) {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}

func isPrivateHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // hostnames still honor the proxy env
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func (c *OpenAIClient) Model() string { return c.model }

// newHTTPRequest builds the POST /chat/completions request for req.
func (c *OpenAIClient) newHTTPRequest(ctx context.Context, req ChatRequest) (*http.Request, []byte, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal chat request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return httpReq, body, nil
}

func (c *OpenAIClient) do(httpReq *http.Request, body []byte) (*http.Response, error) {
	resp, err := c.http.Do(httpReq)
	if err != nil {
		if dump := os.Getenv("LLM_DEBUG_DUMP"); dump != "" {
			_ = os.WriteFile(dump, body, 0o600)
		}
		return nil, fmt.Errorf("chat completions request failed: %w", err)
	}
	return resp, nil
}

// httpErrorFromResponse consumes the response body and builds an HTTPError.
func httpErrorFromResponse(resp *http.Response) error {
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	httpErr := &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       truncate(string(respBody), 500),
	}
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			httpErr.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return httpErr
}

func (c *OpenAIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	httpReq, body, err := c.newHTTPRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(httpReq, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, httpErrorFromResponse(resp)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read chat response: %w", err)
	}
	var out ChatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode chat response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("chat completions returned no choices")
	}
	return &out, nil
}

// streamChunk is one SSE `data:` payload of a streaming chat completion.
type streamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// ChatStream performs a streaming chat completion: it forces stream mode,
// asks the server for a final usage chunk, reports content/reasoning
// increments through onDelta as they arrive, and returns the aggregated
// message (content concatenated, tool_calls merged by index across their
// fragmented id/name/arguments pieces).
//
// Compatible with the OpenAI streaming dialect used by Ollama and vLLM
// (delta.content / delta.tool_calls / delta.reasoning_content, terminated
// by a `data: [DONE]` line or clean EOF).
func (c *OpenAIClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(Delta)) (*ChatResponse, error) {
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}
	httpReq, body, err := c.newHTTPRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(httpReq, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, httpErrorFromResponse(resp)
	}

	var (
		msg          Message
		finishReason string
		usage        Usage
		sawChunk     bool
		done         bool
		toolPos      = map[int]int{} // stream tool-call index -> position in msg.ToolCalls
	)
	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	for !done {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			done = consumeStreamLine(strings.TrimRight(line, "\r\n"), &msg, &finishReason, &usage, &sawChunk, toolPos, onDelta)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("stream interrupted: %w", err)
		}
	}
	if !sawChunk {
		return nil, fmt.Errorf("chat completions stream returned no choices")
	}
	msg.Role = "assistant"
	return &ChatResponse{
		Choices: []Choice{{Message: msg, FinishReason: finishReason}},
		Usage:   usage,
	}, nil
}

// consumeStreamLine handles one SSE line. It reports whether the stream is
// finished (`data: [DONE]`).
func consumeStreamLine(line string, msg *Message, finishReason *string, usage *Usage, sawChunk *bool, toolPos map[int]int, onDelta func(Delta)) bool {
	if !strings.HasPrefix(line, "data:") {
		return false // blank separator, comments, event: lines
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		return true
	}
	var chunk streamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return false // ignore malformed keep-alive/comment payloads
	}
	if chunk.Usage != nil {
		*usage = *chunk.Usage
	}
	for _, ch := range chunk.Choices {
		*sawChunk = true
		if ch.FinishReason != "" {
			*finishReason = ch.FinishReason
		}
		d := ch.Delta
		if d.Content != "" || d.ReasoningContent != "" {
			msg.Content += d.Content
			msg.ReasoningContent += d.ReasoningContent
			if onDelta != nil {
				onDelta(Delta{Content: d.Content, ReasoningContent: d.ReasoningContent})
			}
		}
		for _, tc := range d.ToolCalls {
			pos, ok := toolPos[tc.Index]
			if !ok {
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{Type: "function"})
				pos = len(msg.ToolCalls) - 1
				toolPos[tc.Index] = pos
			}
			call := &msg.ToolCalls[pos]
			if tc.ID != "" {
				call.ID = tc.ID
			}
			if tc.Type != "" {
				call.Type = tc.Type
			}
			if tc.Function.Name != "" {
				call.Function.Name += tc.Function.Name
			}
			call.Function.Arguments += tc.Function.Arguments
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
