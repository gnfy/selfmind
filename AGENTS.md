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
- `docs/provider-runtime.md`
- `docs/provider-runtime.zh-CN.md`

## Handoff Runbook

Use this checklist whenever a future AI/coding agent picks up work in this
repo:

1. Read this file, then read the specific docs linked above for the area being
   changed. For provider/model work, `docs/provider-runtime.md` is mandatory.
2. Inspect `git status --short` before editing. The worktree is often dirty;
   do not revert unrelated changes or generated output you did not create.
3. Locate the current boundary before changing code. Prefer `rg` and focused
   file reads over broad guessing.
4. For analysis-only user requests, do not edit code. For implementation
   requests, carry the work through code, tests, and, when relevant, a WSL
   binary build.
5. Keep changes scoped to the requested behavior. If a fix exposes a larger
   architectural problem, document the follow-up instead of silently doing a
   risky refactor.
6. After touching core runtime, provider, gateway, task, tool, or TUI behavior,
   update this file or the relevant document when the rule for future
   development has changed.

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
- Hard tasks should not appear stalled. Long-running runs must emit visible
  progress through events, status text, or outbound channel notifications. A
  timeout, failed tool call, or model error should lead to diagnosis, retry or a
  clear handoff, not silent abandonment.
- CLI and IM have different feedback contracts. CLI/TUI should stream assistant
  text and tool progress when possible. IM channels such as Weixin should not
  stream every token; instead they should send concise "working" notices,
  important tool/approval milestones, and a final answer or handoff.
- Keep channel UX channel-specific, but task semantics shared. A CLI run,
  Weixin message, Telegram message, or future web request should all create or
  resume the same account/person/task/run/event/approval/artifact lifecycle.
- File and terminal tools must run inside the active workspace scope. Preserve
  `WorkspaceScopeMiddleware` and extend it when adding tools that touch files,
  processes, or paths.
- Model/provider choice should go through role-based routing. Keep roles such
  as `coding_agent`, `memory_extract`, `background_review`, `skill_curator`,
  and `semantic_recall` stable so they can become SaaS policy keys later.
- Provider discovery, credential resolution, model-list fetching, and profile
  overrides belong in `internal/modelruntime`. Do not add vendor auth probing
  or model-list fetch logic directly to `internal/app/agent.go` or LLM
  adapters.
- Provider integration must follow the Provider Runtime boundary documented in
  `docs/provider-runtime.md`: `ProviderProfile` describes a vendor, `Resolver`
  produces a resolved `Runtime`, app wiring turns the runtime into an
  `llm.Provider`, and adapters only implement protocol transports.
- Prefer existing protocol adapters (`openai_chat`, `openai_compatible`,
  `anthropic_messages`, `codex_responses`) when adding model vendors. Add a new
  Go adapter only when the wire protocol is genuinely different.
- Put provider-specific behavior in `ProviderQuirks` first: auth header,
  tool-schema repair, system-message mode, thinking behavior, User-Agent, and
  capability metadata. Do not scatter provider-name checks across CLI, gateway,
  IM adapters, or app setup.
- User YAML `provider_profiles.*.quirks` currently supports
  `auth_header`, `tool_schema`, `system_message_mode`, `thinking_mode`, and
  `user_agent`. Capability flags such as tool/streaming/vision support belong
  in built-in Go profiles.
- Kimi Coding Plan should default to `kimi-coding` +
  `anthropic_messages` + `https://api.kimi.com/coding` +
  `kimi-for-coding`, with Moonshot tool schema repair, no Anthropic `thinking`
  field on that path, `max_tokens=32000`, `User-Agent: claude-code/0.1.0`,
  and HTTP/2 disabled for the Kimi /coding transport. The transport must also
  restrict TLS ALPN to `http/1.1`; otherwise the server can negotiate `h2`,
  send HTTP/2 frames, and Go will report `unexpected EOF` or a malformed
  HTTP/1.x response. This mirrors Hermes' HTTP/1.1 client behavior.
- MiniMax Coding Plan should default to Anthropic-compatible endpoints
  (`https://api.minimax.io/anthropic` or `https://api.minimaxi.com/anthropic`),
  Bearer auth, and fallback models `MiniMax-M3`, `MiniMax-M2.7`,
  `MiniMax-M2.7-highspeed`, `MiniMax-M2.5`. MiniMax OAuth token refresh belongs
  in `internal/modelruntime`, not in LLM adapters.
- Role-based model overrides must pass the same runtime fields as the default
  provider: headers, max tokens, reasoning effort, thinking, service tier, and
  quirks. CLI, gateway, IM, and future SaaS policy should all resolve providers
  through the same `modelruntime.Resolver`.
- Keep `context_length` and `max_tokens` separate. `context_length` is the
  model's total input+output context window and drives UI usage display,
  context budgeting, and future compression policy. `max_tokens` is only the
  single-response output cap sent to a provider. Do not display `max_tokens` as
  the model context size, and do not hardcode fake windows such as `1M` in CLI
  or IM status surfaces.
- When adding or changing core runtime code, add concise intent/boundary
  comments for exported types, non-obvious control flow, compatibility paths,
  and invariants. This especially applies to `internal/modelruntime`, agent
  wiring, gateway runner/control, tool dispatch, and memory/skill learning.
  Comments should explain why the boundary exists or what must stay true; do
  not restate simple assignments, and do not leave mojibake or hidden-encoding
  comments.
- P2 auth reuse is intentionally limited to Codex CLI, Claude Code, Gemini
  CLI, and Qwen CLI. Other model vendors should use API keys, custom
  OpenAI-compatible endpoints, or `provider_profiles`.
- Tool calling should stay Hermes-like: pass tool schemas as native LLM
  `tool_calls` where the provider supports it, preserve `tool_call_id` on
  tool result messages, and keep `[TOOL:...]` only as a compatibility fallback.
- Tool exposure should be planned per turn, not left entirely to the model.
  Stable coding examples, explanations, and learning/advice requests should not
  expose `web_search`/`web_extract` unless the user explicitly asks to search,
  browse, inspect a URL, check official docs, or retrieve current/latest
  external information.
- Per-turn tool policy is centralized in `internal/kernel/task_strategy.go`.
  Future agent work should extend `TaskStrategy` classification, `ToolMode`,
  `PlanPolicy`, and `WebPolicy` there instead of adding ad hoc keyword checks
  in CLI, IM, provider adapters, or prompt-building code. The same strategy
  must filter native tool schemas and legacy `[TOOL:...]` fallback calls before
  execution.
- Gateway intent routing is centralized in `internal/gateway/router/intent*.go`
  and returns `router.IntentResult`. Keep rule-based cues configurable through
  `intent.rules` in `config.yaml`; use `intent.mode: rules|hybrid|llm` to
  control whether the lightweight LLM classifier participates. Do not add new
  task/chat/continue keyword checks in TUI, IM adapters, or HTTP handlers.
- Low-confidence intent should ask a short clarification question instead of
  silently creating a task or dropping the message into casual chat. This is
  especially important for IM/SaaS usage, where an accidental task can pollute a
  shared person/task timeline.
- The gateway must build `TaskStrategy` from the original user content before
  adding daemon, workspace, attachment, task, or resume context. Pass it to the
  agent with `kernel.WithTaskStrategy`; otherwise simple coding examples can be
  misclassified as repo tasks just because the gateway prompt contains
  `workspace_root` or `task_id`.
- Use `update_plan` only when the selected `TaskStrategy.PlanPolicy` permits
  it. Simple identity/model questions, one-shot code snippets, examples, and
  stable explanations should answer directly. Repo inspection, debugging,
  CI/CD, multi-file edits, and long verification tasks should expose local
  tools and visible progress; debugging and CI/CD should require a plan.
- Long-running agent work must emit structured progress events. Use
  `agent.thinking` for model-decision phases, `tool.started`/`tool.output`/
  `tool.completed` for tool execution, and `turn.completed` for final state.
  CLI/TUI should show these steps live; IM channels should keep token streams
  collapsed but preserve working notices, event records, and failure summaries.
- Tool results must be packaged through the Agent result envelope before they
  reach the model, TUI, run events, or future artifacts. Keep raw tool output,
  model-bounded content, and user-visible preview as separate surfaces; do not
  reuse one truncated string for all three. UI/event previews should be compact
  and UTF-8 safe, while model content can use a bounded head/tail view with a
  clear note when the full output is too large for context.
- Tool and command output shown in CLI/TUI should be human-readable. Do not dump
  raw JSON payloads into the transcript unless the user explicitly asked for
  raw protocol details. Summarize plan updates, tool calls, and errors in a
  compact form.
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
- Catalog installs must preserve Hermes-style provenance. Store install
  records under `~/.selfmind/<tenant>/skills/.catalog/lock.json`, mark usage
  source as `catalog-installed`, reject same-name directory or legacy `.md`
  collisions by default, and only overwrite when the user explicitly passes
  `--force`.
- Forced catalog reinstalls must move the previous copy into
  `~/.selfmind/<tenant>/skills/.catalog/backups/` before writing the new copy.
  Do not silently replace user-installed or hand-written skills without a
  backup and explicit force.
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
- `internal/kernel/task_strategy.go`: per-turn task classification and tool,
  plan, web, and progress policy shared by CLI/IM/web entrypoints.
- `internal/gateway/httpapi/outcome.go`: structured run outcome extraction for
  task status, handoff, and IM/CLI status cards.
- `internal/gateway/cli/controller.go`: Bubble Tea state orchestration. Keep
  new visual behavior in smaller renderer/component files when possible.
- `internal/kernel/llm/model_gateway.go`: role-based model routing.
- `docs/provider-runtime.md` and `docs/provider-runtime.zh-CN.md`: mandatory
  provider integration rules and checklist for future model vendors.
- `internal/kernel/llm/anthropic_adapter.go`: Anthropic Messages adapter.
- `internal/kernel/llm/adapters.go`: OpenAI-compatible, OpenRouter, MiniMax
  legacy, and generic OpenAI-compatible adapters.
- `internal/modelruntime/`: provider metadata, credential resolution, external
  CLI auth reuse, and live model catalog/cache.
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

When working from WSL in this checkout, the equivalent command is:

```sh
cd /mnt/d/wwwroot/ai/selfmind
GOWORK=off /usr/local/go/bin/go test ./...
```

For provider-runtime changes, always run at least:

```sh
GOWORK=off /usr/local/go/bin/go test ./internal/modelruntime ./internal/kernel/llm ./internal/app
```

## WSL Build And Smoke Test

When the user expects to try the result in WSL, build the Linux amd64 binary and
copy it into the user's local path:

```sh
cd /mnt/d/wwwroot/ai/selfmind
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 /usr/local/go/bin/go build -trimpath -ldflags="-s -w" -o dist/selfmind-linux-amd64/selfmind ./cmd/selfmind
cp dist/selfmind-linux-amd64/selfmind ~/.local/bin/selfmind.new
chmod +x ~/.local/bin/selfmind.new
mv -f ~/.local/bin/selfmind.new ~/.local/bin/selfmind
~/.local/bin/selfmind --help
~/.local/bin/selfmind model check
```

Do this after changes to CLI/TUI, model runtime, gateway startup, provider
configuration, or anything the user is actively testing through
`~/.local/bin/selfmind`.

## Documentation Maintenance

- Update `AGENTS.md` when a new invariant, workflow, or "future AI must know"
  rule is introduced.
- Update `docs/development-guide*.md` for broader engineering explanations.
- Update `docs/provider-runtime*.md` for model provider rules, provider quirks,
  Kimi/MiniMax behavior, OAuth behavior, or new model-vendor checklists.
- Keep user-facing README changes separate from internal development rules.
