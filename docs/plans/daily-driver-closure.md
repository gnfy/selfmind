# Daily-Driver Runtime Closure

## Purpose

Close the production gaps exposed by sustained personal-edition use without
adding SaaS, remote Runner, or multi-tenant scope. The plan makes runtime
evidence trustworthy first, then fixes deterministic failures, restores
periodic memory governance and delivery recovery, and reduces native tool
schema cost through bounded on-demand exposure.

## Ownership

- Owner: SelfMind project owner
- Approver: project owner
- Review date: 2026-09-11

## Scope

1. Forward provider request fingerprints through every production wrapper and
   make stale or missing diagnostic evidence explicit.
2. Fix FTS query encoding, typed tool-error envelopes, stale work-unit
   references, and stale Skill candidate recovery.
3. Persist memory-governance scheduling state, catch up overdue work after
   restart, and retry soon after foreground deferral under a bounded budget.
4. Diagnose new delivery backlog, coalesce historical final results, and keep
   recovery durable, deduplicated, and non-blind.
5. Correct usage, context, maintenance, approval, and report-window semantics,
   including machine-readable developer evidence.
6. Introduce work-unit-scoped deferred tool activation only after at least
   seven days of request-fingerprint evidence.
7. Track a read-only developer-agent Skill under `.agents/skills` for
   repeatable multi-window runtime audits. A developer-only marker keeps it out
   of the SelfMind runtime and npm package.
8. Close the watcher foreground-liveness gap: prove polling commands are
   observation-only, make successful registration an automatic lifecycle
   handoff, and render terminal writeback as background finalization.
9. Align foreground, background-review, delegation, watcher-finalization, and
   compaction prompts with each role's actual capabilities and resume evidence.

## Non-Goals

- Adding a background LLM role or expanding foreground system prompts with an
  always-on operational audit checklist.
- Weakening approval, sandbox, credential, delivery-session, or tenant safety
  boundaries.
- Treating zero memory additions, zero pins, low recall selection, or zero
  approval denials as defects without eligible denominators and outcome data.
- Blindly replaying historical `sent_unconfirmed` or stale final-result rows.
- Adding public configuration for temporary rollout controls.
- Implementing SaaS, enterprise, remote control-plane, or Runner topology.

## Delivery Batches

1. **Evidence clock:** VCR fingerprint forwarding, provider-wrapper conformance
   tests, and memory-report freshness/due state.
2. **Deterministic correctness:** FTS-safe search, stable error envelopes,
   stale plan identifiers, and stale Skill candidates.
3. **Memory liveness:** persistent due state, startup catch-up, short foreground
   deferral, and bounded shadow-first backlog processing.
4. **Delivery closure:** transition diagnostics, new-backlog correction, and
   dry-run coalesced recovery or dismissal of historical final results.
5. **Diagnostic semantics:** complete context totals, real usage reporting,
   uncapped or explicitly lower-bound windows, actionable maintenance state,
   and correlated approval funnels.
6. **Tool economics:** direct/deferred/hidden exposure, monotonic work-unit
   activation, durable resume behavior, and stable schema ordering.
7. **Developer audit:** a concise read-only Skill that compares 24-hour,
   7-day, and 30-day/all-time evidence and labels facts, inferences,
   hypotheses, and evidence gaps.
8. **Watcher liveness:** observation-only watcher commands, automatic handoff,
   and background terminal finalization.
9. **Role prompt quality:** capability-derived tool contracts, a dedicated
   background-review surface, bounded delegation context, and evidence-complete
   compaction handoffs.

## Evidence and Acceptance

- At least 99% of calls through fingerprint-capable providers emit a request
  prefix fingerprint or an explicit unsupported reason.
- Search literals such as numbers, punctuation, FTS operators, paths, and mixed
  Chinese/English text never surface raw SQLite syntax errors; multi-term
  natural-language queries retrieve ranked partial matches instead of silently
  requiring every prompt word.
- Model-visible tool failures use stable categories and recovery hints while
  raw local diagnostics remain capture-backed and redacted. One failure has one
  classification line, and external-origin SQL errors remain actionable rather
  than being mislabeled as SelfMind control-store failures.
- Overdue memory governance attempts within fifteen idle minutes, survives
  daemon restart without resetting its due time, and records successful empty
  passes instead of relying on judgement counts.
- Delivery diagnostics separate window inflow, current backlog, historical
  terminal state, and oldest age. Historical recovery produces one audited,
  deduplicated summary rather than replaying every result.
- Usage and context totals include native tool schemas; report windows expose
  generation time, actual coverage, scanned rows, and truncation/lower-bound
  state.
- Deferred-tool rollout starts only after the evidence-clock baseline exists.
  The target is no more than 6k schema tokens or 20% of request input, at least
  30% lower uncached input per comparable run, no increase in unavailable-tool
  failures, and fewer than 10% extra model turns from discovery.
- User-visible recovery changes have production-path eval cases. Scheduling,
  migrations, aggregation, wrapper forwarding, and exposure mechanics have Go
  tests, including released-database upgrade fixtures where durable schema
  changes are necessary.
- A successful watcher registration produces `waiting_external` without a
  second provider call; while the watch is pending, a new person task starts
  immediately. The short terminal-finalization run remains visible as
  background work and follows the one-active-run queue contract.
- Every role prompt names only tools actually exposed to that role. Background
  review receives no foreground task strategy or repository instructions;
  delegated workers cannot mutate parent lifecycle or durable-learning state;
  and compaction preserves verification, failed attempts, waits, and paths in a
  data-fenced handoff.
- The installed npm/WSL binary and restarted daemon expose the verified build;
  repository-only `go run` evidence is insufficient.

## Observation Gates

- Collect request fingerprints for at least seven days before expanding
  deferred exposure beyond a small cold-tool cohort.
- Observe memory-governance liveness and usefulness for one to two weeks; do not
  use a daily ADD quota as success criteria.
- Observe the automatic-triage and human-decision funnels for one to two weeks
  before changing any approval threshold. Until a stable per-call correlation
  id is persisted across both stages, report them as separate denominators.

## Implementation Status (2026-08-21)

- Implemented: provider wrapper fingerprint forwarding and explicit coverage;
  FTS-safe session search; typed model-safe tool errors; stale work-unit and
  Skill recovery; durable memory-governance scheduling; exact-row delivery
  recaps scoped to the exact platform account; paged/lower-bound reports; and
  corrected usage/context/maintenance semantics. Automatic triage and human
  approval decision funnels are both durable but are not yet claimed as
  per-call correlated.
- Implemented but rollout-gated: direct/deferred/hidden native schema filtering,
  `tool_search` activation, latest-work-unit replay, and same-batch plan-boundary
  isolation. Automatic external deferral has an empty reviewed allowlist and is
  disabled until a seven-day usage/fingerprint baseline selects a real cold
  cohort.
- Implemented: external watcher commands are restricted to proven read-only
  observations, successful registration ends the foreground run directly, and
  TUI finalization no longer presents a foreground elapsed-time clock.
- Implemented: prompt assembly is role- and capability-aware; background review
  has a bounded memory/session surface, delegation has a parent-facing contract
  with fresh loop state and scoped runtime authority, and compaction separates
  its locked contract from untrusted conversation data while preserving resume
  evidence.
- Developer-only audit Skill is tracked at
  `.agents/skills/selfmind-daily-driver-audit`, with one canonical body for
  Codex, Gemini, Qwen, Claude, and other repository coding agents. Its marker
  keeps it out of SelfMind runtime discovery and the npm payload.
- Live npm/WSL package and daemon verification is complete on local build
  `.20260821.5` with control schema v2. Still open: sustained live evidence for
  request fingerprints and memory scheduler liveness; delivery inflow
  diagnosis; an exact per-call approval correlation id; and the review verdict
  for enabling the initial deferred cohort.

## Exit Verdict

At the review date, archive this plan only when the code gates and observation
gates have explicit evidence. If implementation is complete but an observation
window remains open, keep the plan active with a new reviewed date and a narrow
remaining-evidence list. Do not mark the plan complete from unit tests alone.
