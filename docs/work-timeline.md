# Work Timeline — Thread history, Run state, and derived Attention

> **Status: current architecture (schema v11).**
> This document owns work-history, continuation, listing, and recall semantics.
> `docs/STATUS.md` owns remaining evidence and priority.

## Decision

SelfMind retains every accountable agent turn as a Run and groups related Runs
in a reversible Thread. A Thread is searchable history and presentation
metadata; it never owns execution state. What currently needs action is an
Attention projection derived from Runs and pending control objects.

The person-level work spine supplies bounded continuity across CLI, IM, cron,
and HTTP. A Thread may improve search and display, but a mistaken automatic
grouping must never select execution scope, permissions, or prompt context.

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

The public commands retain `/tasks`, `/task`, and historical `task_*` ids for
compatibility. These are presentation names over the Thread model, not a
second Task aggregate.

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
`queue_user_input`) and read-only tools are not evidence. An OBSERVE
projection never demotes a pinned or evidence-bearing Thread.

A tool-free direct answer remains unlisted by default. Promotion is monotonic
for automation: it never hides a listed Thread and never reopens an archived
Thread. Only an explicit person control may reopen an archive.

Main may advise `NEW`, `OBSERVE`, or `RESUME` through bounded work-history
tools, but the gateway owns every state transition. Optional model disposition
must not block foreground work and cannot grant execution authority.

## Continuation authority

Explicit controls and structured edges take priority:

```text
explicit /new, /resume, /choose
  -> structured approval / clarification / platform reply edge
  -> daemon-origin gate
  -> active Run: durable steer
  -> idle: ordinary Main Run with bounded history tools
```

- `runs.parent_run_id` is the only continuation ownership edge. The child is
  created transactionally after tenant, person, Thread, resumability, scope,
  and unclaimed-parent validation. A partial unique index prevents two
  processes from claiming the same parent.
- `/resume <number>` resolves the exact Run from the endpoint-local snapshot
  the user saw. Full Run ids are restart-safe. A Thread id is accepted only
  when it has exactly one unresolved Run; ambiguity is never guessed.
- Explicit resume clears that Run's Attention dismissal and may reopen an
  archived Thread.
- A validated natural-language continuation has two shapes. When the selected
  Run shares the interaction Run's execution domain (same workspace and
  identical execution roots), has no unfinished loop checkpoint, and the
  interaction has produced no effect yet, `work_select(resume)` claims the
  parent atomically at tool time (`ClaimInteractionContinuation`): the
  interaction Run moves onto the parent's Thread, the parent's plan is restored
  as a durable `plan.updated` event, and the parent's bounded resume context is
  the tool result, so Main continues in the same turn with one Run and no
  queue; one pre-effect correction re-points that claim
  (`RetargetInteractionContinuation`). A workspace, execution-root, or
  checkpoint mismatch creates a correctly scoped transfer child at
  finalization, because a Run's execution domain never changes in place.
- Active user input is durably steered. Main decides at a safe checkpoint
  whether it updates current work, creates a new plan item, or queues
  independent work. Daemon-originated content never steers from text.

Approval and clarification replies carry structured ids through the durable
queue. Prose such as “resume after approval” has no routing authority.

## Attention and settled history

`WorkTimeline.Attention` derives one item per exact Run from:

1. a currently running Run;
2. a pending approval or clarification;
3. a pending/running external watcher; or
4. an unclaimed resumable Run with no child that is still the latest Run of
   its Thread; a later Run in the same Thread causally supersedes an older
   parked one. An `interrupted` Run counts only when it left work evidence (a
   plan, a non-lifecycle side-effect tool row, an approval, clarification, or
   watcher, a parent edge, or next steps).

Items are person-partitioned; same-channel items rank first, then pinned
Threads and stronger live signals outrank recency. `/status`, the attach
digest, task cards, and the compatibility `Task.status` projection all read
this one derivation instead of judging status separately.

`/task <number|id> complete` means “dismiss this exact Run's Attention,” not
“rewrite Runs to done.” It refuses a running Run, and it refuses while that Run
still has a pending approval, a pending clarification, or a live watcher: an
object that still needs an answer is answered, rejected, or cancelled, never
hidden. It preserves approvals, watcher history, handoffs, and artifacts, and
can be reversed by explicit resume. `/stop` without an active Run dismisses
only the exact pinned Run under the same rule. `/task ... archive` changes
presentation only and does not cancel effects or resolve pending input.

A Thread is settled when it is listed and has no undismissed running Run,
pending approval/clarification, active watcher, or unclaimed resumable Run.
Settled is a query result, never a stored status.

## Commands and presentation

- `/tasks` shows current Attention, not every historical Thread.
- `/tasks done` shows listed settled history.
- `/tasks archived` shows archived history; `/tasks all` shows all retained
  Threads.
- `/tasks search <text>` searches complete retained history, including titles,
  Run input summaries, handoffs, and changed paths. It is not limited to a
  recent-five or seven-day window.
- `/task <number|id> ...` accepts the last rendered ordinal or a stable id for
  detail, runs, rename, pin, unpin, complete, archive, and references.
- `/status` prefers the active Run, then the highest Attention item. It never
  invents a current Thread from recency.
- Startup digest reports only derived Attention. Old settled interactions do
  not produce “still needs attention.”

Ordinals are endpoint-local snapshots with a bounded lifetime; cross-endpoint
automation uses stable ids and structured reply metadata.

## Context and recall

The durable context path remains:

```text
control.db -> selector -> TaskRuntimeContext -> RuntimeContextBundle -> prompt
```

The compatibility type name does not confer Task authority. Context is
assembled from separately budgeted slices:

1. latest user message;
2. bounded work-spine tail and compaction summary;
3. exact parent-Run handoff, events, artifacts, plan, and checkpoint when one
   was validated;
4. workspace conventions and current state;
5. person preferences;
6. bounded recall hits.

`work_search` is the complete local structured/FTS base. `work_inspect` reads
one exact, person-owned Run with bounded output. `semantic_recall` may expand a
query, but it is optional, fail-open, and never selects a parent. Recall misses
can be repaired in-turn by broader search, exact inspection, workspace reads,
or a user clarification.

The boundary note remains explicit: prior history is reference only; the latest
user message is the authoritative instruction.

## Retention, governance, and reset

Automatic retention may archive only an old, unpinned, listed, settled Thread.
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

- One active Run per person until another approved plan changes concurrency.
- Thread grouping and visibility never select workspace, sandbox, credentials,
  approval scope, or prompt context.
- A Run's execution domain never changes in place. A same-domain direct claim
  re-points Thread membership and restores a plan only because the domain is
  identical; it never changes scope.
- Raw cross-endpoint transcripts are never merged.
- Daemon-originated messages cannot attach or steer through natural-language
  cues.
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
recall degradation, and daemon-restart exact resume. Full `selfmind selfcheck`
is required before release; sustained CLI/IM daily-driver evidence remains a
release gate rather than a schema requirement.
