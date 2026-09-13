package llm

import (
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

func (c *OpenAIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if dump := os.Getenv("LLM_DEBUG_DUMP"); dump != "" {
			_ = os.WriteFile(dump, body, 0o600)
		}
		return nil, fmt.Errorf("chat completions request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read chat response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
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
		return nil, httpErr
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
