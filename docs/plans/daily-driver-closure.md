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
- Status: paused while
  [`external-skill-packages.md`](external-skill-packages.md) holds the active
  slot. The review date stands; no scope item is withdrawn.

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
10. Close the reviewed Skill-presentation hardening gaps before merging or
    releasing the new presentation contract: exact curator/delivery budgets,
    repairable oversized legacy mains, canonical usage evidence, and
    actionable cross-surface diagnostics.
11. Replace heuristic Skill-evolution claims with the accepted learning and
    repair lifecycle in `docs/skill-learning-and-repair.md`: observation-only
    Fast Path profiling, workspace-first learned assets, class-specific repair
    evidence, and bounded staleness review.

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
10. **Skill presentation hardening:** close the three merge blockers first,
    then align bundle/API budgets, stale-ref recovery, Doctor remediation, and
    compatibility cleanup without weakening the hidden rollback path.
11. **Skill learning and repair:** correct false shadow semantics first, then
    introduce control-managed workspace scope, deterministic-versus-semantic
    repair thresholds, and dependency/verification freshness evidence without
    adding a foreground model call.

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
- A curator-created package that is authorized for automatic publication is
  delivered as `full` by the production activation builder under the same
  resources, byte budget, and token budget. An already paged legacy main can
  receive a narrow repair only when its instruction bytes and estimated tokens
  do not grow and unrelated sections/resources remain identical.
- Skill stats derive from durable activation/work-unit outcomes or are labeled
  historical. Bundles and HTTP clients report the executing agent's real
  aggregate budget. Candidate identity drift returns `candidate_stale` with a
  refresh action. Doctor distinguishes registered, hidden, and provider-visible
  schemas and provides an exact safe repair plus verification command for every
  runtime-remediable finding.
- Ordinary successful runs increment observations only: they never count as a
  shadow match, authorize Fast Path advice, or revive a degraded candidate.
  Automatically learned Skills default to a logical workspace scope without
  writing generated files into the user's repository. Repair publication is
  tied to the exact failed parent version and uses the class-specific evidence
  thresholds in `docs/skill-learning-and-repair.md`.
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

### Skill Presentation Review (2026-08-25)

The main implementation direction is accepted: per-Skill tools are hidden from
provider catalogs, Doctor previews the real adapter catalog, candidate refs are
required, active-main and catalog budgets are context-proportional, delivery is
immutable through compaction, and model/slash/binding activation shares one
service. The review found the following remaining gaps; verdicts distinguish a
real defect from an overstated diagnosis so fixes target the actual boundary.

| # | Review verdict | Required treatment |
| --- | --- | --- |
| 1 | Partly confirmed, merge-blocking. Removing `SkillMetricsMiddleware` stopped writes to legacy call/fail counters, but those counters are not the curator's canonical decision source and the old pruner only removes stale metric rows. | Migrate `/skills stats` and any remaining consumer to durable activations and terminal work-unit outcomes, label old rows historical, then remove the middleware, unused store parameter, and obsolete metric-row pruner together. Do not restore middleware merely to preserve a misleading counter. |
| 2 | Confirmed, merge-blocking. Curator source authorization exceeds the production delivery body budget by at least the envelope allowance and diverges further with resource paths; raw-byte validation also misses the token ceiling. | Resolve one real `RuntimeContextBudget`, sort proposed resource paths, and run the exact activation delivery builder. CREATE may auto-publish only when the result is `full`; remove the `+512` heuristic. |
| 3 | Confirmed, merge-blocking. Narrow PATCH preserves unrelated bytes, but whole-main validation rejects every already oversized Skill, so paged legacy assets cannot be repaired automatically. | Apply the dual PATCH rule from the Skill contract: current full mains stay full; current paged mains may not grow in instruction bytes or estimated tokens and must preserve resources/unrelated sections byte-for-byte. Package compaction is a separate workflow. |
| 4 | Main allegation rejected; remediation gap confirmed. Terminal work-unit and run-finalization transactions already delete issued refs and a focused test covers normal expiry. Doctor can still find terminal/orphan rows that normal cleanup did not reach, but currently offers no repair. | Add dry-run-first `selfmind maintenance prune-skill-candidate-refs`, transactional `--apply`, Doctor ownership details, and a post-repair verification command. |
| 5 | Confirmed with corrected scope. Bundle delivery uses the fallback budget, but a bundle is a multi-Skill aggregate rather than a fourth single-Skill activation path. Giving each member the full budget would multiply context cost. | Feed the executing gateway budget into bundle resolution and fairly allocate one aggregate byte/token ceiling across members. Test the total, not per-member equality with model/slash/binding. |
| 6 | Partly confirmed. Re-reading by name is intentional live precedence/drift detection and `expectedSkillKey` prevents incorrect activation; candidate identity mismatch currently falls through to an unstructured task-binding error. | Preserve the second resolution check, but return structured `candidate_stale` plus `skills_list` refresh for issued refs. Keep binding-specific fallback wording only for task bindings. Add a root-precedence drift test. |
| 7 | Confirmed. HTTP `messageContextBudget` reconstructs the default budget instead of reporting `Gateway.RuntimeContextBudget`. | Report the executing gateway/agent budget and test unknown, 32K, and 128K metadata cases. |
| 8 | Confirmed diagnostic ambiguity. `tools=active` counts registered hidden schemas while the provider catalog is smaller, so the label suggests the wrong denominator. | Report `registered_active`, `hidden`, and `provider_visible` separately; keep adapter preview as the provider truth source. |
| 9 | Mixed. Hidden `skill:<name>` dispatch is dormant in normal slash flow but remains directly dispatchable and tested; its `skill.activated` event lost dedicated coverage. Worker budgets are currently homogeneous, so heterogeneous-worker drift is a future seam rather than a present defect. | Treat legacy dispatch as a named rollback channel with event coverage and removal criteria. Assert homogeneous worker budgets now; carry per-agent budget at checkout only when heterogeneous workers are introduced. |

#### Merge and Follow-up Gates

- **P0, before merge:** close findings 1-3 and add focused tests for a
  resource-heavy CREATE, token-heavy/CJK main, paged legacy PATCH non-growth,
  PATCH growth rejection, and canonical durable stats.
- **P1, immediately after P0:** close findings 4-7 with bundle aggregate-budget
  tests, root-precedence `candidate_stale`, real HTTP budget cases, and a Doctor
  prune/verify test.
- **P2, cleanup after shadow evidence:** close findings 8-9, restore explicit
  compatibility event coverage, and remove dormant code only when usage
  telemetry satisfies the documented removal criteria.
- The release gate remains `selfmind selfcheck`; provider-visible catalog and
  message-path changes also require the focused Go suites and the cassette-backed
  Skill lifecycle eval path. Repository tests alone do not authorize installing
  or restarting the daemon.

#### Implementation Closure (2026-08-25)

Findings 1-9 are closed in the repository implementation. `/skills stats` now
aggregates durable activations and terminal work-unit outcomes; the legacy
middleware/store/pruner path is retired and old metric rows are explicitly
historical. Curator CREATE uses the exact production byte/token/resource
delivery builder, while paged PATCH repairs cannot grow instruction bytes or
estimated tokens. Bundles share one executing-agent budget, HTTP reports that
same budget, issued-ref root drift returns `candidate_stale`, and worker pools
assert homogeneous budgets.

Doctor now names `registered_active`, `hidden`, and `provider_visible`
denominators and reports exact terminal/orphan candidate-ref owners. The new
`maintenance prune-skill-candidate-refs` path previews by default, applies one
transactional live-ref-excluding query, and verifies an empty remainder.
Dedicated legacy `skill:<name>` activation-event coverage remains in place as
the rollback channel's removal gate. The cassette-backed local-full release
path is green. Installed-binary/daemon evidence and sustained observation remain
open; these are not unimplemented presentation-contract items.

### Skill Learning and Repair Implementation (2026-08-26)

The P0-P3 core mechanics in `docs/skill-learning-and-repair.md` are implemented.
Workflow profiling is observation-only until a real comparison contract exists.
Curator CREATE defaults to an isolated control-managed workspace root; repair
classification uses daemon-observed failures and class-specific thresholds;
semantic candidates accumulate immutable evidence snapshots; exact repaired
regressions quarantine; and environment-bound guards plus verified-compatible
previous-version checks preserve the ordinary-planning path.

Control schema v5 persists guard environment, dependency fingerprints,
verification environment, and last-verified time. Its ordered migration has a
focused v4-shape test and the released beta.15 fixture still upgrades and
reopens through the current schema. Remaining gates are sustained personal
workflow evidence, installed-binary/daemon verification, and a separately
designed explicit or cross-workspace route for widening a learned Skill to user
scope.

## Exit Verdict

At the review date, archive this plan only when the code gates and observation
gates have explicit evidence. If implementation is complete but an observation
window remains open, keep the plan active with a new reviewed date and a narrow
remaining-evidence list. Do not mark the plan complete from unit tests alone.
