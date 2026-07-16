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
- `docs/work-timeline.md` (approved target for task/context semantics — the
  person-level work spine; mandatory before touching task attach, working
  history, context assembly, recall, or `/tasks`)
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
  `MessageProcessor`. **DAEMON-ONLY (2026-07-10, ACTIVE PLAN P0-3): the TUI is
  ALWAYS a thin client — there is NO in-process agent path and NO fallback.**
  Every entrance (CLI, IM, cron, HTTP) executes inside the one daemon, so the
  worker pool, auth manager, per-workspace serialization, and control.db owner
  apply across terminals, and person-partitioned state (memory/session/
  checkpoints) is never split across a divergent local partition. If the daemon
  cannot be reached or started, the client FAILS with an actionable message
  (gateway status / `gateway run` / doctor) — it must never silently run a local
  agent. Do NOT reintroduce `SELFMIND_TUI_INPROC`, an in-process gateway build
  in the TUI path, or a "fall back to in-process" branch. Agent-backed slash
  commands go through the safelisted `/v1/dispatch`, which refuses
  workspace-mutating and code-exec tools — those require a real agent turn.
  Details: `docs/worker-pool-design.md` §8.
- Platform adapters only parse/authenticate/send platform payloads. The
  gateway owns identity binding, workspace lookup, task/run state, and agent
  dispatch. Approval state lives in `control.approval_requests` and gateway
  handlers; IM adapters may render buttons or parse callbacks but never own
  approval lifecycle. User approval references (list ordinal, unique `apr_`
  prefix, bare token with one pending) resolve only through the shared
  resolver in `internal/gateway/httpapi/approval_resolver.go` — clients and
  adapters pass the raw token to the gateway, never resolve ordinals locally,
  so `/approve 1` means the same approval on every surface. The same
  display-order-equals-resolution-order contract covers tasks and workspaces:
  `/task <n>`/`/resume <n>` resolve through `resolveTaskReference`
  (`httpapi/task_view.go`, /tasks open-card order) and `/workspace <n>` through
  `resolveWorkspaceReference` (`httpapi/workspace_resolver.go`, /workspaces
  order) — any numbered list a command accepts a number for must share ONE
  sorted fetch with its renderer.
- Gateway control commands (`/status`, `/stop`, `/tasks`, `/workspaces`,
  `/resume`, `/workspace`) stay pre-agent and must not consume model tokens.
- Keep the per-person active-run guard until the worker pool fully replaces
  it; the shared Agent object is not safe to run freely in parallel.
- New work queues per person; continuation never queues. When a run is active,
  a genuinely new message is enqueued in `control.task_queue` and auto-started
  when the run finishes (`RunCoordinator.drainQueue`, per-person re-entrancy
  guard, boot drain via `Server.DrainQueuedAtBoot`) — never rejected as "busy".
  An `IntentContinue` message is NOT new work: it STEERS the active run — it is
  injected into that run's steering channel on EVERY entry (`/v1/message` as
  well as the thin-client `/v1/runs/steer`), via the shared
  `Server.steerActiveRun`, so a continuation from any surface (CLI, IM, web)
  reaches the running task; a full/absent steering channel falls back to the
  honest busy reply, never a silent drop. A continuation must never be queued.
  A drained item becomes an ordinary async run, so the worker pool still
  schedules it — do not add a second serialization layer.
- Task = work LABEL, and `resolveTask` is a harmless PRE-LABEL guess (Work
  Timeline P3, landed 2026-07-06 — `docs/work-timeline.md` is mandatory
  reading before touching this area). Explicit evidence stays deterministic:
  caller `task_id`, `IntentContinue` (router cue or short acceptance), or the
  one-shot `/resume` pin (which alone may reopen an ARCHIVED label). Every
  other agent-bound message — sync, async, queued-drain, cron — pre-labels
  onto the person's current OPEN (non-terminal, non-archived) label, else a
  fresh placeholder. The guess is safe because labels never gate context
  (spine P1 + recall P2) and the EXECUTION workspace follows the REQUEST for
  pre-label turns (`workspaceForTask`); a wrong guess is display-only. After
  finalization the async `PostRunAnalyzer` performs at most one explicitly
  role-routed maintenance call (`tasks.maintenance_model_role`, default
  `memory_extract`; nil = keep labels and skip automatic extraction). The same
  JSON result contains durable user/workspace facts plus KEEP /
  MOVE:<task_id> / TITLE:<title> / INBOX. MOVE re-points the run + its
  events/artifacts transactionally (`Store.ReassignRun`, deleting an
  auto-created placeholder left with zero runs), TITLE names a NEW placeholder
  exactly once (established labels rename only via `/task <id> rename`), and
  every non-KEEP decision writes a `label.assigned` event. All failure paths
  preserve the completed run. Do NOT add a second turn/final fact extractor,
  profile synthesis call, or ingress task-routing/disambiguation layer
  or a pre-agent continue-vs-new LLM call — both were evaluated and rejected
  (the implicit-continuation upgrade was removed in P3).
- Task governance is post-run and reversible, never an ingress decision.
- Do not call post-run maintenance on every run. It is eligible for a new
  placeholder, real cross-label ambiguity, or a substantive durable outcome.
  Explicit attachment is never relabeled; trivial established-label turns
  with no durable facts skip the model call.
  `INBOX` moves a casual/diagnostic run to one hidden, archived inbox label per
  person/workspace; the run/events remain auditable, while Inbox is excluded
  from `/tasks`, recall, current-task selection, and continuation.
  `/task <id> pin|unpin` is explicit user authority. Automatic retention may
  archive only old visible terminal tasks with no live run or pending human
  input; it never deletes history, touches open/interrupted work, or overrides
  a pin.
- Run events use a per-run sink installed with
  `kernel.WithEventChannel(ctx, ch)`. Never swap the shared
  `Agent.EventChannel` in gateway code (legacy local-TUI fallback only).
- Stuck-run invariant: a task may only be `running` while a run is actually
  executing; after any finalization or recovery sweep no task may remain
  `running` with zero live runs (between-turns tasks park as `in_progress`,
  recovered ones as `interrupted` — both non-terminal/resumable).
  `Store.FinishRun` writes terminal run statuses only, and
  `Store.MarkInterruptedRuns` (boot sweep + 60s in-daemon sweep in
  `httpapi/run_recovery.go`) must keep excluding the active-run registry.
- External systems that need minutes of polling must use the durable
  `watch_external` handoff, not repeated model turns. The registering run ends
  as `waiting_external`; the daemon owns the persisted bounded command,
  interval, success/failure patterns, timeout, and final notification. The
  watch command passes through the same scope, egress, risk, and approval
  middleware as an ordinary terminal call. Never add a second unsupervised
  polling goroutine or keep a person's active-run slot occupied while waiting
  for CI/CD or another external service.
- A daemon-recovery interruption is actionable user state. Recovery sweeps
  enqueue one concise preferred-channel notification and append the durable
  `run.recovery_notified` marker only after enqueue succeeds; retries and
  restarts must not duplicate the notice or silently discard an undelivered
  one.
- Maintenance-provider authentication, quota, and other non-retryable errors
  block the durable maintenance job immediately. Do not consume the normal
  retry horizon on a terminal provider response. `/diag` must expose the
  paused background-learning state; a daemon restart may grant one fresh
  probe so recovered credentials can unblock the queue.
- Maintenance failover is explicit and cheap-role-only. Resolve
  `tasks.maintenance_model_role` first, then the ordered
  `tasks.maintenance_fallback_roles`; skip missing roles and never invent a
  fallback to the primary coding model. A provider outage must not silently
  turn background labeling, memory extraction, or review into expensive
  foreground-model traffic.
- Cross-endpoint steering has two durable states: `run.steered` means the
  daemon accepted the guidance, while `run.steering_consumed` means the agent
  applied it to a later model request. The consumed event records metadata,
  never the raw steering text.
- Final asynchronous answers must carry outbound kind `final_result`. When a
  task is explicitly continued from CLI and a recent IM final result is
  `sent_unconfirmed` or `failed`, inject a bounded delivery-continuity advisory
  so the agent can restate the result. Do not use this advisory for soft
  pre-label guesses or as another resend path.
- Approval heuristics must use the root selected by the request's
  `ExecutionScope`, not the daemon process cwd. A path already admitted by the
  active workspace/allowed-roots scope must not be treated as outside the
  project merely because the user switched workspaces after daemon startup.
- `/tasks` state is derived from the latest structured run outcome. Preserve
  distinctions such as daemon recovery, provider interruption, context
  overflow, verification incomplete, and durable external waiting; never
  collapse all resumable states into a generic `paused` label.
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
- Outbound `sent_unconfirmed` is TERMINAL for the retry queue (a blind resend
  reuses the same stale platform session and risks duplicates). The only
  legitimate resend is the inbound-triggered one-shot catch-up
  (`delivery.Service.CatchUpUnconfirmed`, fired from `ProcessMessage` when an
  IM inbound just refreshed the platform session): each row is claimed
  at-most-once (`outbound_messages.catchup_at`, claim-before-send), only rows
  inside `gateway.delivery_catchup_max_age` qualify, and one catch-up replays a
  bounded oldest-first batch. Never add another resend path for unconfirmed
  rows, and never let a catch-up attempt clear the claim.

## Context & Memory

- Project-convention files (AGENTS.md et al.) are injected by `ContextScanner`
  (`internal/kernel/context_scanner.go`) on a budget INDEPENDENT of the
  person-memory layer (facts + profile), so raising one never starves the other.
  Discovery is root→leaf (git/workspace root down to cwd, one highest-priority
  file per level, deeper = higher precedence, emitted last). Budget is dynamic
  (scaled to the model window, floored/ceilinged). NEVER drop a file whole for
  being large — head/tail truncate with a `read_file` pointer to the full path.
  The block is fenced as UNTRUSTED workspace data that operator/user
  instructions and safety policy outrank (a cloned repo's AGENTS.md must not
  inject instructions via the IM path). `filenames` order is precedence; README
  is intentionally excluded (human-facing, low signal). Do not reintroduce a
  fixed per-file byte skip.
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
- `internal/kernel/context_engine.go` stays on the streaming hot path: a bounded
  recent-history slice and no LLM work while under budget. Over-budget context
  compacts by DEFAULT — the middle turns become ONE structured summary while the
  head (system + original task) and tail (recent turns) stay verbatim; it never
  silently drops the oldest turns when a summarizer is wired. The summarizer is
  the cheap `memory_extract` role (via `Agent.SetSummaryProvider`), OFF the main
  provider, called only at the over-threshold moment — never per turn. The
  summary prompt (and a deterministic tool-arg path harvest) MUST retain the
  created/modified/read file paths. Fall back to deterministic trim only when no
  summarizer exists. `SELFMIND_SYNC_CONTEXT_SUMMARY` is legacy (compaction runs
  without it); guard against empty/larger/recursive summaries.
- Memory facts, session FTS recall, task handoffs, task events, and artifacts
  are separate durable sources. Ranking/embedding work extends the selector
  layer — never another append path in `agent.go`, gateway handlers, or IM
  adapters.
- A canonical memory's `last_accessed_at` means it was actually injected into
  the model prompt, not merely scanned as a selection candidate. Touch only the
  post-budget selected ids (plus unconditional pinned rows); touching every
  active row defeats age-based archival. Native tool dispatch must pass the
  active logical `_workspace_id` alongside `_tenant_id`, and project/environment
  memory writes must derive `workspace:<id>` scope from it. User preferences
  remain global. Never infer workspace memory scope from cwd text inside the
  memory tool.
- Human memory UX is a read model over durable evidence, not a dump of the
  `facts` table. `/memory` groups related evidence into stable human categories
  and hides UUID/provenance noise; `/memory raw` preserves the auditable view,
  while `search -> show -> correct|forget|pin|unpin` is the management path.
  Pinning an existing reference preserves its content, evidence, and scope;
  unpinning removes unconditional injection but retains user-confirmed authority.
  Read-model
  grouping must be deterministic, bounded, UTF-8 safe, and non-destructive.
  User corrections retain the fact reference and become `SourceUser`; never
  let automatic extraction silently override a pinned or user-corrected fact.
  Add new governance fields to evidence/canonical models, not directly to TUI
  formatting. `/memory history` is the joined human view over explicit learning
  changes and canonical governance events; reversible merge/archive events must
  remain undoable from that view. Never claim a merge is reversible unless its
  snapshot also preserves observation-to-member evidence ownership.
- Silent memory self-organization is the primary product path; human search,
  correction, pinning, and forgetting are safety controls, not routine cleanup.
  Do not persist every user message. One bounded post-run maintenance decision
  must choose `SKIP`, `ADD`, `REINFORCE`, `SUPERSEDE`, or `CONFLICT` against
  nearby canonical memory. Consolidation is two-stage: deterministic same-scope
  retrieval may only propose candidates, while the explicitly configured cheap
  model makes the semantic decision. Similarity alone must never merge facts.
  Pinned and `SourceUser` facts are immutable to automatic maintenance, and
  pinned facts are ALWAYS injected into the prompt ahead of selected facts,
  outside the bounded selection slots (`buildSystemPrompt`) — never let them
  compete for or be truncated by the extracted-fact budget. The legacy profile
  synthesizer was deleted (2026-07-11, dead code that starved pinned
  visibility); never reintroduce a profile-synthesis model call. Keep
  consolidation retryable, checkpointed, and lower-priority than foreground
  runs. Consolidation apply is mode-gated (owner decision 2026-07-12, the
  legacy store is mostly test data): `shadow` (default) writes nothing but
  dry-runs the SAME deterministic gates and annotates `would_apply` in the
  report; `merge-only` applies gated MERGE (confidence + no-novel-token),
  REINFORCE (canonical must restate one member's text VERBATIM — the member's
  original is what gets written, never model wording), and ARCHIVE
  (reversible); SUPERSEDE is report-only for consolidation — only intake, with
  fresh evidence, may supersede. Judgement checkpoints carry
  `consolidationJudgeVersion` (`internal/app/memory_consolidator.go`); bump it
  whenever the judge prompt or an apply gate changes so cached decisions
  re-judge instead of feeding a newer gate.
  A terminal run's maintenance input and model proposal are durable: save the
  replay payload before dispatch, freeze `proposal_json` before any memory/task
  mutation, and reuse it after a crash. Apply failures MUST fail the maintenance
  job rather than log-and-succeed; canonical proposal items are idempotent by
  run/analyzer/decision key. A fresh payload-attachment race stays pending,
  retries are bounded, and automatic governance pauses for foreground runs by
  default. Every statement lookup or status mutation is target+scope+hash
  scoped; identical text in another workspace is a different belief;
  see `docs/memory-self-organization-eval.md` and
  `docs/memory-governance.zh-CN.md` (layered observation/canonical schema,
  maintenance-job idempotency, intake decision policy — landed 2026-07-11).
- Working-context history is the person-level WORK SPINE (P1 of
  `docs/work-timeline.md`, landed 2026-07-06 — mandatory reading before
  changing keying, context assembly, or recall). Every agent-bound turn of a
  person — task-bound, casual, cron — appends ONE slim turn entry (user text +
  assistant final answer + touched file paths harvested from tool args +
  source tag like `[cron]`) under the constant `kernel.SpineTrajectoryKey`;
  the storage tenant is the person, so the key is person-scoped. Tool
  intermediates and the system prompt must NEVER enter the spine — they stay
  in run events. Load (`ContextEngine.BuildMessages` via `ContextComposer`)
  and save (`Agent.saveHistory`) MUST use the same key; the spine tail replays
  as alternating user/assistant messages, cross-endpoint and cross-task.
  Legacy compat is a READ-ONLY chain (old `task:<id>` key, then
  `TaskRuntimeContext.PriorChannel`, or the old channel key for taskless
  turns), consulted when the spine is empty or a task has no spine entry yet;
  the first save migrates forward. Internal subsystem turns (delegation,
  `:background_review`) stay channel-keyed and never write the spine. Per-turn
  assembly order and slice budgets live in `internal/kernel/context_composer.go`
  (ContextComposer contract; slice ④ is reserved for P2 recall via
  `RuntimeContextBundle.Recall`). This does NOT change chat-transcript
  channel-locality (transcripts stay per channel in `channel_messages`); the
  spine is the durable working-state layer only. FTS indexing keeps the
  task-derived session id (`Agent.sessionKey`; `IndexSession` idempotent per
  session id) and is never keyed by the spine.
- Storage partitions are PER-PERSON for run-written data: daemon agent runs
  execute with `identity.PersonID` as the storage tenant, so memory facts,
  session FTS, and checkpoints live under the person partition — while the
  daemon's SKILLS dir is keyed by the control tenant the agent was built with.
  `/v1/dispatch` must scope per tool (`personPartitionTools` in
  `httpapi/handlers_dispatch.go`): person-partitioned tools get PersonID,
  skill tools get the control tenant. Never inject one scope for all tools —
  that regression made client `/memory list` read an empty partition. The
  structured `GET /v1/sessions` API and the TUI `/search` command are always
  person-partitioned.
- Automatic recall v1 (Work Timeline P2, `docs/work-timeline.md` "Semantic
  recall"): the gateway selector (`httpapi/recall.go` on `Server.Recall`)
  attaches ≤3 bounded, EPHEMERAL `TaskRuntimeContext.RecallSlices` per turn
  (session FTS + task label cards via `control.ListTaskCards`; one slice per
  work line, label card beats raw session fragment; current task excluded;
  control-command/short-message skips). Recall renders only into the per-turn
  system context block — never the messages array, never persisted history —
  and must never block or fail the turn: `semantic_recall`-role expansion runs
  only when that role is explicitly configured, time-bounded, degrading to
  raw-term FTS. New recall sources implement `httpapi.RecallSource` (the v2
  embedding seam); do not add recall calls outside the selector. The memory
  store partitions by PERSON (the agent's storage tenant is `person_id`) —
  search sessions with the person id, not the control tenant.

## Agent-First Routing & Task Strategy

- SelfMind is strict agent-first. Except for explicit slash commands (`/help`,
  `/model`, `/status`, `/tasks`, `/task`, `/events`, `/approvals`, `/approve`,
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
- `web_search` (`internal/tools/web_search.go`) is a BACKEND REGISTRY, not a
  scraper. Hosted APIs (Tavily/Brave/Serper/Firecrawl) are the quality path;
  SearXNG is self-host; DuckDuckGo HTML scraping is a best-effort fallback
  ONLY — never promote it back to the default. Config is exactly TWO fields
  (`web.search_backend` + `web.api_key`) — ONE backend, ONE key, NO fallback
  chain (a configured backend that fails returns an error, it never silently
  switches engines). Keys live in `config.yaml` `web:` (the detached `setsid`
  daemon does not inherit shell env, so env-only credentials silently fail);
  `app.InitTools` passes `cfg.Web` in as `WebSearchOptions`. **Honesty
  invariant: a backend failure (non-200, anti-bot challenge, missing
  credential) MUST return an error, never "No results found".** Reporting an
  outage as an empty result made the model burn calls rephrasing and invent
  negative conclusions — "No results found" is reserved for a backend that
  actually ran and returned zero hits.
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
- Delegation nesting is bounded STRUCTURALLY, not by a runtime counter (tool
  execution has no context channel to carry depth). Sub-agent backends are
  always freshly cloned in `buildDelegateSubBackend` (`internal/app/delegation.go`)
  — NEVER the shared parent dispatcher — with `delegate_task` stripped unless the
  depth budget (`delegation.max_depth`, default 1 = flat) allows another hop, in
  which case a fresh nested delegate wired to depth+1 is added; at the budget the
  sub-agent is a leaf with no delegation tool. Never hand a sub-agent the parent
  backend directly (it re-exposes `delegate_task` → runaway recursion) and never
  mutate the parent dispatcher to filter tools. Batch fan-out is bounded by
  `max_subtasks`, concurrency by `max_concurrent`.
- Tool results go through the Agent result envelope before reaching the
  model, TUI, or run events. Raw output, model-bounded content (head/tail
  view with an explicit too-large note), and compact UTF-8-safe user preview
  are three separate surfaces — never one truncated string for all three.
  Large outputs are ARTIFACT-BACKED (W1, 2026-07-12): capture is capped at
  2MiB head/tail (codex-style, the dropped middle exists nowhere); an
  over-24KB model surface is spooled by the per-run gateway sink
  (`kernel.ToolArtifactSink`, `httpapi/tool_artifacts.go`) to
  `<data>/tool-output/<person>/<art_id>.txt` + a `tool_output` task-artifact
  row, and the truncation note tells the model to read omitted ranges with
  the read-only `tool_output_view` tool (person-scoped filesystem resolve, ≤
  24KB/call) instead of re-running the command. Artifact-backed tool results
  older than 3 iterations shrink in place (`shrinkAgedToolResult`) — lossless
  ONLY because the artifact stays addressable; never age-shrink a tool result
  without an artifact reference. Spool failures degrade to the plain note;
  spooling must never fail a tool call.
- CLI/TUI output must be human-readable: summarize plans, tool calls, and
  errors; never dump raw JSON unless the user asked for protocol details.
  Failure events carry compact diagnostics (`error_category`,
  `diagnostic_hint`, duration, preview, truncation markers).
- User-facing interaction prompts (approval hints, control-command replies,
  TUI notices, IM notification text) are English-only — no bilingual strings
  (owner decision, 2026-07-04). Functional keyword lists that PARSE Chinese
  user/model text (outcome-section headings, continuation cues) are not UI
  text and keep their Chinese entries.
- An approval rejection is a user decision, not a retryable failure. The
  `operation rejected` / `operation cancelled by user` error strings from
  `tools.SmartApprovalMiddleware` are a stable contract with kernel's
  `isUserRejectionErr` (`tool_result.go`), which swaps the diagnose-and-retry
  instruction for a do-not-retry instruction. Keep both sides in sync.
- The approval hard floor (`hardlineToolCall` in `tools/middleware.go`) is an
  UNBYPASSABLE deny set that fires BEFORE any approval-mode bypass — full-auto
  included — for irreversible, never-legitimate ops (recursive delete of the
  filesystem root or a protected system/home root, `mkfs*`, `dd`/redirect over
  a raw disk device, fork bomb, host shutdown/reboot/`init 0|6`). It returns
  `operation blocked by safety policy: …`, deliberately DISTINCT from the
  user-rejection strings: `isUserRejectionErr` must NOT match it (a hard block
  is a safety-policy decision, not a user preference). Keep the set TIGHT —
  merely "dangerous" ops stay in `dangerousToolCall` behind normal approval —
  and never let a mode, a session grant, or an LLM triage step (H2) override it.
  Both the floor AND `dangerousToolCall` read the exec payload via
  `execCommandPayload` (command/code/script — so `execute_code`'s `args["code"]`
  is inspected, not just `args["command"]`) and classify WRAPPER-UNWRAPPED
  segments (`expandCommandSegments`): a shell `-c` script and
  `sudo/doas/env/xargs/nohup/timeout/nice/ionice/setsid/stdbuf/command` prefixes
  are unwrapped (bounded depth 3) to their real inner program, so
  `bash -c "rm -rf /"` / `sudo rm -rf /` cannot slip past. An unparseable wrapped
  payload degrades to dangerous (approval), never to a hard block — the floor
  only denies what it can positively identify.
- Network egress is a first-class DANGEROUS class (`egressCommand` in
  `tools/middleware.go`, folded into `dangerousToolCall`): curl/wget/nc/socat/
  scp/sftp/rsync/ssh/… by wrapper-unwrapped basename, plus `/dev/tcp/`·
  `/dev/udp/` redirects — the exfiltration half of the IM-injection threat
  (untrusted inbound message → terminal data-out). It is dangerous, NOT
  hardline: on-request (default) and smart modes ask/triage it; full-auto still
  bypasses it by the documented full-auto contract (owner decision 2026-07-09 —
  do NOT promote egress to an unbypassable tier without owner sign-off). `git`
  is intentionally excluded (push/pull to configured remotes = fatigue). Keep it
  its own named function so it stays testable and tightenable.
- Running ARBITRARY CODE always asks: `approvalNeeded` returns true for
  `isExecTool` (`terminal`/`shell`/`execute_command`/`execute_code`) in
  on-request AND smart modes, regardless of the dangerous heuristic. The
  heuristic is a read-side NARROWING optimization, never a gate that lets
  unprompted code execution through (it once left `execute_code` fully
  unapproved). Only full-auto (and auto-edit for edits) bypasses; the hard floor
  still fires above everything.
- Class-level approval memory (`approval_grants` table + `ApprovalGrantStore`):
  approving a coarse action CLASS (`approvalPatternKey`) for a task (session) or
  person (persistent) suppresses later same-class asks. The pattern key is a
  CLASS, never the exact command; hard-floor and content-level denials are never
  eligible. Reply grammar carries the scope (`/approve [n] task|always`, bare
  `yt`/`ya`); persisted `/mode` lives in `person_settings` (`approval_mode`).
- Smart-mode LLM triage (H2, `tools/approval_triage.go`) is layer 4 of the
  funnel: it sits strictly BELOW the hard floor and the class-grant allowlist
  and ABOVE the human ask. Only in `smart` mode, only for a dangerous
  (non-hardline) op with no matching grant, a cheap `ApprovalJudge` (injected
  via `ExecutionScope.Judge`, backed by a cheap role model kept OFF the run's
  main provider) returns APPROVE / DENY / ESCALATE. APPROVE auto-runs and
  records a task-scope class grant (judge consulted at most once per class per
  task); DENY blocks with the user-rejection contract (`operation rejected: …`,
  do-not-retry). It NEVER fails open: no judge, ESCALATE, any judge error, an
  unrecognized reply, or the bounded (15s) timeout all fall through to the human
  ask — never an auto-approval. The judge prompt strips shell comments and wraps
  the command in `<command>` delimiters treated as untrusted data
  (prompt-injection defense).
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
- Control-command metadata is CROSS-ENDPOINT: the canonical catalog is the
  leaf package `internal/gateway/command` (name, aliases, summary/usage,
  `Scope` Gateway|Local, `SyncControl` async-hint). Gateway (`httpapi` `/help`,
  `suggestControlCommand`/reject gate), IM adapters (`weixin`,
  `handlers_channels` async-hint via `command.IsGatewayControl`), and the TUI
  all READ it — never re-hand-list command names, help text, the async-hint, or
  the near-miss decision. Execution stays where it is: the gateway
  `tryHandleControlCommand` switch is authoritative and must keep a registry
  entry per case (drift-guarded by `command.TestKnownMatchesGatewayContract` +
  httpapi `TestEveryRegistryGatewayCommandIsHandledBySwitch`). Approval-mode
  words come from `tools.IsKnownApprovalModeWord` /
  `tools.CanonicalApprovalModes`, not per-endpoint lists. TUI-only
  (`Scope: Local`) commands stay wired in `internal/gateway/cli/slash_commands.go`
  but are never gateway-routable.
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
