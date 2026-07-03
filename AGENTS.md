# SelfMind Agent Notes

This file is for future AI/coding agents continuing work in this repo. It is
auto-injected into every agent session, so it holds the rules you need on every
task; per-domain details live in the docs it points to. Pointers marked
**mandatory** mean: read that doc before changing that area.

## Current Direction

**North star (Phase 1): cross-endpoint continuity for one person.** SelfMind is
a personal always-on gateway. One human (`person_id`) binds many platform
accounts (CLI, WeChat, Telegram, Web); chats stay channel-local, while tasks,
runs, handoffs, approvals, memory, and skills follow the person. The three
Phase-1 acceptance scenarios and the continuity contract live in
`docs/identity-continuity.md` — work that does not serve those scenarios or fix
a defect on their path needs an explicit reason.

SaaS is deferred: keep `tenant_id` plumbing intact so a later multi-tenant
control plane is not blocked, but do not design for SaaS now. Do not grow
provider breadth, IM channel count, or surface-level parity with other coding
CLIs (TUI cosmetics, command breadth) beyond what the scenarios need. **Agent
execution quality — planning, reliable tool calling, error diagnosis/recovery,
bounded context, verification — is the competence bar and always in scope**:
continuity of a badly executed task is worthless. The arbiter is the
day-in-the-life eval scorecard (`selfmind eval scorecard`), not feature
comparison against other tools; see `docs/identity-continuity.md` "Two bars".

Read first:

- `docs/identity-continuity.md` (north star, identity model, continuity contract)
- `docs/STATUS.md` (implementation snapshot and the only live priority list)
- `docs/architecture-constraints.md` (mandatory guardrails; zh-CN mirror exists)

Domain docs (**mandatory** before changing that domain):

- Providers/models: `docs/provider-runtime.md` (zh-CN mirror exists)
- Skills: `docs/skills-architecture.md`
- Eval: `docs/eval-loop.md`
- Context lifecycle: `docs/context-lifecycle.zh-CN.md`
- Worker pool / daemon-client: `docs/worker-pool-design.md` §8
- TUI rendering: `docs/tui-terminal-first-hybrid.md`

## Handoff Runbook

1. Read this file, then `docs/STATUS.md` before assuming any feature is
   missing — several items older docs call "to do" are already done. Then read
   the domain doc for the area being changed.
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
   update this file or the relevant domain doc when the rule for future
   development has changed.
7. Any change that alters user-visible behavior must, in the same PR, update
   the matching `docs/STATUS.md` row and add or update an eval case in
   `evalcases/` when the behavior sits on the message path. Doc drift and an
   empty eval gate are regressions, not chores to defer.

## Non-Negotiable Invariants

- `selfmind` is the single binary; the daemon runs as `selfmind gateway run`.
  Never reintroduce a separate daemon entrypoint binary. Keep
  `cmd/selfmind/main.go` thin; user-facing command parsing belongs in
  `internal/cliapp`.
- Chat transcripts are channel-local, never mirrored across channels. The
  shared cross-channel layer is durable work state in `control.db`: tenants,
  persons, accounts, workspaces, tasks, runs, events, handoffs, approvals,
  notifications.
- `person_id` = the same human; `account_id` = a platform binding
  (`cli/local`, `wechat/openid`, …). Channel UX may differ, but a CLI run, a
  Weixin message, and a web request must create/resume the same
  person/task/run/approval lifecycle. See `docs/identity-continuity.md`.
- File, terminal, patch, and process tools must run inside the active
  workspace scope (`WorkspaceScopeMiddleware`); extend it when adding tools
  that touch files, processes, or paths.
- No cross-tenant or cross-test global mutable state. Process-wide objects
  need clear lifecycle ownership, injected by `internal/app` or the gateway
  runner. `kernel` must not depend on `gateway` or concrete tools.
- Linux server is the official release target (amd64/arm64 + systemd assets);
  Windows/macOS are best-effort until hardened.

## Gateway, Identity & Concurrency

- Multi-terminal concurrency is solved by **daemon-client convergence**, not
  cross-process locks: every terminal converges on ONE gateway daemon, so the
  worker pool, the process-global auth manager, per-workspace serialization,
  and the single `control.db` owner apply across terminals. Never add
  cross-process business locks (auth-file locks, DB write locks, workspace
  file locks); the `gateway.lock` flock (daemon single-instance) is the only
  legitimate cross-process lock.
- Clients reach the daemon via `gateway.EnsureRunning` (discover-or-autostart,
  race-safe) and the `internal/gateway/client` daemon-backed
  `MessageProcessor`. The rich TUI is a thin client by **default**;
  `SELFMIND_TUI_INPROC=1` opts back into in-process, and the client path falls
  back automatically if the daemon can't start. Agent-backed slash commands go
  through the safelisted `/v1/dispatch`, which refuses workspace-mutating and
  code-exec tools — those require a real agent turn. Details:
  `docs/worker-pool-design.md` §8.
- Platform adapters only parse/authenticate/send platform payloads. The
  gateway owns identity binding, workspace lookup, task/run state, and agent
  dispatch. Approval state lives in `control.approval_requests` and gateway
  handlers; IM adapters may render buttons or parse callbacks but never own
  approval lifecycle. User approval references (list ordinal, unique `apr_`
  prefix, bare token with one pending) resolve only through the shared
  resolver in `internal/gateway/httpapi/approval_resolver.go` — clients and
  adapters pass the raw token to the gateway, never resolve ordinals locally,
  so `/approve 1` means the same approval on every surface.
- Gateway control commands (`/status`, `/stop`, `/tasks`, `/workspaces`,
  `/resume`, `/workspace`) stay pre-agent and must not consume model tokens.
- Keep the per-person active-run guard until the worker pool fully replaces
  it; the shared Agent object is not safe to run freely in parallel.
- Run events use a per-run sink installed with
  `kernel.WithEventChannel(ctx, ch)`. Never swap the shared
  `Agent.EventChannel` in gateway code (legacy local-TUI fallback only).
- User-visible task state derives from structured run outcomes
  (`api.RunOutcome`) and handoffs, not ad hoc status text. Clients decide
  accepted/busy/completed/failed from `MessageResponse.turn` /
  `MessageResponse.context`, not by parsing prose.
- CLI and IM have different feedback contracts: CLI/TUI streams text and tool
  progress; IM sends concise working notices, key tool/approval milestones,
  and a final answer or handoff — never token-by-token streams.
- Long-running work must never look stalled: emit structured progress events
  (`agent.thinking`, `tool.started`/`tool.output`/`tool.completed`,
  `turn.completed`). A timeout, failed tool, or model error leads to
  diagnosis, retry, or a clear handoff — not silent abandonment.

## Context & Memory

- Durable context flows through the selector contract only:
  `control.db -> gateway/httpapi context selector -> kernel.TaskRuntimeContext
  -> kernel.WithTaskRuntimeContext -> Agent.buildSystemPrompt`. Never inject
  raw control rows, event JSON, artifact metadata, or full tool output into
  prompts or channel messages.
- `TaskRuntimeContext` is selected background context, not a user message; it
  must stay bounded and explainable. Per-turn context is assembled as
  `kernel.RuntimeContextBundle` (workspace, task, selected memory, notes,
  budgets) before prompt rendering — extend the bundle or its selector, never
  append prompt fragments in unrelated handlers.
- `internal/kernel/context_engine.go` stays on the streaming hot path: bounded
  recent-history slice, no synchronous LLM summarization by default
  (diagnostic-only compaction gates behind `SELFMIND_SYNC_CONTEXT_SUMMARY`).
- Memory facts, session FTS recall, task handoffs, task events, and artifacts
  are separate durable sources. Ranking/embedding work extends the selector
  layer — never another append path in `agent.go`, gateway handlers, or IM
  adapters.

## Agent-First Routing & Task Strategy

- SelfMind is strict agent-first. Except for explicit slash commands (`/help`,
  `/model`, `/status`, `/tasks`, `/events`, `/approvals`, `/approve`,
  `/reject`, `/stop`, `/new`, `/resume`, `/workspace`, `/workspaces`),
  natural-language input from any channel must reach the agent. Never add
  pre-agent direct-answer routers for greetings, identity/model questions,
  "simple" snippets, explanations, or advice. Program-level model status
  belongs behind `/model`; "who are you / what model are you" questions go to
  the agent.
- Short-acceptance continuation is part of the contract: a short acceptance (`ok`,
  `继续`, `可以`, …) after a proposed plan continues the same task context; if
  nothing is resumable, it routes to the agent as normal input. The resolution
  algorithm is in `docs/identity-continuity.md`.
- Tool exposure is a guardrail, not a classifier. Per-turn policy is
  centralized in `internal/kernel/task_strategy.go`; it may hide
  `web_search`/`web_extract` unless the user explicitly asks for external
  information, and may disable tools only for pure direct-answer turns. It
  must never decide input is "just chat" and bypass the agent.
- `TaskStrategy` stays coarse (channel mode, plan policy, web policy, safety
  limits) — no keyword taxonomies. The gateway builds it from the ORIGINAL
  user content before adding daemon/workspace/attachment/resume context, then
  passes it via `kernel.WithTaskStrategy`.
- Intent routing is centralized in `internal/gateway/router/intent*.go`.
  `intent.rules` cover only explicit routing/skill/search commands and
  high-confidence resume cues; no intent mode may reroute ordinary messages
  into a direct/casual path.
- `update_plan` is optional and model-decided unless a strategy requires it.
  For ambiguous artifact-producing CLI requests, prefer a cheap read-only
  workspace probe before choosing inline answer vs. file vs. clarification;
  on IM, ask the user to bind/select a workspace before writing.

## Models & Providers

> **Mandatory:** read `docs/provider-runtime.md` before touching providers,
> auth, adapters, or model routing. It holds the full quirk tables, the
> Kimi/MiniMax coding-plan specifics (endpoints, ALPN/HTTP-1.1 transport,
> schema repair), and the Codex Responses contract.

- Provider integration follows the Provider Runtime boundary:
  `ProviderProfile` (declarative vendor) → `Resolver` → `Runtime` →
  `llm.TransportConfig` → transport registry
  (`internal/kernel/llm/transport.go`). `internal/app` translates Runtime to
  TransportConfig but never picks concrete adapters via provider-name
  switches. Discovery, credential resolution, model-list fetching, and
  profile overrides live in `internal/modelruntime` only.
- Prefer existing protocol adapters (`openai_chat`, `openai_compatible`,
  `anthropic_messages`, `codex_responses`); add a Go adapter only for a
  genuinely different wire protocol. Provider-specific behavior goes in
  `ProviderQuirks` first — never scattered provider-name checks in CLI,
  gateway, IM, or app setup.
- Hard invariants for stateless Responses providers (full rules in
  provider-runtime.md): serialize `store=false` + stream-only when the quirks
  demand it; replay tool turns as `function_call` then matching
  `function_call_output` in the same input; map internal tool names to
  provider-safe `^[a-zA-Z0-9_-]+$` names at the adapter boundary and back.
- External auth reuse is limited to Codex CLI, Claude Code, Gemini CLI, Qwen
  CLI, and SelfMind-owned OAuth (e.g. MiniMax). Credential parsing/refresh
  lives in `internal/modelruntime`; adapters call `Runtime.TokenGetter` per
  request and may use `Runtime.TokenRefresher` + one replay on auth failure.
  Surface stale-login errors as actionable text, never raw provider JSON.
  Other vendors use API keys or `provider_profiles`.
- Model choice goes through role-based routing; keep the role names
  `coding_agent`, `memory_extract`, `background_review`, `skill_curator`,
  `semantic_recall` stable. Role overrides must pass the same runtime fields
  as the default provider (headers, max tokens, reasoning effort, thinking,
  service tier, quirks) and resolve through the same `modelruntime.Resolver`.
- Keep `context_length` (total window; drives usage display and budgets) and
  `max_tokens` (per-response output cap) separate. Never display `max_tokens`
  as the context size or hardcode fake windows like `1M`.

## Tools & Safety

- Tool calling stays native-first: native LLM `tool_calls` where supported,
  `tool_call_id` preserved on results, `[TOOL:...]` only as a compatibility
  fallback.
- Only clearly read-only tool batches run in parallel. Terminal, file writes,
  patches, memory/skill mutation, process control, delegation, and unknown
  tools run sequentially unless a dedicated safety policy says otherwise.
- Tool results go through the Agent result envelope before reaching the
  model, TUI, or run events. Raw output, model-bounded content (head/tail
  view with an explicit too-large note), and compact UTF-8-safe user preview
  are three separate surfaces — never one truncated string for all three.
- CLI/TUI output must be human-readable: summarize plans, tool calls, and
  errors; never dump raw JSON unless the user asked for protocol details.
  Failure events carry compact diagnostics (`error_category`,
  `diagnostic_hint`, duration, preview, truncation markers).
- Tool failures are diagnostic evidence, not stop conditions. Diagnose before
  retrying: identify the ecosystem from high-signal files (`go.mod`,
  `package.json`, `pyproject.toml`, `Cargo.toml`, `composer.json`, CI files,
  …), inspect the matching runtime/package-manager state (e.g. a Go
  workspace error → check `go env GOWORK`/`GOMOD`, `go.work`, `go.mod`), and
  only then change the command. Never bake project-specific env overrides
  into generic tools.
- When changing core runtime code, add concise intent/boundary comments for
  exported types, non-obvious control flow, and invariants — why the boundary
  exists, not what the line does. No mojibake or hidden-encoding comments.

## Skills & Learning

> **Mandatory:** read `docs/skills-architecture.md` before changing skill
> discovery, invocation, mutation, catalog, or curator behavior. It holds the
> root-precedence, read-only-boundary, context-budget, catalog-provenance,
> and governance contracts.

- Keep skill handling layered: `skills_list` (metadata) / `skill_view` (read)
  / `skill_manage` (mutate + hot reload) / `skill_catalog` (install/audit) /
  `skill_bundle` (grouped workflows). Slash invocation resolves bundles
  before individual skills.
- Skills are instruction assets, not auto-executed shell tools; scripts a
  skill mentions still run through normal tools and safety checks.
- Catalog installs keep durable install provenance (`.catalog/lock.json`);
  `--force` reinstalls must back up the previous copy first.
- Memory and skill mutations write tenant learning-audit records; user-facing
  history/undo goes through the shared `memory` / `skill_manage` actions —
  no channel-specific history files or private rollback paths.

## CLI / TUI

- Do not keep growing `internal/gateway/cli/controller.go`; it orchestrates
  state, routes Bubble Tea messages, and connects components. Reusable
  transcript/composer/pager/modal/command behavior belongs in
  `internal/ui/components` or a dedicated gateway/cli module.
- New transient pages (help, detail, status, task, model, search) use
  `internal/ui/components/Pager` or another reusable surface — no one-off
  viewport logic in the controller.
- Slash command metadata lives in the shared registry
  (`internal/gateway/cli/slash_commands.go`) used by dispatch, `/help`, and
  editor hints; never duplicate name/description/usage across files.
- Rendering and file-size guardrails: `docs/architecture-constraints.md`
  (mandatory) and `docs/tui-terminal-first-hybrid.md`.

## Eval Loop Contract

> Details and case format: `docs/eval-loop.md`.

- When a bug report becomes repeatable, add/update an `evalcases/**/*.yaml`
  case. `selfmind eval run` must go through the same gateway/agent path as
  real input — no eval-only shortcuts around identity, workspace, strategy,
  tools, adapters, or context selection.
- JSONL traces under `evalruns/` are local diagnostics, never committed; by
  default traces hash user/model text (`--record-content` is local-only).
- Eval runs are data-isolated by default: every case (record and replay) gets
  a throwaway temp data dir, and the runner finalizes any run left `running`.
  Never make eval write to the configured `control.db` again (`shared_data:
  true` is the explicit per-case opt-out); `selfmind eval clean` removes
  historic residue. Cassettes under `.vcr/` are keyed by case ID and are
  independent of the data dir.
- P0 checks stay deterministic (non-empty output, no mojibake, no raw
  provider JSON, tool-event visibility, bounded duration, …). Later scoring
  layers must not weaken them.

## Key Files

- `cmd/selfmind/main.go`: thin entrypoint → `internal/cliapp` (CLI layer,
  gateway lifecycle/client commands, TUI bootstrap).
- `internal/gateway/httpapi/server.go`: HTTP API + IM webhook flow; handlers
  in `handlers_*.go`; run lifecycle + active-run registry in
  `run_coordinator.go`.
- `internal/gateway/httpapi/context_selector.go`: bounded task
  handoff/event/artifact/workspace selection per turn.
- `internal/gateway/httpapi/continue_resolver.go`: cross-channel continue/
  resume resolution (see `docs/identity-continuity.md`).
- `internal/gateway/httpapi/outcome.go`: structured run outcome extraction.
- `internal/control/store.go`: control-plane SQLite schema.
- `internal/gateway/router/intent*.go`: centralized intent routing.
- `internal/kernel/task_strategy.go`: per-turn tool/plan/web/progress policy.
- `internal/kernel/context_engine.go`: bounded message window (hot path).
- `internal/kernel/task_runtime_context.go`: `TaskRuntimeContext` +
  `RuntimeContextBundle` prompt contract.
- `internal/kernel/event_context.go`: per-run event sink injection.
- `internal/kernel/native_tool_call.go`: native/fallback tool calls, result
  envelope, parallel-execution policy.
- `internal/kernel/llm/`: transports (`transport.go`, `adapters.go`,
  `anthropic_adapter.go`, `responses_adapter.go`), role routing
  (`model_gateway.go`), VCR (`vcr.go`), flight recorder (`flight.go`).
- `internal/modelruntime/`: provider profiles, credential resolution,
  external CLI auth reuse, model catalog/cache, auth manager.
- `internal/tools/`: workspace scope (`workspace_scope.go`), skills
  (`skill_service.go`, `skill_runtime.go`, `skill_manage.go`,
  `skill_catalog.go`, `skill_bundles.go`, `skill_curator.go`), memory,
  session search, learning audit (`learning_audit.go`).
- `internal/gateway/cli/`: TUI (`controller.go`, `transcript_renderer.go`,
  `slash_commands.go`, `command_handlers.go`).
- `internal/eval/` + `evalcases/`: offline eval loop and repo-owned cases.

## Local Test Command

In this workspace, run tests with:

```powershell
$env:PATH="$env:USERPROFILE\.cache\selfmind-tools\go1.26.3\go\bin;" + $env:PATH
$env:GOWORK='off'
go test ./...
```

`GOWORK=off` avoids the parent workspace file excluding this module. This is a
repository-local development command, not a generic terminal-tool default;
runtime tools should return real command output and let the agent diagnose
workspace/module errors before choosing any env override.

From WSL in this checkout:

```sh
cd /mnt/d/wwwroot/ai/selfmind
GOWORK=off /usr/local/go/bin/go test ./...
```

For provider-runtime changes, always run at least:

```sh
GOWORK=off /usr/local/go/bin/go test ./internal/modelruntime ./internal/kernel/llm ./internal/app
```

## WSL Build And Smoke Test

When the user expects to try the result in WSL, build the Linux amd64 binary
and install it into the user's local path:

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

- `AGENTS.md` is the single source of truth for AI/coding-agent rules. Other
  tool entry files (`CLAUDE.md`, `GEMINI.md`, `QWEN.md`) only forward here —
  never duplicate rules; keep them tracked in git. This file is injected into
  every session: keep rules here short and grouped, push details into domain
  docs with a **mandatory** pointer, and never remove an invariant just to
  save lines — comprehension outranks token savings.
- `docs/identity-continuity.md` is the canonical direction doc (north star,
  identity model, continuity contract). Update it only when the product goal
  or the continuity mechanism itself changes, not for per-feature status.
- `docs/STATUS.md` is the implementation snapshot and the **only** live
  priority list. When a change moves a capability between
  Missing/Partial/Done, update the row in the same PR. No other doc may
  declare its own P0 list; point at `docs/STATUS.md` instead.
- Historical planning docs (`selfmind-evolution-roadmap.md`,
  `selfmind-evolution-design.md`, `p0-p1-development-plan.zh-CN.md`,
  `daemon-im-saas-architecture.md`) were deliberately removed from the tree on
  2026-07-03 so fresh agent analysis never absorbs stale narratives; their
  useful content lives in `docs/identity-continuity.md`. Retrieve originals
  via git history only for archaeology — never resurrect their backlog items
  or code samples, and do not re-add historical planning docs to the tree.
- Update `docs/development-guide*.md` for broader engineering explanations;
  `docs/provider-runtime*.md` for provider rules, quirks, Kimi/MiniMax/OAuth
  behavior, and vendor checklists; `docs/skills-architecture.md` for the
  skill contract.
- Keep user-facing README changes separate from internal development rules.
- For bilingual docs (`*.md` + `*.zh-CN.md`), the English file is canonical
  for rules; keep the Chinese version in sync or mark it as a translation.
