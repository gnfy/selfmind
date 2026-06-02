# SelfMind Agent Notes

This file is for future AI/coding agents continuing work in this repo.

## Current Direction

SelfMind is evolving from a local CLI assistant into a long-running personal
gateway that can later become a SaaS control plane. The key product idea is:

- one always-on SelfMind process can work 24/7;
- CLI, IM, and Web clients can all command it;
- different platform accounts can be bound to the same human (`person_id`);
- each user's workspace is isolated;
- chats are channel-local, while task state, runs, handoffs, memory, and skills
  are shared.

Read the full local architecture note first:

- `docs/daemon-im-saas-architecture.md`

## Architecture Rules

- Treat `selfmind gateway` as the product entrypoint for multi-terminal work.
  `selfmindd` is only a hidden compatibility wrapper while the codebase
  stabilizes. The CLI/TUI can still run locally, but IM/Web integration should
  go through gateway APIs.
- Keep `cmd/selfmind/main.go` thin. User-facing command parsing and CLI client
  behavior belong in `internal/cliapp`; do not grow business logic in `cmd`.
- Treat Linux server as the official release target. GitHub Releases should
  package only the `selfmind` Linux `amd64` / `arm64` binaries plus systemd
  service/install assets until Windows and macOS have production hardening.
- Keep chat transcripts channel-local. Do not automatically mirror CLI messages
  into IM, or IM messages into CLI.
- Share durable state through `control.db`: tenants, persons, accounts,
  workspaces, tasks, runs, events, handoffs, approvals, and notifications.
- Use `person_id` as the "same human" identity. Use `account_id` for platform
  bindings such as `cli/local`, `feishu/ou_xxx`, `wechat/openid`.
- Platform adapters should parse/authenticate/send platform payloads only. The
  gateway owns identity binding, workspace lookup, task/run state, and agent
  dispatch.
- Keep gateway control commands lightweight and pre-agent. `/status`, `/stop`,
  `/tasks`, `/workspaces`, `/resume`, and `/workspace` should not consume model
  tokens.
- Preserve the per-person active-run guard and the gateway's serialized agent
  call until a real worker pool exists; the current Agent object is not safe to
  run freely in parallel.
- File and terminal tools must run inside the active workspace scope. Preserve
  `WorkspaceScopeMiddleware` and extend it when adding tools that touch files,
  processes, or paths.
- Model/provider choice should go through role-based routing. Keep roles such
  as `coding_agent`, `memory_extract`, `background_review`, `skill_curator`,
  and `semantic_recall` stable so they can become SaaS policy keys later.
- Tool calling should stay Hermes-like: pass tool schemas as native LLM
  `tool_calls` where the provider supports it, preserve `tool_call_id` on
  tool result messages, and keep `[TOOL:...]` only as a compatibility fallback.
- Only execute clearly read-only tool batches in parallel. Terminal, memory,
  skill mutation, file writes, patches, process control, delegation, and unknown
  tools should run sequentially unless a dedicated safety policy says otherwise.

## Important Files

- `cmd/selfmind/main.go`: thin user entrypoint that calls `internal/cliapp`.
- `internal/cliapp/`: selfmind CLI application layer, including gateway
  lifecycle commands, gateway client commands, and TUI bootstrap.
- `cmd/selfmindd/main.go`: hidden compatibility wrapper around the gateway
  runner.
- `internal/gateway/api/`: gateway HTTP request/response DTOs shared by
  clients and handlers.
- `internal/gateway/httpapi/server.go`: local HTTP API and IM webhook
  normalization.
- `internal/control/store.go`: control-plane SQLite schema and persistence.
- `internal/tools/workspace_scope.go`: workspace execution boundary.
- `internal/kernel/llm/model_gateway.go`: role-based model routing.
- `internal/kernel/native_tool_call.go`: native/fallback tool-call conversion,
  structured result messages, and safe parallel execution policy.
- `internal/tools/session_search.go`: tenant-aware history search.

## Local Test Command

In this workspace, run tests with:

```powershell
$env:PATH="$env:USERPROFILE\.cache\selfmind-tools\go1.26.3\go\bin;" + $env:PATH
$env:GOWORK='off'
go test ./...
```

`GOWORK=off` avoids the parent workspace file excluding this module.
