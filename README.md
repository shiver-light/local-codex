# local-codex

A lightweight, local **coding agent** — a minimal local counterpart to OpenAI
Codex / Claude Code. It is not a chatbot: it reads your repository, searches
code, patches files, runs builds and tests, reacts to failures, inspects
`git diff`, and reports what it did.

## Features

- Autonomous agent loop: `LLM -> tool calls -> results -> LLM -> ... -> done`
- OpenAI-compatible LLM backend (Ollama, vLLM, llama.cpp, OpenAI, ...)
- Built-in tools: `list_files`, `read_file`, `search_code`, `apply_patch`,
  `run_command`, `git_status`, `git_diff`
- Workspace sandbox: file access is confined to the workspace root
  (blocks `../`, absolute-path and symlink escapes)
- Shell command policy: `safe` / `ask` (user approval) / `blocked`
- Interactive CLI and a Web UI (React + SSE live event stream)
- Structured JSON event logging, session store behind an interface

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md).

```
CLI / Web UI -> API Server -> Agent Loop -> LLM Client (OpenAI-compatible)
                                   |
                                   v
                        Tool Registry -> Workspace / Permission / Shell / Git
```

## Requirements

- Go 1.23+
- Node 20+ (only for the Web UI)
- `rg` (ripgrep) recommended; falls back to `grep`
- An OpenAI-compatible LLM endpoint

## Build

```sh
make build        # produces bin/local-codex
make test         # run all tests
make web          # build the Web UI into web/dist
```

## Quick start (CLI, Ollama)

```sh
ollama serve
ollama pull qwen3-coder

export LLM_BASE_URL=http://127.0.0.1:11434/v1
export LLM_API_KEY=ollama
export LLM_MODEL=qwen3-coder

go run ./cmd/local-codex ./example-project
```

Then type a task:

```
> 修复 MQTT reconnect bug，并运行相关测试
```

## Quick start (Web UI)

```sh
make web
./bin/local-codex --serve ./example-project
# open http://127.0.0.1:8080
```

## vLLM example

```sh
python -m vllm.entrypoints.openai.api_server --model Qwen/Qwen2.5-Coder-7B-Instruct

export LLM_BASE_URL=http://127.0.0.1:8000/v1
export LLM_API_KEY=EMPTY
export LLM_MODEL=Qwen/Qwen2.5-Coder-7B-Instruct

local-codex ./your-project
```

## Configuration

YAML (`config/config.yaml`) plus environment variables; **env wins**.

| Env | YAML | Default |
|---|---|---|
| `LLM_BASE_URL` | `llm.base_url` | `http://127.0.0.1:11434/v1` |
| `LLM_API_KEY` | `llm.api_key` | `ollama` |
| `LLM_MODEL` | `llm.model` | `qwen3-coder` |
| `MAX_AGENT_ITERATIONS` | `agent.max_iterations` | `30` |
| `PORT` | `server.port` | `8080` |
| `SHELL_DEFAULT_TIMEOUT` | `shell.default_timeout` | `120` |

## Adding a tool

```go
type myTool struct{}

func (myTool) Name() string        { return "my_tool" }
func (myTool) Description() string { return "..." }
func (myTool) Schema() map[string]any {
    return map[string]any{
        "type": "object",
        "properties": map[string]any{
            "arg": map[string]any{"type": "string"},
        },
        "required": []string{"arg"},
    }
}
func (myTool) Execute(ctx context.Context, args json.RawMessage) (tools.Result, error) {
    // ...
}

registry.Register(myTool{})
```

Register it where built-ins are registered (`internal/tools/builtin.go`) or in
your own wiring code. The agent loop picks it up automatically.

## Security model

- All file paths are resolved against the workspace root; `..`, absolute paths
  outside the root, and symlink escapes are rejected.
- Shell commands are parsed into a syntax tree (`mvdan.cc/sh`) and every
  command that would execute — including pipelines, multi-line scripts,
  command substitutions and commands wrapped by `env`/`nohup` — is classified
  by `permission.Policy`:
  - `safe`: read-only commands (`ls`, `cat`, `head`, `tail`, `wc`, `rg`,
    `grep`, `find` without `-delete`/`-exec`, `diff`, ...), read-only git
    subcommands (`status`, `diff`, `log`, `show`, ...), and
    `go build`/`test`/`vet`/`list`/... (these can execute arbitrary code —
    accepted tradeoff for a coding agent; see policy source comments).
  - `ask` (require user approval): everything else — interpreters and build
    tools (`python`, `node`, `make`, `npm`, `pip`, `cargo`, ...), git write
    subcommands (`push`, `clean`, `reset --hard`, ...), network tools
    (`curl`, `ssh`, `docker`, ...), wrappers like `xargs`, and anything the
    parser cannot resolve statically.
  - `blocked`: `sudo`, `shutdown`, `reboot`, `mkfs*`, `dd`, mounts, plus
    recursive force `rm` whose target contains `/`, `~` or `*` (detected on
    parsed argv, so whitespace/flag variants cannot bypass it), writes to
    `/dev/*`, and fork bombs.
- The agent never runs `git commit` / `git push` unless explicitly asked.
- API keys are never written to logs.

## Development plan

- [x] CLI agent loop with tool calling
- [x] Web UI with live events, approvals, git diff view
- [ ] SQLite session store
- [ ] Streaming LLM responses
- [ ] Docker / SSH / GDB tools
- [ ] Multi-session Web UI

## License

MIT
