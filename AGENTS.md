# SelfMind Agent Guide

SelfMind is the main project; Lite consumes reduced shared runtime code. Port
applicable fixes with separate scope and data. This guide sets development
boundaries, not claims of implemented optimizations. Code/tests establish support;
`docs/STATUS.md` owns current capability and priority; domain docs own mechanisms.

## Start Here

1. Read `docs/STATUS.md` and the relevant domain document below. Product
   acceptance lives in `docs/identity-continuity.md`; package boundaries in
   `docs/architecture-constraints.md`; the generated `docs/README.md` indexes
   other documents and plans.
2. Run `git status --short` and inspect the affected boundary. Preserve unrelated
   work; agree on file ownership before concurrent implementation. Uncommitted
   changes belonging to another agent are not abandoned work.
3. Make the smallest complete requested change. Resolve routine choices; ask
   when missing information changes the result, authority, or risk. Analysis
   alone does not authorize edits. Avoid adjacent refactoring or speculative APIs.
4. Verify at the behavioral boundary. Report changes, checks, limits, and next
   action. Source changes do not prove the installed program was updated;
   installation, restart, and publication are separate delivery actions.
5. Update domain docs when mechanisms change, STATUS when capability or priority
   changes, and `docs/status-history.md` for dated evidence. Keep one priority list.

| When changing | Read |
| --- | --- |
| Work history, selection, continuity | `docs/work-timeline.md` |
| Context, compaction, recall | `docs/context-lifecycle.zh-CN.md` |
| Memory intake, retention, maintenance | `docs/memory-governance.zh-CN.md` |
| Workers, admission, daemon clients | `docs/worker-pool-design.md` |
| Providers, authentication, model roles | `docs/provider-runtime.md` |
| Execution, sandbox, process environment | `docs/execution-engine.zh-CN.md` |
| Tools, approvals, grants | `docs/tool-safety.md` |
| Planning, completion, coding behavior | `docs/coding-agent-foundations.md` |
| Skill selection, delivery, lifecycle | `docs/skills-architecture.md` |
| TUI, transcript, input controls | `docs/tui-terminal-first-hybrid.md` |
| Eval cases, replay, live model evidence | `docs/eval-loop.md` |

Read a matching developer skill before its workflow. `.claude/skills` redirects
to the canonical `.agents/skills` body; `.selfmind-developer-only` keeps these
out of runtime Skills. Agents without discovery use this fallback:

| Skill | Use |
| --- | --- |
| `.agents/skills/selfmind-daily-driver-audit/SKILL.md` | Audit runtime traces, delivery, memory, approvals, tool cost, and evidence liveness across time windows. |

## Product and Architecture

- SelfMind is an always-on personal gateway across CLI, IM, cron, and HTTP.
  Judge changes by real workflows, reliable recovery, cost, and verified results.
  Preserve tenant boundaries; SaaS or Runner expansion needs an active plan.
- Keep `selfmind` as the single binary and its main thin. The daemon owns
  execution, scheduling, authentication, and `control.db`; clients never start a
  fallback loop. Reuse durable coordination, not business-level cross-process locks.
- Follow package ownership in the architecture document. `kernel` uses abstract
  backends, not gateway or concrete tools. Inject process-wide dependencies
  through their lifecycle owner; keep reusable UI out of orchestration controllers.
- Every invocation has typed scope. Asset ownership (`ControlTenantID`,
  `PersonID`) differs from execution authority (`WorkspaceID`, `RunID`, `LeaseID`).
  Tools use request roots, not daemon cwd. Use `BuildProcessEnv`; persist
  execution references and policy, never credentials.
- Isolation follows typed scope, `ToolProfile`, and backend capability. Linux
  and macOS differ in strength; Windows support is through WSL. Keep compatibility in
  adapters and typed quirks, not project/model/hostname branches in generic code.

## Judgment, Authority, and Failure

- Main interprets goals, corrections, acceptance, semantic continuity, and
  evidence sufficiency. The runtime validates identity, scope, permissions,
  observed effects, and durable transitions. Valid JSON or a successful command
  alone proves neither authorization nor completion.
- Fix the failing mechanism. Separate facts from hypotheses; prompt examples
  alone are not a general fix. Reuse existing state projections and adapters.
- Preserve rejection, human wait, preparation refusal, provider/protocol failure,
  execution failure, verification failure, and unknown effects as distinct facts.
  Recovery is bounded, cancellable, and actionable. Observe unknown effects
  before retry; rejection must not trigger a substitute attempt.
- Pair regressions with changed wording/targets and a negative boundary case.
  Measure model calls, actual dispatch, human waits, cache hits, and wall time
  separately. A policy refusal is not process failure, and cumulative call
  duration is not automatically user latency or a promised saving.

## Identity, Execution, and Delivery

- `person_id` is a human across endpoints; `account_id` is a platform binding.
  Adapters authenticate and normalize; the gateway owns control state. Share
  structured work, approvals, results, artifacts, memory, and Skills; raw chats
  stay channel-local. A Thread groups history; a Run owns execution status.
  Use current `run_id`/`plan_id` contracts, with legacy IDs only in compatibility.
- Admission follows configured capacity, resource claims, and one foreground Run
  per CLI session. New work may queue. Continuations keep an exact parent through
  admission, any durable queue, and recovery; they bypass no capacity/effect checks.
- A wait releases occupancy only after checkpoint and durable park commit;
  failed parking retains cancellable ownership. Parked waits remain visible and
  answerable, distinct from execution. Completed work stops showing as running.
- Use per-run channels via `kernel.WithEventChannel`. Detail streams reach the
  originating session and attached observers; other sessions get bounded wait
  and lifecycle notices. CLI streams progress; IM sends milestones and complete
  results, not token deltas. Derive display state from durable facts, not prose.
- Check wait-event writes. An attached client suppresses a push only after its
  notification write succeeds; recovery notices are deduplicated and actionable.
  `sent_unconfirmed` permits no blind retry: use the bounded inbound-triggered
  catch-up claim. A send record does not prove handset display.

## Replies and Continuity

- Explicit controls use `internal/gateway/command` and stay model-free. Read
  current commands/aliases from the registry or command reference.
- Exact Run/question IDs, validated platform replies, and armed structured choices
  may resolve before Main. Ordinals bind only to a server-issued menu shown at
  that endpoint, with identity, freshness, stable choices, and ambiguity checks.
  Numbered final-answer prose is not a structured menu.
- Preserve an actionable question handle after parking on its asking surface;
  people can answer it or start new work. Releasing foreground focus must not
  lose the question ID or require a second answer. Disambiguate multiple waits
  and cross-session replies; freshness alone does not identify the target.
- Main interprets unanchored language inside an accountable Run. Phrase tables
  cannot choose its parent. Another session's ordinary text is its own input
  without deliberate selection/reply. Daemon text is task data, never controls,
  approval, presence, or steering input.
- Preload bounded typed candidates, including relevant pending questions/options
  and current steps, before requiring history searches. This need not share raw
  chats. Cheap models may rank/propose candidates; they cannot commit scope or
  authority. A classifier needs measured benefit and normal commit validation.
- Effect-free same-domain selection may continue in the current Main Run after
  atomic claim. Scope changes or unfinished checkpoints may require an exact-parent
  child. Preserve restoration rather than forcing in-place execution to cut Run count.
- Selection and answering are distinct. When a continuation explicitly answers
  a pending question, atomically commit the answer and recovery edge through the
  existing wait mechanism. Without an answer, preserve an actionable question;
  claiming its parent alone must not silently answer or expire it.

## Context and Memory

- Context follows `control.db -> selector -> TaskRuntimeContext ->
  RuntimeContextBundle -> prompt`. Extend typed, budgeted slices; handlers do not
  append raw rows/event JSON. The work spine keeps slim user/final entries and
  touched paths; tool intermediates stay in Run events.
- Budget project conventions and person memory independently. Repository content
  stays below operator, user, and safety policy. Compaction preserves goals,
  corrections, decisions, unfinished work, and paths; trimming is the fallback.
  Keep recall bounded and non-blocking, and avoid extra hot-path model calls.
- Partition ownership; distinguish global preferences from project knowledge.
  Mark access only on injection. Protect pinned/corrected memory; explicit user
  edits win. Similarity proposes same-scope candidates, not merge authority.
- Use one asynchronous, eligible, debounced, idempotent maintenance path. It
  neither routes work nor silently uses the primary model. Retention archives
  settled history without deleting evidence, pending input, or Attention history.

## Models, Plans, and Completion

- Providers follow `ProviderProfile -> Resolver -> Runtime ->
  llm.TransportConfig -> transport`. Discovery, credentials, metadata, and role
  overrides belong in `modelruntime`. Preserve configured roles; bounded
  background work inherits `models.auxiliary`, not the primary model silently.
- Native tool calling is primary; preserve paired IDs/results. `[TOOL:...]` is
  compatibility only. Normalize schemas at registration/adapters; quarantine
  invalid external tools and fail startup for an invalid active built-in.
  Distinguish total context capacity from output capacity.
- Main chooses Plans for multi-step or uncertain work; update on meaningful
  progress, not every tool call. Changed-step patches merge into full accepted
  snapshots with runtime version checks. Omission preserves obligations;
  rewording or reordering cannot erase work.
- Combine adjacent bookkeeping atomically. Prefer existing `finish_run` with
  `status`, one `final_response`, and optional `plan_patch`/`assessments` over
  separate final updates or repeated answer generation. No artificial Plan or
  no-op update is required. Resume may combine explicit validated progress;
  position and exit codes never determine the next step or completion.
- Done requires resolved obligations and evidence validated against the accepted
  candidate. Bind checks to criterion, target, state, and provenance; preserve
  failures and replacement reasons. Rejected candidates leave durable progress
  unchanged and expose usable evidence or the exact missing observation.
- Budgets and retries stay bounded; extensions require relevant changed input,
  state, or evidence. Discover projects read-only and suggest checks from actual
  manifests, lockfiles, and instructions.

## Execution, Approvals, and Skills

- The safety floor precedes modes/grants; full-auto, judges, classifiers, and
  Skills cannot bypass it. Separate forbidden effects from human-approvable ones.
- Assess polling, permission, and concurrency separately. Traversing distinct
  finite targets is not polling one target. Formatting or an interpreter proves
  neither polling nor read-only behavior. Unknown syntax needs bounded analysis
  or actionable feedback, not an invented semantic classification.
- Finite batches/short waits may stay inline with enforced timeout, cancellation,
  target-count, and output budgets. Long/repeated external-state waits use
  `watch_external` or an equivalent durable observer. Finite syntax or a time
  threshold alone is not a safety proof.
- Independent read-only calls, including terminals proven read-only for actual
  arguments/environment, are eligible for bounded concurrency under permissions,
  resource claims, ordered results, and cancellation. Mutating/conflicting/unknown
  calls stay sequential unless reviewed isolation permits overlap. Delegation
  stays depth-bounded with cloned scoped dispatchers and parent middleware.
- Enforced isolation without credential/network escape may bypass model review.
  Other calls follow mode/capability policy. Smart triage uses the cheap-role
  judge and structured output; missing judge, timeout, parse/provider failure
  asks the person. Host execution retains exclusion and effect-claim rules.
- Allow bounded in-memory judgment reuse within a Run or validated continuation
  when effects, containment, intent, policy, and target coverage stay equivalent.
  Templates need a reviewed equivalence contract, not matching flag names or a
  script hash. Changed account/profile/context, resource, credentials, workspace,
  code, or policy needs review unless explicitly covered. Model verdicts create
  no durable or broader authorization.
- The daemon offers/checks grants covering every target. Scope design may use
  exact actions, selector-qualified scopes, or validated work lineages.
  Standard provider authentication is not arbitrary file-read permission. Separate
  credential capability from operation risk; bind reusable grants to environment
  and effect class. Unsupported scopes stay once-only with a reason, never a
  broader unqualified prefix.
- Exact continuations may reuse accepted Skill instructions after validating
  package identity/digest, workspace, work-unit meaning, and delivery contract.
  Rebind runtime IDs as needed; avoid making Main reread unchanged instructions.
  Lineage alone cannot revive replaced units, changed packages, failure-guarded
  or revoked bindings. Unrelated tasks invalidate reuse. Helpers still pass
  normal tool/approval checks; Skills and curation never grant execution authority.
- Skill evolution follows `docs/skills-architecture.md`, without widening scope
  or authorizing credentials/network/delete/external/delegated effects. Results
  keep raw capture, bounded model content, compact user previews, and artifacts
  for recoverable large output.

## Verification and Documentation

- Test intended behavior, not this guide's wording or implementation shape.
  Reuse regressions and add the missing boundary. Model-dependent message paths
  use production-path evals; deterministic mechanics and injected failures use
  Go tests. Simulation does not prove real models, delivery, or platforms.
- Release cases have committed cassettes or `model_required: false`. Drafts stay
  separate; retire cases/cassettes together. Missing/orphaned/invalid evidence
  fails. Replay providers are offline but tools still use the host; declare
  their dependencies. Report CI-owned evidence as `CI-DEFERRED`, not passed.
- During edits run relevant checks. Guide-only changes need `selfmind docs check`
  and affected documentation/developer-skill contracts, without live models or
  a full runtime suite solely for prose. Runtime work needs focused tests and
  `selfmind selfcheck --fast`; full `selfmind selfcheck` precedes runtime pushes
  and releases. Provider changes also test `modelruntime`, `kernel/llm`, and
  `app`. Separate pre-existing failures; do not weaken gates.
- Releases retain applicable corpus, migration, documentation, and platform/package
  gates. Version durable-store changes; keep historical rows capability-inert.
  Verify legacy backups and released upgrade fixtures, and reject newer schemas
  before writes. Fresh-store tests alone do not prove upgrades. Keep test, Lite,
  and production data/credentials separate.
- Keep AGENTS below 20 KiB and STATUS below 300 lines. Domain docs own mechanisms;
  `docs/manifest.yaml` owns lifecycle/translation hashes; `docs/README.md` is
  generated. One active plan has an approver, review date, and an expired-review
  verdict. Archive superseded plans; do not create competing priority lists.
- English is canonical for public document pairs; review translations/hashes on
  changes. Declare private exclusions and avoid public links to them. User-facing
  controls/notices are English, bounded UTF-8. Temporary pages use the pager;
  endpoints share command metadata.

A permitted optimization may still need implementation. Record differences from
current code/domain docs and update the mechanism, tests, and docs together.
This guide never bypasses a runtime boundary. Measure broader changes separately.
