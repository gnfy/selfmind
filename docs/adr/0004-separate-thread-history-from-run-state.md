---
status: accepted
---

# Separate Thread history from Run execution state

SelfMind groups related Runs in a Thread so work remains searchable and
continuable across endpoints, but only a Run owns execution lifecycle. Thread
visibility is reversible presentation metadata, while current Attention is
derived from Runs, approvals, clarifications, and watchers. This
replaces the former Task aggregate whose persisted status mixed work history,
execution truth, and a user-maintained to-do list; keeping both models would
create competing authorities, so the migration has one durable Thread source.

## Amendment 2026-09-03

Review of the first daily-driver window tightened four rules without changing
the decision above.

- **Dismissal refuses live control objects.** `/task <n> complete` and a
  `/stop` with no active Run dismiss Attention only for the exact pinned Run,
  and refuse while that Run still owns a pending approval, a pending
  clarification, or a live watcher. Hiding an object that still needs an answer
  would recreate the stale-Task problem this decision removed.
- **One resumable item per Thread, and interrupted needs evidence.** A parked
  Run is `resumable` Attention only while it is the latest Run of its Thread;
  a later Run in the same Thread causally supersedes it. An `interrupted` Run
  counts only when it left work evidence: a plan, a non-lifecycle side-effect
  tool row, an approval, clarification, or watcher, a parent edge, or next
  steps. Same-channel items rank first. Every status surface (`/status`, the
  attach digest, task cards, and the compatibility `Task.status` projection)
  reads this one derivation; `UpdateTaskStatus` no longer accepts a status.
- **Promotion requires work evidence.** A Thread leaves `unlisted` only on the
  same evidence classes. Lifecycle tools (`finish_run`, `update_plan`,
  `work_select`, `queue_user_input`) and read-only tools are not evidence;
  creating an approval, clarification, or watcher promotes immediately; an
  OBSERVE projection never demotes a pinned or evidence-bearing Thread.
- **Same-domain natural-language resume is claimed in the same turn.** When
  Main calls `work_select(resume)` and the selected Run shares the interaction
  Run's execution domain (same workspace and identical execution roots), has no
  unfinished loop checkpoint, and the interaction has produced no effect yet,
  the gateway claims the parent atomically at tool time
  (`control.ClaimInteractionContinuation`), re-points the interaction Run onto
  the parent's Thread, restores the parent's plan as a durable `plan.updated`
  event, and returns the parent's bounded resume context as the tool result, so
  Main continues the work in the same turn: one Main Run, no queue. One
  pre-effect correction to another same-domain Run uses
  `control.RetargetInteractionContinuation`. A workspace, execution-root, or
  checkpoint mismatch still creates a transfer child at finalization, because
  execution scope never changes in place; the direct claim is legal precisely
  because it requires an identical domain. This reverses the earlier statements
  that the interpretation Run is never retargeted in place and that every
  domain receives an exact-parent child.
