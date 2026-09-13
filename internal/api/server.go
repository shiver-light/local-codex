// Package api exposes the agent over HTTP: task submission, session state,
// SSE event streaming, approvals, git views and the file tree. It also
// serves the built web UI from web/dist when present.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"local-codex/internal/agent"
	"local-codex/internal/config"
	"local-codex/internal/logging"
	"local-codex/internal/permission"
	"local-codex/internal/session"
	"local-codex/internal/tools"
	"local-codex/internal/workspace"
)

// maxRequestBody caps JSON request bodies accepted by the API.
const maxRequestBody = 1 << 20 // 1 MiB

type Server struct {
	cfg      *config.Config
	ws       *workspace.Workspace
	agent    *agent.Agent
	store    session.Store
	broker   *agent.EventBroker
	approver *permission.BrokerApprover
	http     *http.Server
	token    string
	// taskSem bounds concurrently running agent tasks; excess submissions
	// are rejected with 429. Sized by maxConcurrentTasks.
	taskSem            chan struct{}
	maxConcurrentTasks int
	// cancels holds the cancel func of each running task, keyed by session
	// id, so POST /api/tasks/{id}/cancel can abort it.
	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc
	// sseHeartbeat is the interval between SSE comment pings; tests shrink it.
	sseHeartbeat time.Duration
}

func NewServer(cfg *config.Config, ws *workspace.Workspace, ag *agent.Agent, store session.Store) *Server {
	token := cfg.Server.Token
	if token == "" {
		token = generateToken()
	}
	s := &Server{
		cfg:                cfg,
		ws:                 ws,
		agent:              ag,
		store:              store,
		broker:             ag.Broker,
		token:              token,
		maxConcurrentTasks: 4,
		cancels:            map[string]context.CancelFunc{},
		sseHeartbeat:       15 * time.Second,
	}
	s.taskSem = make(chan struct{}, s.maxConcurrentTasks)
	// Approvals requested by tools are published as events so the web UI can
	// render Approve/Reject buttons; Resolve unblocks the tool. The event
	// carries only the random id — clients fetch the full command from
	// GET /api/approvals/{id}, so the broadcast itself leaks nothing.
	s.approver = permission.NewBrokerApprover(func(r permission.Request) {
		s.broker.Publish(logging.Event{
			Type: "approval",
			Data: map[string]any{"id": r.ID},
		})
	})
	ag.Registry = tools.NewBuiltinRegistry(ws, s.approver,
		time.Duration(cfg.Shell.DefaultTimeout)*time.Second,
		time.Duration(cfg.Shell.MaxTimeout)*time.Second)

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /api/health", s.handleHealth)
	apiMux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	apiMux.HandleFunc("POST /api/tasks/{id}/cancel", s.handleCancelTask)
	apiMux.HandleFunc("GET /api/sessions", s.handleListSessions)
	apiMux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	apiMux.HandleFunc("GET /api/events", s.handleEvents)
	apiMux.HandleFunc("POST /api/approvals", s.handleApproval)
	apiMux.HandleFunc("GET /api/approvals", s.handleListApprovals)
	apiMux.HandleFunc("GET /api/approvals/{id}", s.handleGetApproval)
	apiMux.HandleFunc("GET /api/git/status", s.handleGitStatus)
	apiMux.HandleFunc("GET /api/git/diff", s.handleGitDiff)
	apiMux.HandleFunc("GET /api/files", s.handleFiles)

	mux := http.NewServeMux()
	// All /api/* endpoints require the bearer token; only static files and
	// GET / are public (they serve the UI shell, which holds no secrets).
	mux.Handle("/api/", s.requireAuth(apiMux))
	mux.HandleFunc("/", s.handleStatic)

	s.http = &http.Server{
		Addr:    net.JoinHostPort(cfg.Server.Host, fmt.Sprint(cfg.Server.Port)),
		Handler: withCORS(mux),
		// Guard against slow-header attacks. Deliberately no Read/Write
		// timeouts: /api/events is a long-lived SSE stream.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Token returns the bearer token required for /api/* requests.
func (s *Server) Token() string { return s.token }

func generateToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.http.Shutdown(shutdownCtx)
	}()
	err := s.http.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// requireAuth demands a valid bearer token. EventSource (SSE) cannot set
// headers, so ?token= is accepted as a fallback for GET requests.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		} else {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="local-codex"`)
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			writeErr(w, http.StatusForbidden, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withCORS echoes the Origin header only when it is a loopback origin
// (http://127.0.0.1:* or http://localhost:*), so the UI can be served from
// any local port (e.g. the vite dev server). All other origins get no CORS
// headers, which makes browsers block the response; combined with the
// bearer-token requirement this prevents drive-by requests from malicious
// web pages.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if isLoopbackOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func boolPtr(b bool) *bool { return &b }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req struct {
		Task string `json:"task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Task) == "" {
		writeErr(w, 400, "body must be JSON with a non-empty \"task\" field (max 1 MiB)")
		return
	}
	select {
	case s.taskSem <- struct{}{}:
	default:
		writeErr(w, http.StatusTooManyRequests, "too many concurrent tasks (max %d)", s.maxConcurrentTasks)
		return
	}
	sess := s.store.Create(req.Task)
	go func() {
		defer func() { <-s.taskSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		s.cancelMu.Lock()
		s.cancels[sess.ID] = cancel
		s.cancelMu.Unlock()
		defer func() {
			cancel()
			s.cancelMu.Lock()
			delete(s.cancels, sess.ID)
			s.cancelMu.Unlock()
		}()
		if _, err := s.agent.Run(ctx, sess, req.Task); err != nil {
			s.broker.Publish(logging.Event{
				Type:      "error",
				SessionID: sess.ID,
				Data:      map[string]any{"error": err.Error()},
			})
			// The UI resets its "running" state on agent_finished; every
			// error exit must emit one or the client spins forever.
			s.broker.Publish(logging.Event{
				Type:      "agent_finished",
				SessionID: sess.ID,
				Success:   boolPtr(false),
				Data:      map[string]any{"error": err.Error()},
			})
		}
	}()
	writeJSON(w, 202, map[string]string{"session_id": sess.ID})
}

// handleCancelTask aborts a running task. Cancelling the task context also
// unblocks (rejects) any approval it is waiting on, since BrokerApprover
// waits on the same context.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.cancelMu.Lock()
	cancel, ok := s.cancels[id]
	s.cancelMu.Unlock()
	if !ok {
		writeErr(w, 404, "no running task for session %q", id)
		return
	}
	cancel()
	writeJSON(w, 200, map[string]bool{"cancelled": true})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.store.List()
	snapshots := make([]*session.Session, len(sessions))
	for i, sess := range sessions {
		snapshots[i] = sess.Snapshot()
	}
	writeJSON(w, 200, snapshots)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "unknown session %q", r.PathValue("id"))
		return
	}
	writeJSON(w, 200, sess.Snapshot())
}

// handleEvents streams all agent events as Server-Sent Events.
// Optional ?session_id= filter.
//
// Events are broadcast to every connected client (no per-session ACL): the
// server assumes a single local user, and every SSE connection has already
// authenticated with the bearer token, so all subscribers are the owner.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("session_id")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}

	events, unsubscribe := s.broker.Subscribe()
	defer unsubscribe()

	// Flush headers immediately so clients don't block waiting for the
	// first event.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Comment-line heartbeats keep proxies and browsers from silently
	// dropping the idle connection; they carry no event data.
	heartbeat := time.NewTicker(s.sseHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e, open := <-events:
			if !open {
				return
			}
			if filter != "" && e.SessionID != "" && e.SessionID != filter {
				continue
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req struct {
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, 400, "body must be JSON with \"id\" and \"approved\"")
		return
	}
	if !s.approver.Resolve(req.ID, req.Approved) {
		writeErr(w, 404, "no pending approval %q", req.ID)
		return
	}
	writeJSON(w, 200, map[string]bool{"resolved": true})
}

// handleListApprovals returns all pending approval requests, so a client
// that (re)connected after the SSE broadcast can recover them.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.approver.ListPending())
}

// handleGetApproval returns the full pending approval request (including the
// command text). SSE approval events carry only the id; clients fetch the
// details here.
func (s *Server) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	req, ok := s.approver.Pending(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "no pending approval %q", r.PathValue("id"))
		return
	}
	writeJSON(w, 200, req)
}

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	tool := &tools.GitStatusTool{WS: s.ws}
	res, _ := tool.Execute(r.Context(), nil)
	writeJSON(w, 200, map[string]any{"status": res.Content, "is_error": res.IsError})
}

func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	tool := &tools.GitDiffTool{WS: s.ws}
	res, _ := tool.Execute(r.Context(), nil)
	writeJSON(w, 200, map[string]any{"diff": res.Content, "is_error": res.IsError})
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	tool := &tools.ListFilesTool{WS: s.ws}
	args, _ := json.Marshal(map[string]any{"path": r.URL.Query().Get("path"), "max_depth": 4})
	res, _ := tool.Execute(r.Context(), args)
	writeJSON(w, 200, map[string]any{"tree": res.Content, "is_error": res.IsError})
}

// handleStatic serves the built React UI from web/dist when it exists.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	dist := "web/dist"
	index := filepath.Join(dist, "index.html")
	if _, err := os.Stat(index); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "local-codex API is running.\nThe web UI is not built; run `make web` and restart.\nAPI endpoints: /api/health, /api/tasks, /api/events, /api/git/diff\n")
		return
	}
	http.FileServer(http.Dir(dist)).ServeHTTP(w, r)
}
