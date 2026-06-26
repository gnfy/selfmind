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
- `docs/context-lifecycle.zh-CN.md`

## Handoff Runbook

Use this checklist whenever a future AI/coding agent picks up work in this
repo:

1. Read this file, then read `docs/STATUS.md` for the current implementation
   snapshot before assuming any feature is missing — several items the planning
   docs call "to do" are already done. Then read the specific docs linked above
   for the area being changed. For provider/model work,
   `docs/provider-runtime.md` is mandatory.
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
- Durable context must flow through the selector contract:
  `control.db -> gateway/httpapi context selector -> kernel.TaskRuntimeContext
  -> kernel.WithTaskRuntimeContext -> Agent.buildSystemPrompt`. Do not inject raw
  control rows, event JSON, artifact metadata, or full tool output directly into
  prompts or channel messages.
- Treat `TaskRuntimeContext` as selected background context, not as a user
  message. It can contain task state, handoffs, events, artifacts, and later
  selected memory/session snippets, but it must remain bounded and explainable.
- Per-turn runtime context should now be assembled as
  `kernel.RuntimeContextBundle` before prompt rendering. The bundle is the
  P0/P1 contract for workspace, task, selected memory, selection notes, and
  token/character budgets. Extend this bundle or the selector that feeds it
  rather than appending new prompt fragments in unrelated handlers.
- `internal/kernel/context_engine.go` must stay on the streaming hot path. It
  should only load a bounded slice of recent channel history and should not make
  synchronous LLM summarization calls by default. If synchronous compaction is
  ever required for diagnostics, gate it behind `SELFMIND_SYNC_CONTEXT_SUMMARY`
  and keep the default path deterministic.
- Existing memory facts, session FTS recall, task handoffs, task events, and
  artifacts are separate durable context sources. Future ranking/embedding work
  should extend the selector layer instead of adding another prompt append path
  in `agent.go`, gateway handlers, or IM adapters.
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
- The runtime-to-LLM boundary is now `llm.TransportConfig` plus the transport
  registry in `internal/kernel/llm/transport.go`. `internal/app` may translate a
  resolved `modelruntime.Runtime` into `TransportConfig`, but it must not choose
  concrete OpenAI/Anthropic/Responses adapters with provider-name switches.
- Prefer existing protocol adapters (`openai_chat`, `openai_compatible`,
  `anthropic_messages`, `codex_responses`) when adding model vendors. Add a new
  Go adapter only when the wire protocol is genuinely different.
- Put provider-specific behavior in `ProviderQuirks` first: auth header,
  tool-schema repair, system-message mode, thinking behavior, User-Agent, and
  protocol-specific request flags such as Codex Responses `store=false` and
  stream-only requests. Do not scatter provider-name checks across CLI,
  gateway, IM adapters, or app setup.
- User YAML `provider_profiles.*.quirks` currently supports
  `auth_header`, `tool_schema`, `system_message_mode`, `thinking_mode`, and
  `user_agent`, plus `responses_store_false` and `responses_require_stream`
  for Responses-compatible endpoints that require stateless/stream-only
  requests. Capability flags such as
  tool/streaming/vision support belong in built-in Go profiles.
- Responses-compatible stateless providers must replay native tool turns as
  `function_call` input items followed by matching `function_call_output`
  items. Do not send a tool output with only a `call_id`; Codex will reject it
  because `store=false` means the server cannot look up prior response items.
- Responses-compatible providers require wire tool names matching
  `^[a-zA-Z0-9_-]+$`. Keep SelfMind's internal tool names unchanged, but map
  them to provider-safe names at the adapter boundary and map returned tool
  calls back to the original names before dispatch. This is especially
  important for skill, MCP, or legacy tools whose internal names may contain
  `:`, `.`, `/`, or spaces.
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
  CLI, Qwen CLI, and SelfMind-owned OAuth providers such as MiniMax OAuth. Keep
  external credential parsing and refresh in `internal/modelruntime`, and make
  adapters consume `Runtime.TokenGetter` before each request instead of caching
  an initial token. If the server returns an auth failure such as
  `token_expired`, adapters may call `Runtime.TokenRefresher` and replay the
  same request once. `codex-cli` must refresh expired `~/.codex/auth.json`
  ChatGPT OAuth access tokens, send Codex Responses requests with
  `store=false`, and surface stale-login/provider-contract failures as
  actionable text, not raw provider JSON. Other model vendors should use API
  keys, custom OpenAI-compatible endpoints, or `provider_profiles`.
- Tool calling should stay Hermes-like: pass tool schemas as native LLM
  `tool_calls` where the provider supports it, preserve `tool_call_id` on
  tool result messages, and keep `[TOOL:...]` only as a compatibility fallback.
- SelfMind is strict agent-first. Except for explicit internal slash commands
  such as `/help`, `/model`, `/status`, `/tasks`, `/events`, `/approvals`,
  `/approve`, `/reject`, `/stop`, `/new`, `/resume`, `/workspace`, and
  `/workspaces`, ordinary natural-language input from CLI, IM, HTTP, or future
  clients must reach the agent. Do not add pre-agent direct-answer routers for
  greetings, identity/model questions, "simple" code snippets, explanations, or
  advice requests.
- Codex-style continuation is part of the contract. If the assistant proposed a
  plan and the user replies with a short acceptance or continuation such as
  `ok`, `yes`, `go ahead`, `continue`, `可以`, `好的`, or `继续`, keep the
  message in the same task/conversation context and let the agent continue the
  previously proposed work. If there is no resumable task, route the message to
  the agent as normal input instead of dropping it into a hardcoded reply.
- Tool exposure is a guardrail, not a classifier. Per-turn policy is
  centralized in `internal/kernel/task_strategy.go`; it may hide externally
  scoped tools such as `web_search`/`web_extract` unless the user explicitly
  asks to search, browse, inspect a URL, check official docs, or retrieve
  current/latest external information. It must not decide that natural language
  is "just chat" and bypass the agent.
- `TaskStrategy` should stay coarse: channel mode, plan policy, web policy, and
  safety limits. Avoid keyword-heavy task taxonomies. If a new product-level
  policy is required, add it here as an agent guardrail and cover it with tests
  before changing gateway behavior.
- P0/P1 task strategy is still agent-first. It may disable tools only for
  pure direct-answer turns that do not need workspace, external, or tool state,
  such as short identity/model questions. Coding examples, explanations,
  analysis requests, and artifact requests should reach the agent with coarse
  guidance rather than a pre-agent answer.
- Gateway `MessageResponse.turn` and `MessageResponse.context` are the
  structured status contract for CLI, IM, HTTP, and eval clients. Prefer these
  fields over parsing human text when deciding whether a turn is accepted,
  busy, completed, failed, or still running.
- Tool events should include compact diagnostic metadata when failures happen:
  `error_category`, `diagnostic_hint`, duration, preview, and truncation
  markers. This lets CLI/IM show Codex-style progress and lets evals detect
  regressions without dumping raw provider or tool JSON.
- Gateway intent routing is centralized in `internal/gateway/router/intent*.go`
  and returns `router.IntentResult`. `intent.rules` are only for explicit
  routing commands, skill/query/search commands, and high-confidence resume
  cues. `intent.mode: rules|hybrid|llm` must not let a lightweight intent model
  override ordinary user messages into a direct/casual path.
- Program-level model status belongs behind `/model`. Natural-language
  questions such as "who are you", "what model are you", or their non-English
  equivalents must go through the agent, which can answer from active runtime
  context.
- The gateway must build `TaskStrategy` from the original user content before
  adding daemon, workspace, attachment, task, or resume context, then pass it to
  the agent with `kernel.WithTaskStrategy`. This prevents resume/workspace
  metadata from becoming hidden routing keywords.
- `update_plan` is optional and model-decided unless a specific strategy marks
  it required. Do not runtime-ban planning for snippets or explanations; instead
  prompt the model to use plans only when the work genuinely needs multiple
  visible steps. Repo inspection, debugging, CI/CD, multi-file edits, runnable
  artifact creation, and long verification tasks should expose local tools and
  visible progress.
- For ambiguous CLI requests that may produce a file or runnable artifact
  (`write a JS game`, `make a small tool`, `create a page`), prefer a cheap
  read-only workspace probe before deciding whether to answer inline, create a
  standalone artifact, or ask a clarification. For IM channels, ask the user to
  bind/select a workspace before writing or running commands when no active
  workspace is clear.
- Long-running agent work must emit structured progress events. Use
  `agent.thinking` for model-decision phases, `tool.started`/`tool.output`/
  `tool.completed` for tool execution, and `turn.completed` for final state.
  CLI/TUI should show these steps live; IM channels should keep token streams
  collapsed but preserve working notices, event records, and failure summaries.
- Tool failures are diagnostic evidence, not automatic stop conditions. Agents
  should inspect cwd, repository/module boundaries, environment, auth state,
  provider protocol constraints, or command help before retrying with a changed
  command. Do not bake project-specific env overrides into generic tools.
- Environment diagnosis must be language-agnostic. First identify the ecosystem
  from high-signal files such as `go.mod`, `package.json`, `pyproject.toml`,
  `requirements.txt`, `Cargo.toml`, `composer.json`, `pom.xml`, `build.gradle`,
  `.csproj`, `Gemfile`, `Makefile`, `Dockerfile`, and CI workflow files. Then
  inspect the matching runtime/package-manager state and choose the next
  command from evidence. For example, a Go workspace/module error should lead
  the agent to inspect `go env GOWORK`, `go env GOMOD`, `go.work`, and
  `go.mod`; a Node project should inspect package scripts and lockfiles; a
  Python project should inspect `pyproject.toml`, virtualenv and test runner
  hints; a PHP project should inspect `composer.json`; Rust should inspect
  `Cargo.toml`. Use explicit cwd or env overrides only when that diagnosis
  supports it.
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
- Skill discovery must flow through the shared skill service. Search visible
  roots in priority order: workspace `.selfmind/skills`, workspace
  `.agents/skills` for Codex compatibility, workspace `skills/`, optional
  environment roots, then the user root `~/.selfmind/<tenant>/skills`.
  Workspace roots may be read-only; mutation tools must refuse writes to
  read-only roots with a clear error instead of silently copying or replacing.
- Skills are instruction assets, not auto-executed shell tools. Dynamic
  `skill:<name>` registrations are compatibility shims only; slash invocation
  and `skill_view` are the supported ways to load a skill into model context.
  Scripts or commands mentioned by a skill must still be run explicitly through
  normal tools and safety checks.
- Enable/disable is tenant-scoped lifecycle metadata. Disabling a skill must
  not mutate read-only workspace or external roots; it should make slash and
  bundle invocation skip the skill until it is enabled again.
- Skill injection must stay bounded and UTF-8 safe. Metadata should be compact,
  full `SKILL.md` content is loaded only on explicit invocation/view, and large
  skill bodies must be truncated with an explicit note rather than corrupting
  output or exhausting the turn context.
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
- `internal/gateway/httpapi/context_selector.go`: selects bounded task
  handoff/event/artifact/workspace slices from `control.db` for one model turn.
  Extend this when adding long-term context sources.
- `internal/control/store.go`: control-plane SQLite schema and persistence.
- `internal/tools/workspace_scope.go`: workspace execution boundary.
- `internal/gateway/cli/transcript_renderer.go`: TUI transcript, startup card,
  and tool-message rendering.
- `internal/gateway/cli/slash_commands.go`: slash command metadata and
  dispatcher registry shared by help/editor/dispatch.
- `internal/kernel/event_context.go`: per-run agent event sink injection.
- `internal/kernel/context_engine.go`: bounded message-window construction for
  model calls. Keep it cheap and deterministic on the default hot path.
- `internal/kernel/task_runtime_context.go`: kernel-owned prompt contract for
  selected durable task context and `RuntimeContextBundle`. Keep `kernel`
  independent of `control.Store`.
- `internal/kernel/task_strategy.go`: per-turn task classification and tool,
  plan, web, and progress policy shared by CLI/IM/web entrypoints.
- `internal/gateway/httpapi/outcome.go`: structured run outcome extraction for
  task status, handoff, and IM/CLI status cards.
- `internal/gateway/cli/controller.go`: Bubble Tea state orchestration. Keep
  new visual behavior in smaller renderer/component files when possible.
- `internal/kernel/llm/model_gateway.go`: role-based model routing.
- `docs/provider-runtime.md` and `docs/provider-runtime.zh-CN.md`: mandatory
  provider integration rules and checklist for future model vendors.
- `docs/skills-architecture.md`: mandatory skill root, scope, invocation,
  context-budget, and governance contract.
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
- `internal/tools/skill_service.go`: shared skill root discovery, scope,
  precedence, and writable/read-only boundary helpers.
- `internal/tools/skill_bundles.go`: YAML skill bundles for grouped workflow
  invocation.
- `internal/tools/skill_catalog.go`: official/local/URL/GitHub skill install
  and audit.
- `internal/tools/skill_fuzzy_patch.go`: tolerant skill content patching.
- `internal/tools/skill_curator.go`: skill lifecycle governance, dry-run,
  reports, archive, and restore.
- `internal/tools/learning_audit.go`: tenant learning history for memory/skill
  mutations, including history and supported undo helpers.
- `internal/eval/`: offline evaluation loop for replaying representative
  prompts through the real gateway/agent path, recording JSONL traces, and
  checking UX/runtime regressions.
- `evalcases/`: repository-owned eval case suites. Keep cases small,
  representative, and safe to run against configured providers.
- `docs/eval-loop.md`: how to add cases, run evals, interpret reports, and
  extend checks.

## Eval Loop Contract

Use the eval loop when changing model providers, tool calling, task strategy,
context selection, skills, CLI/TUI rendering, IM feedback, or any behavior that
previously regressed in user testing.

- Add or update an `evalcases/**/*.yaml` case whenever a bug report becomes a
  repeatable scenario. Good cases cover identity/model answers, simple code
  snippets, continuation (`可以`/`继续`), codebase inspection, provider errors,
  tool progress, and mojibake prevention.
- `selfmind eval run <case-or-dir>` must go through the same gateway and agent
  path as real CLI/IM input. Do not add eval-only shortcuts that bypass
  identity binding, workspace selection, task strategy, tools, provider
  adapters, event recording, or context selection.
- JSONL traces under `evalruns/` are local diagnostics and must not be
  committed. By default, eval traces should hash user/model text and store only
  previews/metrics. Use `--record-content` only for local debugging with
  non-sensitive prompts.
- P0 checks are deterministic heuristics: non-empty output, no mojibake, no raw
  provider JSON, no leaked XML tool tags, no provider stack dump, expected
  workspace, continuation behavior, tool event visibility, bounded duration,
  context overflow detection, max/min tool call counts, tool error counts, and
  basic first-token/tool metrics in reports.
- Future P1/P2 work should add provider matrices, synthetic fixtures,
  model-as-judge scoring, artifact verification, screenshot/terminal
  interaction checks, and trend reports without weakening the P0 deterministic
  checks.

## Local Test Command

In this workspace, run tests with:

```powershell
$env:PATH="$env:USERPROFILE\.cache\selfmind-tools\go1.26.3\go\bin;" + $env:PATH
$env:GOWORK='off'
go test ./...
```

`GOWORK=off` avoids the parent workspace file excluding this module.
This is a repository-local development command, not a generic terminal-tool
default. Runtime tools should return the real command output and let the agent
diagnose workspace/module errors before choosing any env override.

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

- `AGENTS.md` is the single source of truth for AI/coding-agent rules in this
  repo. Other tool entry files must only forward to it, never duplicate rules:
  `CLAUDE.md`, `GEMINI.md`, and `QWEN.md` each just point at `AGENTS.md`. When a
  new AI tool is onboarded, add a thin forwarder for it rather than copying
  rules. Keep `CLAUDE.md`/`GEMINI.md`/`QWEN.md` tracked in git so every clone and
  CI run sees the same rules.
- `docs/STATUS.md` is the current implementation snapshot. When a change moves a
  capability between Missing/Partial/Done, update the matching row in the same
  PR. Do not add per-feature status notes to the historical roadmap docs; record
  state in `docs/STATUS.md` instead.
- The planning docs (`docs/selfmind-evolution-roadmap.md`,
  `docs/selfmind-evolution-design.md`, `docs/p0-p1-development-plan.zh-CN.md`)
  are historical intent and are partially superseded. They each carry a status
  banner pointing to `docs/STATUS.md`; do not treat their "to do" lists as the
  current backlog.
- Update `AGENTS.md` when a new invariant, workflow, or "future AI must know"
  rule is introduced.
- Update `docs/development-guide*.md` for broader engineering explanations.
- Update `docs/provider-runtime*.md` for model provider rules, provider quirks,
  Kimi/MiniMax behavior, OAuth behavior, or new model-vendor checklists.
- Keep user-facing README changes separate from internal development rules.
- For bilingual docs (`*.md` + `*.zh-CN.md`), the English file is canonical for
  rules; keep the Chinese version in sync or mark it as a translation.
