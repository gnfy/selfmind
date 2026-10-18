# Runtime Closure Repair: Agent Execution Guide

[中文执行指南](runtime-closure-execution.zh-CN.md)

Prepared 2026-10-15 for the existing [session-concurrency plan](../plans/session-concurrency.zh-CN.md), whose review date is 2026-10-18. This is a subordinate execution guide, not another active plan. The owner authorized the A–F implementation below on 2026-10-17. Installation follows the release gate; real IM tests remain owner-deferred.

## A–F implementation (2026-10-17)

This sequence supersedes R0–R5 as the execution order; those packages remain
regression evidence. Main judges meaning; runtime coordinates existing ledgers.

| Batch | Change | Exit |
| --- | --- | --- |
| A | Freeze the dirty baseline; isolate temporary Go copies; fix frozen windows and shared read-only argument proof. | Reproducers fail before repair and pass afterward, including ordinary-read controls. |
| B | Separate uncommitted finish-input repair from committed lifecycle limits. | Exact repeats are refused, changed valid input can finish without unrelated verification, and real missing evidence stays unresolved. |
| C | Add explicit `finish_run.plan_patch` with `final_response`. Preview transaction-local Plan/evidence changes and always roll them back; revalidate and commit through `MaterializeRunFinalization`. | Final Plan, findings, body and Run state commit together; changed input/cancellation, write failure and replay cannot create partial success. |
| D | Main explicitly associates versioned current tool observations with conditions. Preserve source facts and frozen windows; retain independent progress in new recovery-contract v4 Runs. | Static reads cannot prove executable gates; unsupported historical output stays inert; accepted associations survive read-back and exact continuity. |
| E | Integrate retrieval, context facts and safe read batching without model-specific policies. | Representative tasks need fewer bookkeeping/correction calls with unchanged evidence and scope protections. |
| F | Run full gates, paired live-provider checks and package/install verification against one frozen source. | Report quality, actual model calls, active elapsed time and token cost; identify deferred evidence rather than claim parity. |

The compound finish contract preserves existing obligations and changes current
step progress; revise criteria/scope through normal planning first. Existing
outcome-only and summary-only calls retain their supported behavior. Preview
success is not a commit: pending input, cancellation and relevant facts are
rechecked inside the write transaction. Only an acknowledged commit ends the
loop, and the receipt carries the exact accepted Plan for display. Do not add
another finalizer, independent state store, or model-name fallback.

## Start here

On the current assignment, execute A–F in dependency order; use R0–R5 as regression coverage. “Ready” means the package follows existing policy; the assignment supplies execution authorization. C1 requires a separate owner decision. C2 and bounded X3 follow their recorded authorization below; X1/X2 require an explicit experiment budget. Deferred experiments do not block ready repairs. Report an expired parent-plan review before expanding scope.

Read `AGENTS.md`, [STATUS](../STATUS.md), and the domain document named in the package. This guide owns this repair sequence; domain documents own product contracts and STATUS owns repository priorities. Earlier local proposals and reviews are research references, not competing execution instructions. A conflict with an existing contract stops only the affected change for review.

Success means correct completion or correct waiting, no repeated effects, no scope leakage, and fewer unnecessary recovery steps. Main interprets goals, evidence sufficiency and user takeover; runtime owns identity, scope, permissions, observed execution and durable transitions. Keep the existing Run, Plan, evidence, effect-claim and selector path. Function layout remains an implementation choice.

## Decisions fixed for this repair

| Topic | Required behavior |
| --- | --- |
| Unknown effects | Related observation explicitly bound to the claim, or audited confirmation by the person. Age and unrelated successful queries settle nothing. |
| Authorization | Current restrictions and the hard floor remain effective after an old grant matches. Smart host judgment retains existing policy. |
| Context selection | Existing person-scoped hints before selection; exact-Run context after validated selection. Thread grouping never selects execution context. |
| Tool input | Keep unknown semantic fields rejected and existing bounded multi-error feedback. Preserve legacy supported inputs; do not guess missing states or silently discard acceptance information. |
| Final answer | Prefer accepted `final_response`; preserve legal summary-only and explicit final-answer-phase compatibility. Prose alone is not completion authority. |
| Waiting | Verify the actual asking session/Run; keep existing bounded hold, pressure/drain park, exact checkpoint and cancellation behavior. |
| Process deadline | A hard execution timeout retains termination semantics. Managed yielding is separate future design. |
| Freshness | Preserve past observations; Main judges present applicability. Reuse still validates source, scope, criterion and recorded input changes. |
| Models | Use registered contracts and protocol/capability adapters. Keep the production model configuration unchanged during repair validation. |

## R0 — Establish a reproducible baseline

**Reason:** the inspected checkout has interleaved tracked and untracked changes. File modification time cannot establish ownership or the source of an installed package.

1. Verify the base and manifest of a supplied handoff snapshot, or capture HEAD, status, staged/unstaged diffs and non-ignored untracked files. Preserve a recoverable local snapshot outside tracked source, including required untracked source/tests/cassettes. Record its content manifest and hash; recheck captured inputs for concurrent changes. Keep credentials and production databases outside the handoff.
2. Reconstruct that snapshot in an isolated checkout. A worktree created from HEAD alone does not contain dirty changes. The implementation checkout must reproduce the recorded content before edits; keep unrelated changes out of the repair patch.
3. Build the candidate binary from that checkout. Save its exact source/snapshot identity and initial gate results. Reproduce the five synthetic failures in [checks S1–S5](runtime-closure-checks.md); a changed source may already contain a fix, in which case verify it instead of reverting it.
4. Produce a package inventory: `missing`, `present-needs-verification`, or `verified`, with source and test references. Existing compact inputs, short IDs, artifact output, carry/adoption, finish correction and hold/park are integration inputs, not new features to implement twice.

**Exit:** another agent can reconstruct the same source and run the synthetic checks without a private database or a personal temporary directory. R1 begins from that recorded snapshot. R0 does not choose a package name, install a binary, or authorize publishing.

## Ready repair packages

| Package | Dependency | Domain and existing owners | Acceptance |
| --- | --- | --- | --- |
| R1 Execution truth and authorization | R0 | [Tool safety](../tool-safety.md); `tools` middleware/observation/grants, `httpapi` error finalization | S1–S5 |
| R2 Hold/park session fidelity | R1 | [Worker pool](../worker-pool-design.md), tool safety; existing HTTP hold, attachment and checkpoint paths | H1–H4 |
| R3 Accepted state and exact continuity | R1 | [Work timeline](../work-timeline.md), [context lifecycle](../context-lifecycle.zh-CN.md); `control` projections, gateway selector, kernel consumers and CLI renderers | P1–P4 |
| R4 Contract and finalization integration | R3 | [Coding foundations](../coding-agent-foundations.md), [eval loop](../eval-loop.md); existing tool preparation, finish recovery and result materialization | F1–F4 |
| R5 Manual continuation working set | R3 | Work timeline and context lifecycle; existing hints, selection and resume context | M1–M3 |

### R1 — Execution truth and authorization

Remove implicit same-chain bulk settlement; use the existing explicit observation binding. Keep current intent/restriction checks effective before dispatch regardless of grants. Distinguish provider timeout under a live caller context from caller cancellation; retain accepted durable outcomes and unresolved effects independently.

Prove read-only operations from their arguments: inspecting `hostname` is different from setting it. Restore conservative treatment of interpreter/Skill scripts; path shape or basename must not mint reusable authority. This package does not design a replacement Skill-grant family.

Patch the faulty behavior, not whole files or “another agent's layer.” Individually review any retained environment/proxy/grant normalization. Exit when S1–S5 fail for the intended reason on faulty behavior and pass on the repair, with positive controls preserved.

### R2 — Hold/park session fidelity

Reuse existing hold/park and checkpoint code. Establish whether the asking session still carries this Run's wait; another CLI's heartbeat is insufficient. Reuse attachment facilities rather than adding a parallel presence framework. Keep the existing maximum hold, queue-pressure yielding and daemon-drain behavior.

Arm the exact pending-call checkpoint before holding. Consuming an answer and removing its pending binding remain atomic. If checkpoint storage fails, retain the existing cancellable/fail-closed path; do not pretend the worker was safely released. Late or revoked authority cannot dispatch.

Exit when held and parked answers converge on the same durable result, and two-CLI, pressure and crash cases H1–H4 pass. No default concurrency or timeout configuration changes belong here.

### R3 — Accepted state and exact continuity

Use the existing accepted `RunPlanProjection`, identified by `(run_id, plan_version)`, across storage, selector, kernel unresolved-work tracking and UI. Requested arguments and late parent events cannot replace newer accepted state. Keep kernel independent of the database and concrete tools.

For exact runtime continuations, carry legal progress and provenance without asking Main to select itself again. For manually selected history, preserve the existing conservative import and explicit Main adoption. Invalidate related reuse on changed scope, criterion, relevant inputs or current failure; preserve unrelated historical facts. A past build success is not proof of current production health.

Exit when P1–P4 compare those consumers after approval, watcher, provider-wait and manual selection, including repeated recovery and stale events. Fix actual duplicate derivation; do not create a second writable “unified state.”

The 2026-10-16 follow-up reads current Plan and closure facts together, exposes
them in selected Run context and committed manual selection, and keeps historical
attachment separate. Verify parentless accepted Plans, current/parent evidence
isolation, changed inputs, unavailable previews and person boundaries without
mutating Plan status merely to obtain references.

### R4 — Contract and finalization integration

Use existing compact Plan input and complete accepted receipts. Keep bounded preparation feedback: whether dispatched, the actionable problem and the permitted correction, with full diagnostics retained internally. Unknown fields and near-name suggestions do not authorize automatic semantic repair.

Use the existing accepted final-body path and ordered final Plan/finish calls. Retain summary-only and native-phase compatibility; missing optional `final_response` is not automatically an input error. A proven not-dispatched format error may use bounded correction; human rejection or unknown effects never unlock a blind retry. Reuse result materialization and delivery/outbox transactions rather than adding another finalizer.

Exit when F1–F4 pass: accepted body is not regenerated, malformed input can be corrected within the existing limit, storage/delivery failure cannot repeat effects, and inspection versus required remediation/user takeover produces the correct outcome. The bounded X3 follow-up below handles same-scope progress; permission and failure barriers remain.

A 2026-10-16 follow-up lets Main select a condition by current `step_id` at
finish instead of recopying call ids. Runtime binding uses current checks or
already accepted adoption, preserves Main's result/reason and source-qualified
v2 findings, and refuses ambiguity or stale/unconfirmed evidence. Historical
bare ids and unadopted proof stay unavailable; v1 findings retain their meaning.

The owner's 2026-10-16 continuation authorizes explicit changed-step input:
`mode: patch` retains omitted steps, expands and validates in the existing Plan
transaction, and matches the runtime-owned version. Full `snapshot` input stays
compatible. Verify complete receipt/event/continuation equivalence, stale and
foreign ids, proof/cancellation gates, concurrent writers and failed commits.
A live probe showed ordinary status advancement still held work calls. The owner's continuation also covers bounded X3 progression within the same full Plan
identity and Work Unit, with normal middleware and steering/failure barriers.
Atomic Plan/outcome finalization remains separate work.

### R5 — Manual continuation working set

Extend existing bounded `WorkContinuityHints` only where needed: exact candidate Run, relevant recent paths, and bounded failure facts. Main still selects the intended Run. After validated selection, reuse `resume_context` for that Run's working set. No extra ingress classifier or Thread-recency selection is introduced.

Old Skill identity/version may be a reference, never automatically active instructions. “Previously read” is not a current file-content assertion. Keep raw transcripts channel-local and budgets in the existing selector. Necessary task identification and fresh verification are not eliminated to meet a call-count target.

Exit when M1–M3 distinguish A/B in one workspace and two workspaces, a changed file, an expired Skill and another person's history. If existing hints already satisfy the checks, record verification instead of adding another slice.

## Local retrieval follow-up (2026-10-16)

The owner authorized the first fact-chain repair batch: correct local search,
line windows and name filtering; preserve compound output before clipping with
the existing capture/artifact path. Keep the legacy list compatible in this
batch: a recorded approval continuation still invokes it, and its retirement
requires a separate Plan/recovery compatibility change.
Reconstruct the current dirty snapshot as in R0 and return only this increment.
Keep Plan state transitions, display, approvals, reasoning and finalization
contracts unchanged. Cover missing roots, literal/regex semantics, bounded
output, artifact failure/read-back and person scope. Record the production
message path with synthetic files, then replay and run the existing release
checks; live probes do not authorize production operations or deployment.

Exit with recovered middle facts, truthful incomplete coverage, unchanged Plan
behavior, the exact tested source identity and separate offline/live evidence.

## Separate decisions and experiments

| Item | Current status | Required next action |
| --- | --- | --- |
| C1 Approve and settle one earlier claim | Owner decision pending | Review the exact semantics below; existing `/effects settle` and once approval remain unchanged until approved. |
| C2 One extra judge attempt | Authorized on 2026-10-15 | Implement the shared budget and failure classes below; final fallback remains human review. |
| X1 Reasoning comparison | Experiment, not enabled | Explicit isolated-route/call/time/cost budget, fixed binary, paired tasks, quality and rescue checks. |
| X2 Multi-call generation | Experiment, not enabled | Probe actual protocol behavior; omission of `parallel_tool_calls` does not by itself prove the cause of single-call output. Execution remains under existing read-only concurrency policy. |
| X3 Normal Plan advancement in a batch | Bounded implementation, 2026-10-16 | Validate full-scope monotonic advancement, unchanged middleware, new-input barriers and real-provider behavior; broader batching remains excluded. |

**C1 proposed contract:** offer the compound option only for one explicitly identified, currently settleable claim. Multiple unresolved claims use existing inspection/individual settlement controls. The option is non-default and says the person knows the earlier outcome and approves this exact call once. Validate the offered option, active actor, pending approval and claim again in the existing control transaction. Commit settlement audit, approval decision and existing durable notification intent together, before waking execution. Exact duplicate submissions replay the recorded result; an independently changed/expired object rejects a stale compound decision without partial writes. Keep ordinary once approval available, and normal dispatch checks still apply. Validate C1 checks before enabling; routine “yes” never settles a claim.

**C2 contract (owner authorized by the follow-up implementation request on 2026-10-15):** at most two judge attempts under one total deadline of 15 seconds, bounded by any shorter caller/provider budget; first attempt at most 10 seconds and at most 80% of the effective remaining budget, second uses only the remaining time. Retry only timeout/transport/empty-response unavailability, never a valid deny/escalate or caller cancellation. Include transport retries in that deadline, fence late answers, and stop if the intent snapshot changes. Still unavailable means human review with an explicit reason, never approval. Validate C2 checks; do not renew an exhausted shorter budget. This authorization does not enable C1 or X1–X3.

For X1, official [Qwen documentation](https://docs.qwencloud.com/api-reference/chat/openai-chat) states that `high` maps to `xhigh`; `medium` is a lower setting, not a repair of an invalid value. Record the wire configuration and usage. Do not upgrade code and change production reasoning together. Automatic per-step reasoning, hard-timeout background conversion and generic unknown-field acceptance are outside this repair.

## Verification and handoff

Use the commands and case mapping in [the check sheet](runtime-closure-checks.md). Every changed message path updates an existing eval or adds a production-wiring case with replay evidence; pure mechanics use Go tests. Missing existing WIP tests are a baseline issue, not evidence they passed. A matched test count of zero is not success.

Each package hands off: source/snapshot identity; scoped diff; reused versus changed behavior; regression/positive-control results; fault-injection and eval output; untested provider coverage; unresolved decisions. Use local technical choices without requesting repeated permission. A policy conflict, irreversible action or unapproved external experiment remains separately gated.

Before integration run focused checks and fast selfcheck. Before pushing run full selfcheck and required race/build/documentation gates from the same candidate source. Update domain documentation and translations for actual behavior changes; STATUS changes only with capability or priority. Stop rollout on repeated effects, scope leakage, erased unfinished work or unsupported completion. Roll back behavior while preserving durable history.

Measure from a fixed post-cleanup baseline: correct work-chain outcome, extra user rescue, calls and contract failures per logical work, duplicate orientation and answer generation, active critical-path time and token cost. Human absence and external build time are separate. Weekly approval counts and estimated percentage savings are observation targets, not release gates. Go/replay success does not establish cross-model adoption or live performance.

Live tests use isolated data/config and explicit provider budgets. Real IM acceptance remains owner-deferred. Installing the local package, restarting the production daemon, settling production claims and publishing require their own user instruction. A package can be implemented and verified while those deployment actions remain pending; report those states separately.

## Local validation handoff (2026-10-16)

The owner has now authorized completing the required repair checks, replacing the local npm installation and safely restarting its daemon for personal validation. Keep the current model/reasoning configuration and durable history. Verify the installed artifact and running build, not just the source checkout. C1 and reasoning/concurrency experiments remain separate; Plan/outcome atomic unification is not a prerequisite for this bounded trial. Retain strict failed provider probes as evidence, without weakening acceptance or claiming cross-model parity.
