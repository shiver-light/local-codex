# TASKS

## Phase 1 — Project scaffolding ✅
- [x] Check working directory / toolchain
- [x] Create project structure
- [x] ARCHITECTURE.md / TASKS.md / README
- [x] go mod init
- [x] config.yaml / .env.example / Makefile / .gitignore

## Phase 2 — Core infrastructure ✅
- [x] internal/config: YAML + ENV loading (env wins) + tests
- [x] internal/llm: OpenAI-compatible client (chat completions, tool calls) + mock tests
- [x] internal/tools: Tool interface + ToolRegistry + tests
- [x] internal/workspace: path confinement (.. / absolute / symlink escape) + tests
- [x] internal/permission: CommandPolicy (safe/ask/blocked) + Approver + tests
- [x] Tools: list_files, read_file (300-line paging), search_code (rg→grep), git_status, git_diff, run_command (stdout/stderr/exit code/timeout)
- [x] `go test ./...` green

## Phase 3 — Agent loop ✅
- [x] System prompt (internal/agent/prompts.go)
- [x] Agent loop: LLM → tool calls → results → LLM
- [x] Max iterations guard, consecutive-repeat guard (nudge once, then abort), context byte budget with truncation
- [x] internal/session: SessionStore (in-memory, interface for SQLite later)
- [x] internal/logging: structured JSON event log
- [x] Mock LLM tests: max-iteration cap, repeat guard, simple flow
- [x] `go test ./...` green

## Phase 4 — Patch + closed loop ✅
- [x] apply_patch (Begin/Update/Add/Delete/End Patch; failures reported to the model, never silent)
- [x] tests/fixtures/sample-repo with real bug + failing test
- [x] E2E agent test (mock LLM): read → wrong patch → test fails → fix → test passes → git diff
- [x] `go test ./...` green

## Phase 5 — CLI ✅
- [x] `local-codex <workspace>` interactive REPL + `--task` one-shot + `--y` auto-approve
- [x] Streaming tool/agent output to terminal
- [x] CLI approval prompt for `ask` commands
- [x] **Live E2E acceptance passed** against remote Ollama qwen3-coder:
      list → read → patch (recovered from a malformed patch) → go test PASS → git diff → summary

## Phase 6 — HTTP API + Web UI ✅
- [x] internal/api: REST (tasks/sessions/git/files) + SSE event stream + approval endpoints + integration tests
- [x] web/: React + TypeScript + Vite (chat, tool log, approvals, file tree, git diff) — builds to web/dist, served by the Go server
- [x] Live API E2E passed: POST /api/tasks drove a full fix loop with the real LLM

## Final acceptance ✅
- [x] `go build ./...` clean, `go vet` clean
- [x] `go test ./... -count=1` green (exit 0)
- [x] `local-codex ./example-project` full loop verified with live LLM
