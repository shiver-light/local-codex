# local-codex Architecture

## Overview

local-codex is a lightweight, local coding agent (similar in spirit to OpenAI
Codex / Claude Code). It reads a local repository, reasons about it through an
OpenAI-compatible LLM, modifies code via patches, runs builds/tests, inspects
git state, and reports results.

```
User (CLI / Web UI)
        |
        v
+-------------------+        +------------------+
|   API Server      |  SSE   |   React Web UI   |
|  (internal/api)   +------->|   (web/)         |
+---------+---------+        +------------------+
          |
          v
+-------------------+
|    Agent Loop     |  internal/agent
+----+--------+-----+
     |        |
     v        v
+---------+  +------------------+
| LLM     |  |  Tool Registry   |  internal/tools
| Client  |  +----+-------+-----+
| (OpenAI |       |       |
|  compat)|       v       v
+---------+  Workspace  Permission / Shell Policy
             (path      (safe / ask / blocked)
             sandbox)
                    |
                    v
             Local Repository
```

## Agent Loop

The core loop (`internal/agent/loop.go`):

1. Build message list: system prompt + session history + user request.
2. Send messages + tool schemas to the LLM.
3. If the response contains tool calls:
   - Execute each call through the `ToolRegistry` (with permission checks).
   - Append tool results to the message list.
   - Continue to the next iteration.
4. Otherwise, return the final answer.
5. Stop conditions: `max_iterations` reached, repeated identical tool calls,
   context cancellation, or context budget exceeded.

## Modules

| Module | Responsibility |
|---|---|
| `internal/llm` | OpenAI-compatible `/v1/chat/completions` client; provider-agnostic interface |
| `internal/tools` | Tool interface, registry, and built-in tools (files, search, patch, shell, git) |
| `internal/workspace` | Workspace root; all file paths are resolved and confined inside it |
| `internal/permission` | Shell command policy (`safe` / `ask` / `blocked`) and approval flow |
| `internal/agent` | Agent loop, system prompt, repetition/loop guards |
| `internal/session` | Session state (messages, tool history, modified files); in-memory store behind an interface |
| `internal/api` | HTTP API + SSE streaming of agent events; bearer-token auth (`server.token` / `LOCAL_CODEX_TOKEN`, random per startup if unset), loopback-only CORS, 1 MiB request bodies, max 4 concurrent tasks |
| `internal/config` | YAML + env configuration (env wins) |
| `internal/logging` | Structured JSON event log (never logs API keys) |
| `cmd/local-codex` | CLI entrypoint (`local-codex <workspace>`) |

## Tool System

Every tool implements:

```go
type Tool interface {
    Name() string
    Description() string
    Schema() map[string]any // JSON Schema for the parameters object
    Execute(ctx context.Context, args json.RawMessage) (Result, error)
}
```

Tools are registered in a `ToolRegistry`; the agent loop never hardcodes tool
names. New tools (Docker, SSH, GDB, ...) are added by implementing `Tool` and
calling `registry.Register(...)`.

Built-in tools: `list_files`, `read_file`, `search_code`, `apply_patch`,
`run_command`, `git_status`, `git_diff`.

## Security Model

- **Workspace confinement**: every file path passed to a tool is resolved
  (symlinks evaluated) and must stay under the workspace root. Absolute paths
  outside the root, `..` escapes, and symlink escapes are rejected.
- **Shell policy**: commands are parsed with `mvdan.cc/sh` and every command
  in the syntax tree (including command substitutions and multi-line scripts)
  is classified `safe` (read-only commands, read-only git subcommands,
  `go build`/`test`/`vet`), `ask` (require user approval via CLI prompt or
  web approval API — the default for interpreters, build tools, git writes,
  unknown programs and anything unparseable), or `blocked` (always refused:
  sudo/dd/mkfs-style programs, `rm -rf` on targets containing `/`, `~` or
  `*`, `/dev/*` writes, fork bombs).
- **No implicit git mutations**: the agent never commits or pushes unless the
  user explicitly asks.

## Context Strategy

Tool-driven context: the model discovers the repo incrementally via
`list_files` / `search_code` / `read_file` (capped at 300 lines per read by
default). No whole-repo embedding, no vector DB. The agent loop guards against
unbounded history growth with a message/byte budget and truncation notices.

## Data Flow (one task)

```
user request
  -> session created (session_id)
  -> loop: LLM -> tool calls -> tool results -> LLM -> ...
  -> every step emitted as an Event (SSE subscribers + structured log)
  -> final assistant message + git diff summary -> user
```
