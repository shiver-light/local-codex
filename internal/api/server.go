// Package api exposes the agent over HTTP: task submission, session state,
// SSE event streaming, approvals, git views and the file tree. It also
// serves the built web UI from web/dist when present.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"local-codex/internal/agent"
	"local-codex/internal/config"
	"local-codex/internal/logging"
	"local-codex/internal/permission"
	"local-codex/internal/session"
	"local-codex/internal/tools"
	"local-codex/internal/workspace"
)

type Server struct {
	cfg      *config.Config
	ws       *workspace.Workspace
	agent    *agent.Agent
	store    session.Store
	broker   *agent.EventBroker
	approver *permission.BrokerApprover
	http     *http.Server
}

func NewServer(cfg *config.Config, ws *workspace.Workspace, ag *agent.Agent, store session.Store) *Server {
	s := &Server{cfg: cfg, ws: ws, agent: ag, store: store, broker: ag.Broker}
	// Approvals requested by tools are published as events so the web UI can
	// render Approve/Reject buttons; Resolve unblocks the tool.
	s.approver = permission.NewBrokerApprover(func(r permission.Request) {
		s.broker.Publish(logging.Event{
			Type: "approval",
			Data: map[string]any{"id": r.ID, "command": r.Command, "reason": r.Reason},
		})
	})
	ag.Registry = tools.NewBuiltinRegistry(ws, s.approver,
		time.Duration(cfg.Shell.DefaultTimeout)*time.Second,
		time.Duration(cfg.Shell.MaxTimeout)*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/approvals", s.handleApproval)
	mux.HandleFunc("GET /api/git/status", s.handleGitStatus)
	mux.HandleFunc("GET /api/git/diff", s.handleGitDiff)
	mux.HandleFunc("GET /api/files", s.handleFiles)
	mux.HandleFunc("/", s.handleStatic)

	s.http = &http.Server{
		Addr:    net.JoinHostPort(cfg.Server.Host, fmt.Sprint(cfg.Server.Port)),
		Handler: withCORS(mux),
	}
	return s
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

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Task string `json:"task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Task) == "" {
		writeErr(w, 400, "body must be JSON with a non-empty \"task\" field")
		return
	}
	sess := s.store.Create(req.Task)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := s.agent.Run(ctx, sess, req.Task); err != nil {
			s.broker.Publish(logging.Event{
				Type:      "error",
				SessionID: sess.ID,
				Data:      map[string]any{"error": err.Error()},
			})
		}
	}()
	writeJSON(w, 202, map[string]string{"session_id": sess.ID})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.store.List())
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "unknown session %q", r.PathValue("id"))
		return
	}
	writeJSON(w, 200, sess)
}

// handleEvents streams all agent events as Server-Sent Events.
// Optional ?session_id= filter.
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

	for {
		select {
		case <-r.Context().Done():
			return
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
