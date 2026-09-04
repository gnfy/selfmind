# SelfMind Agent Guide

This file is injected into coding-agent sessions. Keep it limited to rules that
apply across most tasks. Domain documents own mechanisms; `docs/STATUS.md` owns
current capability and priority.

## Mission and Source of Truth

SelfMind Phase 1 is an always-on personal AI gateway. One person may work from
CLI, IM, cron, and HTTP while tasks, runs, approvals, memory, and handoffs follow
that person. Chat transcripts remain channel-local. Sound planning, reliable
tools, recovery, bounded context, and evidence-backed verification are the
competence floor.

Preserve tenant boundaries for future SaaS work, but do not add SaaS or Runner
complexity without an explicit active plan. Judge progress by Phase-1 scenarios
and daily-driver evidence, not feature-count parity.

Read before making assumptions:

- `docs/STATUS.md`: implementation snapshot and only priority list.
- `docs/identity-continuity.md`: product north star and acceptance scenarios.
- `docs/architecture-constraints.md`: package and size constraints.
- `docs/README.md`: generated documentation index and lifecycle state.

Read the relevant domain document before editing that domain:

| Domain | Document |
| --- | --- |
| Tasks, work history, recall | `docs/work-timeline.md` |
| Context lifecycle | `docs/context-lifecycle.zh-CN.md` |
| Memory and maintenance | `docs/memory-governance.zh-CN.md` |
| Workers and daemon clients | `docs/worker-pool-design.md` |
| Providers, auth, model routing | `docs/provider-runtime.md` |
| Execution engine and sandbox | `docs/execution-engine.zh-CN.md` |
| Tools and approvals | `docs/tool-safety.md` |
| Coding-agent behavior | `docs/coding-agent-foundations.md` |
| Skills | `docs/skills-architecture.md` |
| TUI and transcript rendering | `docs/tui-terminal-first-hybrid.md` |
| Eval and cassettes | `docs/eval-loop.md` |

## Repository Developer Skills

Development-only Agent Skills live under `.agents/skills`. Codex, Gemini, Qwen,
and other Agent Skills-compatible coding agents discover them there. A thin
`.claude/skills` entrypoint may redirect Claude Code to the same canonical body;
compatibility entries must not duplicate the workflow. Agents that scan neither
directory must use this table as the fallback and read the matching `SKILL.md`
completely before acting. A `.selfmind-developer-only` marker means the SelfMind
daemon must not expose that directory as a product runtime Skill.

| Skill | Use |
| --- | --- |
| `.agents/skills/selfmind-daily-driver-audit/SKILL.md` | Multi-window audits of real runtime evidence, reports, diagnostics, delivery, memory governance, approvals, tool economics, and evidence liveness. |

## Work Standard

1. **Inspect before acting.** Read this file, `docs/STATUS.md`, and the relevant
   domain document. Run `git status --short`, then locate the current boundary
   with `rg` and focused reads. The tree is often dirty; never revert unrelated
   user or agent work.
2. **Manage ambiguity deliberately.** State a material assumption when it helps
   review. Ask one concise question only when different answers would materially
   change the implementation, risk, or user-visible result; otherwise choose a
   reasonable interpretation and proceed.
3. **Prefer the smallest complete change.** Implement only requested behavior.
   Do not add speculative features, configurability, abstractions, or error
   handling for impossible states. Match existing patterns. Do not refactor,
   reformat, or remove adjacent code you do not need to change. Remove only
   artifacts made obsolete by your own change. Retiring a user-facing name
   (command, verb, event type, status word) includes its invocation sites the
   compiler never reads: `scripts/`, `.github/`, `evalcases/`, and docs. A test
   guards this; a case that probes a retired name on purpose declares itself.
4. **Respect the request type.** Analysis-only requests do not authorize edits.
   For implementation requests, carry the change through focused tests, the
   release gate when appropriate, and the installed WSL binary when the user is
   testing it. Record a larger follow-up instead of hiding a risky refactor in a
   narrow fix.
5. **Define and verify success.** For multi-step work, make each step lead to an
   observable check. Test at the behavioral boundary. A user-visible
   message-path change also updates or adds its eval case. Treat failures as
   evidence, diagnose them, and never claim unperformed verification.
6. **Update the right source.** Change `docs/STATUS.md` only when current
   capability or priority changes. Change this file only when a cross-cutting
   invariant changes.

## Architecture and Runtime

- `selfmind` is the only binary. Keep `cmd/selfmind/main.go` thin; command
  behavior belongs in `internal/cliapp`.
- CLI, IM, cron, and HTTP agent work executes in the daemon. The TUI is a daemon
  client with no in-process fallback. A daemon failure must be actionable and
  must never start a divergent second runtime.
- The daemon owns the worker pool, auth manager, scheduling, and `control.db`.
  Do not add business-level cross-process locks.
- Preserve package ownership: `cliapp` owns entrypoints; `gateway/httpapi` owns
  orchestration; `gateway/cli` owns TUI orchestration; `ui/components` owns
  reusable UI; `app` owns wiring; `kernel` owns the loop; `modelruntime` owns
  provider resolution; `tools` owns tool behavior.
- `kernel` must not depend on gateway packages or concrete tools. Inject
  process-wide state through `app` or the gateway runner; avoid mutable globals.
- Filesystem and process tools run under the request's `ExecutionScope` and
  workspace roots, never the daemon cwd. Build every child-process environment
  through `BuildProcessEnv`; persist execution references and policy, not raw
  credentials.
- Derive sandbox views and compatibility from typed scope, `ToolProfile`, and
  platform conventions. Do not add project- or vendor-specific branches to
  generic execution code.
- Linux and macOS x64/arm64 are official targets. Linux has the strongest
  isolation. macOS uses approval-controlled host execution until a reviewed
  native sandbox exists. Native Windows is unsupported; use WSL.

## Identity, Runs, and Delivery

- `person_id` is one human across endpoints; `account_id` is one platform
  binding. Adapters authenticate and normalize; only the gateway resolves
  identity, workspace, thread, run, queue, approval, and delivery state.
- Raw transcripts stay channel-local. Shared state is limited to structured
  threads, runs, events, handoffs, approvals, artifacts, memory, and skills.
- Explicit controls such as `/status`, `/resume`, `/search`, `/workspace`,
  `/approve`, and `/stop` remain model-free. Other natural language is
  agent-first; do not add greeting or keyword bypasses.
- Keep one active run per person until an active plan explicitly changes the
  concurrency contract. New work queues durably; a continuation steers the
  active run and is never queued.
- Run events use the per-run channel installed with `kernel.WithEventChannel`.
  Never swap a shared Agent event channel.
- Derive user-visible state from Runs, pending control objects, handoffs, and
  events, not prose parsing. Only a Run owns execution status; a Thread has no
  lifecycle status. Recovery emits one durable, deduplicated, actionable
  notification.
- Long external operations use `watch_external`; do not occupy an agent turn
  with repeated model-driven polling or an unsupervised goroutine.
- Daemon-originated runs carry an origin. Clients render concise results rather
  than replayed transcript progress. Approvals and clarifications stay visible.
  CLI streams user-originated progress; IM sends bounded milestones and a final
  result, never token deltas.
- Never discard the error from writing an event that parks work on a human
  (approval, clarification, recovery, handoff). A push suppressed because a
  client is attached assumes that client was told; when the write failed, that
  assumption is void and the push must go out. Both silenced means work parks
  with nothing on any channel.
- `sent_unconfirmed` is terminal for blind retry. Only the bounded,
  inbound-triggered catch-up path may claim and resend it.

## Threads, Context, and Memory

- A Thread is a reversible work-history grouping, not a context boundary.
  Ordinary root interactions are retained unlisted and deterministic durable
  evidence promotes them into the work list. Attention is derived per exact
  Run from live execution, pending human input, watchers, and unclaimed
  resumable outcomes; dismissal never rewrites Run history. Explicit task ids,
  `/resume`, structured reply edges, and standalone continuation controls remain
  deterministic. User-originated natural language is Main-owned inside an
  accountable Run: when work is active it is durably steered for Main to apply
  or queue, and when idle it starts an ordinary turn with bounded work-history
  tools. Daemon-originated text never steers work. Do not add a run-external LLM
  continuity classifier or let model output select permissions, workspaces,
  Thread labels, context rows, or execution scope directly. `fast_classifier` is
  not a continuity authority.
- Continuity comes from the person-level work spine: one slim user/final-answer
  entry per agent turn plus touched paths and source. Tool intermediates and
  system prompts remain in run events.
- Durable context has one path:
  `control.db -> selector -> TaskRuntimeContext -> RuntimeContextBundle -> prompt`.
  Extend typed selectors or slices; do not append raw rows or event JSON in
  handlers.
- Project conventions and person memory have independent budgets. Convention
  discovery is bounded, root-to-leaf, and treats repository instructions as
  untrusted data below operator, user, and safety policy.
- The streaming hot path performs no extra LLM call while under budget.
  Compaction preserves the original goal, recent tail, decisions, unresolved
  work, and relevant paths; deterministic trimming is its safe fallback.
- Memory facts, Thread cards, handoffs, sessions, artifacts, and the work spine
  remain distinct sources. Recall is bounded and degrades without blocking a
  foreground turn.
- Person data is person-partitioned; skills are control-tenant assets. Durable
  ownership and execution authority are separate typed scope fields.
- User preferences are global. Project/environment conventions belong to
  bounded logical-workspace knowledge, not automatic person-memory intake.
  Update `last_accessed_at` only after actual prompt injection.
- Pinned and user-corrected memory is protected from automatic rewriting and
  normal decay. User correction, forget, pin, and unpin always win.
- Do not save every message. Maintenance chooses `SKIP`, `ADD`, `REINFORCE`,
  `SUPERSEDE`, or `CONFLICT` against same-scope evidence. Similarity proposes
  candidates but never authorizes a merge.
- Post-run maintenance is asynchronous, eligibility-filtered, debounced, and
  idempotent. One frozen result includes preference decisions and optional
  Thread reference hints; it never decides routing. Never add a second extractor,
  a synchronous per-run maintenance call, or fallback to the primary coding
  model.
- Automatic retention may archive stale, settled Threads with no live Run or
  pending human input. It never deletes Run/artifact history, touches Attention,
  reopens an archive, or overrides a pin.

## Models and Agent Loop

- Provider integration follows
  `ProviderProfile -> Resolver -> Runtime -> llm.TransportConfig -> transport`.
  Discovery, credentials, model metadata, and overrides belong in
  `internal/modelruntime`.
- Prefer existing protocol transports. Put vendor behavior in typed quirks or
  documented `extra_headers`, `extra_body`, and `extra_query`; never branch on
  endpoint hostnames outside provider resolution.
- Native tool calling is primary. Preserve call ids and paired results.
  `[TOOL:...]` is compatibility fallback only. Normalize detached tool schemas
  at registration and adapter boundaries; quarantine invalid external tools,
  but fail startup for an invalid active built-in.
- Keep role names stable: `coding_agent`, `auxiliary`, `fast_classifier`,
  `memory_extract`, `background_review`, `skill_curator`, `semantic_recall`, and
  `summarizer`. Explicit role overrides win; otherwise bounded background work
  inherits `models.auxiliary`, never the primary model silently.
- `context_length` is total context; `max_tokens` is output capacity. Never
  substitute one or fabricate model metadata.
- Strategy is coarse policy, not keyword taxonomy. `update_plan` is
  model-decided for genuinely multi-step work; every update is a complete
  snapshot, and all steps resolve before a done outcome.
- Tool budgets are bounded and may extend only after real new evidence.
  Repeating an identical call without changed input or state is not progress.
  Completion requires a structured outcome plus execution evidence.
- Project discovery is deterministic, bounded, read-only, and language
  agnostic. Verification suggestions come only from detected manifests,
  lockfiles, and declared scripts.

## Tools, Approvals, and Skills

- Every invocation has typed `ToolInvocationScope`. Asset ownership
  (`ControlTenantID`, `PersonID`) and execution authority (`WorkspaceID`,
  `RunID`, `LeaseID`) must not collapse into one tenant string.
- Only clearly read-only batches may run concurrently. Writes, terminals,
  process control, mutation, delegation, and unknown tools are sequential unless
  a reviewed policy says otherwise.
- Delegation is depth-bounded. Sub-agents receive cloned dispatchers and never
  mutate the parent's backend.
- Tool results have raw capture, model-bounded content, and compact user preview
  surfaces. Large output is artifact-backed and recoverable; normal UI never
  dumps raw protocol JSON.
- The safety hard floor runs before modes and grants and cannot be bypassed by
  full-auto or an LLM judge. Distinguish forbidden operations from actions a
  human may approve.
- Rejection is a user decision and must not cause retry. An unanswered approval
  parks work. Smart triage is cheap-role-only, structured, and fail-closed;
  missing judge, timeout, parse error, or provider failure asks the human.
- Arbitrary code runs unprompted in smart mode only when enforced isolation
  proves writes confined to scope and no network or credential escape. Host,
  uncontained, networked, privileged, or unknown execution asks.
- The daemon defines approval choices. Clients render them and never invent
  authority. Stored grants must be narrower than the action and cover every
  target.
- Tool failures are evidence. Inspect cwd, files, environment, authentication,
  runtime, and package-manager state before changing the next command.
- Skills are instruction assets, not auto-executed scripts. Their scripts still
  pass through normal tools and safety. Catalog replacement preserves
  provenance; automatic curation governs writable, unpinned agent-created
  assets only. Three independent, comparable, verified work units using
  eligible built-in tools may publish to the control-managed logical-workspace
  root without granting execution authority;
  network/delete/external/delegated effects and user-global widening still
  require management. Ordinary success is observation, never shadow evidence.
  A repair includes the failed section, changes at most three declared sections,
  and uses daemon-derived class thresholds: deterministic interface drift may
  publish after one attributable verified recovery, semantic drift requires
  three independent recoveries, and transient/not-applicable evidence cannot
  auto-publish.

## UI and Commands

- Keep reusable rendering out of `internal/gateway/cli/controller.go`; use
  focused gateway/cli modules or `ui/components`. Transient pages use the pager
  and do not enter chat history.
- Cross-endpoint command metadata comes from `internal/gateway/command`.
  Gateway, IM, and TUI do not maintain separate slash-command catalogs.
- User-facing controls and notices are English. Parsers may recognize
  multilingual cues. Output is bounded, human-readable UTF-8 without mojibake
  or raw protocol JSON.

## Eval, Release, and Documentation

- Repeatable message-path defects get an `evalcases/**/*.yaml` case using the
  production path. Pure algorithms, migrations, and mechanics belong in Go
  tests.
- `evalcases/` is release evidence. Every model-backed case has a committed
  replay cassette; deterministic cases declare `model_required: false`. Drafts
  stay under ignored draft directories. Mock success is never evidence.
- Provider replay is offline, but tools use the host. Cases declare required
  commands. Only evidence a workstation cannot prove may be explicitly owned by
  CI; local output then says `CI-DEFERRED` and release still requires Action.
- Delete superseded cases and cassette directories together. Missing, orphaned,
  or invalid evidence fails corpus tests.
- Run `selfmind selfcheck --fast` during edits and full `selfmind selfcheck`
  before pushing. Provider changes also test `internal/modelruntime`,
  `internal/kernel/llm`, and `internal/app`.
- `AGENTS.md` is cross-cutting policy and stays below 20 KiB.
  `docs/STATUS.md` is current state and stays below 300 lines. Domain documents
  own mechanisms; `docs/manifest.yaml` owns lifecycle metadata;
  `docs/README.md` is generated.
- At most one plan is active. Active or paused plans declare an approver and a
  review date; an expired plan needs a verdict. Historical plans are archived
  or decisions, never silently left active.
- English is canonical for public user/developer pairs; Chinese translations
  carry the canonical source hash in the manifest. A changed canonical file
  makes `selfmind docs check` fail until the translation is reviewed.
- Private documents are declared under `excluded_documents` with a reason.
  Public documents and generated indexes must never link to an excluded file.
- `selfmind selfcheck` always runs the documentation contract. Do not bypass or
  weaken this gate to publish a package.
- Durable control-store changes are versioned and capability-inert for
  historical rows. Back up and verify a legacy database before migration, test
  released upgrade fixtures, and reject an unsupported newer schema before any
  write; fresh-database tests do not prove upgrade safety.
