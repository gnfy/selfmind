# Worker Pool & Queue — Design (W1b)

Status: largely shipped — see the inline shipped markers and `docs/STATUS.md`
for what remains. Builds on the shipped `ExternalAuthManager`
(`docs/external-auth-manager.md`). Part of `docs/daily-driver-readiness-plan.md`
W1. Design note; `AGENTS.md` is canonical.

## 1. Problem (confirmed in code)

A single shared `Agent` serializes every run through a global mutex
(`internal/kernel/agent.go:423` `a.runMu.Lock()`), so a long CLI run blocks a
WeChat message or a cron job even across different people/workspaces. The reason
the Agent can't just be unlocked is **per-run mutable state on the struct**:

- `a.runLLM` — the active provider, set per run under `runMu`
  (`agent.go:465`, reset via defer);
- evolution counters `a.toolCallCount` / `a.turnReviewCount` (mutated/reset
  during a run, ~`agent.go:999`).

Two concurrent turns on one Agent would clobber these. The `WithX` methods are
construction-time config, not per-run.

Already present (reuse): `RunCoordinator` holds a per-person active-run registry
(`internal/gateway/httpapi/run_coordinator.go`, keyed by person and exact Run handle).
`RunConversation` is invoked from the gateway router (`router/gateway.go:119/164`),
the local CLI path (`cli/agent_events.go:116`), and delegation sub-agents
(`app/multi_agent.go:165`, `app/delegation.go:80` — these already build a
separate `subAgent`, so they don't share per-run state).

## 2. Topology decision — pool of N single-threaded Agent workers (Option A)

Each worker owns its **own** `*Agent` (own `runLLM`/counters → no cross-worker
race) and runs **one turn at a time**. Workers share **stable, concurrency-safe
deps**: one `MemoryManager`, one tools backend, providers (through the global
auth manager + one shared `http.Client` per provider), the control store.

- **Why not Option B** (one Agent running parallel turns): that requires moving
  `runLLM` + counters off the struct into a per-run object/context — a broad,
  risky refactor of the run loop. Option A reuses the **proven single-turn
  Agent code unchanged** and isolates per-run state by construction. Option B
  stays a future cleanup.

A worker keeps its `runMu` as a cheap re-entrancy guard (a worker is
single-threaded by construction, so it's uncontended).

## 3. Scheduler / queue policy (RunCoordinator becomes the dispatcher)

- **Per-person**: production still admits at most one active interactive Run
  per human. A transaction checks the capacity when creating each Run; the
  registry retains cancel/steer handles for several Runs but is not authority.
- **Per-workspace write-serialization**: a turn that may write files acquires a
  per-workspace token; concurrent same-workspace writes queue (avoid clobber).
  Enforced read-only turns skip it; an unknown workspace takes a conservative
  person-scoped token.
- **Cross-workspace + read-only** turns dispatch to any free worker
  concurrently.
- **Per-provider** concurrency cap + 429 backoff (esp. Kimi static key — no
  auth hazard but provider-rate-limited).
- Bounded dispatch queue: when all workers are busy a run **waits with a visible
  "queued" event** (don't silently block the caller; surface progress per the
  AGENTS.md "hard tasks must not look stalled" rule).

Durable queue ownership (shipped 2026-08-08) is a lease, not a status guess.
Claiming a row atomically writes an opaque claim token, lease deadline, and
attempt generation; only that token may bind the created run or renew the
lease. Run creation and queue binding now commit in one transaction. Boot
retries an unbound claim, while a bound Run with uncertain effects keeps its
identity for exact-Run recovery instead of being blindly dispatched again.
The claim path refuses legacy queued rows already bound to a Run. A completed
watcher finalization whose result delivery was interrupted retries only the
durable outbound result, using the original Run's recorded reply.
This contract is deliberately runner-ready: a future remote worker can carry
the same token without changing queue semantics.

At a test capacity above one, the queue scans due rows in priority/FIFO order
and leaves a directory-blocked source in place while another source with
non-overlapping physical roots may fill a free slot. It uses the worker pool's
symlink-aware root identity and preserves per-source order; the worker pool
still makes the final lock decision. Unknown roots remain conservatively
blocked behind any active Run. Production's person limit remains one.

## 3a. Scheduling primitive — shipped

`internal/runpool` (`Pool`, race-tested) implements §3's core: bound total
concurrency to N workers, serialize jobs sharing a non-empty key (workspace),
run distinct keys in parallel, and return `ctx.Err()` for a cancelled waiter
without running. Dependency-free and tested in isolation; wiring into
`RunCoordinator` is the next step. The per-person guard stays where it is (the
existing `active map[personID]`), applied before dispatch.

## 3b. Integration — shipped (flagged, default off)

- `Gateway` gained `pool *runpool.Pool` + `agents chan *kernel.Agent` and
  `EnableWorkerPool(extra)`; `runConversation` checks out a worker (serialized
  per workspace via `workspaceSerialKey`) when the pool is set, else calls the
  single agent exactly as before. `runAgentStreaming` routes through it.
- `app.MaybeEnableWorkerPool` reads `SELFMIND_WORKERS` (default 1), builds N-1
  fully independent worker agents (own `InitAgent`+`InitTools`, sharing only the
  serialized memory/skill stores + global auth manager), and enables the pool;
  wired in the daemon runner (`internal/runtime/gateway/runner.go`), the only
  execution path. **N=1 is a no-op → default path byte-identical.**
- Tests: `runpool` (race), `TestWorkerCountParsesEnv`, `TestWorkspaceSerialKey`,
  `TestEnableWorkerPoolWiring`. Existing suite green at default.
- **Pending: real soak** at `SELFMIND_WORKERS=4` (CLI + WeChat + cron) before
  raising the default — the concurrent-execution correctness can't be fully
  headless-verified. While one run per person holds, N>1 only overlaps
  different people's runs; the session-concurrency plan
  (`docs/plans/session-concurrency.zh-CN.md`) owns the default change
  and the soak.

## 4. Flag & rollout (zero risk until opted in)

- `SELFMIND_WORKERS=N` — default **1** = today's single-worker serialized
  behavior (no change). `N>1` enables the pool.
- `gateway.max_active_work_runs` — default **1**. Explicit capacity 2 or 3
  requires at least as many `SELFMIND_WORKERS`; daemon startup rejects a
  mismatched or out-of-range setting. This is a controlled test setting while
  the session-concurrency release gates remain open, not a default rollout.
- Step 1: an `AgentFactory` that builds a worker Agent from shared deps; wire a
  `Dispatcher` in `RunCoordinator` behind the flag; default 1 keeps the current
  path.
- Step 2: route gateway-router + CLI runs through the dispatcher; delegation
  sub-agents already use separate instances (verify they draw workers from the
  pool or stay independent — independent is fine).
- Step 3: enable `N>1` in dev, run the verification below, then make a sensible
  default.

## 5. Concurrency-safety audit (gate before enabling N>1)

Each must be verified safe-to-share or made per-worker:

- **MemoryManager / control store**: backed by `database/sql` (concurrency-safe
  pool); ensure SQLite is WAL or serialized and there is no unsynchronized
  in-memory map. Verify.
- **Tools backend / dispatch**: concurrent dispatch across workers must be safe;
  shared tool state (e.g. the background process registry) must be keyed and
  guarded. Verify per tool; workspace-scope middleware already binds paths.
- **Providers / adapters**: shared `http.Client` is safe; auth via single-flight
  manager (done). One client per provider, never per worker.
- **Reflector / ReviewEngine / contextEngine / extractors**: if they hold mutable
  state, give each worker its own instance (cheap) rather than sharing.

Anything not provably safe → per-worker instance via the factory.

**Audit results (so far):**
- Memory store: `SQLiteProvider` uses `db.SetMaxOpenConns(1)`
  (`internal/kernel/memory/sqlite_provider.go:75`) → all access serialized to one
  connection: **safe** (no lock race/corruption), only a throughput serialization
  point. ✅
- Control store: `?_journal=WAL&_sync=NORMAL` + `SetMaxOpenConns(1)`
  (`internal/control/store.go:125`) → **safe**. ✅
- `MemoryManager.providers` map: written only by `RegisterProvider`
  (`internal/kernel/memory/provider.go:44`). **Safe if construction-only** —
  confirm it is never called mid-run; if it can be, guard with a mutex. ⚠️
- `SemanticExpander` cache: has its own `sync.Mutex` (`memory/expander.go:20`) →
  **safe**. ✅
- **TODO before enabling N>1**: audit the tools backend dispatch + background
  process registry; give each worker its own Reflector/ReviewEngine/
  contextEngine/extractors (cheap) rather than sharing.

**Conclusion:** Option A is viable — the shared persistence layers are already
serialized-safe; per-worker isolation is needed only for the cheap in-memory
engines and must be verified for the tools/process registry.

## 6. Verification

- `-race` tests: N workers dispatched concurrently on distinct
  persons/workspaces → no race, correct isolation; two same-workspace write
  turns serialize; per-person 2nd run is guarded.
- Auth under load: N parallel runs on codex → exactly one token refresh
  (single-flight, already covered) and no login breakage.
- Soak (real): `SELFMIND_WORKERS=4`, CLI long run + concurrent WeChat task +
  cron job all progress; provider-stall on one worker doesn't block others
  (ties into W1c resilience).
- Same-person live soak: `python3 scripts/soak-parallel-runs.py --config
  ~/.selfmind/config.yaml --capacity 3 --third-im` uses isolated control data
  and tests two CLI Runs plus one IM Run, exact IM supplement routing, and
  overlapping terminal execution. `--restart --capacity 2` checks exact
  continuation and no duplicate local effects after a forced daemon crash.

## 7. Non-goals

- Not distributed/multi-process workers — single-process pool only.
- Not Option B (per-run-state extraction) — future cleanup.
- Resilience (timeouts/stall handling) is W1c, layered on top.

## 8. Multi-terminal: daemon-client convergence (the framing fix)

The pool above solves concurrency **inside one process**. Earlier SelfMind
versions let the rich Bubble Tea TUI build its own gateway and open
`control.db`, which made every terminal a separate owner process. That design
has been removed: CLI/TUI, IM, HTTP and scheduled work now execute through one
daemon. The clients do not own an agent, worker pool, auth manager, or control
database.

Studying mature single-daemon agent stacks (one runs a single app-server daemon
with terminals as Unix-socket clients, all serialization + one `AuthManager` in
that process; another runs a single gateway with TUI/dashboard as
JSON-RPC/WebSocket clients) showed both converge
on the same answer: **one owner process; every UI is a thin client.** Neither
coordinates multiple terminals with cross-process business locks — that path adds
complexity (auth file locks, cross-process DB write serialization, per-workspace
file locks) to work around a problem that disappears if you collapse to one
process. This matches the existing `AGENTS.md` rule that `selfmind gateway` is the
multi-terminal product entrypoint.

So the worker pool is correct but was **mounted on the wrong door**: it lives in
the daemon, while the TUI bypassed the daemon. The fix is to route the TUI
through the daemon, after which the pool, the single auth manager, per-workspace
serialization, and one `control.db` owner all apply to multi-terminal use.

### 8a. Shipped (daemon-only foundation)

- `gateway.EnsureRunning` (`internal/runtime/gateway/ensure.go`): discover a live
  daemon or auto-start a detached `selfmind gateway run`, then wait on `/health`.
  Race-safe across processes — the `gateway.lock` flock in `Acquire()` guarantees
  exactly one daemon wins a concurrent start; losers observe `ErrAlreadyRunning`
  and wait on the winner. Tested (`ensure_test.go`).
- CLI client auto-start: `selfmind send/status/workspace ...` and the
  `SELF_USE_GATEWAY` REPL call `EnsureRunning` before their first request
  (`internal/cliapp/gateway_autostart.go`), but only for a loopback target — a
  remote URL is never auto-started.
- Daemon-backed `MessageProcessor` (`internal/gateway/client`): POSTs `/v1/message`
  (synchronous answer = source of truth) and concurrently polls
  `/v1/tasks/events` for the person's current task, mapping each `control.Event`
  back to an `llm.StreamEvent` and feeding it into the ctx stream observer the TUI
  already consumes (`httpapi.StreamObserverFromContext`). Streaming is
  best-effort; correctness never depends on it. Tested (`client_test.go`).
- Rich TUI thin-client mode (`internal/cliapp/tui_client.go`): `EnsureRunning`,
  build a UI-only controller, install the daemon `MessageProcessor`, and route
  agent-backed commands through daemon APIs. There is no in-process opt-out or
  correctness fallback.
- Verified headlessly end-to-end against an isolated daemon (temp data dir +
  unused port): first call auto-starts and answers `/status`; a second call
  reuses the running daemon (no restart); `send` round-trips a real turn through
  `/v1/message`; clean stop.

### 8b. Slash-command parity in client mode — shipped

Agent-backed slash commands now work in client mode through a single dispatch
seam (`uiModel.dispatch`): it routes to the daemon's safelisted
`/v1/dispatch` endpoint (`Gateway.DispatchTool` → agent backend). This covers
the dispatch-backed subcommands —
`/skills` (history/undo/catalog/install/audit/delete/pin/reload), `/memory`
(history/remove/undo/pin), `/bundles`, `/checkpoint`. Commands backed by
tenant-scoped local helpers (`/skills list/view/search/archive`, `/curator`)
already work client-side; `/status` and `/resume` route through the message
processor. The dispatch safelist is read/curate/learning-management tools only —
**workspace-mutating / code-executing tools are refused** (HTTP 403) so
`/v1/dispatch` is not a backdoor around workspace scope, approval, and run
events. Person-partitioned memory/session reads use structured daemon APIs;
commands not yet supported remotely return a clear notice rather than opening a
second local ownership path.

### 8c. Workspace serialization: enforced capability — shipped

`workspaceSerialKey` uses the final tool capability, not a `ToolMode` hint.
The normal agent-first surface exposes write tools even when the mode says
`LocalRead` or `Web`, so these turns still take the physical-root lock. A
no-action turn or a trusted verification-only recovery (whose dispatch guard
refuses mutation) can share a read view. Missing roots take a person-scoped
fallback lock rather than the empty key. Tested in
`router/workspace_serial_test.go` and `router/gateway_pool_test.go`.

### 8d. Daemon-only ownership — shipped

The bare `selfmind` TUI auto-starts or attaches to the daemon and runs only as
a thin client. If the daemon cannot start, startup fails with actionable
diagnostics; it never creates a local agent as a fallback. `/memory list` and
session search use daemon-owned, person-partitioned data, so every terminal and
IM endpoint sees one consistent history.

### 8e. Interactive approval in client mode — shipped

Tool approvals now prompt inline in the client TUI. The daemon already appends an
`approval.requested` event to `control.db` and blocks the run polling for a
response (`RunCoordinator.toolApprovalHandler`); the client's unified event stream maps
that event to `MsgApprovalRequest`, the TUI shows an inline `Approve? [y/N]`
prompt, and the answer is sent via `Client.RespondApproval` →
`/v1/approvals/respond`, which unblocks the run. Bare Enter denies (safe
default). The `selfmind approve/reject <id>` and IM-button paths still work for
async/out-of-band approval. (Tested at the mapping/respond/parse layers; the
end-to-end path requires a live approval-triggering run to exercise.)

### 8f. Remaining

- **Unified live streaming is shipped**: `/v1/events/stream` carries durable
  task/run events and ephemeral assistant deltas in one `RunEvent` envelope.
  Durable events resume with `Last-Event-ID`; the synchronous message response
  remains the final-answer source of truth. The daemon assembles that answer
  from the run's live deltas, which the kernel's event channel may drop when
  its consumer falls behind. When the kernel reports such a loss
  (`stream_incomplete` on `turn.completed` and on the run's result), the run's
  own answer, which arrives on a channel that never drops, replaces the
  assembled copy. CLI consumes both classes, while
  IM/cron deliberately project low-frequency milestones and the final result.
- **Real multi-terminal soak** at `SELFMIND_WORKERS>1` to validate ordering,
  provider pressure and workspace serialization under sustained load.
- **Per-provider concurrency cap + 429 backoff**: deferred by design — belongs at
  the LLM adapter / model-gateway boundary (provider identity + the 429 response
  live there), not the run scheduler. Low value for single-user multi-terminal
  (per-person active-run guard already bounds concurrency); matters at SaaS
  scale. Env-gated, default off.
- Tools / background-process registry audit for N>1 (§5 TODO).
