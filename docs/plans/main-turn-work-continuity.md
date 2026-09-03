# Main-Turn Work Continuity

## Purpose

Make natural conversation the normal way to continue, observe, or add to work
across CLI, IM, restart, and workspace boundaries. The Main model should
understand the user's message once, inside an accountable Run with useful
context. The gateway remains the authority for identity, execution scope,
parent claims, queueing, approvals, and delivery.

This plan replaces the temporary run-external Main continuity admission. It
does not replace explicit commands, introduce another classifier, or let a
model mutate control state directly.

## Ownership

- Owner: SelfMind project owner
- Approver: project owner
- Approved: 2026-09-02
- Review date: 2026-09-09
- Status: paused — superseded at the domain-model level by
  `threaded-work-history-redesign.zh-CN.md`; its remaining real-IM evidence
  gates carry forward there.

## Intended outcome

1. While one Run is active, ordinary user-originated natural language is
   durably steered to that Run. The same Main model decides whether it improves
   the current work or describes independent work that should be queued.
2. While idle, an ordinary audited Main Run starts immediately with a bounded
   work-spine tail and small structured history hints. Main may progressively
   search and inspect older work before proposing a continuation.
3. Explicit controls and structured return edges stay deterministic and
   model-free. Daemon-originated messages never steer work by textual cues.
4. The gateway validates every continuation against person ownership, status,
   execution scope, checkpoint state, and the unique parent claim before
   committing it.
5. Ambiguity normally degrades to separate queued work. Human choice is
   reserved for materially different authority, scope, or irreversible-effect
   outcomes.

## Non-goals

- Changing the one-active-Run-per-person concurrency contract.
- Redesigning general planning, tool retry policy, watcher support, or
  execution recovery. Those mechanisms keep their existing contracts.
- Sharing raw transcripts across endpoints.
- Making `semantic_recall` an authority or a foreground dependency.
- Adding provider-specific, project-specific, or language-specific routing
  rules.
- Keeping two continuity model calls for shadow comparison.

## Core invariants

### Submission and active work

- Explicit commands, approval and clarification replies, claimed choices,
  supplied reply edges, and exact Run controls execute before natural-language
  handling.
- User-originated natural language received during active work is persisted
  before acknowledgement. CLI and IM use the same rule.
- Accepted input is delivered at the next safe agent checkpoint. It does not
  cancel an in-flight tool, sub-agent, or dispatched effect. `/stop`, approval
  rejection, and other explicit control decisions retain higher priority.
- Main may revise the current plan only when the input relates to active work.
  Independent input becomes a durable queued work item; it does not silently
  expand the active plan.
- If Main cannot safely tell whether input is related, the default is separate
  queued work. Clarification is required only when different interpretations
  would change authority, execution scope, or an irreversible effect.
- An exact natural-language request to continue other historical work while a
  Run is active becomes an exact queued continuation. It never switches the
  active Run in place.
- A finalization race cannot lose accepted input. Unconsumed steer input is
  atomically transferred to the next turn queue.

### Idle Main turn and progressive recall

- With no active Run, ordinary natural language creates one normal, audited
  Main Run before semantic interpretation. There is no run-external Main or
  `fast_classifier` continuity decision.
- The initial prompt may contain a bounded person work-spine tail and small,
  structured candidate hints. Hints are evidence, not a forced choice and not
  a hard recency limit.
- A person-scoped, read-only `work_search` searches the complete retained work
  index using deterministic structured filters and FTS. `semantic_recall` may
  expand the query, but failure or timeout falls back to local search without
  blocking the foreground Run.
- A bounded `work_inspect` retrieves structured status, handoff, plan, artifact
  references, and relevant event summaries for selected Runs. It never exposes
  another endpoint's raw transcript.
- Work-history tools use typed `ToolInvocationScope`, bounded result surfaces,
  and the read-only concurrency class.

### Continuation commit and correction

- Main proposes `NEW`, `OBSERVE`, or `RESUME` through a typed broker result.
  It does not write task, Run, workspace, queue, or delivery state.
- RESUME ends the audited interpretation Run with a structured transfer and
  creates a fresh exact-parent child in every execution domain. The child is
  the only Run that claims the parent; its execution scope, checkpoint state,
  and inherited durable plan are established before Main starts. The gateway
  revalidates person, task, Run state, execution domain, and unique parent
  ownership, and execution scope never changes in place.
- An implicit wrong RESUME receives priority correction handling. Before any
  non-read-only effect, approval, clarification, watch, artifact, handoff, or
  outbound delivery, the gateway may perform an audited retarget. After that
  boundary it stops further expansion, explains the observed effects, and asks
  the user how to proceed. Historical events are never erased or presented as
  if the first claim did not occur.
- Parent-domain execution policy wins over the endpoint's current directory.
  A conflict is shown to the user rather than silently changing scope.

### Observation and delivery

- A progress question during active work receives an immediate deterministic
  status card in the source endpoint after its input is durably accepted. Main
  may provide a richer answer at a safe checkpoint.
- An idle OBSERVE is an accountable interaction/reference Run linked to the
  observed work, not a visible ordinary task label.
- Final delivery remains on the originating endpoint. A steering endpoint gets
  acknowledgement and status, not a duplicate final result. Only an explicit,
  validated "send the result here" request creates a structured delivery
  override to a bound endpoint.
- Raw channel transcripts stay channel-local; shared continuity uses only
  structured Runs, tasks, events, handoffs, plans, artifacts, and work-spine
  entries.

## Target flow

```text
incoming message
  -> authenticate person and endpoint
  -> deterministic controls / structured return edges / daemon-origin gate
  -> active Run?
       yes -> persist steer -> acknowledge with status card
              -> Main consumes at safe checkpoint
                   related -> update current work/plan
                   independent or uncertain -> durable separate queue item
                   exact other continuation -> durable exact-parent queue item
       no  -> create ordinary audited interaction Run
              -> compose bounded spine tail + structured hints
              -> Main answers, or calls work_search/work_inspect
              -> NEW / OBSERVE / RESUME proposal
              -> gateway validation and ContinuationCommit
                   same scope -> atomic claim-update
                   different scope/checkpoint -> structured transfer child
                   unsafe ambiguity -> clarify before material effect
```

## Module boundaries

### TurnSubmission

Owns the deterministic decision among `Start`, `SteerExact`, `Queue`, and
`RecoverExact`. It persists accepted active input, emits the acknowledgement
status card, preserves explicit-control priority, and transfers unconsumed
input across restart or finalization races. It does not infer task identity.

### WorkContextBroker

Owns bounded candidate hints plus person-scoped `work_search` and
`work_inspect`. It returns structured evidence only. Optional semantic query
expansion may improve recall but cannot authorize a continuation or block the
foreground path.

### ContinuationCommit

Owns parent validation, same-scope claim-update, cross-scope structured
transfer, and pre-effect audited retarget. Its state changes are transactional,
person-scoped, race-tested across independent database connections, and
recorded as typed events.

## Implementation batches

### Batch 0: Freeze the contract and baseline evidence — complete

- Register this plan as the sole active plan and mark the current run-external
  Main admission as temporary compatibility behavior.
- Record production-path baselines for admission latency, choice/clarification
  rate, correction rate, and provider failures without storing message text.
- Add one internal rollback gate for the transition. It is temporary rollout
  machinery, not a permanent public routing mode.
- Do not expand the current admission seam or add new cassettes that make it a
  long-term contract.

Acceptance: documentation checks pass; the current runtime remains accurately
documented; metrics needed for the cutover verdict are queryable.

### Batch 1: Durable active-Run steer — complete

- Route user-originated natural language to the active Run after deterministic
  controls and daemon-origin checks.
- Persist before acknowledging, render the immediate source-endpoint status
  card, and consume only at safe Main checkpoints.
- Let Main classify consumed input as related, independent, or an exact queued
  continuation. Preserve the current plan for the latter two.
- Atomically move unconsumed input to the next-turn queue when a Run completes,
  interrupts, or the daemon restarts.

Acceptance: CLI and IM steer tests prove no loss, no tool cancellation, related
plan revision, unrelated durable queueing, exact historical queueing, and
finalization-race recovery.

### Batch 2: In-turn progressive work recall — complete

- Start one normal Main Run on the idle path with bounded spine and structured
  hints.
- Add `work_search` and `work_inspect` through the normal scoped tool registry.
- Keep local FTS and structured search authoritative; make `semantic_recall`
  optional query expansion with a bounded fail-open path.
- Represent OBSERVE as a reference interaction so default task lists do not
  accumulate progress-question labels.

Acceptance: new work starts without a separate admission call; history older
than recent hints can be found and resumed naturally; semantic recall failure
does not delay or misroute the foreground request; cross-endpoint raw
transcripts are absent from tool and prompt surfaces.

### Batch 3: Validated continuation commit — complete

- Add the atomic same-scope claim-update path and its cross-connection race
  tests.
- Add structured cross-scope/checkpoint transfer and correct-scope child
  creation without mutating an active lease.
- Add the pre-effect audited retarget window and the post-effect stop-and-ask
  path.
- Preserve exact explicit `/resume` and structured reply behavior.

Acceptance: duplicate parent claims fail without partial state; cross-workspace
resume never changes scope in place; checkpoint resumes retain their existing
reconstruction fidelity; a wrong pre-effect implicit resume can be corrected
with a complete audit trail.

### Batch 4: Delivery and interaction projection — complete

- Keep one final-result destination by default while acknowledging every
  accepted steering endpoint.
- Add the explicit bound-endpoint delivery override.
- Project interaction/reference Runs without promoting them to ordinary task
  labels or hiding their audit and spine entries.

Acceptance: CLI work can be queried from IM immediately, its result still
returns to CLI unless explicitly moved, and `/tasks` does not fill with
progress-question labels.

### Batch 5: Cut over and remove temporary admission — complete in code

- Enable the Main-turn path by default after all mandatory gates pass.
- Remove the run-external Main admission, its timeout/mode configuration,
  obsolete resolver code, and superseded eval cases/cassettes together.
- Keep `fast_classifier` only for separately documented roles such as approval
  triage; it has no continuity authority.
- Remove the temporary rollback gate after installed-runtime and daily-driver
  evidence meet the review verdict.

Acceptance: one foreground Main understanding pass per ordinary idle message;
no continuity provider call exists outside a Run; docs, config references,
tests, and cassettes describe only the landed mechanism.

## Implementation status (2026-09-02)

Landed locally and enabled by default:

- active user input is persisted as steer, acknowledged with status, consumed
  at a safe Main checkpoint, and transferred to the foreground queue if the Run
  finalizes before consumption;
- Main can leave related guidance in the current Run/plan, queue independent
  input, or queue an exact historical continuation without changing the active
  execution domain;
- idle natural language creates one audited Main Run with no run-external
  admission call;
- person-scoped `work_search`, `work_inspect`, and advisory `work_select` are in
  the production and eval registries; OBSERVE/reference interactions are hidden
  from ordinary task lists while retaining their Run and event audit;
- gateway validation applies the no-material-effect boundary and creates a
  correctly scoped exact-parent transfer child; natural-language progress
  sentences no longer become deterministic continuation claims merely because
  they contain words such as "just now";
- production-path eval cassettes cover both a fresh request beside old work and
  cross-endpoint natural-language progress through search/inspect/observe.
- every RESUME creates an exact-parent transfer child under the unique parent
  claim, including same-domain work; the child durably inherits the parent's
  plan before execution, and independent database connections prove only one
  claimant wins;
- one pre-effect `work_select` correction is retained with `correction_of`
  audit data, while the material-effect boundary rejects later retargeting;
- `set_delivery_target` accepts only a server-issued steering input from a
  bound non-CLI endpoint and stores a restart-safe per-Run delivery override
  (schema v10);
- the run-external Main resolver, compatibility timeout/mode, rollback
  environment gate, eval seam, and in-memory delivery override are deleted.

Code, local deterministic evidence, packed-package lifecycle smoke, and local
npm install/launchd restart against the v9 daily-driver database are complete.
The migration created a verified v9-to-v10 backup and the restarted daemon
reports schema 10/10. Still open before the plan review verdict:

- repeated real CLI-to-IM progress, correction, restart, scope-transfer, and
  explicit delivery-move evidence through the 2026-09-09 review.

## Mandatory validation matrix

All rows are release-blocking for the cutover:

| Scenario | Required evidence |
| --- | --- |
| Active CLI/IM input | Durable steer, immediate status, no lost accepted input |
| Related vs independent input | Related input revises current plan; independent input queues without altering it |
| Cross-endpoint progress | Immediate structured status without waiting for the active Run to finish |
| Idle new work | Fresh work is not captured by history and makes no run-external Main call |
| Older history | Natural language finds and resumes work beyond recent hints through progressive search |
| Cross-workspace resume | No mid-Run execution-scope switch; correct-scope child or clarification |
| Recall degradation | Local FTS remains usable when `semantic_recall` times out or fails |
| Wrong implicit resume | Pre-effect correction is auditable; post-effect handling stops and asks |

Additional invariant tests cover explicit-control priority, daemon-origin
isolation, person partitioning, bounded tool output, restart recovery, final
delivery ownership, checkpoint fidelity, and cross-connection parent-claim
races. User-visible message paths require production-path eval cases and
committed replay cassettes; migrations and transactional mechanics use Go
tests.

## Cutover metrics and verdict

The 2026-09-09 review uses:

- critical eval pass rate (must be 100%);
- accepted-steer loss or duplicate rate (must be zero);
- incorrect implicit RESUME corrections and their effect-boundary class;
- clarification/choice rate and false-new-work reports;
- foreground latency with and without optional semantic expansion;
- multi-endpoint daily-driver evidence across restart and workspace changes.

Elapsed time alone does not approve the operational verdict. A failed mandatory
row is fixed on the single Main-turn path; it does not reintroduce
`fast_classifier`, the deleted admission, or another model seam.

## Rollback

Rollback means restoring the last compatible binary and its verified database
backup only if a schema change requires it. The v10 delivery table is
capability-inert for historical rows, and older binaries ignore it. Durable
steer, queue, audit, and parent rows remain readable; rollback never rewrites
them or changes execution scope.
