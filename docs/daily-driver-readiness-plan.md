# SelfMind Daily-Driver Readiness Plan

Status: largely shipped (W1a/W3 ✅, W1b/W1c 🟡) — see inline markers and
`docs/STATUS.md` for the live priority list. Design note; `AGENTS.md` is
canonical, and the Phase-1 north star (cross-endpoint continuity) lives in
`docs/identity-continuity.md` — the W1 concurrency work is an enabler for that
north star, not the headline goal itself.

Companion design docs (do not duplicate here):
- TUI: `docs/tui-terminal-first-hybrid.md`
- Auth: `docs/external-auth-manager.md`

## 1. Definition of done — what "trustworthy primary tool" means

Measurable gates, not vibes:

- **Concurrency**: a long CLI run does NOT globally block a WeChat message or a
  cron job. Same-workspace writes are serialized (no clobber); cross-workspace
  and read-only turns run in parallel. OAuth refresh never breaks login under
  parallelism.
- **Resilience**: a stalled provider, slow tool, or stream error degrades
  gracefully (timeout → diagnosis → retry or clear handoff), never hangs the
  whole tool. One bad run can't sink availability.
- **TUI stability**: hybrid survives a 2h+ multi-tool session with resize across
  WSL/SSH/tmux/Windows Terminal — no freeze, leak, or objectionable flicker.
- **Memory trust**: remembers correct durable facts, does NOT promote short-term
  state to long-term, is scoped (CLI project vs IM personal vs global), and is
  explainable + correctable. A memory eval guards against regressions.

## 2. Workstreams

### W1 — Concurrency & multi-end (the #1 blocker)

Root cause confirmed: a single shared Agent serialized by global `runMu`
(`internal/kernel/agent.go:423`); `RunCoordinator`
(`internal/gateway/httpapi/run_coordinator.go`) already owns the run lifecycle
and an active-run registry, but there is no worker scheduling.

- **W1a — ExternalAuthManager** ✅ shipped (`internal/modelruntime/authmanager.go`,
  race-tested; codex + minimax migrated, Kimi pass-through). Process-global,
  per-auth-file, single-flight refresh + quarantine + atomic write. Removes the
  concurrent-OAuth-refresh hazard before parallel workers.
- **W1b — Worker pool + queue policy** 🟡 shipped behind `SELFMIND_WORKERS`
  (default 1 = unchanged): `internal/runpool` scheduler (race-tested) + Gateway
  checkout + `app.MaybeEnableWorkerPool` (wired in both the CLI and daemon paths).
  Per-workspace serialization, bounded concurrency. **Framing correction:** the
  pool only helps *within one process*; multi-terminal concurrency requires
  **daemon-client convergence** (every terminal → one daemon), not cross-process
  locks. Foundation shipped — `gateway.EnsureRunning`, CLI auto-start,
  `internal/gateway/client` daemon-backed `MessageProcessor`, TUI client mode
  behind `SELFMIND_TUI_CLIENT=1` — see `docs/worker-pool-design.md` §8. **Pending:**
  default the TUI to client mode + slash-command parity over the API; per-write
  serialization + per-provider cap; real soak at N>1. Original spec:
  a pool of Agent workers sharing stable
  deps (memory, tools, providers, auth manager, one `http.Client` per provider).
  Each worker runs **one turn at a time** (no data race), but workers run in
  parallel. Scheduling policy:
  - per-**person** active-run guard stays (one interactive run per human);
  - per-**workspace** serialization for write turns (avoid concurrent file
    clobber on the same project);
  - cross-workspace and read-only/query turns run concurrently;
  - per-**provider** concurrency cap + 429 backoff (esp. Kimi).
- **W1c — Run resilience** 🟡 progress watchdog shipped
  (`internal/runpool` `WithWatchdog`, race-tested; wired in
  `router/events.go`, gated by `SELFMIND_RUN_IDLE_TIMEOUT`, default off). A run
  that emits no progress event for the idle window is cancelled with an
  actionable "stalled — freed the worker, retry/refine" handoff, so a stuck
  provider/tool doesn't hang the tool or block the pool. Remaining (optional):
  per-run wall-clock cap and an automatic single retry on transient stall
  (per-attempt retry already exists in the agent/adapters).

### W2 — TUI hardening & cleanup

- **W2a — Soak test** (gate before deleting legacy): 2h+ session, many tool
  calls, repeated resize, on WSL / SSH / tmux / Windows Terminal. Watch for
  freeze, memory growth, scrollback ordering/flicker under heavy streaming.
- **W2b — `tea.Println` maturity**: only if soak surfaces ordering/flicker —
  batch/sequence committed cells, guard rapid-stream redraws.
- **W2c — Delete legacy path**: once W2a passes, remove the alt-screen viewport,
  `controller_mouse.go` selection, app scroll, `renderCache`, and the
  `SELFMIND_TUI_LEGACY` hatch. Large simplification.
- **W2d — File-change & history polish**: write_file overwrite real diff ✅
  shipped — `write_file` captures the pre-image and returns a bounded unified
  diff (`unifiedLineDiff` in `internal/tools/linediff.go`), rendered colored via
  `renderWriteFileCell`; tested. Remaining: `/history` in-overlay search +
  filter by run/task + `control.db`-backed history beyond the window.

### W3 — Memory governance ("越用越放心")

Current: `Fact{ID, Target, Content, CreatedAt}` (`internal/kernel/memory/fact.go`)
+ `ProfileSynthesizer` (`internal/kernel/profile_synthesizer.go`). Usable v1;
thin metadata, no confidence model, coarse selection.

- **W3a — Fact metadata** ✅ shipped: `Fact` gained `source`, `scope`,
  `confidence`, `created_from_run`, `last_verified_at`
  (`internal/kernel/memory/fact.go`); SQLite migration adds the columns to
  existing DBs (`addMissingColumns`, backward-compatible — legacy rows read with
  zero metadata); `AddFactMeta` writes them, `GetFacts` reads them. Tested
  (round-trip + migration). Population (by extractors) is W3b/W3c.
- **W3b — Confidence scoring** ✅ shipped: `BaseConfidence` (by source),
  `RepetitionBoost`, `EffectiveConfidence` (90d half-life decay + 20% floor;
  legacy/unscored → neutral 0.5) in `memory/governance.go`. Tested.
- **W3c — Scope separation** ✅ shipped: `DeriveFactScope` (user prefs → global,
  environment facts → `workspace:<id>`); the fact + turn extractors now write
  via `AddFactMeta` with source/scope/confidence/created_from_run
  (`kernel/fact_meta.go`). Tested.
- **W3d — Confidence/scope-aware selection** ✅ shipped (memory dimension):
  `SelectFacts` ranks facts by decayed confidence × scope relevance and is wired
  into `buildSystemPrompt` (replaces plain last-20; legacy facts not dropped).
  Tested.
- **W3d — cross-source event relevance** ✅ shipped: `rankTaskEvents` +
  `eventTypeWeight` (`gateway/httpapi/context_ranker.go`) keep the most relevant
  task events (outcomes/handoffs/errors/plan/tool) within the per-turn budget
  instead of just the most recent; the selector now fetches a 40-event window
  and ranks to 8. Tested. Remaining: a single unified pool fusing memory +
  events + artifacts + summaries across the kernel/httpapi boundary (each
  dimension now ranks within itself).
- **W3e — `/memory` transparency and governance UX** ✅ shipped: `/memory`
  is a short health dashboard and category directory; it never expands the
  full fact list. `/memory category <name> [page]` provides a paged, ranked,
  actionable view with stable short references, while `/memory conflicts`
  isolates the only items that normally require human attention. `/memory
  show` explains canonical status, protection, confidence and supporting
  observations; `/memory search` returns short references; `/memory correct`
  promotes user authority and `/memory forget` remains audited. `/memory raw`
  is the explicit evidence/provenance view, and history/undo/pin remain.
- **W3f — Memory eval** ✅ shipped: `TestMemoryEvalGuarantees` +
  `TestMemoryEvalProviderRoundTrip` encode the guarantees (user-stated outranks
  turn-extracted, fresh-high beats stale-low, workspace scoping, legacy
  retained) as regression guards over the full write→read→select pipeline.

## 3. Dependencies & recommended sequence

```
W1a (auth manager)  ──►  W1b (worker pool + queue)  ──►  W1c (resilience)
                                   │
W2a (soak, continuous during daily use) ──► W2c (delete legacy)
W3 (governance) runs in parallel; gate "越用越放心" on W3a–W3c + W3f
W2d / polish: last
```

Recommended order:
1. **W1a** — auth manager (unblocks safe parallelism). ← start here
2. **W1b** — worker pool + queue (the headline capability) — while **W2a** soak
   runs in the background of daily use.
3. **W1c** — resilience.
4. **W3a–W3c + W3f** — memory governance + eval (makes it trustworthy to rely on
   memory).
5. **W2c** — delete legacy TUI once soak is clean; then **W2d / W3d / W3e**
   polish.

Rationale: W1 is the only thing that actually blocks the "multi-end 24/7"
target; W3 is what converts "usable" into "trusted"; W2 is mostly verification +
cleanup. Each step is independently shippable and eval-guarded; the
`SELFMIND_TUI_LEGACY` hatch and the transparent auth manager keep risk bounded.

## 4. Acceptance checklist (the gate)

- [ ] CLI long run + concurrent WeChat task + cron job all progress (no global
      block); same-workspace writes serialized; OAuth refresh single-flight (one
      refresh under N parallel callers); no login breakage.
- [ ] Per-provider rate-limit backoff verified (Kimi parallel, no auth/429
      cascade).
- [ ] A stalled provider/tool times out and hands off without hanging others.
- [ ] 2h soak across WSL/SSH/tmux/Windows Terminal: no freeze/leak/flicker;
      legacy path deleted.
- [ ] Memory: facts carry source/scope/confidence/freshness; scoped correctly;
      `/memory` shows provenance + allows correct/delete; memory eval green.
- [ ] Eval suite covers concurrency auth, codex store=false/stream-only/account
      header, memory recall — all green in CI.

## 5. Out of scope (this phase)

- Real security sandbox (namespace/seccomp/cgroup) — tracked separately.
- Full SaaS control plane / billing / multi-tenant isolation hardening.
- Embedding-based semantic recall (future selector extension).
