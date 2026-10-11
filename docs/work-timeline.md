# Work Timeline — Thread history, Run state, and derived Attention

> **Status: current architecture (schema v11).**
> This document owns work-history, continuation, listing, and recall semantics.

While a Run is active, `/status` selects that exact Run's plan, handoff, and
bounded progress events. It shows the current model/tool phase and event age;
a previous Run's watcher handoff or next steps are not current execution state.
Pending approvals and clarifications remain visible across endpoints.
When closure lacks a current check, feedback also offers explicit prior-check
adoption if the existing provenance and input validator finds that option legal.
Main still assesses sufficiency and supplies `reuse_prior_verification=true`
and `reuse_reason`; prose about reuse never adopts evidence. Failed or stale
checks, changed criteria, scope or inputs retain the current-check requirement.
> `docs/STATUS.md` owns remaining evidence and priority.

## Decision

SelfMind retains every accountable agent turn as a Run and groups related Runs
in a reversible Thread. A Thread is searchable history and presentation
metadata; it never owns execution state. What currently needs action is an
Attention projection derived from Runs and pending control objects.

The person-level work spine supplies bounded continuity across CLI, IM, cron,
and HTTP. A Thread may improve search and display, but a mistaken automatic
grouping must never select execution scope, permissions, or prompt context.
Each prior spine turn enters a new Run as a provenance-marked reference record,
including its exact Run ID when available and how that turn ended
(`turn_outcome`: a `finish_run` status, `completed`, or `incomplete: <reason>`).
The outcome describes that turn, not the Run's current state. Its former user
text is quoted data, not a live user-role message. An absent final answer means
only that the spine did not record one; it does not prove that the prior Run is
pending. The current
input remains the sole active user instruction. A and B may both remain open in
one workspace or different workspaces; Main uses exact Run inspection and the
validated selection path to continue either one.

## Domain model

```text
Person
  ├─ endpoint-local Conversation transcripts
  ├─ person-level Work Spine (slim user/final turn entries)
  ├─ Memory (stable preferences and corrections)
  └─ Threads (searchable groupings)
       └─ Runs (accountable execution attempts)
            ├─ events / handoff / artifacts / plan / tool ledger
            └─ approvals / clarifications / watchers / continuation edge

Attention = projection(Runs, approvals, clarifications, watchers)
```

- **Conversation** is the transcript visible in one endpoint. Raw transcripts
  do not become shared cross-endpoint state.
- **Interaction** is one ordinary retained request and answer.
- **Run** owns status, cancellation, recovery, plan, effects, and timestamps.
- **Thread** owns only identity, title, summary, kind, visibility, pinning, and
  activity time. It has no status, blocker, next-step, or active-Run column.
- **Work Thread** is a Thread with durable work evidence and `listed`
  visibility.
- **Attention** is current actionable work. It is not persisted as a Thread
  lifecycle and can be dismissed without rewriting history.
- **Memory** stores durable personal preferences and corrections, not work
  progress, project state, or transcript archives.

Historical `task_*` ids are still accepted as references for continuity with
older transcripts, but the tasks and task commands are gone: they presented a
Thread list, and Attention derives per exact Run.

## Why the old Task lifecycle was removed

The former Task row mixed four different concerns: history grouping, execution
status, a current-task pointer, and a user-maintained to-do list. Real usage
showed the failure mode: unrelated requests accumulated under one Task, old
`waiting_user` outcomes stayed open indefinitely, and `/resume` displayed work
the user already considered finished.

Three routing strategies were also rejected from production evidence:

1. Blind recency attach silently captured new work under an unrelated Task.
2. An ingress LLM classifier decided which context to load before it had that
   context, added latency, and could corrupt execution authority.
3. A sticky `current_task` pointer made display state act like continuation
   evidence.

Schema v11 therefore performs a single-source migration:

- `tasks` becomes `threads`;
- `task_runs` becomes `runs`;
- subordinate `task_id` references become `thread_id` where they name the
  grouping;
- `current_task` is removed;
- old Task execution columns are not copied into Thread;
- historical public-upgrade rows remain visible as `work + listed`, while new
  ordinary roots start as `interaction + unlisted`.

Migration is ordered, atomic, and preceded by a verified `control.db` backup.
There is no dual-write or long-lived legacy table.

## Ingress, grouping, and promotion

IM adapters save a platform message ID, original input, and person ownership
before gateway dispatch. A receipt moves from `pending` to `dispatching` before
the handler can cause effects and to `accepted` only after the handler returns.
After a crash, `pending` may be retried; `dispatching` is deliberately held for
inspection because an effect may already have happened. A refusal that the
gateway proves happened before admission is not an uncertain effect: a
shutdown drain, an unresolvable sender, a store read that fails before anything
is claimed, or a failed write that would have admitted the input (a queue row,
a multi-Run choice, or a choice claim or route). Each such write is a
transaction or a statement that sees neither the caller's cancellation nor a
deadline, because the SQLite driver reports an ended context even after a lone
statement committed; only then does its failure prove that nothing was saved.
The mailbox writes behind steering follow the same rule. The receipt returns
to `pending`, the sender sees a short cause and that the message will be
retried — internal errors stay in the log — and the adapter receives the typed
not-dispatched result. An answer
to a continuity choice that arrives during a shutdown drain is refused before
the claim, so the choice keeps its saved request for the redelivered answer.
An answer that has already claimed its choice is never refused, since a
redelivery would find the choice used: when its request cannot be saved —
including a steer answer whose Run ended before its follow-up work was
saved — the receipt stays `dispatching` with that reason, and the held result
carries a notice telling the sender to check before sending the request
again. Guidance saved to a busy Run's mailbox stays that input's admission
when back-pressure cannot expire it, or when the Run's finalizer already
deferred it to the queue, so it is neither queued again nor refused. Any later
failure stays `dispatching` with a recorded reason. Each receipt transition —
save, claim, release, accept — runs on its own bounded context that the
caller cannot cancel, so a long turn cannot use up the bound of the
transition after it, and a WeChat `Stop()` or a dropped webhook connection
cannot strand a proven refusal as `dispatching`. For a claim, release,
acceptance, or recorded reason the bound covers only the wait for the store:
once its statement starts it runs to completion, because the SQLite driver
reports an ended context even after a lone statement committed. An error from
one therefore means it changed nothing, and a slow claim dispatches its input
instead of leaving a claimed receipt that nothing runs. After a failed claim,
release, or acceptance the gateway rereads the receipt within a fresh bound
and reports only what it says: a claim it then finds `dispatching` is held,
and its reason only fills a reason the claim holding the receipt has not
recorded; a release that finds the receipt `pending`, under its own claim or
a later one, is still a refusal, because the next claim admits one
dispatcher; and an acceptance it finds `accepted` under its own claim still
delivers the reply. When the receipt cannot be read
after a failed claim or release, nothing is promised and the source keeps the
message for its redelivery to find out. After a failed acceptance the turn
has already run, so the outcome is uncertain: the receipt records why when it
can still be written, and the source moves past the message. Only the atomic
claim admits a dispatcher, so concurrent redeliveries of a pending receipt run
it once. The claim also advances the receipt's claim generation and hands it
to its dispatcher; release, acceptance, and a recorded reason are each one
conditional statement that applies only while the receipt is `dispatching`
under that generation, so a transition an earlier claim sends late cannot
reset, settle, or annotate a later claim. A dispatcher that finds the receipt
held or settled under another generation no longer speaks for it: it records
nothing there and holds the outcome as uncertain, never as a refusal that
promises a retry. A receipt saved before generations
existed reads generation 0 and claims from it like a new one; 0 names no
claim, so a receipt already `dispatching` from that time stays held. A
redelivery from a different owner is rejected; otherwise its
receipt's state decides first: an `accepted` receipt is a silent duplicate and
a `dispatching` one stays uncertain. Only a
never-started receipt is compared with the redelivery, and only on the fields
that say what the message is and where it goes — platform message ID, sender,
chat, text, routing edges, and media identity. WeChat uses each media item's
kind, file name, and content key with the reply, approval, and clarification
edges; the webhook uses each attachment's kind, name, and path or URL with the
tenant, task, workspace, and native reply target. A refreshed context token,
timestamp, retry counter, or download parameter does not make it a new
message, except for a WeChat media item without a content key: its download
reference is then its only identity, so a re-issued one reads as a different
message. A rejected redelivery is final: the sender is told once without a
retry promise, the source moves past it, and a never-started receipt records
why. Weixin commits its sync cursor only after each message in the batch is
accepted, durably held as uncertain, or rejected; a refused message keeps the
cursor in place and is fetched again with backoff, including after a restart.
Its retry notice, a rejection notice, and a held answer's notice are each sent
once; no other uncertain outcome reaches the chat. `/diag inbound` lists
only the current person's unresolved receipts with their recorded reason; it
never retries them. Historical first-seen rows remain terminal on upgrade.
Work-bearing IM messages without a stable platform ID are rejected before
dispatch; otherwise an identical redelivery cannot be distinguished from a
second legitimate request.

Every accepted root user turn creates a fresh `interaction + unlisted` Thread
and a Run. The Run remains searchable even when the Thread is absent from the
ordinary work list. A continuation child inherits its exact parent Thread.

Automation may promote a Thread in place to `work + listed` only when durable,
language-independent evidence exists:

- a multi-step plan is persisted;
- a non-read-only effect is recorded in the tool ledger;
- an approval, clarification, or external watcher is created (this promotes
  immediately);
- a Run ends resumably (`waiting_user`, `waiting_external`, `blocked`,
  `verification_partial`, or `interrupted` with work evidence);
- a deliberate exact-parent continuation is created; or
- the final handoff contains next steps or changed files.

Lifecycle tools (`finish_run`, `update_plan`, `work_select`,
`queue_user_input`) and read-only tools are not evidence. Neither is a command
the dispatcher proved read-only: the ledger records it with effect class
`observation`, so `git status` alone does not make a question resumable work.
The replay class stays `side_effect` for recovery. An OBSERVE projection never
demotes an evidence-bearing Thread.

Trusted built-in calls that only write runtime attribution carry ledger effect
class `run_bookkeeping`. Skill activation, bounded Skill viewing and fallback
do not close the continuation window or count as target execution. Their retry
and approval rules stay unchanged; historical unclassified rows stay guarded.

A tool-free direct answer remains unlisted by default. Promotion is monotonic
for automation: it never hides a listed Thread and never reopens an archived
Thread. Only an explicit person control may reopen an archive.

Main may advise `NEW`, `OBSERVE`, or `RESUME` through bounded work-history
tools, but the gateway owns every state transition. Optional model disposition
must not block foreground work and cannot grant execution authority.
For an otherwise new user-originated turn, the selector also places at most
three same-channel-first Attention cards in `TaskRuntimeContext`. These cards
contain bounded structured Run state, never transcripts or tool output. They
make a short reply discoverable by the normal Main call even when
`semantic_recall` deliberately skips short text; they remain hints, and Main
must call `work_select` before observing or resuming prior work. Explicit
`/new` and daemon-originated turns receive no such hints.

## Continuation authority

Explicit controls and structured edges take priority, but only for the
person's own input, and only over the person's own work: a client-supplied
`task_id`, reply edge, approval, question, choice, Run, or watcher id that
names another person's work reads exactly as an id that does not exist. A
`task_id` the person does not own therefore starts a fresh root, because a
Run takes its owner from its Thread:

```text
daemon-origin gate: task text only, a command is refused
  -> explicit /new, /resume, /choose
  -> structured approval / clarification / platform reply edge
  -> active Run: durable steer
  -> idle: ordinary Main Run with bounded Attention hints and history tools
```

- Daemon-originated text never acts as a control. A scheduled prompt is
  stored text, often the model's own words, so the gate comes first: such a
  turn skips every explicit control, approval and clarification answer,
  numbered choice, and continuity control, and its text is task input. A
  schedule whose text begins with a command is refused with a notice and
  starts nothing. It acts only through an account that already exists and is not
  presence at an endpoint, so it neither marks a terminal attached nor lets
  the IM catch-up replay an unconfirmed push. A schedule that fires while the
  person is busy queues below the person's own input and drains as a
  scheduled turn still. Watcher, recovery, approval, and wakeup continuations
  drain from the queue into their exact Run and never pass the control path
  at all.

- Brief acceptances and continuations in any language follow the ordinary
  Main path. No phrase list binds an execution parent or forces a history
  choice before Main interprets the request. Accepting a new proposal can
  proceed without resuming a historical Run.
- `runs.resumes_run_id` is the only continuation ownership edge: this run
  resumes that run. The resuming row is created transactionally after tenant,
  person, Thread, resumability, scope, and unclaimed-target validation. A
  partial unique index prevents two processes from claiming the same target.
  Schema v12 renamed the column from `parent_run_id`, which invited two
  readings the edge never carried — a topic hierarchy, and general causal
  descent. The legacy reverse pointer `resumed_by_run_id` stays read-only.
- `/resume <number>` resolves the exact Run from the endpoint-local snapshot
  the user saw. Full Run ids are restart-safe. A Thread id is accepted only
  when it has exactly one unresolved Run; ambiguity is never guessed. The
  selection binds the selecting endpoint's next user agent input only. The
  exact Run is captured before admission or queueing; an older queued input,
  another CLI session, or a scheduled turn cannot consume it. Historical
  person-wide resume pins remain display compatibility, not input authority.
- A durable continuity choice is person-scoped: `/choose <choice_id> <number>`
  answers it from any of the person's endpoints. A bare number answers only
  the one recent choice shown on the same account and channel; from another
  CLI window or chat it is that endpoint's own input. A reply, approval, or
  clarification edge on the message skips the bare-number lookup entirely, so
  a native reply "2" reaches its edge target.
- When Main leaves an IM input waiting on a choice, the actionable prompt enters
  the durable outbox before the inbound receipt is accepted. Notification parts
  commit together and carry a typed choice reference in their internal row IDs;
  sending rechecks that the exact choice is still pending. An answered or expired
  choice suppresses its old prompt. A failed outbox save preserves the input and
  holds its receipt with `/choose` recovery controls. `delivery_enqueued` tells
  IM adapters not to duplicate a reply the outbox owns. New Run notices display
  the exact Run ID so one chat can distinguish concurrent work.
- An interrupted outbound `sending` claim has an unknown effect. It never
  becomes an automatic retry: stale-claim recovery exposes `sent_unconfirmed`
  for the existing bounded inbound catch-up path. Catch-up claims also enter
  `sending` before transport dispatch, and their consumed claim survives a
  receipt-save failure. A platform acceptance whose receipt cannot be saved
  follows the same rule with or without a native message ID.
- Explicit resume clears that Run's Attention dismissal and may reopen an
  archived Thread.
- A validated natural-language continuation has two shapes. When the selected
  Run shares the interaction Run's execution domain (same workspace and
  identical execution roots), has no unfinished loop checkpoint, and the
  interaction has produced no effect yet, `work_select(resume)` claims the
  parent atomically at tool time (`ClaimInteractionContinuation`): the
  interaction Run moves onto the parent's Thread, its own versioned Plan is
  imported in that claim transaction with stable logical step ids and exact
  source-step references, and `plan.updated` announces the committed snapshot.
  Work Units remain Run-local; an echoed parent Work Unit id is translated only
  when it belongs to that exact source step, while unrelated stale ids fail.
  The bounded resume context follows exact lineage for prior-step evidence as
  historical context, each check with its age at selection. Main may
  explicitly adopt an unchanged successful check with a reason in
  `update_plan`; the runtime verifies its source step, criterion, scope, and
  that the exact effective checks in the source Work Unit's evidence window
  remain fresh against their declared inputs, including source-tail and child
  changes. An unrelated report write does not invalidate a check with explicit
  independent dependencies; historical undeclared dependencies remain
  conservative. A later check outside the frozen window cannot replace that
  source proof, including references saved by older binaries. A read-only look
  first never forfeits
  adoption. Other checks stay open. Main continues in the same turn with one Run and no
  queue; one pre-effect correction re-points that claim
  (`RetargetInteractionContinuation`). A workspace, execution-root, or
  checkpoint mismatch creates a correctly scoped transfer child at
  finalization, because a Run's execution domain never changes in place.
- Repeating an identical `update_plan` after a verification precondition fails
  remains blocked until the accepted Plan version or a successful check bound
  to one of its current steps changes. The retry guard uses that durable
  evidence revision; it does not treat a cosmetic argument rewrite, an
  unrelated check, or a failed check as progress. A rejected multi-step close
  reports every open verification obligation, each with its next action,
  without committing part of the proposed snapshot.
- A Run that ends without final prose renders its result from runtime state,
  never from progress narration: a structured `finish_run` outcome when one was
  recorded, otherwise the accepted Plan's completed and open steps followed by
  recorded file and verification evidence. Display bounds count characters, so
  CJK text keeps the same visible length as ASCII.
- New Runs use recovery contract v3 for Plan cancellations. Main judges whether
  a step remains necessary and explains its decision. A `not_required` verdict
  references either a completed `replacement_step_id` with the same established
  success criterion and required verification (for a step planned without a
  criterion, a replacement that states its own), or a `scope_change_quote`
  copied from one actual user input in the exact work lineage: the whole
  message, one whole sentence, or a meaningful word-aligned span. Punctuation
  is not a semantic veto: Main judges whether an instruction, including a
  politely phrased question, changes scope or hands work over. Merely finding
  the quote does not make that judgment. Text that may have been cut (a Run
  summary ending in "...", or bounded start-event text) offers no whole message
  or last sentence. The runtime
  checks those
  references and provenance within the Plan transaction; it does not infer
  scope from command names or failure wording. An unsupported verdict remains
  pending with an `unfinished` assessment, while independent steps may proceed.
  Explicit user takeover keeps its quoted, scoped handoff contract. Schema v25
  adds inert evidence fields; historical v1/v2 Runs keep their authority, while
  a new continuation reassesses inherited cancellations under its own contract.
- A validated takeover or scope-change quote records which input it matched:
  `steering:<mailbox id>` for consumed guidance, `run_started:<run id>` for the
  text a Run started with, or `run_input:<run id>` for a Run's input summary.
  When several inputs hold the quote, the earliest on the lineage wins: an
  earlier Run's inputs before a later Run's, and within one Run its request
  (start-event text, then input summary) before guidance in arrival order. The
  runtime owns this source: `update_plan` drops one a model supplies. Restating
  the same quote keeps the recorded source, a changed quote records the input
  it now matches, a step that is no longer cancelled drops it, and an exact
  continuation inherits it. Every plan surface (the live plan, a plan resumed
  by `/resume` or a direct `work_select` claim, `/status`, and `work_inspect`)
  names the authorizing input in the person's terms (`authorized by your
  guidance at 14:02`), never an id or more of the input than the quote; an
  input no longer stored names nothing. Cancellations recorded before schema
  v26 have no source and keep none when restated or inherited.
- Read-only discovery uses the same trusted registration facts for dispatch,
  recovery, and plan-progress accounting. A proven read-only command (ledger
  effect class `observation`) is discovery too and leaves the selection window
  open; an unproven command still closes it, so Attention guidance asks Main to
  select before running commands. Captured `tool_output` artifacts are
  observation storage and follow the continuing Run; originating mutations
  still block implicit selection through their execution ledger. Deliverable
  artifacts and pending human/external control objects retain their guards.
- A selection blocked by material state returns `work_selection_blocked`, not
  a successful transfer proposal. A typed control-plane pause ends the current
  turn before any further dispatch; ordinary recoverable tool failures do not
  acquire this authority. Selection lookup is scoped by event type and exact
  owned Run, so progress volume cannot hide an earlier failed claim.
  A scope/checkpoint transfer remains proposed
  until the gateway commits its queue receipt. Rejected final outcomes replace
  speculative next steps with the actual required human decision; no queue
  receipt means no promised automatic continuation.
- Direct selection checks the activation against the imported Plan's current
  Work Unit. A retained activation keeps its pinned package; a replaced or
  abandoned unit expires its Skill instructions and viewed pages in Main's
  context. Neither selection nor activation deletes an unfinished obligation
  or grants the Skill's scripts permission to execute.
- Active user input is durably steered. Main decides at a safe checkpoint
  whether it updates current work, creates a new plan item, or queues
  independent work. Daemon-originated content never steers from text. The
  input keeps the workspace and execution roots its own request froze, so
  work queued from it, by Main or because the run ended first, runs there
  rather than with the roots of the run it was steered into. Input that
  arrived without roots, such as guidance posted to one run, and rows
  accepted before schema v18 keep that run's roots.
  A validated explicit Run reply is distinct from ordinary prose: if the
  Run ends before consuming it, its durable exact-target provenance keeps the
  selected work and that Run's physical roots. A resumable target retains its
  exact parent edge; a settled target receives task-pinned follow-up without
  reopening the settled Run. Main may still explicitly classify consumed
  guidance as independent work. Historical ordinary rows gain no new edge.

Approval, clarification, and external-watch completion carry structured ids
through the durable queue. Watch finalization claims its exact parent before
inheriting the plan and bounded observation targets/results. Recent user
additions and corrections follow at most eight Runs of that exact lineage;
unrelated Runs in the same Thread are excluded. These records inform Main and
approval triage without granting additional execution authority.
An external-effect claim an earlier Run left unresolved follows the same
exact lineage: its continuation must release it with a read-only observation
bound to the claim, or have the person settle it, before its next conflicting
effect, and the continuation's bound watchers may release it. Another Thread's
Runs never release it (see `tool-safety.md`).

Approval and clarification replies carry structured ids through the durable
queue. Prose such as “resume after approval” has no routing authority.

An exact daemon wakeup whose parent is already continued or terminal does not
become new work and does not requeue as a capacity wait. Its unbound queue row
ends as failed after an idempotent explanation reaches the work event and the
configured delivery outbox. Failed notice writes retain the row for a later
pass. The durable scheduling event projects to a CLI background notice on both
live delivery and replay, before queue settlement; its event id deduplicates
reconnect delivery. CLI presence alone does not suppress the keyed outbox
fallback: a newly attached client may have started after the saved event.
A claim-token check prevents retirement from changing a newer attempt or
a row already bound to a Run. The saved input, human decision, and unfinished
Run remain intact; `/resume` inspects the remaining work. The same rule applies
when the parent changes between queue selection and atomic child creation.
Person-authored replies keep their existing same-person lineage traversal.

A typed synchronous admission refusal means no worker or finalizer started.
The drain returns only its exact still-unbound claim to the same queued row
for a later periodic retry. A failed return write keeps only that confirmed
non-dispatch token in the coordinator's cleanup retry set; the normal scheduler
tick retries the same CAS before scanning due rows. It never reaps an expired
live claim, a bound Run, or a newer token. A restart uses existing boot recovery.
If a worker fails before Run creation, no Run finalizer has saved its result:
the exact unbound input remains retryable, its session receives a bounded
`queue.start_failed` notice, and configured IM delivery uses the keyed outbox.
The durable queue input, not the temporary notice, owns recovery. Neither this
failure nor a capacity refusal chains an immediate drain; the next tick retries.
Each bounded drain holds a refused source and scans other independent sources.
A still-started unbound head also blocks its source in the store selector, so a
failed return write cannot let later input overtake it. Failed notice enqueue
is retried independently of work capacity through the same keyed outbox.
A worker with a bound Run retains its finalizer and exact-Run recovery.

When a queued request needs a continuation choice, the choice, scoped question
event, exact-token queue handoff and prepared outbox parts commit together.
Any failed write rolls back the handoff and preserves the original input;
restart cannot duplicate the choice or lose its notification intent. The CLI
replays the question only on its source session. A preferred IM endpoint uses
an explicit `/choose` notice without widening bare-reply authority, and delivery
rechecks the saved choice so an answered or expired question is not sent.

### Admission snapshot and continuation fidelity

A Run's authority is frozen once, when its work is admitted, as a versioned
admission snapshot (`runs.admission_json`, `task_queue.admission_json`, schema
v26) written in the transaction that creates the Run or queue row. It records
the strategy identity (task class, tool mode, plan and web policy, channel
mode, verification-only), the resolved approval mode, the recovery mode,
execution profile and watch id of system work, the origin (a schedule and its
job), the standing-grant cutoff of a schedule, and the person's request with a
bounded summary.

- A new admission builds it from the authority in force at that moment: a
  direct turn, a turn queued behind busy work or a model change (frozen when
  it is queued, with the instant it was admitted), a
  scheduled fire (cron origin, job id, web opt-in, authorization instant; a
  turn tagged as scheduled counts even without the request's own origin), a
  durable choice answer (built from the person's original words, never from
  the answer that routed them), a `work_select` transfer (the interaction's
  own snapshot), and a coordination Run (no tools). A turn the person types
  to continue earlier work is a new admission too: it carries their words
  and the mode in force when they send it, and only the structured edges
  below hand a parked Run's authority on. A channel name longer than a
  snapshot identifier records a bounded channel mode, which only labels the
  prompt; recording never refuses a turn.
- A new admission freezes the person's mode for the whole lineage, so it never
  guesses one. A persisted `/mode` that cannot be read is retried once and the
  admission is then refused before anything is accepted: a durable inbound
  receipt returns to pending, a queue write is refused, and a queued row from
  before snapshots that needs the person's mode stays queued and unclaimed
  until it can be read. A request that names its own mode needs no read.
- A daemon continuation runs under its exact parent's snapshot. Approval,
  clarification and provider-wait rows copy the parked Run's snapshot inside
  their own transaction, and a caller may only restate it. A resource wakeup
  copies the parked Run's; a watcher finalization copies the registering
  Run's and adds the watch id (and, for a watch registered before
  continuation receipts, the unattended finalization profile); a recovery
  copies the interrupted Run's and adds its recovery mode, where
  verification-only narrows the strategy to the read-only surface (a required
  plan becomes optional; a parent with no planning tool gains none, and a
  parent that was verification-only never widens back). A trigger only ever
  narrows what it inherits.
- Claiming a queue row binds a Run that records the row's snapshot verbatim
  in the binding transaction; a different one is refused.
- Work that waited runs under the narrower of its admitted mode and the
  person's persisted `/mode`, and only when that preference was written at or
  after the work was first admitted (`admitted_at`, set by a new admission and
  copied verbatim by every continuation). A bare minimum against the current
  mode would demote an explicit per-run full-auto Run after a provider wait.
  Modes rank by what they ask: read-only 0, on-request and auto-edit 1, smart
  2, full-auto 3 (a test holds the order to `approvalNeeded`; smart is not a
  pure ceiling because its judge can also deny). A loosening never widens;
  tightening and then loosening again leaves the admitted mode. The stored
  snapshot is never rewritten, so each hop decides again from the original, and
  a narrowed Run records a `run.authority_narrowed` event naming both modes.
  A preference that cannot be read gives the work no more than the product
  default. A snapshot with no admission time (none a new admission writes)
  gives a stored preference no benefit of the doubt, so it narrows however old
  it is; with no stored preference the admitted mode is kept. The preference
  store is second-granular, so a preference written in the second the work was
  admitted counts as after it. A parked approval you answered still passes (its
  authorization is keyed on the action, not the mode), and a Run that is
  already running keeps its mode; `/stop` stops it.
- A scheduled fire that waited in the queue is checked against its schedule
  before the drain claims it (rows whose snapshot records a cron origin; a row
  with no snapshot, a row no schedule wrote, and a daemon with no scheduler are
  not asked about). A schedule that was deleted, or switched off while it
  recurs, drops the fire: the person is told once (an event on the work's
  thread when the row has one, and a push recorded in the outbox) and the row
  is cancelled, so nothing runs under authority a gone schedule no longer
  grants. A one-time schedule is switched off right after its first attempt,
  so its queued fire still runs. A lookup that fails leaves the row queued and
  unclaimed for a later drain, neither run nor dropped. The fire keeps the web
  opt-in and standing-class cutoff its snapshot recorded; they are not
  narrowed to the schedule's current values.
- The drain restores approval mode, origin, execution profile, watch id,
  standing-grant cutoff and strategy from the row's snapshot. Scheduling text
  is never re-classified, so it can neither earn nor lose web access or a
  verification-only surface. The row's key (or, for a scheduled turn, its
  cron queue class) still names the trigger (approval, watcher, recovery,
  resource, provider wait, cron) for presentation and claim rules, so a
  scheduled turn queued behind busy work drains as a scheduled turn with the
  authority its snapshot recorded. The class and the snapshot's origin are
  written from one rule, so they cannot disagree. Standing approval classes
  follow the admitted origin, so an approval answered on a schedule's work
  keeps the schedule's cutoff.
- Text the gateway refuses records nothing. Scheduled text that begins with a
  command, and a job whose account is unbound or whose chat is not its
  person's own, start no Run and write no queue row, so there is no snapshot
  of authority they never had. Scheduled text that is task input is admitted
  like any other turn and records a scheduled snapshot.
- A continuation Run's input summary, `/status` line and work-spine entry
  name the admitted request, not the scheduling text it ran on.
- Rows without a snapshot (accepted before schema v26, deferred steering,
  or a continuation whose parent has none) keep the earlier derivation from
  the row's key and the person's current settings, and the Run they start
  freezes that derivation for its own continuations. A continuation whose
  parent has no snapshot does not record its scheduling text as the person's
  request, so none of its descendants inherits it as one. Deferred steering
  stays here until the steering mailbox can freeze a snapshot when guidance
  is accepted; the queue row it becomes is written later.
- A snapshot that cannot be read fails closed. A row whose own snapshot, or
  whose exact parent's, is unreadable never starts: it holds its source, `/queue`
  marks it `[cannot start: /queue drop <n>]`, and nothing re-derives a possibly
  wider authority for it. The person is told on the work's thread (an event),
  live on an attached terminal, and by a push to their preferred IM. The push
  is only recorded in the outbound queue inside the drain, which serves every
  person in turn: the outbox sends it and retries it, so a messaging platform
  that never answers cannot stall anyone's work. The push is withheld for an
  attached terminal only when the thread event was written. When the thread
  write or the outbox write fails the notice is attempted again after five
  minutes (a push already in the outbox is not retried by the notice), and the
  idempotent writes never duplicate it.

The snapshot carries no execution sandbox policy. A durable watcher instead
freezes the policy its registering call ran under in its execution binding,
beside the boundary the registration's approval resolved and the in-place
credential writes it covered; every poll runs under that policy narrowed,
never widened, by the current configuration, is pinned to that boundary, and
is held to those writes. A watcher registered before they were recorded
follows the configuration, resolves under the policy alone, and covers no
writes. A
continuation Run still takes its sandbox policy and prompt framing (sandbox
note, action budget) from the daemon configuration at its start, and the
execution boundary of each call it makes is decided and pinned at dispatch like
any other Run's: a looser mode restored from the parent's snapshot does not
move a call past the policy then in force.

## Attention and settled history

`WorkTimeline.Attention` derives one item per exact Run from:

1. a currently running Run;
2. a pending approval or clarification;
3. a pending/running external watcher; or
4. an unclaimed resumable Run with no child that is still the latest Run of
   its Thread; a later Run in the same Thread causally supersedes an older
   parked one. An `interrupted` Run counts only when it left work evidence (a
   plan, a non-lifecycle side-effect tool row that is not a proven
   observation, an approval, clarification, or watcher, a resume edge, or next
   steps). A Run whose typed completion reason
   is `work_selection_rejected` records a refused relationship decision rather
   than unfinished historical work, so it remains in history but does not
   enter Attention or `/resume`.

Items are person-partitioned; same-channel items rank first, then stronger
live signals outrank recency. `/status`, the attach
digest, task cards, and the compatibility `Task.status` projection all read
this one derivation instead of judging status separately.

Dismissal means “dismiss this exact Run's Attention,” not “rewrite Runs to
done.” It refuses a running Run, and it refuses while that Run still has a
pending approval, a pending clarification, or a live watcher: an object that
still needs an answer is answered, rejected, or cancelled, never hidden. It
preserves approvals, watcher history, handoffs, and artifacts, and can be
reversed by explicit resume. `/stop` without an active Run is the surviving
spelling and dismisses only the exact pinned Run under that rule. Archiving
changes presentation only and does not cancel effects or resolve pending
input.

A Thread is settled when it is listed and has no undismissed running Run,
pending approval/clarification, active watcher, or unclaimed resumable Run.
Settled is a query result, never a stored status.

## Commands and presentation

- A bare `/resume` shows work without executing it. CLI uses the transient Run
  viewer; IM uses current Attention as the endpoint-local numbered list.
- `/resume <n|run_id>` continues one Run exactly, by selecting it: the
  selecting endpoint's next user agent input goes to that Run. A Thread id is still accepted when it
  has exactly one unresolved Run. `/stop <n|run_id>` clears one item without
  running it; both reference forms accept the shortened Run id the rows print.
- The listing states the actions that apply to it rather than a sentence naming
  two of them. Bare `/stop` (cancel the Run executing now) and `/watchers`
  appear only when the list holds an item of that kind, so no offered action
  answers "there is nothing to do that to". The block is a Markdown list: the
  terminal renders this reply through a CommonMark parser, where indented lines
  fold into the preceding paragraph. Endpoint-local affordances, such as the
  terminal's bare-number picker, are added by that client and never restate the
  shared block; the picker is armed only by a listing with items in it.
- `/search [query]` searches complete retained history on every endpoint,
  including titles, Run input summaries, handoffs, and changed paths. It is not
  limited to a recent-five or seven-day window. In the terminal, `/search
  current` is the local full-fidelity view of the conversation in progress.
- Pinning and archiving are removed: both let a display field decide what needs
  the person now. Dismissal on the exact Run is the only hide.
- `/status` prefers the active Run, then the highest Attention item. It never
  invents a current Thread from recency.
- Startup digest reports only derived Attention. Old settled interactions do
  not produce “still needs attention.”

Ordinals are endpoint-local snapshots with a bounded lifetime; cross-endpoint
automation uses stable ids and structured reply metadata.

## Context and recall

Recovery handoffs obtain the original goal only from the exact `resumes_run_id`
lineage within the same person and execution scope, and include bounded user
steering additions. A missing, cyclic, overly deep, or incompatible edge leaves
the original goal unknown; grouping labels never connect independent work.
Technical daemon continuation input is not substituted for the person's goal.

The durable context path remains:

```text
control.db -> selector -> TaskRuntimeContext -> RuntimeContextBundle -> prompt
```

The compatibility type name does not confer Task authority. Context is
assembled from separately budgeted slices:

1. latest user message;
2. bounded work-spine references, folded only by recorded resume lineage
   (latest result plus earlier request; unknown edges remain separate), and
   compaction summary;
3. exact parent-Run handoff, events, artifacts, inherited Plan, source-step
   evidence summary, and bounded selected results from a completed checkpoint;
   only an incomplete checkpoint restores the full loop ledger;
4. workspace conventions and current state;
5. person preferences;
6. bounded recall hits.

Exact Plan inheritance also commits `plan.evidence_inherited` in the existing
journal: a versioned direct reference to each original bound check, with its
criterion, target, source Plan and invalidation facts. Repeated approval/provider
waits carry the reference without expanding the legacy eight-Run lookup window.
The selector freezes check age and bounded unresolved external blockers for the
Run. `work_inspect` exposes `plan_offset` pages of 12 steps and their source
references, so prompt omissions remain recoverable. Main explicitly adopts sufficient old proof; inheritance does not complete
steps or release effects. Changed dependency inputs, acceptance conditions or
execution scope prevent adoption. Missing legacy references remain unknown.

`work_search` is the complete local structured/FTS base. `work_inspect` reads
one exact, person-owned Run with bounded output; its next-step notice uses the
resumability rule `work_select` enforces, so it proposes resuming only a Run
that selection will accept and names the resumable Run of the same work when
the inspected one is settled. `semantic_recall` may expand a
query, but it is optional, fail-open, and never selects a parent. Recall misses
can be repaired in-turn by broader search, exact inspection, workspace reads,
or a user clarification.

The selected Plan, user corrections, blockers and source evidence references
are mandatory context, not byte-cut prefixes of a local slice. Optional history
uses the remaining slice budget; the request-level token gate rejects a core
that cannot fit. The loop starts from the same selected current-Run Plan and
replaces it only with accepted update/direct-resume receipts. New checkpoints
mark generated controls and references separately from actual person input;
legacy user entries remain conservatively protected. Runtime controls expire
on continuation or when the plan condition changes; original user constraints
and paired tool receipts survive.

The boundary note remains explicit: prior history is reference only; the latest
user message is the authoritative instruction.

## Retention, governance, and reset

Automatic retention may archive only an old, listed, settled Thread. What
protects one is evidence — a live Run or unanswered human input — never a
display flag: Thread pinning was removed in schema v16 because it let a
presentation bit decide what counts as work.
The final archive write repeats every live-fact predicate in its transaction so
a concurrently created Run, approval, clarification, watcher, or resumable
outcome cannot be hidden.

Post-run maintenance remains asynchronous and does not route Threads. It may
extract explicit durable preferences and propose reference search hints. A
reference is an alias/search hint, never context or continuation authority.

For personal-edition testing, `selfmind maintenance reset-work-history` is the
formal reset path:

- default is aggregate-only dry run;
- `--apply` requires the gateway to be stopped;
- running Runs, live watchers, and started queue rows cause a refusal;
- a verified SQLite backup is created before deletion;
- Thread/Run history and dependent control records are removed for the current
  tenant;
- identities, accounts, workspaces, person settings, memory, grants, provider
  state, and published Skills are preserved.

Public upgrades never invoke this reset automatically.

## Non-negotiable invariants

- At most `gateway.max_active_work_runs` (default 2) work Runs per person and
  one foreground Run per CLI session; further work queues durably.
- Thread grouping and visibility never select workspace, sandbox, credentials,
  approval scope, or prompt context.
- A Run's execution domain never changes in place. A same-domain direct claim
  re-points Thread membership and restores a plan only because the domain is
  identical; it never changes scope.
- Raw cross-endpoint transcripts are never merged.
- Daemon-originated messages cannot attach or steer through natural-language
  cues, and their text is never a command, an answer, a choice, or a
  continuity control.
- Wrong grouping is reversible display metadata; execution and effect history
  remain immutable and auditable.
- No ingress LLM classifier and no extra LLM call on the streaming hot path.
- Person ownership is checked on every list, search, inspect, promote, dismiss,
  and resume operation.

## Verification contract

Go tests cover fresh creation, deterministic promotion, derived Attention,
dismiss/reopen, exact-Run resume, ownership isolation, cross-process parent
claim competition, v10-to-v11 migration invariants, retention races, and reset
backup/refusal/preservation.

Production-path evals cover direct chat remaining unlisted, multi-turn
promotion, approval continuation, CLI-to-IM observation, unrelated new work,
recall degradation, daemon-restart exact resume, and a scheduled `/approve`
that leaves the person's parked approval pending. Full `selfmind selfcheck`
is required before release; sustained CLI/IM daily-driver evidence remains a
release gate rather than a schema requirement.

Daily diagnostics group logical work by the committed `Run.ResumesRunID`
relation, including manual in-turn selection and ancestors outside the report
window. Window Run counts and event-level terminal counts remain separate;
latest chain state comes from durable Runs. Bounded traversal reports an
evidence gap rather than silently dropping an unavailable parent. Cost per
observed chain covers only the selected window and includes partial chains.
Wait finalization preserves the structured cause (provider, occupied external
resource, or human input), independently of the agent turn's transport EOF.
Ordinary command counts and failures are execution evidence; they do not
become criterion-bound verification merely because a command returned zero.

Outbound rows that name a platform account are checked against the person's
current active bindings immediately before a send or inbound-triggered catch-up.
An unbound or reassigned endpoint never receives the prior owner's result. The
failed row and diagnostic stay with the original person for explicit recovery;
transport recovery does not infer or create a new identity binding. Legacy rows
with only an explicit channel retain that channel contract and gain no account
reply authority. A successful HTTP response with an unreadable send receipt is
`sent_unconfirmed`, including catch-up, and cannot enter blind retry.
