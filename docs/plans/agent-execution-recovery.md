# Agent Execution and Recovery Closure

## Purpose

Raise the personal-edition agent from a collection of individually durable
features to one evidence-backed execution loop. Planning, tool dispatch,
failure handling, external waiting, daemon recovery, and completion must share
one contract so an ordinary tool or transport failure cannot strand work or
silently replay an uncertain side effect.

The implementation extends the existing Agent Loop, tool ledger, loop
checkpoint, watcher, Run parent edge, and environment lease. It does not add a
business workflow engine, an ingress classifier, a second foreground model, a
remote Runner, or a permanent parallel loop.

## Ownership

- Owner: SelfMind project owner
- Approver: project owner
- Review date: 2026-09-15
- Status: active. This plan pauses
  [`external-skill-packages.md`](external-skill-packages.md); that plan keeps
  its existing scope and review date.

## Invariants

1. User work ends as `completed`, `waiting_user`, `waiting_external`,
   `blocked`, or `cancelled`. `interrupted` is an internal recovery state, not
   a satisfactory user outcome.
2. The main model chooses semantic plans, verification, and genuinely different
   strategies. Runtime validates authority, effects, evidence, repetition, and
   completion without a second hot-path classifier.
3. A dispatched effect is never replayed merely because observation or
   verification failed. Unknown outcomes are observed first.
4. The active Run's environment lease is immutable. A capability or environment
   change creates a child Run with a new generation; evidence for an older
   effect remains bound to its original generation.
5. Linux and macOS share behavioral outcomes, not containment mechanisms.
   Linux may use enforced isolation; macOS remains approval-controlled unless
   a bounded operation is proven safe within typed scope.
6. Existing approvals, clarifications, watcher rows, and historical interrupted
   runs retain frozen semantics. An upgrade never auto-runs historical work.

## Batch 0: Stop deterministic recovery loops

- Parse shell structure before calling a command active polling. Finite literal
  status batches are allowed; while/until, C-style, dynamic, interactive, and
  `watch` loops remain blocked.
- Expand the read-only observation catalog for supported provider queries such
  as `aws codebuild batch-get-projects`.
- Validate watcher command proof and state patterns before approval. The first
  real network/credential observation remains after authorization.
- Extend the current typed tool result with preparation phase, retryability,
  effect state, state change, and alternative strategies.
- Stop telling the model that watcher registration is the only legal recovery.
  Unsupported registration returns bounded alternatives and no side effect.

Acceptance: the production-path reliability case rejects one unsupported
watcher before approval, records `different_strategy/not_dispatched`, switches
to a bounded observation tool, and completes without a watcher retry.

Status: implemented on 2026-09-01; focused Go tests, full `go test ./...`, the
cassette-backed production path, full release selfcheck, packed npm smoke, and
the installed macOS arm64 npm binary pass.

## Batch 1: Durable plan and effect correlation

Use the next unused control schema version (v9 at plan approval time). The
migration is additive and capability-inert for historical rows.

1. Persist full Run plan versions and ordered plan steps. Each step has a
   server-issued stable id, status, text, and optional success criteria.
2. Make the control projection authoritative for completion; keep the current
   in-memory `PlanStore` only as a hot cache and UI surface.
3. Extend the existing tool ledger with effect id, plan-step id, strategy,
   effect class, environment generation, result reference, and verification
   state. Preserve its monotonic `prepared -> dispatched -> terminal/unknown`
   rule.
4. Version the existing loop checkpoint with a typed recovery payload that
   references the durable plan and effects instead of duplicating them.
5. Give new Runs a recovery-contract version. Historical rows default to zero
   and cannot enter automatic recovery.

Acceptance includes released-database upgrade fixtures, unsupported-newer-
schema refusal, stable plan ids across reorder/update, and crash tests before
dispatch, after dispatch, and after outcome recording.

Status: implemented on 2026-09-01. Schema v9 keeps historical Runs at recovery
contract version zero and opts new Runs into version one. One control-backed
Run-plan module owns complete plan versions, server-issued step ids, work-unit
projection, and completion validation. Tool ledger rows correlate effect,
plan step/version, strategy, environment generation, hashed result reference,
and verification state. Loop checkpoints carry a versioned recovery reference
instead of copying plan/effect rows. Released beta.15 upgrade, newer-schema
refusal, crash-window, plan reorder, multi-work-unit Skill switching, full Go,
and fast offline gates pass. The combined full release selfcheck and installed
v8-to-v9 package migration smoke also pass.

## Batch 2: Strategy-aware replanning

- Add an injected recovery-policy seam to the kernel; storage remains in the
  control layer and orchestration remains in the gateway.
- Normalize strategies as `observe`, `mutate`, `wait`, `verify`, or `interact`.
- Treat a cosmetic argument change with the same step, target, failure class,
  and strategy as repetition. Permit one evidence-backed correction per
  strategy; extend budget only after new evidence, state, environment,
  capability, user input, or a genuinely different strategy.
- Correct an invalid model tool/protocol call once. A repeated contract failure
  becomes an actionable `blocked_model_protocol` or
  `blocked_model_capability` outcome.
- Separate operation evidence from verification evidence. Missing optional
  verification may be reported; missing required verification cannot produce
  completion.

Status: implemented on 2026-09-01. The injected kernel policy normalizes
attempts to `observe`, `mutate`, `wait`, `verify`, or `interact`; keys failures
by durable plan step, target, strategy, and environment; permits one changed-
input correction; releases the guard only after new evidence/state; refuses an
identical or exhausted strategy before dispatch; and requires observation for
an unknown effect. Refusals are typed as `blocked_model_protocol` or
`blocked_model_capability` with `not_dispatched` effect state and bounded
alternatives. Plan steps may explicitly require executable verification;
successful completion then reads evidence-derived work-unit verification from
the durable projection. Existing plans default to optional verification.

## Batch 3: Safe automatic continuation

Add a gateway recovery coordinator that consumes the durable contract:

1. Specialist approval, clarification, and watcher recovery paths keep
   ownership of their rows.
2. A daemon/provider interruption before any effect may enqueue one idempotent
   child Run using the primary foreground route.
3. A dispatched unknown effect enqueues verification-only recovery and is never
   replayed.
4. New user foreground input outranks recovery that has not dispatched an
   effect. A dispatched effect must first be observed or safely stopped.
5. Recovery uses an exact `parent_run_id`, a new immutable environment lease,
   and origin-aware delivery. Any endpoint for the same person may query or
   continue it without receiving another endpoint's raw transcript.

An unrecoverable result includes the original goal, completed steps, uncertain
effects, cause, attempted strategies, unlock condition, and exact resume path.

Status: implemented on 2026-09-01. The control layer admits only new
recovery-contract Runs and excludes specialist-owned approval, clarification,
and watch waits. Eligible daemon/provider interruptions enqueue one
idempotent, exact-parent recovery child below foreground priority. Unknown
effects enter verification-only mode; the kernel exposes trusted read-only
tools and refuses mutations before dispatch. Recovery-origin children do not
recursively auto-recover, and historical Runs remain inert.

## Batch 4: Structured durable waits

- Introduce a watcher spec version whose observation adapter returns
  `pending`, `succeeded`, or `failed`; raw regex remains a compatibility
  fallback.
- Freeze a successful preflight receipt containing command/argv hash,
  environment generation, adapter version, target, deadline, and capabilities.
- Add `all` and `any` wait groups, defaulting to `all`, with exactly one
  aggregate finalization and recovery child Run.
- A successful watcher completes only the wait step. The main Agent still owns
  verification and remaining plan work.
- Provider-specific observation grammar lives in tool registry adapters, never
  in kernel or generic gateway policy.

Status: implemented on 2026-09-01. Watch spec v3 uses the registry-owned
`status_json.v1` adapter, persists a successful preflight receipt, and retains
v1/v2 regex rows as frozen compatibility data. Run-local `all` and `any`
groups declare two to eight members, use a transactional one-winner aggregate
verdict, settle other members without child finalizations, and emit one durable
resolution event for reporting and reconciliation.

## Batch 5: Rollout and compatibility removal

Extend the current GitHub Actions jobs rather than adding another workflow.
Deterministic Go tests cover reducers, migrations, races, crash windows, and
platform capability tables. Offline cassettes cover model/tool recovery and
CLI-to-IM continuation; CI receives no real model or cloud production key.

Daily-driver evidence tracks clear terminal outcome rate, unexpected
interruptions, automatic recovery success, repeated strategies, unknown-effect
verification, watcher outcomes, post-failure approvals, and Linux/macOS
differences. Legacy behavior is removed in the next beta only after migrations
and rollback are proven, CI is green, critical evals are 100%, and diverse
daily-driver tasks cover local edit, external mutation, supported and
unsupported watchers, daemon/model interruption, and CLI-to-IM continuation.

Status: implemented locally on 2026-09-01. Existing Go and selfcheck jobs remain
the release gates; no workflow or production credential was added. The daily
report now derives automatic-recovery schedules and child outcomes, recovery
guardrail refusals, unknown-effect verification mode, wait-group outcomes, and
post-failure approvals from durable events. Full local release gates and the
installed npm v8-to-v9 migration smoke pass; CI and diverse multi-day
daily-driver evidence determine whether the next beta may remove compatibility
behavior.

## Rollback

New schema rows are additive and versioned. Runtime rollback disables creation
and claiming of the new recovery contract; it does not delete columns or
reinterpret historical rows. Because an older binary correctly refuses a newer
database schema, replacing a v9 binary with a v8 binary is not a normal
rollback. Restoring the verified pre-migration backup is disaster recovery and
may discard newer user data.

## Evidence gates

- `GOWORK=off go test ./...`
- `selfmind selfcheck --fast` during edits and full `selfmind selfcheck` before
  publication
- Linux and macOS Actions, including existing npm/package smoke jobs
- Installed npm package, gateway restart, and actual `selfmind` command smoke
- No duplicate Action workflow and no live production credential in CI
