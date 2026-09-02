# Work Timeline — the Person-Level Work Spine

> **Status: approved target architecture (owner decision, 2026-07-06).**
> Implementation lands in packages P1–P4 (see "Implementation packages" and the
> live rows in `docs/STATUS.md`). Until a package lands, the CURRENT behavior
> documented in `AGENTS.md` and `docs/identity-continuity.md` stands. This doc
> is the single source of truth for WHERE task/context semantics are going and
> WHY — read it before touching task attach, working history, context
> assembly, recall, or `/tasks`.

## The decision in one line

**The source of truth for context is a person-level continuous work timeline
(the "spine"); `task` keeps its name but is demoted from a context boundary to
a work label/view; per-turn context is assembled by a ContextComposer; and
natural-language continuity is interpreted by Main inside an accountable Run
using bounded work-history tools.**

## Why: three failures of ownership-by-guessing (evolution history)

Future agents: do NOT re-introduce any of these. Each was observed live.

1. **Blind recency attach (removed 2026-07-05, commit 957d877).** The original
   `CurrentTaskForChannel` fallback attached any ordinary message to the
   channel's most recent task without looking at content. A brand-new request
   inherited an unrelated parked task's identity and missing workspace →
   out-of-root approval storms.
2. **Explicit-evidence-only attach (the 2026-07 over-correction).** The
   fix flipped the default to "no continuation evidence → always a NEW task".
   Safe against capture, but at the time context was still task-scoped, so it
   spawned context-LESS tasks per message → fragmented context. NOTE
   (2026-08-31): simplification P2 deliberately returned to task-per-message —
   now safe because context is spine- and parent-run-scoped (labels never gate
   context) and derived display priority keeps one-shot rows out of the
   default list. The failure here was context loss, not task count.
3. **The broad fix we rejected: an ingress TaskAttachDecider** (LLM router
   classifying "which task does this message belong to" before the run).
   Structurally inverted: it decides *which context to load* while having *no
   context* — a pre-agent classifier of exactly the kind this repo bans, with
   an irreducible error rate, whose failures silently corrupt execution
   context. Confidence thresholds, audit tables, and confirmation prompts are
   patches over that inversion, not a cure. A later bounded continuity resolver
   narrowed the input to gateway-issued Run cards, but still paid for a second
   Main understanding pass outside a Run. The current Main-turn design keeps
   its useful typed validation boundary while moving interpretation into the
   normal audited turn; the old resolver is rollback-only compatibility.

The common root cause: using recency/pointers/ingress classification as a
proxy for "which work does this belong to", when the only reliable judge is
the model **with the context already in hand**. Hence the flip:

```
task-first:   message → guess task → load that task's context → run
spine-first:  message → load spine context → agent acts (asks if ambiguous)
              → run gets labeled afterwards
```

A recall miss is recoverable in-conversation: Main can use the complete local
structured work index, ask the user, or inspect the workspace. Optional
semantic expansion never authorizes a parent and never blocks the foreground
turn.

## Architecture

```
Person (cross-endpoint identity, unchanged)
  └─ Work Spine (person-level, cross-endpoint, append-only)
       ├─ entry = one TURN: user text + assistant conclusion + artifact refs
       │   (tool intermediates stay in task_events; recall can fetch them —
       │    the spine stays narrative-sized, it must never become a tool log)
       ├─ write rules: only the person's agent-bound turns append, one entry
       │   per turn, completion-time order; cron results append tagged with
       │   their source; subagents never write directly (the parent run
       │   summarizes); single-active-run-per-person serialization (existing)
       │   bounds interleaving
       └─ Runs (execution units; the CONTROL PLANE IS UNCHANGED: approvals,
            queue, events, interrupted-run recovery, hard floor, grants,
            presence, delivery routing all stay keyed exactly as today)
            └─ task = label (the existing tasks table, semantics demoted:
                 display/resume handle only; NEVER gates what the model sees)
```

### Run ownership inside a label

A task is still only a display/resume label, so one label may contain more than
one unfinished work line. Completing a later run must never erase every older
unfinished run under that label.

- A deterministic issue key found at ingress is stored atomically on the run as
  `task_runs.work_key`. It is display/closure evidence only: it never selects a
  workspace, context slice, permission, sandbox, or execution environment.
- Selecting a label by explicit task id or one-shot `/resume` pin does not by
  itself claim unfinished runs. Only a structured reply edge, a deterministic
  standalone continuation control, a claimed durable choice, or a validated
  continuity RESUME decision creates a durable ownership edge.
- A deliberate continuation owns exactly one predecessor. Structured reply
  edges and standalone controls resolve that predecessor deterministically.
  Natural language is interpreted by Main inside a Run: `work_search` returns
  bounded cards from complete retained structured history, `work_inspect`
  reads one exact Run, and `work_select` records an advisory relationship. The
  gateway revalidates the exact target and creates a correctly scoped transfer
  child. A missing, stale, foreign, or already-claimed target never becomes a
  guessed parent.
- The parent run is resolved READ-ONLY before the child run is created
  (`resolveParentRun`), and every context channel — the selector slice, the
  resume block, and loop-checkpoint restore — consumes that one answer.
  `task_runs.parent_run_id` (schema v7) is the durable ownership edge: the
  child claims its parent ATOMICALLY inside its own creation transaction
  (`StartRunOptions.ParentRunID`), which re-validates tenant/person/task
  agreement and the parent's resumable, unclaimed state; the unique partial
  index `idx_task_runs_parent_once` is the cross-process backstop, and a lost
  race surfaces as `ErrParentRunClaimed` with nothing created. The legacy
  reverse `resumed_by_run_id` is read-only compatibility (backfilled by the
  v7 migration where an exact single-parent relationship existed). Final
  reduction may close only the work represented by the claimed edge;
  unrelated unfinished work keeps the label open.
- Structured return edges outrank every guess (simplification P1): a
  daemon-originated approval continuation binds the parked approval's origin
  run via the approval row (`attach reason approval_resume`), and
  an answer to a parked clarification is committed atomically with a durable
  queue row carrying `ClarifyID`; draining that row binds the exact origin run
  (`clarify_resume`). Gateway shutdown preserves the pending question while
  ordinary cancellation and terminal/already-claimed origins expire it, so a
  later answer cannot drift to another work line. Likewise,
  platform-proven reply metadata (`MessageRequest.ReplyToRunID`, preserved
  across the durable queue) binds the exact run it answers (`reply_to_run`).
  The old "Resume task after parked approval …" prose no longer routes
  anything, and a clarify answer never lands on "the oldest pending" — one
  pending question is answered structurally, several get a deterministic
  numbered pick.
- Run-scoped context (2026-08-31 simplification): full task context exists
  only keyed by the resolved parent run — its finalization handoff
  (`handoff_run_<run_id>`), its events, its artifacts, its plan, its
  checkpoint. A full-mode attach without an exact parent downgrades to the
  bounded task card (summary/next steps; no handoff, no artifacts, no
  events). Daemon-originated turns (cron/watch/approval) never steer the
  person's active run via a text cue.
- The wait authority is the run itself (simplification §10.3): a run parked in
  `waiting_user` / `verification_partial` / `blocked` / `interrupted` that no
  child has claimed through the atomic `parent_run_id` edge keeps the task in
  that state. Pending approvals and clarifications are their own live rows.
  The legacy `task_blockers` table lost this authority — no new rows are
  created, `finish_run` has no blocker parameter, and remaining open legacy
  rows are settled as hygiene when their origin run is claimed.
- Task display status is reduced from the finishing run's outcome plus live
  wait evidence (pending human input, active watches, queued watcher
  finalization, unclaimed resumable runs). A newer unrelated successful run
  therefore cannot erase unfinished parked work; claiming and completing the
  parked run releases it atomically. `selfmind maintenance task-audit` reports
  projection drift read-only and reconciles only through the same reducer.

### Per-turn context (ContextComposer)

The existing selector + `RuntimeContextBundle` formalized into a fixed slice
list, each with its own budget:

1. latest user message
2. spine tail (recent turns verbatim, cross-endpoint)
3. spine compaction summary (default-on compaction already shipped)
4. semantic-recall slices (see below)
5. relevant artifacts/files (artifact manifest already shipped)
6. workspace current state
7. person memory / preferences
8. open run / pending approval / pending question state

The compaction/summary block carries a boundary note, verbatim:
*"The history summary is reference only. The latest user message is the only
authoritative instruction. If it changes direction, the latest message wins."*

### Semantic recall (the new load-bearing wall, built in two tiers)

- **v1 — SHIPPED (P2, 2026-07-06; canonical source 2026-07-26):**
  `semantic_recall`-role query expansion + bounded retrieval over indexed
  sessions, task label cards, governed task references, workspace knowledge,
  and canonical memory, wired into the
  selector as an automatic slice instead of a model-invoked tool only.
  Implementation: `internal/gateway/httpapi/recall.go` (`RecallEngine` on
  `Server.Recall`, called from `selectedTaskRuntimeContext`) → new bounded
  `kernel.TaskRuntimeContext.RecallSlices` rendered under
  `[Recall — possibly related prior work; reference only]` in the runtime
  context block (ephemeral: system-prompt only, never the messages array,
  never persisted into working history). Sources v1: indexed sessions
  (person-partitioned FTS, includes `task:<id>` task sessions) and task label
  cards (title + current_summary + latest handoff summary/changed files via
  the live read-only `control.ListTaskCards` JOIN — queried, not mirrored
  into FTS, so cards can never go stale; artifacts/changed files surface
  through the card's work line). Governed task references bridge learned
  names/identifiers to the same task card: active references may route an
  exact mention, while candidates are recall-only and conflicts abstain.
  Workspace knowledge is a deterministic, versioned projection of authorized
  convention files (`AGENTS.md`, `.selfmind.md`, and peers): bounded sections
  are replaced when file hashes change and removed when files disappear; they
  remain procedural workspace knowledge rather than person memory. The final
  source is canonical memory (person-partitioned,
  global/current-workspace scoped, validity-filtered CJK/lexical similarity;
  pinned rows stay in the unconditional memory block). Budget: ≤3 slices,
  ~400-char excerpts, one
  slice per work line (label card beats raw session fragment), current task
  excluded; control-command-shaped and <6-rune messages skip recall entirely.
  Expansion runs ONLY when a `semantic_recall` role model is explicitly
  configured (`app.SemanticRecallExpander` — never the main coding model),
  bounded by a 3s timeout, degrading to raw-term FTS on any failure. The
  expansion contract receives the query as JSON-fenced untrusted data and emits
  at most five narrow lexical variants: aliases, acronyms, former names, likely
  historical wording, or useful cross-language equivalents. It preserves exact
  identifiers and must not answer the query, infer a new intent, or broaden it
  into generic topics; recall is not limited to technical conversations.
  A first transient live failure disables expansion and schedules a bounded
  retry without flashing a user notice; a failed retry emits one degraded
  notice naming the actual provider/model, and successful recovery emits one
  matching recovery notice. Authentication/model/configuration failures remain
  immediately visible. Lexical FTS remains available throughout.
  Observability: redacted `context.recall` task event (source counts + refs,
  no excerpts). Canonical `last_accessed_at` changes only for rows that survive
  the shared budget and are actually injected; selected canonical rows are
  excluded from the static fallback block in the same turn. A separate
  redacted `context.recall_usage` event records lexical overlap between the
  selected slices and the final answer. This is a trend signal for adoption,
  not proof that recall caused the answer.
- **v2 (later):** true embedding vector index (spine entries + label cards +
  artifacts). Interface reserved in v1: `httpapi.RecallSource`
  (`Search(ctx, RecallQuery) []RecallHit` with work-line dedupe keys) — an
  embedding-backed source registers alongside the FTS sources without
  reshaping the selector, budget, or dedupe logic.
- Degradation chain: recall miss → the agent asks in-turn, or
  inspect-before-build reads the workspace (both already shipped). Never a
  silent wrong attach.

### Labels (task demoted, name kept) — simplified 2026-08-31 (P2)

- Every root run OWNS a fresh task; a continuation's child run inherits its
  parent's task through the claimed parent edge. There is no pre-label guess
  left to repair, so the post-run KEEP / MOVE / NEW / TITLE / INBOX routing was
  removed (`postRunAnalyzerVersion` 3): post-run maintenance is memory
  extraction plus reference search hints only, and a legacy frozen v2 proposal
  replays with its task decision audited and ignored
  (`label.assigned` `decision: ignored_legacy`).
- Task status, summary, and next steps are DERIVED projections: every
  finalization commits the reduced status (`resolveFinalTaskStatusTx` over
  pending approvals/questions, live runs, watches, and open blockers) plus the
  run's card. The weak-attach lifecycle deferral (`PreserveTaskLifecycle` /
  `PreserveTaskCard`) is gone.
- Titles remain stable: the provisional truncated first input stands until a
  human renames (`/task <id> rename`). Nothing auto-renames.
- The default `/tasks` open view ranks by a DERIVED display priority instead
  of hiding one-shot Q&A in an Inbox: pinned first, then work waiting on the
  person (open blockers / pending approvals / pending questions), then real
  work lines (several runs, artifacts, or recorded next steps), then recency.
  Ranking never changes retention, search, or continuity. The hidden Inbox
  label (`kind='inbox'`) is no longer created; historical inbox rows remain
  readable for audit, and `tasks.inbox_enabled` is parsed but ignored.
- **Task References are aliases and search hints only.** They never route a
  message, never load task context, and never move the current-task pointer.
  Automatic promotion is frozen: run-count support keeps a reference at
  `candidate` (a recall signal); only an explicit user confirmation activates
  one, and two confirmed bindings for one value conflict and abstain.
  Ticket-shaped keys (`RUQX-224`) stay bounded display metadata.
- `current_task` is a UI convenience projection (written for `/status`
  display), never continuation authority.

### Ingress continuity authority

```
message → control-command filter
  → structured return edge (approval / clarification origin run / platform reply_to_run_id)
  → /new --run, /choose, task_id, /resume pin, and standalone continue controls
      remain deterministic
  → daemon-originated turn → never consult continuity inference
  → active Run?
      yes → persist steer → acknowledge with structured status
              → same Main consumes at a safe checkpoint
                   related → apply to current work/plan
                   independent → queue fresh work
                   exact other work → queue exact-parent continuation
      no → create one fresh audited Main Run
              → bounded spine/recall context
              → optional work_search → work_inspect → work_select
              → gateway validates OBSERVE/RESUME relationship
```

- There is no model call before an idle Run is created. `work_search` and
  `work_inspect` are read-only, bounded, and person-scoped; `work_select` writes
  an advisory typed proposal only. The gateway alone may commit queue, parent,
  workspace, approval, or delivery state.
- Pending choices are person-partitioned and single-use (schema v8). A bare
  number is accepted only when exactly one choice was created in the last 30
  minutes; `/choose <choice_id> <number>` or platform `choice_id` metadata stays
  valid for 24 hours and works from another bound endpoint. The saved request is
  a short-lived bounded snapshot erased atomically when claimed (expired choice
  metadata is pruned after seven days); resolution audit is retained for 90
  days and stores only an input hash,
  candidate IDs, typed decision/evidence, provider/model, latency, and error
  class—never raw transcript or prompt content. A claimed human choice appends
  a `correction_of` decision edge for routing evaluation; it is not person
  memory and does not rewrite a Task Reference automatically.
- Content ambiguity inside a selected run remains the coding agent's job. Cron,
  watch, approval, and other daemon-originated turns never steer or disambiguate
  from text.

### Main-turn rollout status

The active plan changes where understanding happens without changing who owns
authority. Its core flow is now the default:

```text
explicit controls / structured replies / daemon-origin gate
  -> active Run: persist user input -> acknowledge with status
       -> the same Main consumes it at a safe checkpoint
          -> related: update current work
          -> independent or uncertain: durable separate queue item
  -> idle: create an ordinary audited Main Run
       -> bounded spine tail + structured hints
       -> optional work_search / work_inspect
       -> gateway validates NEW / OBSERVE / RESUME
```

- There is no continuity model call before the idle Run is created. Candidate
  hints help Main start cheaply but never impose a recent-history limit.
- `work_search` uses complete retained structured/FTS history;
  `semantic_recall` may expand a query but failure never blocks local search or
  the foreground turn. `work_inspect` returns bounded run-scoped evidence, not
  raw cross-endpoint transcripts.
- Validated continuation currently uses a structured transfer and creates the
  correctly scoped exact-parent child for every domain; a Run's execution
  domain never changes in place. The direct same-domain claim-update remains a
  plan item rather than a current capability.
- Active natural-language input is durable steer on both CLI and IM. It does
  not cancel an in-flight tool. Main may update the current plan only for
  related input; independent or uncertain input is queued without changing the
  plan.
- A progress question receives an immediate deterministic status at its source
  endpoint. Final delivery remains with the origin unless the user explicitly
  requests a validated bound-endpoint override.
- A wrong implicit RESUME may be audited and retargeted only before any material
  effect boundary. Afterwards the agent stops expansion, reports evidence, and
  asks; history is never erased to simulate a rollback.

Pre-effect audited retarget, explicit delivery override, and final deletion of
the compatibility resolver remain open in the active plan.

### Run completion versus label lifecycle

- Every accepted agent turn has a terminal run outcome and a durable
  `run.outcome` event, including a direct answer that does not invoke
  `finish_run`.
- A direct answer completes that run but leaves its reusable work label open
  (`in_progress`) for normal follow-ups. It must not silently close the label.
- A structured `finish_run` outcome is authoritative and may close, park, or
  mark the label as waiting. Run status and label lifecycle are related but
  intentionally not the same state machine.
- A watcher finalization outcome carries a nested `external` result. The outer
  status reports whether the agent successfully verified and recorded the
  result; `external.status` reports the observed build, deployment, or other
  target. A failed external target can therefore coexist with a successful
  finalization run while the task reducer keeps the label blocked. This avoids
  misreporting an external failure as an agent execution failure.

### /tasks view (same name, aggregated display) — SHIPPED (P3, 2026-07-06)

Default shows open/running/waiting/paused labels with `run: N 次`, latest
activity, next-step hint; done collapses (`/tasks done|archived|all` expands);
`/task <n|id>` detail, `/task <n|id> runs|rename|pin|unpin|complete|archive|references` and
`/task <id> reference add|remove <name>` (archive is a terminal
status: hidden from open lists, recall label cards, and the pre-label guess;
only an explicit `/resume <n|id>` reopens it). `complete` is explicit person
authority over label lifecycle: it preserves historical run outcomes while
expiring pending input and queued continuation rows; it refuses live runs and
external watchers. Completed labels are also reopened only by explicit
`/resume`. Archive uses the same parked-input/queued-continuation cleanup and
live-effect guard. Open-list ordinals resolve an endpoint-local 30-minute snapshot of
the exact ranked/grouped cards the person saw; a stable ID is the restart-safe
fallback. Short ids (`task_xxxxxxxx`) are shown and accepted back.
Implementation: `httpapi/task_view.go`, shared by CLI
and IM via the control-command path; `/task` is registered in
`internal/gateway/command`. `/tasks search <keyword>` queries complete visible
history, including prior run summaries and handoff file paths. Status/view,
workspace, keyword, and pagination are applied in SQLite (`--workspace`,
`--page`, `--limit`) instead of filtering a fixed recent window in memory; the
default open view stays bounded by `tasks.default_list_limit`.

### Task governance (reversible)

Tasks remain work labels, but a long-lived assistant also needs label hygiene:

- `work` and `recurring` labels are visible. The hidden `inbox` kind is no
  longer created (historical rows stay readable for audit); one-shot Q&A is
  ranked after pinned, human-waiting, and evidence-rich work. It remains
  searchable and may appear after stronger rows in the bounded default view.
- One logical `PostRunAnalyzer` result per eligible run covers explicit user
  preference decisions and reference search-hint proposals — no task or
  workspace-memory decisions. Several same-person, same-workspace results may share one
  provider call according to `tasks.maintenance_debounce`,
  `tasks.maintenance_max_wait`, and `tasks.maintenance_batch_max_runs`. It
  uses the stable `memory_extract` semantic role: `models.roles.memory_extract`
  is the optional advanced override and `models.auxiliary` is the shared
  floor. It never runs at ingress and never changes the context the completed
  run saw.
- Analysis is eligibility-gated on durable evidence (outcome structure or a
  substantive input/result pair) or a reference surfacing in the user text.
  Compact preference statements remain eligible; tiny low-information turns
  skip the call.
- A person may pin/unpin a visible task. Retention never archives pinned,
  open, interrupted, active, or human-waiting work.
- Automatic retention only changes an old terminal label to `archived`; it
  never deletes runs, events, handoffs, artifacts, or user-authored titles.

## What does NOT change

Control plane (runs/approvals/queue/clarify/recovery/hard floor/grants),
presence + delivery routing, person/account identity model, compaction (A),
artifact manifest (B), transport resilience (Zero),
all command names (`/tasks`, `/task`, `/resume`, `/new` — the `/work` rename
was considered and rejected: pure churn), and the eval system.

## Explicitly out of scope (rejected designs — do not resurrect)

Ingress TaskAttachDecider / confidence thresholds / attach-decision audit
table (simplified to run→label provenance), lifecycle+execution dual status
split, task relations/dependency fields, task split/merge, per-platform
session keys (hermes-style), Honcho-style dialectic user modeling.

## Implementation packages (live priority lives in docs/STATUS.md)

- **P1 spine:** person-keyed turn-level history (slimmed entries) + boundary
  note + ContextComposer slice formalization + legacy-key compat reads.
  (Builds on the task-keyed trajectory mechanism from 2026-07-06 — stable
  keys, fallback reads, task-session indexing all carry over.)
- **P2 recall v1:** query-expansion + FTS as an automatic Composer slice;
  label-card/artifact indexing; embedding interface reserved. Parallel to P1.
- **P3 labels & view: landed 2026-07-06; superseded by the 2026-08-31
  simplification:**
  pre-label and the post-run labeler routing were removed — every root run
  owns its task, `/tasks` ranks by derived display priority, and `/task`
  subcommands (runs/rename/pin/unpin/archive), retention governance, and
  complete-history search remain.
- **P4 eval:** the ten acceptance scenarios below, recorded cassettes,
  zh/en/elliptical/cross-endpoint coverage.

One package per worktree; full tests + `-race`; deploy; owner verifies before
the next package (standing workflow).

## Acceptance scenarios (definition of done)

> Coverage shipped (P4, 2026-07-06): `evalcases/timeline/` + the scenario→test
> table in its README.

1. "用JS写九七游戏" → new label, new run.
2. "再多做几个角色" → context continuous via spine tail (no routing); a new
   root task per message (P2), with the answer still building on the game —
   grouping is display-only and never a context boundary.
3. "帮我总结今天股市" → new label; game chatter in the spine does not
   interfere (models handle topic interleaving inside a window).
4. "这个游戏动画不好看" with two game labels → the agent ASKS which one,
   in-turn.
5. Task started on CLI; WeChat says "继续优化那个九七游戏" → spine is
   cross-endpoint, context is simply there.
6. `/tasks` ranks pinned, waiting, and evidence-rich work before one-shot rows;
   manual rename/merge remains the correction path for display grouping.
7. A wrong-looking grouping only affects display; `rename`/`merge` fixes it
   without touching parent chains or authority.
8. Long-run: after compaction the agent still states the original goal;
   last week's artifact comes back via recall.
9. Approvals / queue / interrupted-recovery / hard floor: zero regression.
10. Label decisions fully recorded and auditable.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Recall misses (new load-bearing wall) | Two-tier build; failure degrades to in-turn asking + workspace inspection — recoverable by design |
| Spine becomes a noise stream | Turn-level slimming; single-active-run serialization; cron entries tagged |
| Third reversal of the attach invariant | This doc records the full causal chain; AGENTS.md points here as mandatory reading |
| One-task-per-message crowds the list | Derived display priority + pin + search; grouping is never a context boundary |
