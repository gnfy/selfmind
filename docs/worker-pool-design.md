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

- **Per-person**: production defaults to at most two active top-level work Runs
  per human. A transaction checks the configured capacity when creating each
  Run; the registry retains cancel/steer handles for several Runs but is not
  authority.
- **Per-workspace write-serialization**: a turn that may write files acquires a
  per-workspace token; concurrent same-workspace writes queue (avoid clobber).
  Enforced read-only turns skip it; an unknown workspace takes a conservative
  person-scoped token.
- **Cross-workspace + read-only** turns dispatch to any free worker
  concurrently.
- A durably checkpointed human wait releases both its worker and physical-root
  occupancy. Its scope, question, approval and effect claims remain durable.
  An exact answer reacquires current occupancy; it does not bypass another
  executing Run. If another Run used overlapping writable roots during the
  wait, the saved pending batch is returned to Main as non-dispatched for
  revalidation. Unavailable or oversized activity evidence takes that same
  conservative path. Prior completed results and the exact human answer stay
  paired; uncertain external claims are never cleared by this release.
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
During normal operation, a confirmed non-dispatch whose claim-return write
failed is retried by the existing scheduler with the original token. Lease
expiry alone never requeues a live worker. A pre-Run worker failure retains the
original input for a later tick and reports startup failure to its own session;
it cannot assume a Run finalizer saved a CLI result or silently settle as done.
The claim path refuses legacy queued rows already bound to a Run. A completed
watcher finalization whose result delivery was interrupted retries only the
durable outbound result, using the original Run's recorded reply.
This contract is deliberately runner-ready: a future remote worker can carry
the same token without changing queue semantics.

A row also carries the admission snapshot the work was accepted with
(`docs/work-timeline.md`, "Admission snapshot and continuation fidelity"),
and the Run a claim binds records it verbatim in the same transaction, so a
drain restores authority instead of resolving it again. A row whose own or
exact parent's snapshot cannot be read is never claimed: it holds its source
like a blocked head, the person is told once, and `/queue drop` removes it.

At a capacity above one, the queue scans due rows in priority/FIFO order
and leaves a directory-blocked source in place while another source with
non-overlapping physical roots may fill a free slot. It uses the worker pool's
symlink-aware root identity and preserves per-source order; the worker pool
still makes the final lock decision. Unknown roots remain conservatively
blocked behind any active Run. The default person limit is two.

## 3a. Scheduling primitive — shipped

`internal/runpool` (`Pool`, race-tested) implements §3's core: bound total
concurrency to N workers, serialize jobs sharing a non-empty key (workspace),
run distinct keys in parallel, and return `ctx.Err()` for a cancelled waiter
without running. Dependency-free and tested in isolation; wiring into
`RunCoordinator` is the next step. The per-person guard stays where it is (the
existing `active map[personID]`), applied before dispatch.

## 3b. Integration — shipped (one worker per admitted Run)

- `Gateway` gained `pool *runpool.Pool` + `agents chan *kernel.Agent` and
  `EnableWorkerPool(extra)`; `runConversation` checks out a worker (serialized
  per workspace via `workspaceSerialKey`) when the pool is set, else calls the
  single agent exactly as before. `runAgentStreaming` routes through it.
  Workers are checked out in FIFO order, so even strictly sequential Runs
  alternate between workers: a part missing from one worker fails every
  other Run.
- `app.InitGateway` is the only construction path for agent workers; the
  daemon runner (`internal/runtime/gateway/runner.go`) and the eval harness
  both call it, and a source guard test keeps either from assembling worker
  parts itself. It builds every worker, the primary included, the same way:
  its own agent and dispatcher (`InitAgent` + `InitTools`), the cron tool
  bound to the one shared scheduler, and the durable background-review queue.
  Workers share only concurrency-safe dependencies: the memory and control
  stores, the global auth manager, the request gate, and one MCP connection
  per configured server. The MCP manager registers the same immutable tool
  adapters in every worker's dispatcher, syncs catalogue changes to all of
  them at once, and keeps a tool whose local name collides in any dispatcher
  out of all of them. Removing a server's tools (disconnect or shutdown) holds
  the same lock as adding them. Each adapter also carries its server's
  read-only markings: the operator's exact names, fixed at connect, and a
  lookup of the calling person's markings in `control.db` for the server's
  configured identity. Every worker therefore gives the same person the same
  answer, and a read-only marking lets two Runs on two workers call one shared
  server without an external claim. An operator name the server does not
  expose is a health warning, not a failure (`docs/tool-safety.md`).
- The hidden per-Skill tools (`skill:*`) are one shared set as well. A Skill
  tool manager starts every worker from the primary's set. A Skill change made
  through any worker, or through `/v1/dispatch`, which always runs on the
  primary, reloads the set once and swaps it into every worker's dispatcher
  in one step, so no Run sees a worker with a Skill another worker lacks or
  has dropped. A delegated sub-agent's tool subset is not attached: a reload
  through it changes only that subset, never the parent's dispatcher.
- Startup compares each worker's runtime context budget and tool catalogue
  with the primary's: every tool's name, description, parameter schema and
  policy metadata, read while MCP catalogue changes and Skill tool reloads are
  held. A difference stops startup with an error naming the worker and the
  missing, unexpected or changed tools.
- Every worker hands background reviews to the durable maintenance queue
  (payload-hash key). A review already queued is handled by that job; only a
  failed queue write leaves the engine's in-process fallback. The maintenance
  worker executes the jobs through the primary agent's engine.
- With a single worker the gateway keeps the single-Agent path.
- `Gateway.SetWorkerLeaseHook` reports each turn's worker by construction
  index (the primary is 0, also on the single-Agent path) after checkout and
  before the agent starts; a hook error ends the turn before the agent runs.
  The daemon installs none. The eval harness uses it to attribute Runs to
  workers and to hold a worker in model-free overlap cases
  (`docs/eval-loop.md`).
- Tests: `runpool` (race), `TestResolveWorkerCount*`,
  `TestWorkerLeaseHook*`,
  `TestRunRefusesInvalidWorkerCountBeforeOpeningStores`,
  `TestWorkspaceSerialKey`, `TestEnableWorkerPoolWiring`,
  `TestInitGatewayBuildsWorkersWithIdenticalCatalogsAndOneMCPConnection`,
  `TestInitGatewayRefusesToStartWhenOneWorkerDiffers` (an unexported
  per-worker build hook makes one worker differ, so dropping the startup
  comparison fails it),
  `TestSequentialRunsOnDifferentWorkersCallMCPAndCron`,
  `TestTwoWorkersCallTheSharedMCPServerInParallel` (race),
  `TestTwoRunsCallAMarkedMCPToolConcurrentlyWithoutClaims` (race, capacity
  two), `TestUnmarkedMCPToolStillClaimsTheUnknownTargetAtCapacityTwo`,
  `TestSkillChangeThroughDispatchReachesRunsOnEveryWorker`,
  `TestSkillChangeThroughAnyDispatcherReachesEveryDispatcher`,
  `TestSkillReloadSwapsTheSkillToolsInOneStep` (race),
  `TestMCPToolManagerHoldCatalogNeverSeesAHalfRemovedServer`,
  `TestSharedToolMetadataReadsNeverWriteTheTool` (race),
  `TestEveryWorkerQueuesBackgroundReviewDurably`, and
  `TestDaemonAndEvalBuildAgentWorkersOnlyThroughInitGateway`. Tests that need
  a real MCP peer build the stdio stub in `internal/tools/mcptest`.
- The session-concurrency plan (`docs/plans/session-concurrency.zh-CN.md`)
  owns the 2026-10-04 owner-authorized default of two, further capacity changes,
  and remaining sustained-throughput and real IM acceptance. Changing the
  default does not declare those gates passed.

## 4. Configuration and rollout

- `gateway.max_active_work_runs` — default **2**, supported range **1–3**;
  daemon startup rejects an out-of-range value. Explicit capacity **1**
  serializes work. Existing explicit values are preserved.
- `SELFMIND_WORKERS=N` — unset or blank: one worker per admitted Run, so the
  count equals `gateway.max_active_work_runs` (two by default, one for
  capacity 1). An explicit N must be a whole number no lower than the
  capacity; values above **16**, however large, are reduced to 16. A lower,
  zero, negative or malformed value stops daemon startup before any store or
  worker is opened, with an error that names `SELFMIND_WORKERS` and, when it
  is too low, `gateway.max_active_work_runs`.
- The default change reuses Run admission, independent worker checkout,
  resource conflict protection and provider request limits. It does not relax
  these checks or alter delegation ownership.

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
- Shared tools: one MCP SDK session per configured server serves every
  worker, and the SDK handles concurrent calls. Each MCP adapter and per-Skill
  tool is one instance registered in every worker and never modified after
  registration; reading a tool's metadata fills defaults into a copy instead
  of the tool (race-tested through the production dispatchers). ✅
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
  actual process overlap when every probe is isolated, or exclusive process
  intervals whenever a probe runs on the host, bound to successful typed
  process receipts. Active-Run peaks prove admission only; model-call
  concurrency has separate request-gate evidence. `--restart --capacity 2`
  checks exact continuation and no duplicate local effects after a forced daemon crash.
- Checkpointed foreground model calls use up to 250ms cancellable capacity
  grace before yielding a worker on route-capacity or
  429 waits. The exact Run, model-call ledger, and delayed queue child commit
  before the Agent is released; a failed checkpoint retains the cancellable
  in-place wait. Go/race tests cover ownership, rollback, and the freed worker.
  A real-provider cooldown and daemon-restart soak remains required before
  raising the default person capacity.

- External resource waits share the admission blocker projection. A Run cannot
  park waiting for itself to observe an uncertain effect. A live other owner
  or effect-bound watcher may provide observation; losing that producer
  atomically preserves the Plan and effect claim in a blocked, resumable
  outcome, with a durable notification retry. This never clears uncertain
  claims or replays an effect. Result-save failure rolls the correction back;
  replay after commit keeps one result and one notification source.

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
  and wait on the winner. A local daemon must also answer on its local control
  socket: one from before the socket answers health but cannot serve the CLI, and
  is named with the restart that fixes it. Tested (`ensure_test.go`).
- CLI client auto-start: `selfmind send/status/workspace ...` and the
  `SELF_USE_GATEWAY` REPL call `EnsureRunning` before their first request
  (`internal/cliapp/gateway_autostart.go`), but only for a loopback target — a
  remote URL is never auto-started.
- Daemon-backed `MessageProcessor` (`internal/gateway/client`): POSTs `/v1/message`
  over the local control socket, the only channel that carries CLI identity (the
  CLI hands the client that transport; there is no fallback to TCP) (synchronous
  answer = source of truth) and concurrently polls
  `/v1/tasks/events` for the person's current task, mapping each `control.Event`
  back to an `llm.StreamEvent` and feeding it into the ctx stream observer the TUI
  already consumes (`httpapi.StreamObserverFromContext`). Streaming is
  best-effort; correctness never depends on it. Tested (`client_test.go`).
- Rich TUI thin-client mode (`internal/cliapp/tui_client.go`): `EnsureRunning`,
  build a UI-only controller, install the daemon `MessageProcessor`, and route
  agent-backed commands through daemon APIs, all on the local control socket where
  the daemon verifies this process as the person's own CLI. There is no
  in-process opt-out or correctness fallback. A process the daemon started
  (a tool, a scheduled job) is never accepted there, so it cannot drive the TUI's
  endpoints either; `selfmind doctor` reports the channel.
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
