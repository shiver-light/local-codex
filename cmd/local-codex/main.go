// Command local-codex runs the local coding agent.
//
//	local-codex [flags] <workspace>
//
// Modes:
//
//	interactive CLI (default): read tasks from stdin
//	--serve: run the HTTP API + Web UI
//	--task "..." : run a single task and exit
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"local-codex/internal/agent"
	"local-codex/internal/api"
	"local-codex/internal/config"
	"local-codex/internal/llm"
	"local-codex/internal/logging"
	"local-codex/internal/permission"
	"local-codex/internal/session"
	"local-codex/internal/tools"
	"local-codex/internal/workspace"
)

func main() {
	var (
		serve      = flag.Bool("serve", false, "run the HTTP API + Web UI instead of the CLI")
		configPath = flag.String("config", "config/config.yaml", "path to YAML config (missing file is OK)")
		oneShot    = flag.String("task", "", "run a single task non-interactively and exit")
		yesAll     = flag.Bool("y", false, "auto-approve commands that require approval")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: local-codex [flags] <workspace>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	workspaceRoot := "."
	if flag.NArg() > 0 {
		workspaceRoot = flag.Arg(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config: %v", err)
	}

	ws, err := workspace.Open(workspaceRoot)
	if err != nil {
		fatal("%v", err)
	}

	logOut := io.Writer(os.Stderr)
	if cfg.Logging.File != "" {
		f, err := os.OpenFile(cfg.Logging.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fatal("open log file: %v", err)
		}
		defer f.Close()
		logOut = f
	}
	logger := logging.New(logOut)
	broker := agent.NewEventBroker()
	llmClient := llm.NewOpenAIClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model)
	store := session.NewMemoryStore()

	ag := &agent.Agent{
		LLM:                llmClient,
		Logger:             logger,
		Broker:             broker,
		MaxIterations:      cfg.Agent.MaxIterations,
		MaxRepeatToolCalls: cfg.Agent.MaxRepeatToolCalls,
		MaxContextBytes:    cfg.Agent.MaxContextBytes,
		Temperature:        cfg.LLM.Temperature,
		MaxTokens:          cfg.LLM.MaxTokens,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *serve {
		srv := api.NewServer(cfg, ws, ag, store)
		fmt.Printf("local-codex server on http://%s:%d (workspace: %s)\n", cfg.Server.Host, cfg.Server.Port, ws.Root())
		if cfg.Server.Token == "" {
			fmt.Printf("generated API token (set LOCAL_CODEX_TOKEN to pin it): %s\n", srv.Token())
		}
		fmt.Printf("web UI: http://%s:%d/?token=%s\n", cfg.Server.Host, cfg.Server.Port, srv.Token())
		if err := srv.ListenAndServe(ctx); err != nil {
			fatal("server: %v", err)
		}
		return
	}

	// CLI mode.
	var approver permission.Approver = permission.NewCLIApprover(os.Stdin, os.Stdout)
	if *yesAll {
		approver = permission.ApproveAll{}
	}
	ag.Registry = tools.NewBuiltinRegistry(ws, approver,
		time.Duration(cfg.Shell.DefaultTimeout)*time.Second,
		time.Duration(cfg.Shell.MaxTimeout)*time.Second)

	events, unsubscribe := broker.Subscribe()
	defer unsubscribe()
	go printEvents(events, os.Stdout)

	fmt.Printf("local-codex — workspace: %s\n", ws.Root())
	fmt.Printf("model: %s @ %s\n\n", cfg.LLM.Model, cfg.LLM.BaseURL)

	if *oneShot != "" {
		if err := runTask(ctx, ag, store, *oneShot); err != nil {
			fatal("%v", err)
		}
		return
	}

	fmt.Println("Type a coding task (empty line or Ctrl-C to quit).")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		task := strings.TrimSpace(scanner.Text())
		if task == "" {
			break
		}
		if err := runTask(ctx, ag, store, task); err != nil {
			fmt.Fprintf(os.Stderr, "\n[task failed] %v\n", err)
		}
	}
}

func runTask(ctx context.Context, ag *agent.Agent, store session.Store, task string) error {
	sess := store.Create(task)
	if _, err := ag.Run(ctx, sess, task); err != nil {
		return err
	}
	// The final answer was already streamed as an agent_message event;
	// here we only add bookkeeping info.
	fmt.Printf("\n=== Task complete ===\n")
	if len(sess.ModifiedFiles) > 0 {
		fmt.Printf("Files changed: %s\n", strings.Join(sess.ModifiedFiles, ", "))
	}
	return nil
}

// printEvents renders the agent event stream on the terminal.
func printEvents(events <-chan logging.Event, out io.Writer) {
	for e := range events {
		switch e.Type {
		case "user_message":
			// already echoed
		case "agent_message":
			fmt.Fprintf(out, "\n[Agent] %s\n", e.Data["content"])
		case "tool_call":
			args, _ := json.Marshal(e.Data["args"])
			fmt.Fprintf(out, "\n[Tool] %s %s\n", e.Tool, prettyArgs(e.Tool, e.Data["args"]))
			_ = args
		case "tool_result":
			status := "ok"
			if e.Success != nil && !*e.Success {
				status = "FAILED"
			}
			fmt.Fprintf(out, "[Tool Result] %s (%s, %s)\n%s\n", e.Tool, status, e.Duration, e.Data["content"])
		case "error":
			fmt.Fprintf(out, "\n[Error] %s\n", e.Data["error"])
		case "agent_finished":
			if e.Success != nil && !*e.Success {
				fmt.Fprintf(out, "\n[Agent stopped] %s\n", e.Data["error"])
			}
		}
	}
}

// prettyArgs shortens tool-call arguments for display (patch bodies etc.).
func prettyArgs(tool string, raw any) string {
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) == nil {
		var parts []string
		for k, v := range m {
			str, _ := v.(string)
			if len(str) > 120 {
				str = str[:120] + "..."
			}
			parts = append(parts, fmt.Sprintf("%s=%q", k, str))
		}
		return strings.Join(parts, " ")
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "local-codex: "+format+"\n", args...)
	os.Exit(1)
}
