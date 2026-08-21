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
disambiguation happens inside the agent's turn — never in an ingress router.**

## Why: three failures of ownership-by-guessing (evolution history)

Future agents: do NOT re-introduce any of these. Each was observed live.

1. **Blind recency attach (removed 2026-07-05, commit 957d877).** The original
   `CurrentTaskForChannel` fallback attached any ordinary message to the
   channel's most recent task without looking at content. A brand-new request
   inherited an unrelated parked task's identity and missing workspace →
   out-of-root approval storms.
2. **Explicit-evidence-only attach (the over-correction, current code).** The
   fix flipped the default to "no continuation evidence → always a NEW task".
   Safe against capture, but real use (iterating on one feature: "再多做几个角色",
   "动画不好看" …) spawned a new context-less task per message → task
   proliferation + fragmented context.
3. **The proposed fix we rejected: an ingress TaskAttachDecider** (LLM router
   classifying "which task does this message belong to" before the run).
   Structurally inverted: it decides *which context to load* while having *no
   context* — a pre-agent classifier of exactly the kind this repo bans, with
   an irreducible error rate, whose failures silently corrupt execution
   context. Confidence thresholds, audit tables, and confirmation prompts are
   patches over that inversion, not a cure.

The common root cause: using recency/pointers/ingress classification as a
proxy for "which work does this belong to", when the only reliable judge is
the model **with the context already in hand**. Hence the flip:

```
task-first:   message → guess task → load that task's context → run
spine-first:  message → load spine context → agent acts (asks if ambiguous)
              → run gets labeled afterwards
```

A recall miss is recoverable in-conversation (the agent asks, searches, or
reads the workspace). A routing miss silently corrupts execution. Choose the
architecture whose failure mode is recoverable.

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
  itself claim unfinished runs. Only a message classified as an explicit
  continuation creates a durable ownership edge.
- A deliberate continuation owns a predecessor only when the selected label has
  exactly one unresolved run. External issue keys and model-written summaries
  are display evidence only; they never decide run ownership. When multiple
  unresolved runs share a label, ambiguity remains visible instead of being
  guessed away.
- `resumed_by_run_id` is the durable ownership edge. Final reduction may close
  only the work represented by that edge; unrelated unfinished work keeps the
  label open.
- A durable `task_blockers` row is the source of truth for a concrete unresolved
  condition (approval, clarification, verification, environment, or an explicit
  interrupted handoff). Historical run statuses are evidence, not live
  blockers. Each turn receives the bounded open-blocker list and may resolve
  only exact ids through `finish_run.resolved_blocker_ids`; deterministic
  approval/clarification/resume events settle only the blocker they own.
- Task display status is reduced from the latest run outcome plus current open
  blockers. A newer successful run therefore cannot erase unrelated unfinished
  work, while a resolved old blocker cannot keep an otherwise completed label
  permanently interrupted.

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

### Labels (task demoted, name kept) — SHIPPED (P3, 2026-07-06)

- After a run finishes, finalization persists replayable evidence without
  calling a model. The daemon eligibility-filters and batches adjacent jobs for
  the same tenant/person/workspace after a five-minute quiet window (bounded by
  a fifteen-minute maximum wait and a configurable run-count cap). One
  cheap-model call may therefore combine reversible task hygiene with durable
  fact extraction for several runs, while returning one independently frozen
  result keyed by each run id. Implementation:
  `httpapi/run_labeler.go` (`PostRunAnalyzer` on `Server`, built by
  `app.NewConfiguredPostRunAnalyzer` from the stable `memory_extract` role;
  an explicit `models.roles.memory_extract` route wins, otherwise it uses
  `models.auxiliary`). Its task decision is KEEP /
  MOVE:<task_id> / TITLE:<short title> / NEW:<short title> / INBOX. `NEW` may
  split independent durable work out of an established weak pre-label; it is
  rejected for explicit task/reference/resume attachments. Its destination id
  is derived from the completed run id, so replay after a crash reuses the
  same label instead of creating another one. MOVE only targets an offered
  open label, every failure preserves the completed run, and semantic
  maintenance runs asynchronously in the daemon under a bounded timeout.
- Titles are stable: generated once (TITLE, new placeholders only; fallback
  stays the truncated first input), never auto-renamed; `/task <id> rename`
  for humans.
- `resolveTask` at run start is a *pre-label guess* (explicit
  `/resume`/task_id/cue wins; else current open label or a new placeholder). A
  wrong guess is corrected post-run by re-pointing the run's task_id —
  `control.Store.ReassignRun` moves task_runs + task_events + task_artifacts
  in one transaction, folds handoffs into the target and deletes an
  auto-created placeholder left with zero runs — harmless, because labels
  never gate context, and the EXECUTION workspace follows the REQUEST for
  pre-label turns. This keeps `task_runs.task_id NOT NULL` and every
  control-plane constraint intact.
- Label decisions are recorded (`label.assigned` task event: decision,
  from/to, run id, bounded reason) so eval can score labeling accuracy.
  Mislabels are display bugs, not context corruption.
- Task identity is governed through **Task References**, not a product-specific
  ticket regex. The existing post-run maintenance call may propose literal,
  entity, or descriptive references; deterministic validation activates an
  automatic reference only after two distinct user-text-supported runs. A
  user-added reference is active immediately. The same normalized reference
  bound to multiple tasks becomes conflicted and ingress abstains. Task titles,
  summaries, recall output, and model prose are never activation evidence.
  Task merge moves and deduplicates these references and their evidence in the
  same transaction as the task history; identity evidence cannot be stranded
  on the archived source label.
- Mention and continuation have different context depth, but neither grants
  execution authority. A unique active reference in an ordinary mention
  attaches only bounded task context; an explicit continuation with the same
  reference may load full task context and update the current-task pointer.
  Both continue to execute in the request/current workspace and neither claims
  unfinished prior runs. Only explicit `/resume`, an explicit task id, or a
  plain continuation resolved from the already-current task may inherit the
  task-bound workspace. Every resolution is recorded as pending, corrected, or
  accepted-but-unverified for diagnostics; a wrong semantic match remains a
  display/context-selection defect, never permission or workspace authority.
- Ticket-shaped keys (for example `RUQX-224`) may still appear as bounded
  display metadata. They never search existing task titles or summaries,
  select a task, claim a prior run, or become context/workspace/permission
  authority. Reusable task identity is learned through governed Task
  References instead.
- Historical `task_runs.work_key` values are not auto-promoted at startup.
  `selfmind maintenance migrate-task-references` audits them explicitly and
  imports only values whose exact surface form occurs in the original user
  input. Two distinct run-level user-text observations are still required for
  automatic activation; inferred legacy metadata remains inert.
- Task cards have source protection (2026-08-08): an ordinary weak pre-label
  attachment to an existing label may add its run, events, handoff, and
  maintenance proposal, but it cannot overwrite that label's stable lifecycle,
  summary, or next steps before label resolution. A deterministic sole label
  or a successful KEEP decision reconciles lifecycle afterward. A placeholder
  created for the current turn may receive its first card. Post-run relabeling
  remains display-only.

### Ingress (simplified) — SHIPPED (P3, 2026-07-06)

```
message → control-command filter (unchanged)
  → run active? → steer / queue (unchanged, G1+G2)
  → else: execute directly with spine context
      resolveTask = explicit /resume|task_id;
      else unique active Task Reference (mention or continuation policy);
      else current open label or new placeholder
```

- The implicit-continuation LLM upgrade (`intent.continue_window`) is REMOVED
  (P3) — attachment no longer affects context, so the call bought nothing.
  `intent.continue_window` in existing configs is ignored (deprecated field);
  `router.UpgradeTaskToContinueWithLLM` was deleted.
- No disambiguation machinery at ingress. Two games in context and an
  ambiguous "这个游戏动画不好看" → the agent asks "九七还是坦克?" as a normal
  turn. Judging with context in hand always beats classifying without it.

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
`/task <id>` detail, `/task <id> runs|rename|pin|unpin|archive|references` and
`/task <id> reference add|remove <name>` (archive is a terminal
status: hidden from open lists, recall label cards, and the pre-label guess;
only an explicit `/resume <id>` reopens it). Short ids (`task_xxxxxxxx`) are
shown and accepted back. Implementation: `httpapi/task_view.go`, shared by CLI
and IM via the control-command path; `/task` is registered in
`internal/gateway/command`. `/tasks search <keyword>` queries complete visible
history, including prior run summaries and handoff file paths. Status/view,
workspace, keyword, and pagination are applied in SQLite (`--workspace`,
`--page`, `--limit`) instead of filtering a fixed recent window in memory; the
default open view stays bounded by `tasks.default_list_limit`.

### Task governance (post-run, reversible)

Tasks remain work labels, but a long-lived assistant also needs label hygiene:

- `work` and `recurring` labels are visible; one `inbox` label per
  person/workspace is hidden and archived.
- One logical `PostRunAnalyzer` result per eligible run combines task-label
  hygiene and durable user/workspace fact extraction. Several same-person,
  same-workspace results may share one provider call according to
  `tasks.maintenance_debounce`, `tasks.maintenance_max_wait`, and
  `tasks.maintenance_batch_max_runs`. It uses the stable `memory_extract`
  semantic role: `models.roles.memory_extract` is the optional advanced
  override and `models.auxiliary` is the shared floor. It may answer `INBOX`
  only for casual,
  identity/model, or one-off diagnostic turns with no durable work thread. It
  never runs at ingress and never changes the context the completed run saw.
  Recent turns remain immediately available from the person work spine; only
  label and long-term-memory governance is delayed.
- Analysis is eligibility-gated: a new placeholder, real cross-label
  ambiguity, or a substantive durable outcome may trigger one call. A simple
  established-label turn with no durable facts skips it. Explicit attachment
  is never relabeled, though a substantive run can still yield memory facts.
- Inbox is excluded from `/tasks`, recall cards, current-task selection, and
  implicit continuation. Runs and events remain stored for audit.
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
- **P3 labels & view: ✅ landed 2026-07-06.** resolveTask → pre-label;
  post-run labeler + re-point; `/tasks` aggregation; `/task` subcommands
  (runs/rename/pin/unpin/archive); hidden Inbox and retention governance;
  complete-history task search; implicit-continuation LLM upgrade removed.
- **P4 eval:** the ten acceptance scenarios below, recorded cassettes,
  zh/en/elliptical/cross-endpoint coverage.

One package per worktree; full tests + `-race`; deploy; owner verifies before
the next package (standing workflow).

## Acceptance scenarios (definition of done)

> Coverage shipped (P4, 2026-07-06): `evalcases/timeline/` + the scenario→test
> table in its README.

1. "用JS写九七游戏" → new label, new run.
2. "再多做几个角色" → context continuous via spine tail (no routing); same
   label, new run.
3. "帮我总结今天股市" → new label; game chatter in the spine does not
   interfere (models handle topic interleaving inside a window).
4. "这个游戏动画不好看" with two game labels → the agent ASKS which one,
   in-turn.
5. Task started on CLI; WeChat says "继续优化那个九七游戏" → spine is
   cross-endpoint, context is simply there.
6. `/tasks` shows aggregated threads, not one row per message.
7. A mislabel only affects display; `rename`/re-point fixes it.
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
| Labeler misbehaves | Harmless domain (display only) + rename/archive human override |
