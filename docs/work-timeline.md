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

- **v1 — SHIPPED (P2, 2026-07-06):** `semantic_recall`-role query expansion +
  FTS (BM25) over spine entries, task label cards, and artifacts, wired into
  the selector as an automatic slice instead of a model-invoked tool only.
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
  through the card's work line). Budget: ≤3 slices, ~400-char excerpts, one
  slice per work line (label card beats raw session fragment), current task
  excluded; control-command-shaped and <6-rune messages skip recall entirely.
  Expansion runs ONLY when a `semantic_recall` role model is explicitly
  configured (`app.SemanticRecallExpander` — never the main coding model),
  bounded by a 3s timeout, degrading to raw-term FTS on any failure.
  Observability: redacted `context.recall` task event (source counts + refs,
  no excerpts).
- **v2 (later):** true embedding vector index (spine entries + label cards +
  artifacts). Interface reserved in v1: `httpapi.RecallSource`
  (`Search(ctx, RecallQuery) []RecallHit` with work-line dedupe keys) — an
  embedding-backed source registers alongside the FTS sources without
  reshaping the selector, budget, or dedupe logic.
- Degradation chain: recall miss → the agent asks in-turn, or
  inspect-before-build reads the workspace (both already shipped). Never a
  silent wrong attach.

### Labels (task demoted, name kept) — SHIPPED (P3, 2026-07-06)

- After a run finishes, a cheap model (memory_extract role) assigns the run to
  an existing OPEN task label or creates a new one (input: turn summary +
  open-label list). Implementation: `httpapi/run_labeler.go` (`RunLabeler` on
  `Server.Labeler`, built by `app.NewRunLabeler` from the memory_extract
  provider; nil in eval = KEEP everything). Contract: one-line reply
  KEEP / MOVE:<task_id> / TITLE:<short title>; MOVE only to an OFFERED open
  label; every failure path degrades to KEEP; runs async post-finalize under
  a 10s bound, never blocking the response.
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

### Ingress (simplified) — SHIPPED (P3, 2026-07-06)

```
message → control-command filter (unchanged)
  → run active? → steer / queue (unchanged, G1+G2)
  → else: execute directly with spine context
      resolveTask = pre-label only (explicit /resume|task_id → that label;
      else current open label or new placeholder)
```

- The implicit-continuation LLM upgrade (`intent.continue_window`) is REMOVED
  (P3) — attachment no longer affects context, so the call bought nothing.
  `intent.continue_window` in existing configs is ignored (deprecated field);
  `router.UpgradeTaskToContinueWithLLM` was deleted.
- No disambiguation machinery at ingress. Two games in context and an
  ambiguous "这个游戏动画不好看" → the agent asks "九七还是坦克?" as a normal
  turn. Judging with context in hand always beats classifying without it.

### /tasks view (same name, aggregated display) — SHIPPED (P3, 2026-07-06)

Default shows open/running/waiting/paused labels with `run: N 次`, latest
activity, next-step hint; done collapses (`/tasks done|archived|all` expands);
`/task <id>` detail, `/task <id> runs|rename|archive` (archive is a terminal
status: hidden from open lists, recall label cards, and the pre-label guess;
only an explicit `/resume <id>` reopens it). Short ids (`task_xxxxxxxx`) are
shown and accepted back. Implementation: `httpapi/task_view.go`, shared by CLI
and IM via the control-command path; `/task` is registered in
`internal/gateway/command`.

## What does NOT change

Control plane (runs/approvals/queue/clarify/recovery/hard floor/grants),
presence + delivery routing, person/account identity model, compaction (A),
artifact manifest (B), transport resilience (Zero), the tasks table schema,
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
  (runs/rename/archive); implicit-continuation LLM upgrade removed.
- **P4 eval:** the ten acceptance scenarios below, recorded cassettes,
  zh/en/elliptical/cross-endpoint coverage.

One package per worktree; full tests + `-race`; deploy; owner verifies before
the next package (standing workflow).

## Acceptance scenarios (definition of done)

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
