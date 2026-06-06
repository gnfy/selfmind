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
- `docs/architecture-constraints.md`
- `docs/architecture-constraints.zh-CN.md`

## Architecture Rules

- Treat `docs/architecture-constraints.md` as mandatory guardrails, not as
  optional suggestions. Future AI/coding agents should read it before making
  broad code changes.
- Do not keep growing `internal/gateway/cli/controller.go`. It should
  orchestrate state, route Bubble Tea messages, and connect components; reusable
  transcript, composer, pager, modal, and command behavior belongs in
  `internal/ui/components` or a dedicated gateway/cli module.
- New transient TUI pages, such as help, detail, status, task, model, or search
  screens, should use `internal/ui/components/Pager` or another reusable
  surface component instead of writing one-off viewport logic in the controller.
- Slash command metadata should move toward a single registry shared by command
  dispatch, `/help`, and editor hints. Do not duplicate a command's name,
  description, and usage across unrelated files. The current registry lives in
  `internal/gateway/cli/slash_commands.go`.
- Avoid adding cross-tenant or cross-test global mutable state. Prefer explicit
  dependencies wired by `internal/app` or the gateway runner.
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
- Approval state belongs to `control.approval_requests` and gateway
  control/API handlers. IM adapters can render approval buttons or parse
  callbacks, but must not own approval lifecycle state.
- Keep gateway control commands lightweight and pre-agent. `/status`, `/stop`,
  `/tasks`, `/workspaces`, `/resume`, and `/workspace` should not consume model
  tokens.
- Preserve the per-person active-run guard and the gateway's serialized agent
  call until a real worker pool exists; the current Agent object is not safe to
  run freely in parallel.
- Gateway run events must use a per-run event sink installed with
  `kernel.WithEventChannel(ctx, ch)`. Do not temporarily replace
  `Agent.EventChannel` in gateway code; that field is only a legacy fallback for
  local TUI paths.
- User-visible task state should be derived from structured run outcomes
  (`api.RunOutcome`) and handoffs, not from ad hoc status text spread across
  handlers.
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
- Keep skill handling layered and progressive: `skills_list` is for metadata,
  `skill_view` reads content, `skill_manage` mutates skills, `skill_catalog`
  installs/audits skills, and `skill_bundle` manages grouped workflows.
- Skill mutations should hot-reload the active registry when possible. Direct
  slash invocation must resolve bundles before individual skills.
- Curator automation should only govern `agent-created` skills by default.
  Manual, catalog-installed, bundled, and pinned skills must not be archived
  automatically.
- Memory and skill mutations should write learning audit records under the
  tenant learning log. Do not add one-off history files in individual tools.
- User-facing learning history and undo should go through the shared
  `skill_manage` and `memory` tool actions so CLI/TUI/IM behavior stays
  consistent and auditable.

## Important Files

- `cmd/selfmind/main.go`: thin user entrypoint that calls `internal/cliapp`.
- `internal/cliapp/`: selfmind CLI application layer, including gateway
  lifecycle commands, gateway client commands, and TUI bootstrap.
- `cmd/selfmindd/main.go`: hidden compatibility wrapper around the gateway
  runner.
- `internal/gateway/api/`: gateway HTTP request/response DTOs shared by
  clients and handlers.
- `internal/gateway/httpapi/server.go`: local HTTP API and IM webhook
  shared message/run flow. Endpoint handlers live in split `handlers_*.go`,
  `active_runs.go`, and `run_events.go` files in the same package.
- `internal/control/store.go`: control-plane SQLite schema and persistence.
- `internal/tools/workspace_scope.go`: workspace execution boundary.
- `internal/gateway/cli/transcript_renderer.go`: TUI transcript, startup card,
  and tool-message rendering.
- `internal/gateway/cli/slash_commands.go`: slash command metadata and
  dispatcher registry shared by help/editor/dispatch.
- `internal/kernel/event_context.go`: per-run agent event sink injection.
- `internal/gateway/httpapi/outcome.go`: structured run outcome extraction for
  task status, handoff, and IM/CLI status cards.
- `internal/kernel/llm/model_gateway.go`: role-based model routing.
- `internal/kernel/llm/anthropic_adapter.go`: Anthropic Messages adapter.
- `internal/kernel/native_tool_call.go`: native/fallback tool-call conversion,
  structured result messages, and safe parallel execution policy.
- `internal/tools/session_search.go`: tenant-aware history search.
- `internal/tools/skill_runtime.go`: progressive skill list/view helpers,
  runtime reload, and direct slash invocation payloads.
- `internal/tools/skill_bundles.go`: YAML skill bundles for grouped workflow
  invocation.
- `internal/tools/skill_catalog.go`: official/local/URL/GitHub skill install
  and audit.
- `internal/tools/skill_fuzzy_patch.go`: tolerant skill content patching.
- `internal/tools/skill_curator.go`: skill lifecycle governance, dry-run,
  reports, archive, and restore.
- `internal/tools/learning_audit.go`: tenant learning history for memory/skill
  mutations, including history and supported undo helpers.

## Local Test Command

In this workspace, run tests with:

```powershell
$env:PATH="$env:USERPROFILE\.cache\selfmind-tools\go1.26.3\go\bin;" + $env:PATH
$env:GOWORK='off'
go test ./...
```

`GOWORK=off` avoids the parent workspace file excluding this module.
