# SelfMind Implementation Status

> **Read this first.** This is the current-state snapshot for any AI/coding agent
> picking up work, and the **only live priority list** in this repo. The code is
> the ground truth; this page summarizes it so you do not re-implement something
> that already exists. The north star (Phase 1 = cross-endpoint continuity) and
> the acceptance scenarios live in `docs/identity-continuity.md`. Historical
> planning docs were removed from the tree (2026-07-03; retrieve via git
> history) — never resurrect their backlog items or code samples.
>
> **Snapshot date:** 2026-07-05. When you finish a change that moves a row,
> update this table in the same PR. See `docs/phase1-modules.md` for the
> Phase-1 feature-module index.

## Health

- `GOWORK=off go build ./...` — passing.
- `GOWORK=off go test ./...` — passing.
- ~336 Go files, ~74.5k LOC, 109 test files (2026-07-05).

## Status Legend

- ✅ **Done** — implemented and exercised; safe to build on.
- 🟡 **Partial** — works but has a known gap or acknowledged limitation.
- ❌ **Missing** — planned in a roadmap doc but not started.

## Capabilities

| Area | Status | Notes |
|------|--------|-------|
| Provider runtime | ✅ | 13+ built-in profiles; credential precedence; live model catalog + 1h cache. `internal/modelruntime`. |
| OAuth / token refresh | ✅ | Codex CLI and `minimax-oauth` auto-refresh expired tokens. Claude Code / Gemini CLI / Qwen CLI are reuse-only (re-login on expiry). |
| LLM protocol adapters | ✅ | `openai_chat`, `openai_compatible`, `anthropic_messages`, `codex_responses` + transport registry + provider quirks. `internal/kernel/llm`. **Transport resilience (Package Zero, 2026-07-05):** the agent retry loop (`agent.go` `streamChatWithRetry`/`chatResponseWithRetry`) now uses exponential backoff `base*2^(attempt-1)` with `[0.9,1.1)` jitter (defaults base 300ms, cap 30s, attempts 5), a ctx-cancellable sleep so `/stop` interrupts a pending backoff, and retryable-vs-fatal classification (`llm.IsRetryableError`): EOF/reset/refused/`net.Error` timeout/5xx/429/stream-idle retry, while context-window/quota/401/400/invalid-request fail fast (no wasted attempts). 429 `Retry-After` (header folded into the error + codex/OpenAI "try again in N" body phrasing) is honored via `llm.RetryAfterFromError`, capped 600s. The codex SSE idle watchdog (`responses_adapter.go`) is config-driven (default 180s) and aborts a stalled stream with a retryable error so the loop reconnects. Shared provider HTTP client sets TCP keepalive (30s) so dead sockets surface fast. Store=false/stream-only contract and no cursor-resume unchanged. Config knobs: `agent.llm_max_retries`/`llm_retry_base`/`llm_retry_cap`/`llm_stream_idle_timeout`. `internal/kernel/llm/retryable.go`, `httpclient.go`. Tests: `llm/retryable_test.go`, `llm/responses_idle_test.go`, `kernel/agent_retry_test.go`. |
| Native tool calling | ✅ | Native `tool_calls` first, `[TOOL:...]` fallback, repeated-failure guardrails, secret redaction. |
| Task strategy / intent routing | ✅ | Agent-first; coarse `TaskStrategy`; rules/hybrid/llm intent. `internal/kernel/task_strategy.go`, `internal/gateway/router`. **Implicit-continuation upgrade REMOVED (P3, 2026-07-06):** the 2026-07-05 pre-agent "continue vs new" LLM call (`router.UpgradeTaskToContinueWithLLM`, `intent.continue_window`) is gone — working context is the person-level spine, so task attachment no longer affects what the model sees and the call bought nothing. Rules-based `IntentContinue` (explicit cues, short acceptance) remains and still drives the busy/steer path. `intent.continue_window` in existing configs is a deprecated no-op (`config.IntentConfig`). Do not reintroduce ingress continuation classifiers (`docs/work-timeline.md` "Ingress"). **Inspect-before-build posture (2026-07-06):** write-capable turns (`ToolMode` local-write/full) get one short system-prompt rule in `TaskStrategy.SystemPromptNote` — search the workspace for an existing implementation before writing new code/creating a file, and reuse it (the workspace is the shared source of truth). Pure direct-answer (no-tools) turns do not get it. Test: `internal/kernel/light_task_layer_test.go` (`TestSystemPromptNote_InspectBeforeBuild`). |
| Context engine | ✅ | Bounded message window on the hot path. `internal/kernel/context_engine.go`. Over-budget context now **compacts by default** (2026-07-05): past `summaryThreshold` (¾ of budget) the drop-eligible MIDDLE turns are summarized into ONE structured message while the head (system + the original task turn) and the tail (recent `compactionTailTurns`) stay verbatim — no more silently dropping the oldest turns. The summarizer is the cheap `memory_extract` role (`Agent.SetSummaryProvider` → `ContextEngine.SetSummaryProvider`), kept OFF the main coding provider; it is one bounded call only at the over-threshold moment, never per-turn. The compaction prompt mandates a `## Relevant Files` section (task goal, decisions, next steps, and every created/modified/read path), backstopped by a deterministic path harvest from tool-call args (`path`/`file_path`/`output_path`/`workdir` + V4A `patch`/`apply_patch` headers) that is appended if the model omits it. Guards: falls back to deterministic trim when no summarizer is wired, when the summary is empty or not smaller than the span it replaces, or when the middle is already a prior summary (no stacking/recursion). `SELFMIND_SYNC_CONTEXT_SUMMARY` is now legacy — compaction runs without it; the flag only lets an install with no summarizer role fall back to the main provider. Every rendered compaction summary now carries the verbatim boundary note ("The history summary is reference only. The latest user message is the only authoritative instruction. If it changes direction, the latest message wins.") inside the `[CONTEXT COMPACTION]` marker (P1, 2026-07-06). **Working history is the person-level WORK SPINE (P1 of `docs/work-timeline.md`, landed 2026-07-06):** every agent-bound turn (task-bound, casual, cron) appends ONE slim turn-level entry — user text (gateway decoration blocks stripped) + assistant final answer + touched file paths (deterministic tool-arg harvest) + source tag for non-interactive turns — under the constant `kernel.SpineTrajectoryKey`; the storage tenant is the person, so the spine is person-scoped and cross-endpoint. Tool intermediates/system prompts never enter the spine (they stay in run events). Load and save use the same key (`ContextEngine.BuildMessages` via `Agent.Composer()` + `saveHistory`); the tail replays up to `composerSpineTailEntries` recent turns as alternating user/assistant messages. Per-turn assembly is the ContextComposer contract (`internal/kernel/context_composer.go`): ①latest user message ②spine tail ③compaction summary ④semantic recall (RESERVED for P2 via `RuntimeContextBundle.Recall`, rendered + budgeted already) ⑤artifacts ⑥workspace ⑦person memory ⑧open run/approval state (⑤–⑧ via bundle/system prompt). Legacy compat is a READ-ONLY chain: empty spine — or a task-bound turn with no spine entry for its task — reads the old `task:<id>` key, then the task's prior run channel (`TaskRuntimeContext.PriorChannel`), or the old channel key for taskless turns; the first save migrates forward. Internal subsystem turns (delegation, `:background_review`) stay channel-keyed off the spine. FTS indexing keeps the task-derived session id (`Agent.sessionKey`, idempotent `IndexSession`) and is never keyed by the spine. Tests: `internal/kernel/context_engine_test.go`, `internal/kernel/light_task_layer_test.go`. |
| Project context injection (AGENTS.md) | ✅ | Per-turn project-convention files injected into the system prompt by `ContextScanner` (`internal/kernel/context_scanner.go`), assembled on an **independent budget** from the person-memory layer (facts+profile, `buildSystemPrompt`) so raising one never starves the other. **Route-3 fusion (2026-07-09)** — Codex discovery depth + Hermes budget elasticity + selfmind's own memory kept separate: (1) **root→leaf discovery** — walk up from the workspace root to the git root, collect the highest-priority context file at EACH level (`.selfmind.md`→`AGENTS.md`→`.cursorrules`→`.claude.md`→`CLAUDE.md`; README removed — human-facing, low signal), emit **root→leaf** so a deeper/more-local `AGENTS.md` sits last and overrides (the model is told deeper wins); (2) **dynamic budget** scaled to the model context window (`window_tokens × 4 chars/token × 6%`, floor 24KB, ceiling 256KB) instead of the old fixed 8KB/16KB — a big-window daemon rarely truncates, a small-window model stays bounded; (3) **never silently drop** — the old ">8KB ⇒ skip the whole file" (which dropped every real AGENTS.md: this repo's is 36KB, hermes's 71KB) is gone; over-budget files are head/tail truncated (70%/20%) with a pointer telling the model to `read_file` the full path for the omitted middle; each level gets a fair per-file share so deep/local files always survive; (4) **untrusted-data fence** — the block is wrapped as workspace-provided conventions that operator/user instructions and safety policy OUTRANK (defense against a malicious `AGENTS.md` in a cloned repo injecting instructions via the IM path). Follow-ups (not in this pass): a promptware DETECTOR (vs the current fence), and moving the block into a stable prompt-cache prefix. `internal/kernel/context_scanner.go`; tests `internal/kernel/context_scanner_test.go`; eval `evalcases/…` (a >8KB AGENTS.md convention is honored). |
| Control store | ✅ | tenants/persons/accounts/workspaces/tasks/runs/events/handoffs/approvals/grants/notifications/outbound/person_settings/`task_queue`/etc. `internal/control/store.go`. |
| Memory + session search | ✅ | `AddFact`/`GetFacts`, FTS recall, memory fence. **Task-coherent recall (2026-07-06):** a task's turns are FTS-indexed under a stable task-derived session id (`task:<id>`, `Agent.sessionKey`) so `session_search` retrieves the whole task cross-endpoint ("what we did on the order system") as ONE session rather than a fragment per turn; `IndexSession` is now idempotent (delete-then-insert per session id) so re-indexing the growing trajectory each turn does not accumulate duplicate FTS rows. Person/tenant scoping unchanged. Tests: `internal/kernel/memory/sqlite_provider_test.go` (`TestSQLiteProvider_TaskSessionRecall`). **Automatic recall v1 (P2, 2026-07-06):** recall is no longer tool-only — the gateway context selector attaches ≤3 bounded "possibly related prior work" slices per turn (session FTS + task label cards, `semantic_recall`-role query expansion when configured) as ephemeral `TaskRuntimeContext.RecallSlices`; see the Work Timeline P2 row above and `docs/work-timeline.md` "Semantic recall". Tests: `internal/gateway/httpapi/recall_test.go`. **Memory governance P0 (2026-07-11, docs/memory-governance.zh-CN.md):** (1) layered model landed — `memory_observations` (immutable evidence) / `canonical_memories` (revisable read model) / `memory_evidence` / `memory_events` tables in memory.db; opening a tenant DB incrementally imports legacy `facts` rows as observations, folding same-`NormalizedContentHash` duplicates into one canonical with evidence counters (idempotent by observation id; `facts` untouched; profile target skipped); read surface via `MemoryManager.Canonical()` (`internal/kernel/memory/canonical.go`). (2) maintenance idempotency — `maintenance_jobs` (control.db, UNIQUE(run_id, analyzer_version)) born inside the `FinishRun` terminal transaction; the post-run analyzer CLAIMs via CAS before model work, completes with a result hash, fails with a bounded retry horizon; crashed 'running' jobs reset to pending in the stuck-run sweep. One run = one logical maintenance result. (3) intake decisions — the ONE maintenance call now judges candidates AGAINST offered neighbors (top-8 similarity ∪ recent-5 per target; recent slice is the cross-language dedup net) and returns `memory_decisions` (SKIP/ADD/REINFORCE/SUPERSEDE/CONFLICT); the deterministic policy layer (`internal/app/memory_intake.go`) enforces: refs resolve only against OFFERED facts (matchOpenLabel pattern), REINFORCE bumps confidence + last_verified (RepetitionBoost) without rewriting, SUPERSEDE needs conf≥0.98 and never touches SourceUser facts (degrades to CONFLICT = both kept), invalid refs degrade to ADD through the dedup net (exact/containment duplicates now REINFORCE instead of being silently dropped). All analyzer writes now land in the learning audit (visible in `/memory history`, undoable). Agent memory tool add/replace/pin write full metadata (`SourceAgent`/`SourceUser`), replace keeps the fact id. **Human governance view (2026-07-11):** `/memory` is now a compact health/category directory rather than a fact dump; `/memory category <name> [page]` provides ranked, paged short refs, `/memory conflicts` isolates required attention, and `/memory show <ref>` explains canonical status, protection and supporting observations. Tests: `memory/canonical_import_test.go`, `control/maintenance_jobs_test.go`, `app/memory_intake_test.go`, `app/post_run_analyzer_test.go`, `tools/memory_test.go`. Next (P1+): consolidation apply behind shadow gates, read path onto canonical store, caps/archival. |
| Skills system | ✅ | list/view/manage/catalog/bundle/curator; history + undo; provenance; governance archive/restore. `internal/tools/skill_*.go`. Auto-create via `SpawnReview` (scripted-provider end-to-end: `background_review_integration_test.go`) and curator governance (pin/manual protection, archive audit + restore: `skill_curator_test.go`) now have deterministic integration coverage. Background-review change claims ("skill created/updated/patched: <name>") are now verified against the toolchain (`skill_view` through the restricted backend) before notifying — a hallucinated claim with no tool call is detected, logged, and reported as no-change instead of being forwarded. |
| Skill metrics + pruning | ✅ | `internal/kernel/skill_store.go` RecordCall/Prune. (Roadmap lists this as "to do" — it is done.) Deterministic Prune coverage in `internal/kernel/skill_store_test.go`. |
| Learning audit | ✅ | Tenant JSONL log + per-change snapshots + undo. `internal/tools/learning_audit.go`. |
| Multi-agent delegation | ✅ | Parallel, semaphore-bounded batch delegation. `internal/app/multi_agent.go`. (Roadmap lists this as serial-only — it is parallel.) **Delegation depth bounded (2026-07-09):** previously `MakeDelegateFn` handed a sub-agent the SHARED parent backend (delegate_task included) on empty toolsets, and the `maxDepth` field was never read — a sub-agent could call `delegate_task` again forever (runaway token/recursion mine). Now a HARD STRUCTURAL bound: sub-agent backends are always freshly cloned (never the shared dispatcher, so the parent's wiring can't be mutated) with `delegate_task` stripped unless the depth budget allows another hop, in which case a fresh nested delegate wired to depth+1 is added; at `depth == MaxDepth` the sub-agent is a leaf with no delegation tool. Config `delegation.max_depth` (default 1 = flat), `max_concurrent` (5), `max_subtasks` (16, batch fan-out cap → clear error when exceeded). `internal/app/delegation.go`, `internal/app/multi_agent.go` (`SetSubBackendBuilder`), `internal/platform/config/loader.go`. Tests: `internal/app/delegation_test.go`. |
| Extended tools | ✅ | `web_search`, `web_extract`, `execute_code`, `delegate_task`, vision, tts beyond file/terminal. |
| MCP client | 🟡 | Real stdio/HTTP JSON-RPC client, multi-server, on-demand tool registration. `sampling/createMessage` not implemented. `internal/tools/mcp_client.go`. **Reader hardening (2026-07-09):** a non-numeric response id no longer panics the stdio reader (safe assertion, message dropped), and a reply arriving after the 30s waiter timeout no longer re-creates an undrained `respChans` entry (`lookupResponseChan` — only the request sender registers channels), closing the unbounded-map leak. |
| Eval loop | ✅ | Real gateway-path runs; P0 deterministic checks + state-predicate oracle (`assert_state`); VCR record/replay for free offline regression; `selfmind eval run/report/repair/scorecard/capture/clean`; day-in-the-life suite with recorded cassettes. **Data-isolated by default**: every run (record and replay) uses a throwaway temp data dir (`shared_data: true` opts out); post-case run-finalization sweep forces leftover `running` rows terminal; `selfmind eval clean [--yes]` removes historic eval residue from a real control.db. `internal/eval`, `evalcases/`. |
| Flight recorder + capture | ✅ | `SELFMIND_FLIGHT_RECORDER=1` records each real turn; `/capture` / `eval capture` promotes the last turn into a replayable eval case — everyday friction becomes a permanent regression test. `internal/kernel/llm/flight.go`, `internal/kernel/flight_recorder.go`, `internal/eval/capture.go`. |
| Telegram adapter | ✅ | Webhook + long poll, signature verify, send. |
| Personal/Enterprise WeChat (Weixin) adapter | ✅ | iLink protocol (`ilinkai.weixin.qq.com`): poll loop, AES, per-peer context_token, typing, media, group/DM policy, dedup. Built-in QR login (`selfmind weixin login`) — no external bridge needed. This is the primary multi-device WeChat path. **2026-07-09 reliability fixes:** (1) `GetUpdates` now checks the in-band `ret`/`errcode` of the HTTP-200 body — session expiry surfaces as `weixin.ErrSessionExpired` instead of reading as an empty success, and the poll loop logs one ERROR ("run `selfmind weixin login`") and drops to a 5-min slow poll rather than silently never receiving again; (2) inbound dedup is now durable — `isDuplicate` backs the in-memory 24h map with `control.Store.MarkInboundSeen` (48h retention, `inbound_dedup` table), so the sync-buffer replay after a daemon restart no longer re-runs the agent on already-processed messages. Tests: `weixin/client_test.go` (`TestGetUpdatesDetectsSessionExpiry`), `weixin/adapter_test.go` (`TestDuplicateDetectionSurvivesRestart`). |
| WeChat Official Account adapter | 🟡 | Inbound passive-reply + signature verify (`internal/gateway/wechat`); outbound now supported via the customer-service `custom/send` sender (`internal/gateway/delivery/wechat.go`, registered as platform `wechat`). Still no message encryption/decryption. |
| Approval lifecycle | 🟡 | DB + API + `/approve` / `/reject` + staged approval modes (`/mode`) done. Approval UX shipped (2026-07-04): all surfaces (control commands, `POST /v1/approvals/respond`, CLI, Telegram buttons) resolve references through one shared resolver (`httpapi/approval_resolver.go`) — list ordinal (`/approve 1`), unique `apr_` prefix, bare `/approve` with a single pending, `task_` ids rejected with a hint; `/approvals` shows tool + bounded args preview + reason + task title; CLI-originated approvals fan out to the person's other bound IM accounts (`notifyApprovalRequested` + `ListAccountsByPerson`); Telegram gets native inline approve/reject buttons (typed `delivery.Message.Kind`, persisted on the outbound row so retries keep buttons) with `callback_query` handled in both the telegram adapter and the generic `/v1/im/*` webhook; `selfmind approve/reject` returns one-line errors, never raw JSON; `selfmind send --mode` threads `approval_mode`. Remaining: the long-poll `internal/gateway/telegram` adapter is still not mounted by the daemon (generic webhook path is), and Weixin stays text-fallback by design. Outbound dispatch is claim-based (`ClaimDelivery`): the immediate attempt and the retry poller are mutually exclusive, fixing the live duplicate approval push. IM approvals are conversational and task-free (owner request 2026-07-04): the push is `Approval needed — reply y or n:` + the command/reason only (no task label, no apr_ id, no ordinal); a bare `y`/`n` (or 好/可以/不行) answers the single pending approval, degrading to a numbered `/approve <n>` list only when multiple runs have approvals pending in parallel. The task concept stays in the control plane, out of the IM UX. CLI-originated async results now fan out to bound IM endpoints (`deliverAsyncResult` → `fanOutToBoundIM`) so a fire-and-forget terminal run's final answer — including a rejection acknowledgment — is visible on WeChat/Telegram instead of vanishing. Watch items: (a) one live WeChat `/reject 1` got no reply, likely a message lost in a gateway-restart window (iLink getupdates canceled mid-poll); (b) two result pushes were `sent` (correct target, iLink API accepted) but never arrived on the phone ~4.7h after the user's last inbound message — suspect iLink proactive-push context_token staleness; verify the weixin sender checks the response errcode and consider marking undeliverable pushes failed for retry. **Pending-notification escrow (2026-07-05):** the initial push is one-shot at creation, so a CLI-attached approval whose CLI then quit used to sit pending invisibly. A `notified_at` column (`approval_requests` + `clarify_requests`, `ensureColumn` migration) is stamped only when a push actually SENDS (never when suppressed); the in-daemon 60s sweep (`run_recovery.go`) now re-pushes pending approvals/clarifies older than `gateway.pending_notify_after` (default 2m; `0` disables) whose person has since detached from the CLI and were never notified, to the single preferred IM — idempotent (marks after `EnqueueAndTry` succeeds, so a crash retries next sweep; boot sweep covers restarts). `Store.MarkApprovalNotified`/`MarkClarifyNotified`/`ListPendingApprovalsForEscrow`/`ListPendingClarifiesForEscrow`; `RunCoordinator.escrowApprovalNotification`/`escrowClarifyNotification`. Tests: `control/escrow_test.go`, `httpapi/escrow_test.go`. |
| Clarify lifecycle (G3) | ✅ | A mid-run agent question is a first-class DB-backed pending question modeled exactly on the approval waiter (2026-07-04). `gatewayClarify` (formerly a stub) creates a `clarify_requests` row (`internal/control/clarifies.go`: `Create`/`Get`/`List`/`Answer`/`Expire`/`ExpireOrphanedClarifies`), appends the `clarify.requested` event, pushes a presence-aware, single-preferred-endpoint notification through the shared `RunCoordinator.routePendingNotification` (same routing as approvals; `notifyClarifyRequested` + `delivery.KindClarify`, body `Question — reply with your answer:`), then blocks polling the row for up to 30 min. An answer recorded from ANY endpoint (`Store.AnswerClarifyRequest`) returns verbatim as the tool result; timeout/expiry returns a best-judgment fallback sentinel so the run never hangs. Inbound: a plain non-slash reply while a question is pending IS the answer (`tryHandleClarifyAnswer`, in `tryHandleControlCommand` above new-task/queue logic and below the bare y/n approval leg). Orphan hygiene rides `MarkInterruptedRuns` next to the approval sweep. Surfaced in `/status`, `/diag`, and the attach digest (`api.DigestClarify`). A question survives the CLI closing exactly like an approval (docs/identity-continuity.md "Runtime attachment model"). Tests: `control/clarifies_test.go`, `httpapi/clarify_inbound_test.go`. |
| CLI / TUI controller | 🟡 | Components partly extracted; `uiModel` in `controller.go` is still a monolith (violates AGENTS.md guidance). |
| TUI rendering (terminal-first hybrid) | ✅ | **Only renderer (2026-07-10):** history committed to native terminal scrollback (`tea.Println`), only the active region redrawn (`history_commit.go`); terminal owns scroll/select/copy. The legacy alt-screen viewport (`SELFMIND_TUI_LEGACY`, `viewport`, `controller_mouse.go`, the per-message render cache) was DELETED with the in-process path — there is no `hybridMode()` switch anymore. Colored patch diffs (`renderPatchCell`), `/history` (full diffs), `/copy`. Codex-style interactive approval panel (2026-07-05): `approval.requested` arms a bordered selector in the ACTIVE region (`ui/components.ApprovalPrompt`, wired by `gateway/cli/approval_flow.go`) — ↑/↓/j/k + Enter or shortcuts y/t/a/n mapping to grant scope ""/task/person on the existing `/v1/approvals/respond` path; Esc does nothing (explicit decision required); "No" opens a deny follow-up composer (Enter = bare deny, text = deny + mid-turn guidance); queued approvals re-arm FIFO; duplicate text notice + "Preparing to run" spinner suppressed while the panel is up; transcript keeps ONE compact `notice` line per request/decision; status bar shows `⏸ waiting approval`. IM/text approval surfaces unchanged. Remaining: write_file overwrite real diff; `/history` search + `control.db` backing. Live plan checklist now renders in **client mode** (2026-07-05): the daemon's `plan.updated` event carries the full structured plan, forwarded by `client.eventToStream` and rendered as an `update_plan` cell so `renderPlanCell` shows the `[x]/[>]/[ ]` steps instead of a stray "plan updated" line (`agent_events.go` `forwardGatewayEvent` + `planJSONFromEvent`); `maxPlanSteps` raised 20→50 so a normal plan is never truncated. Status bar always shows the effective approval mode (`statusLine` `mode:<effective>`), learned from `GET /v1/digest` `approval_mode` at startup and updated by `/mode`. See `docs/tui-terminal-first-hybrid.md`. |
| Run execution coordinator | 🟡 | `RunCoordinator` (`httpapi/run_coordinator.go`) owns the run lifecycle (`runMessage`/`startAsyncRun`), the active-run registry, and all pre/post-run helpers (workspace/task resolution, execution scope, approval handler, context assembly, stream aggregation, outcome persistence). Server is now the HTTP/orchestration layer. Worker pool shipped behind `SELFMIND_WORKERS` (see Multi-terminal concurrency row). Async-run task visibility fixed (2026-07-04): every run (sync and async) now syncs the person's `current_task` pointer to the task it resolved (`syncCurrentTask`, same `SetCurrentTask` mechanism as `/new`/`/resume`), and `/status` prefers the active run's task over the pointer (`Server.statusReply`); regression tests in `httpapi/task_visibility_test.go`. Stuck-run recovery shipped (2026-07-04): **invariant — after any finalization or recovery sweep, no task may remain `running` with zero live runs** (`running` means "a run is executing right now"; between-turns tasks park as `in_progress`). Enforced by: `Store.FinishRun` coercing non-terminal run statuses to `done`; `Store.MarkInterruptedRuns` flipping heartbeat-stale runs *and* repairing orphaned `running` tasks; a boot sweep (threshold 0 — the `gateway.lock` flock guarantees leftover running runs are dead) plus a 60s in-daemon sweep (12× the 10s run heartbeat) that always excludes the active-run registry (`httpapi/run_recovery.go`). Recovered `interrupted`/`in_progress` tasks stay non-terminal and resumable via `继续`/`/resume`. Tests: `control/runtime_test.go`, `httpapi/run_recovery_test.go`. **Post-run labeler (P3, 2026-07-06):** after finalization (under the `WithoutCancel` finalize ctx, AFTER the response is assembled), `labelFinishedRunAsync` (`httpapi/run_labeler.go`) asynchronously asks the cheap `Server.Labeler` (memory_extract role via `app.NewRunLabeler`; nil in eval) whether the turn's pre-label guess was right: KEEP no-ops, MOVE re-points the run + events/artifacts to an offered open label (`Store.ReassignRun`, transactional; deletes an auto-created placeholder left with zero runs, folds its handoffs, repoints `current_task`), TITLE names a NEW placeholder once. Every non-KEEP decision writes a `label.assigned` event; every failure degrades to KEEP; it never blocks the response (10s bound, `labelerWG` tracked). Only pre-label (guessed) attaches are labeled — explicit task_id/cue/pin attaches are the user's decision. Tests: `httpapi/run_labeler_test.go`, `control/task_labels_test.go`. **Async panic firewall (2026-07-07):** async runs (IM, queue drain, cron, detached CLI) have no net/http per-request recover, so an unrecovered panic in an agent turn used to crash the whole gateway daemon. Fixed on two layers: the router's agent-streaming goroutines (`router.runAgentStreaming` + the task-streaming variant) now `defer recoverStreamPanic`, converting a panic into a stream `Err` event the run finalizer already handles (task → interrupted); and `startAsyncRun`'s goroutine has a `recover` (`recoverAsyncRun`) that logs the stack, finalizes the run failed + task interrupted, delivers a failure notice, and (via the existing endActive + drainQueue defers) frees the person's slot so they are never wedged. Tests: `httpapi/run_panic_test.go`. **Parked-task wording (2026-07-06):** a task parked `in_progress`/`interrupted` with NO live run finished its turn — it is not busy. `/status` (`formatTaskStatus`) now renders `Status: in_progress (turn finished — reply to continue, or /new)` and `/tasks` (`task_view.go` cards, given the person's active task id) labels such tasks `[paused]` (the active one `[running]`), so the user stops reading a completed turn as "still working" (observed live: 13 min staring at `in_progress`). Stored status value and the state machine are UNCHANGED — user-facing wording only; a genuinely running task still shows elapsed. Tests: `httpapi/parked_status_test.go`. |
| Multi-terminal concurrency (daemon-client) | 🟡 | Decision: converge every terminal on ONE gateway daemon instead of cross-process locks. Foundation shipped: `gateway.EnsureRunning` (discover-or-autostart + health wait, race-safe via the `gateway.lock` flock); CLI client paths (`selfmind send/status/...`) auto-start a local daemon; `internal/gateway/client` daemon-backed `MessageProcessor` (sync `/v1/message` answer + best-effort event poll → ctx stream observer). Client mode is the ONLY TUI path (2026-07-10): the in-process gateway build and the `SELFMIND_TUI_INPROC` opt-out were deleted — a daemon that can't start fails with actionable guidance, never a local agent. Chat + agent-backed slash commands (`/skills`, `/memory` incl. `list`, `/bundles`, `/checkpoint`) run on the daemon via a safelisted `/v1/dispatch` (workspace-mutating/code-exec tools refused 403); `/status`/`/tasks` route via the message processor; `/skills stats`,`/model` switch show a client-mode notice. Worker pool (`internal/runpool` + `SELFMIND_WORKERS`, default 1) runs inside that daemon. `workspaceSerialKey` serializes **write** turns only (read turns concurrent, Exclusive/SharedRead semantics). Interactive tool approval works in client mode (Codex-style TUI approval panel driven by the `approval.requested` event → `/v1/approvals/respond`, incl. grant scope; see TUI rendering row). The message-based-channel working notice (`router.WorkingNotice`) is English-only (2026-07-05: "Got it — SelfMind is working on this…"; the stray bilingual TUI composer hint in `history_commit.go` was also de-duplicated to English). **Remaining**: soak at N>1; per-provider cap (adapter layer, deferred). (Session search over the daemon + in-process deletion shipped 2026-07-10, see ACTIVE PLAN P0-3.) See `docs/worker-pool-design.md` §8. |
| Process sandbox | 🟡 | Unix process-group isolation only; **not** a security sandbox (no namespace/seccomp/cgroup). Windows is a no-op. `execute_code` emits a one-per-process WARN noting it runs with full host access under the current user. Since 2026-07-07 `execute_code` is at least approval-gated (see Approval modes row), but the residual risk stands: approved code still has unrestricted host access — real isolation is P2 (Highest-Value Next Work item 9). |
| Feishu / Lark adapter | 🟡 | Inbound via the generic `/v1/im/feishu` webhook (verification-token / encrypt-key signature, challenge); outbound via `delivery.FeishuSender` (tenant_access_token + `im/v1/messages`, chat_id/open_id routing). Config drives both. Encrypt-envelope AES decryption still TODO (use plaintext mode). **Inbound redelivery dedup (2026-07-09):** the `/v1/im/*` webhook now acknowledges a duplicate delivery 200 without re-running the agent — keyed by the platform's own id (`imMessageID`: generic `message_id`/`event_id`, Feishu `header.event_id`/`event.message.message_id`, QQ `d.id`, Telegram `update_id`), persisted in `control.inbound_dedup` (48h retention) so it survives restarts; no-id payloads and dedup-store errors fail open. `handlers_channels.go`, `control/inbound_dedup.go`; tests `httpapi/handlers_channels_test.go`, `control/inbound_dedup_test.go`. |
| QQ official bot adapter | 🟡 | Inbound via `/v1/im/qq` webhook (group/C2C/guild events parsed into a `group:`/`c2c:`/`channel:` target); outbound via `delivery.QQSender` (app access token + per-target message API). Active push only — webhook ed25519 signature verify and passive `msg_id` threading are follow-ups. Inbound redelivery dedup by `d.id` shipped 2026-07-09 (see the Feishu row — shared `/v1/im/*` mechanism). |
| Production hardening batch (2026-07-09) | ✅ | Pre-production fixes from the 2026-07-08 full audit: (1) **config fail-fast** — `gateway run` now aborts with `load config: …` on a broken config instead of half-starting with an empty one (`runtime/gateway/runner.go`; LoadConfig still auto-creates the default template on first run); (2) **cron.db SQLite hygiene** — WAL + `busy_timeout=5000` + `MaxOpenConns(1)`, same as control.db (`app/gateway.go`); (3) **data privacy on shared hosts** — data dir 0700 and `control.db`/-wal/-shm chmod 0600 best-effort at open (`control.OpenStore` `tightenStorePerms`). |
| Pinned memory injection (profile synthesis removed) | ✅ | **2026-07-11:** the legacy `ProfileSynthesizer` was DELETED as dead code — it had no callers, so pinned facts never reached the model and a stale profile row could be injected forever. `buildSystemPrompt` now reads the `pinned` target directly and injects user-confirmed facts FIRST, unconditionally, outside the bounded `SelectFacts` slots (they never compete with extracted facts and never decay out). Do not reintroduce a profile-synthesis model call; the "knows you" signal is pinned + high-score facts. Tests: `internal/kernel/pinned_injection_test.go`. |
| Scheduled tasks (cron) | ✅ | SQLite-backed scheduler with timezone; jobs run a real agent turn and deliver the result to their channel (e.g. daily summary → WeChat); `web` opt-in per job; built-in liveness canary; idempotent built-in jobs. `internal/kernel/task/cron`, `internal/gateway/httpapi/cron_executor.go`. |
| Self-check & CI gate | ✅ | `selfmind selfcheck` (build + test + offline eval) and `.github/workflows/ci.yml`; strict offline VCR replay (`ErrCassetteMiss`) so the gate never burns provider quota. `require_cassette: true` cases fail (not skip) when their cassette is missing; `SELFMIND_EVAL_MIN_CASES` (set to 3 in CI) fails the gate when fewer cases actually replay. `internal/cliapp/selfcheck_commands.go`. |
| Continuity eval coverage | ✅ | Cases + gate + cassettes recorded and committed; selfcheck replays 7 cases offline. `evalcases/continuity/` (cross-endpoint `/status`, `继续` resume, stranger identity isolation via per-turn `platform_user_id`), all `require_cassette: true`; record with `SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity` and commit `.vcr/continuity_*`. `continuity-task-attach.yaml` rewritten for P3 pre-label semantics (2026-07-06): an ordinary follow-up now runs under the open current label by design, so the case no longer asserts `require_task_switch`; its old cassette predates the rewrite — re-record before it gates. |
| CLI image input | ✅ | Image-path detection in input + clipboard screenshot paste (`/paste-image`, Ctrl+V auto-detect; WSL/macOS/Linux); routed to the `vision_analyze` tool via the attachment pipeline. Clipboard requires a local GUI (not over SSH). `internal/gateway/cli/attachments.go`, `clipboard.go`. |
| Approval modes | ✅ | Staged `on-request` / `read-only` / `auto-edit` / `full-auto` / `smart` via `/mode`, enforced in `SmartApprovalMiddleware`; on-demand y/N reuses the clarify bridge. `internal/tools/middleware.go`. **Layered approval funnel shipped (H1, 2026-07-04):** the middleware now runs (1) an unbypassable **hard floor** (`hardlineToolCall`) that fires before any mode bypass — full-auto included — for irreversible ops (recursive delete of `/`,`/home`,`$HOME`,`/etc`,`/usr`,`/var`,`/boot`; `mkfs*`; `dd of=/dev/sd*|nvme*`; redirect over a raw disk device; fork bomb; `shutdown`/`reboot`/`halt`/`poweroff`/`init 0|6`), returning `operation blocked by safety policy: …` (distinct from the user-rejection contract; kernel `isUserRejectionErr` does NOT match it); (2) mode bypass; (3) **class-level approval memory** — approving a coarse *class* (`approvalPatternKey`, e.g. any `chmod` → `exec:invokes dangerous command: chmod`) for the task (session) or person (persistent) via the durable `approval_grants` table skips later same-class asks; (4, smart mode) **LLM triage**; (5) human ask. Reply grammar: `/approve [n] task` / `/approve [n] always|person` (or bare `yt`/`ya`) records the grant scope; `/mode` is now an IM control command persisting per-person `approval_mode`. **Smart-mode LLM triage shipped (H2, 2026-07-04):** in `smart` mode a dangerous (non-hardline) op with no matching grant is triaged by a cheap role model (`background_review`, off the main run provider) before the human ask — APPROVE auto-runs AND records a task-scope class grant (judge consulted at most once per class per task), DENY blocks with the user-rejection contract (`operation rejected: blocked by safety triage`, do-not-retry), ESCALATE / no judge / any error / 15s timeout falls through to the human ask (fails SAFE, never open). The judge (`ApprovalJudge` interface in `internal/tools/approval_triage.go`, injected via `ExecutionScope.Judge`) strips shell comments and wraps the command in `<command>` delimiters with a prompt-injection defense. The hard floor stays ABOVE triage (hardline ops never reach the judge). `internal/tools/middleware.go`, `internal/tools/approval_triage.go`, `internal/app/approval_judge.go`, `internal/control/approval_grants.go`, `internal/gateway/httpapi/approval_resolver.go`. **Live mode lookup (2026-07-05):** the mode is resolved at EACH ask, not frozen at run start — `ExecutionScope.ModeGetter` (installed by `installExecutionScope`) re-resolves with run-start precedence (explicit request mode wins, else the CURRENT persisted `person_settings.approval_mode`, else on-request), so `/mode smart` sent from IM mid-run governs the in-flight run's later approval decisions (triage included). The TUI no longer force-sends its local default mode: requests omit `approval_mode` unless the user ran TUI `/mode` this session, so the persisted preference governs (`selfmind send --mode` stays explicit). Tests: `tools/approval_mode_live_test.go`, `httpapi/mode_command_test.go` (`TestApprovalModeLiveLookupMidRun`), `cli/approval_mode_omission_test.go`. **`/mode` retro-resolves ALREADY-pending approvals (2026-07-06):** live-mode lookup only governed the NEXT ask, so an approval asked BEFORE the switch stayed blocked forever (observed live: a `read_file` approval sat pending for minutes after `/mode smart`). `approvalModeReply` now re-checks the person's currently-pending approvals under the new mode via the shared classifier `tools.EvaluateModeDecision` (hard floor authoritative — a hardline pending op is NEVER auto-approved by any mode; smart consults the same `ApprovalJudge`, fails safe to still-pending on ESCALATE/no-judge/error; full-auto/auto-edit auto-approve their non-dangerous classes; on-request/read-only leave everything pending). Settled rows flip status so the blocked waiter (`server.go` ~812, 1s poll) wakes without a new wakeup channel; the reply reports the count (`Re-checked 1 pending approval: 1 auto-approved.`). `retroResolvePendingApprovals`/`approvalReasonIsDangerous` in `httpapi/server.go`, `tools.EvaluateModeDecision`/`ModeDecision` in `tools/middleware.go`. Tests: `httpapi/mode_retro_test.go`. **Security hardening (2026-07-07):** (1) arbitrary-code exec (`terminal`/`shell`/`execute_command`/`execute_code`) now ALWAYS requires approval in on-request AND smart modes (`approvalNeeded` returns true for `isExecTool`), not only when the dangerous heuristic fires — closing the hole where `execute_code` (payload in `args["code"]`, not `args["command"]`) ran with NO approval by default; (2) both the hard floor (`hardlineToolCall`) and the dangerous heuristic (`dangerousToolCall`) now read the exec payload via `execCommandPayload` (command/code/script) and inspect **wrapper-unwrapped** segments (`expandCommandSegments`): a shell `-c` script, `sudo`/`doas`/`env`/`xargs`/`nohup`/`timeout`/`nice`/`ionice`/`setsid`/`stdbuf`/`command` prefixes (bounded recursion depth 3) are unwrapped to their real inner program, so `bash -c "rm -rf /"`, `sudo rm -rf /`, `env rm -rf $HOME` are caught; an unparseable wrapped payload degrades to dangerous (approval) but NOT hardline (the floor only denies what it can positively identify). Tests: `tools/middleware_wrapper_test.go`, `tools/approval_mode_test.go`. **Network egress classifier (2026-07-09):** `dangerousToolCall` now flags network-egress commands (`egressCommand`: curl/wget/nc/ncat/netcat/socat/telnet/ftp/tftp/scp/sftp/rsync/ssh by wrapper-unwrapped basename, plus `/dev/tcp/`·`/dev/udp/` redirects) as a first-class safety class — the exfiltration half of the IM-injection threat (untrusted message → terminal data-out). It is its own named function (testable/tightenable independently), reuses the already-unwrapped segments so `sudo curl` / `bash -c "curl…"` can't hide, and excludes `git` (push/pull to configured remotes = pure fatigue). Egress is a **dangerous** class, so on-request (default) and smart modes ask/triage it; **full-auto still bypasses it by the existing documented contract** (owner decision 2026-07-09 — keep egress in on-request/smart only, not a new unbypassable tier). Tests: `tools/middleware_egress_test.go`. |
| Mid-turn steering | ✅ | Works in-process (`internal/kernel/steering.go` + controller `steerCh`) **and** in client mode: input typed during a daemon run is forwarded via `POST /v1/runs/steer` (`httpapi/handlers_steer.go`) into the active run's buffered steering channel (`kernel.WithSteering`, sync + async paths), leaving an auditable `run.steered` event. Daemon refusals surface honestly in the TUI (409 no active run / 429 buffer full → transcript notice, never silently dropped). **Cross-endpoint steering closed (2026-07-09):** the thin-client path only covered the TUI; a continuation arriving on the ordinary `/v1/message` entry (IM/web) while a run was active used to bounce off a bare "busy" reply, so WeChat could not steer a running task. Both entries now share one core (`Server.steerActiveRun` in `handlers_steer.go`): an `IntentContinue` message on `/v1/message` injects into the SAME active-run steering channel (non-blocking; a full buffer or absent channel falls back to the honest busy reply, never a silent drop) and returns an `accepted` turn (`formatSteeredIntoRun`) plus a `run.steered` event — the cross-endpoint takeover the north star requires. Continuations still never queue. Covered by unit tests (`httpapi/steer_active_run_test.go`, `httpapi/handlers_steer_test.go`, `httpapi/queue_test.go`), not VCR. |
| Task queueing (G1+G2) | ✅ | New work while a run is active is **queued, not rejected** (2026-07-04). Durable `control.task_queue` (`internal/control/queue.go`: `EnqueueQueued`/`ListQueued`/`NextQueued`/`CountQueued`/`MarkQueued`/`ClearQueued`/`ListAllQueued`/`RequeueStartedQueued`). `ProcessMessage` enqueues a genuinely-new message behind the active run with an honest acceptance (`Queued behind the running task (N ahead)…`); a continuation (`IntentContinue`) is never queued — it **steers the active run** on every entry (2026-07-09; see Mid-turn steering row), falling back to the busy reply only when the steering channel is full/absent. On every run finalization (sync + async paths) `RunCoordinator.drainQueue` auto-starts the next queued item as a normal async run (per-person `draining` re-entrancy guard; reverts the row to `queued` if a fresh run races the slot). Boot drain (`Server.DrainQueuedAtBoot`, wired in the gateway runner) requeues `started` rows and resumes pending work after a restart. Visibility: `/queue` (list) / `/queue clear`; `/stop` cancels the active run and then drains. `httpapi/queue.go`, `httpapi/run_coordinator.go`. Tests: `control/queue_test.go`, `httpapi/queue_test.go`. **Queue done-state (2026-07-07):** a drained row's async run finalization now marks the row `QueueStatusDone` (the coordinator threads the queue id onto the drained `MessageRequest.QueueID`; `runMessage` marks done in a deferred terminal-path write). Previously the row stayed `started` with no back-reference, so boot recovery (`RequeueStartedQueued`) re-ran already-completed work at every restart (only masked by `maxQueueRestarts=1`). `RequeueStartedQueued` still touches only `started` rows, so `done` rows are never resurrected. Test: `httpapi/queue_test.go` (`TestDrainedItemMarkedDoneNotRequeued`). |
| Task-attach semantics | ✅ | **Pre-label guess (Work Timeline P3, 2026-07-06 — supersedes the 2026-07-05 explicit-evidence-only rule).** Explicit evidence stays deterministic: caller `task_id`, `IntentContinue` (router cue / short acceptance), or the one-shot `/resume` pin (`person_settings.resume_pin_task`, consumed by the next agent-bound message; the pin alone may reopen an ARCHIVED label). Every other agent-bound message — sync, async, queued-drain, cron — pre-labels onto the person's current OPEN (non-terminal, non-archived) label, else a fresh placeholder. Harmless by construction: labels never gate context (spine P1 + recall P2), the EXECUTION workspace follows the REQUEST for pre-label turns (`workspaceForTask` — a guessed label's binding is never stamped or inherited), and the post-run labeler re-points wrong guesses (see the Run execution coordinator row). `resolveTask` returns a `taskAttach{created,preLabel}` provenance flag (`httpapi/server.go`, `httpapi/continue_resolver.go`). Resume carries real work state (2026-07-05): a resumed run keeps the **task's own workspace** even when the request carries a different client-cwd workspace (`workspaceForTask` prefers `task.WorkspaceID`, fixing a cross-endpoint `继续` that ran in the terminal's dir and tripped out-of-root approvals), and `withResumeContext` now injects a bounded (≤10) `files_this_task_created_or_changed` list merged from the handoff and the task's file-mutating tool events (`resumeChangedFiles`) — so an interrupted run (no handoff) still tells the continuation which file to edit instead of rediscovering and overwriting the wrong one. Harvest hardened 2026-07-05 to cover `write_file`/`edit`/`edit_file` (path via `path`/`file_path`/`output_path`) and `patch`/`apply_patch` (V4A `Update`/`Add`/`Delete`/`Move File` headers), never `read_file`. Tests: `httpapi/task_attach_test.go`, `httpapi/server_test.go` (`TestResumeContextIncludesCreatedFilesFromEvents`, `TestResumeChangedFilesHarvestsPatchAndEditPaths`, `TestWorkspacePreservedOnResume`); eval: `evalcases/continuity/continuity-task-attach.yaml` (cassette pending). |
| Observability / diagnostics | ✅ | Self-serve diagnostics so the owner never re-describes bugs by hand (2026-07-04). `selfmind doctor [--out FILE]` (`internal/cliapp/doctor_commands.go`): a redacted bundle — gateway status (live HTTP status, else on-disk PID record), last 10 runs (status/title/elapsed/last_error), pending approvals, queued tasks, `sent_unconfirmed`/`failed` pushes, presence snapshot (durable `accounts.last_seen_at`), per-channel activity, and the last 50 gateway.log lines. Read-only; works whether or not the daemon is up (reads control.db + log directly). `/diag` control command returns a compact phone-friendly snapshot (active run + elapsed, queued count, pending approvals, last error, recent events). Content redacted via `tools.RedactSensitive`. Store queries: `control.ListRecentRunsForPerson`, `control.CountChannelMessagesByChannel` (`internal/control/doctor_queries.go`). Tests: `cliapp/doctor_test.go`, `httpapi/queue_test.go` (`/diag`). |
| Task governance | ✅ | Post-run, reversible label hygiene (2026-07-10): additive task metadata (`kind`, `visibility`, `pinned`, `archived_at`, `last_activity_at`) migrates every old row to visible normal work without deleting or renaming it. The single cheap-model `PostRunAnalyzer` supports `INBOX` and durable fact extraction in the same bounded call; casual/identity/diagnostic runs may move to one hidden archived Inbox per person/workspace, while runs/events stay durable and Inbox is excluded from `/tasks`, recall cards, current-task selection, and continuation. `/task <id> pin|unpin` is explicit user authority. A configurable 6h sweep archives only stale visible terminal work (`tasks.auto_archive_*`), excluding pinned, active, open/interrupted, and pending approval/question tasks; archive is reversible via `/resume` and history is never deleted. Default `/tasks` is SQLite-filtered and paged by view/workspace/keyword. Tests: `control/task_governance_test.go`, `app/post_run_analyzer_test.go`, `httpapi/run_labeler_test.go`, `httpapi/task_view_test.go`; eval: `evalcases/timeline/timeline-task-governance.yaml`. |
| Skill variant evolution / sandbox test | ❌ | Old roadmap P3 (doc removed; see git history); not started, and out of scope for the north star. |

### Memory Governance Closeout (2026-07-12)

- Existing canonical references support `/memory pin <ref>` / `unpin <ref>`
  without changing evidence or scope. `/diag memory` reports status,
  protection, scope, visible-topic, consolidation-candidate counts, and the
  effective governance mode.
- Prompt access accounting touches only canonical rows actually selected for
  injection; scanning a candidate no longer refreshes archival freshness.
  Native tool dispatch carries `_workspace_id`, keeping project/environment
  facts workspace-scoped while user preferences remain global.
- Next: calibrate real-history shadow precision, enable high-confidence
  `merge-only`, finish the canonical cutover, then tune caps/archival and
  FTS+CJK memory search.

## Highest-Value Next Work (by priority)

These are the live gaps, ordered by their distance from the north star
(`docs/identity-continuity.md` — the three continuity scenarios). This section
is the only priority list in the repo; other docs must point here.

### ACTIVE PLAN — Daemon-only + north-star experience (approved 2026-07-10)

Owner-approved consolidation (2026-07-10, multi-agent cross-review calibrated).
Core goal: CLI / IM / cron / HTTP API are ALL just entrances; everything enters
ONE daemon; the daemon is the only agent runtime. Supersedes item 7 below and
absorbs the weixin-push remainder of item 1(0). Order within a phase is the
execution order.

**P0 — north-star closure**

- **P0-1 IM delivery loop completion — ✅ shipped and owner-verified on a real
  WeChat device 2026-07-10.** iLink ret/errcode checking and
  `sent_unconfirmed` (terminal, no blind retry — duplicate risk) were already
  shipped. New: (a) **catch-up re-push** — any IM inbound (the weixin adapter
  saves the fresh context_token before dispatch) fires
  `delivery.Service.CatchUpUnconfirmed` (goroutine off `ProcessMessage`,
  30s-bounded ctx) which re-delivers that person+platform+channel's
  `sent_unconfirmed` rows behind THREE anti-duplicate rails: at-most-once per
  row (`outbound_messages.catchup_at` claim column, claim-before-send via
  `Store.ClaimDeliveryCatchUp`), a freshness window
  (`gateway.delivery_catchup_max_age`, default 4h), and a per-catch-up cap
  (default 3) oldest-first; a re-push that is STILL unconfirmed keeps its
  consumed claim — never a duplicate drip. (b) `/diag` shows `Outbound (24h):
  sent N, unconfirmed N, failed N` + the newest undelivered reason
  (`Store.CountOutboundByStatusSince`). (c) `selfmind doctor` explains the
  stale-context_token failure mode and the reply-to-recover path. Tests:
  `control/catchup_test.go`, `delivery/catchup_test.go`. The owner verified
  text, async completion, file delivery, approval round-trip, reminder, and
  diagnostics on a real phone; one observed iLink `ret=-3` remains visible in
  `/diag` as a failed push rather than a false success.
- **P0-2 cassette gate integrity — ✅ shipped and re-recorded 2026-07-10.**
  Root cause fixed: the VCR per-session counter was
  process-global and never reset, so a case re-run in one process continued
  numbering (recording 0001+ holes — the observed `.vcr/continuity_task_attach/`
  corruption — and breaking same-process replays). Now `llm.ResetVCRSession`
  runs at the start of EVERY case execution and record mode wipes the case's
  previous cassette generation first (`llm.WipeVCRSessionRecordings`,
  runner wiring in `internal/eval/runner.go`). `HasCassetteSession` is strict:
  0000.json required AND gap-free 0000..max ("any *.json" would mask exactly
  this corruption). The owner re-recorded a gap-free
  `.vcr/continuity_task_attach/0000..0005.json` generation and
  `continuity-task-attach.yaml` now requires the cassette. Tests:
  `llm/vcr_test.go`; acceptance: strict offline `selfmind selfcheck`.
  Acceptance: `selfmind selfcheck` passes strictly offline.
- **P0-3 session-search-over-daemon, THEN delete in-process — parity ✅ shipped
  2026-07-10; deletion still pending.** Landed: (1) **dispatch partition fix**
  — daemon runs store memory/session-FTS/checkpoints under the PERSON id
  (`RunAgentWithEvents(ctx, identity.PersonID, …)`), but `/v1/dispatch` forced
  the CONTROL tenant on every tool, so client `/memory list` read an empty (or
  stale in-process-era) partition. Now person-partitioned tools (`memory`,
  `session_search`, `checkpoint`) dispatch with `identity.PersonID`; skill
  tools keep the control tenant (the daemon skills-dir key)
  (`handlers_dispatch.go` `personPartitionTools`). (2) **structured
  `GET /v1/sessions`** — search (`?q=`), recent, and message window
  (`?session_id=&around=&window=`), always person-partitioned, backed by
  `Server.Sessions` (`SessionsBackend`, wired to the memory manager in the
  runner); nil-safe 503 (`handlers_sessions.go`). (3) **TUI `/search`** —
  works in BOTH modes via the `m.dispatch` seam (client → daemon dispatch with
  the fixed partition; in-process → local dispatcher), closing the named
  parity gap; the dead in-process-only `sessionSearchFn` wiring was removed.
  Tests: `httpapi/handlers_sessions_test.go`. **Deletion step ✅ done
  2026-07-10:** `SELFMIND_TUI_INPROC` and the in-process fallback are GONE —
  `runTUI` always runs the thin client (`cliapp/root.go`); a daemon that can't
  be reached/started FAILS with actionable guidance (gateway status / `gateway
  run` / doctor), never a local agent. `EnsureRunning` timeout error now names
  the address, wait, cause, and where to look (stale gateway.lock / port /
  config). The legacy alt-screen TUI (`SELFMIND_TUI_LEGACY`, viewport,
  `controller_mouse.go`, render cache) was removed with it — hybrid is the only
  renderer. Scope note: "eval covers the daemon path" means eval keeps
  exercising the full gateway `Server` code path (in-process harness,
  data-isolated) — it does NOT spawn daemon OS processes.

- **P0-4 execution continuity + post-run/task governance — ✅ shipped
  2026-07-10.** Thin CLI clients subscribe to daemon-owned
  `GET /v1/runs/stream` SSE and render assistant deltas while the final message
  response remains the source of truth; IM/cron keep stage/final delivery only.
  The agent recovers opening and partial-stream EOF/reset/idle failures through
  bounded non-stream continuation, compacts again on context-window rejection,
  and never executes an incomplete native tool call. One app-layer
  `PostRunAnalyzer` call (explicit `tasks.maintenance_model_role`, default
  `memory_extract`) combines KEEP/MOVE/TITLE/INBOX with durable user/workspace
  facts; the old per-turn/final/profile calls are no longer wired. Task listing
  is SQLite-filtered and paged by view/status/workspace/keyword; failed first-
  run placeholders are deleted only when they have no durable history. Tests:
  `client/client_test.go`, `httpapi/live_stream_test.go`,
  `kernel/agent_retry_test.go`, `app/post_run_analyzer_test.go`,
  `control/task_governance_test.go`, `httpapi/run_labeler_test.go`.

P0 acceptance: CLI/IM/cron all execute in the daemon; no `SELFMIND_TUI_INPROC`;
no automatic local-agent fallback; WeChat reliably receives progress/approval/
completion/failure; continuity eval replays offline.

**P1 — long-conversation quality**

- **P1-1 native-tools prompt dedup — ✅ shipped 2026-07-10.** Tool definitions
  were DOUBLE-SENT (native `ChatRequest.Tools` AND the full name list +
  descriptions + param schemas in the system prompt). Now `buildSystemPrompt`
  keeps the behavior contract always, but emits the name-list/descriptions/
  schemas only for providers WITHOUT native tool support, probed via
  `llm.ProviderSupportsNativeTools` (the `NativeToolsCapable` interface,
  forwarded through the role router and VCR wrappers to the provider actually
  routed this turn). Native providers get a one-line note instead.
  `kernel/context…`, `llm/provider.go`, adapters. Tests:
  `kernel/tools_prompt_dedup_test.go`.
- **P1-2 context usage breakdown — ✅ shipped 2026-07-10.** Each turn emits a
  `context.breakdown` run event (`kernel.ComputeContextBreakdown`:
  identity/tools/project_context/memory/runtime/history token shares by section
  marker); `/diag` reads the newest one back and renders a one-line share view
  (the TUI shows it via `/diag`). Tests: `kernel/context_breakdown_test.go`.
- **P1-3 stable/volatile prompt split + prompt-cache prefix — ✅ shipped
  2026-07-10.** `buildSystemPrompt` groups content into a STABLE prefix (soul,
  guidance, tool contract+defs, skills) and a VOLATILE suffix (runtime context,
  memory/profile, per-turn conditionals like frontend guidance), joined
  stable-then-volatile so the cacheable prefix is maximized. The pre-fix bug
  injected volatile runtime context BETWEEN soul and tools. No AGENTS.md
  diffing (rejected — upkeep outweighs the marginal cache win). Tests:
  `kernel/prompt_layering_test.go`.

**P2 — engineering governance**

- **P2-1 split server.go / controller.go — ✅ shipped 2026-07-10** (no behavior
  change; same-package function relocation, compiler+tests as the safety net).
  `server.go` 1928→751: `*RunCoordinator` methods → `run_coordinator_lifecycle.go`,
  control-command cluster → `control_commands.go`. `controller.go` 1691→720:
  message loop → `controller_update.go`, view/render → `controller_view.go`,
  transcript mutators → `controller_transcript.go`, constructors →
  `controller_init.go`. All under the 800-line architecture-constraints line.
  Remaining follow-up (STATUS item 11, separable/low-risk): physically delete
  the unmounted dead adapters (`gateway/telegram`, `gateway/wechat`,
  `platform/wechat`, `GatewayDeps.Bridge`) — not done here to avoid a
  cross-package excision under the same change.
- **P2-2 docs — ✅ shipped 2026-07-10.** AGENTS.md carries the daemon-only
  development invariant (no in-process agent path / no fallback / no
  `SELFMIND_TUI_INPROC`); STATUS rows and `docs/tui-terminal-first-hybrid.md`
  updated for the legacy-TUI removal.

One-liner: make cross-endpoint takeover actually reliable → make the eval gate
trustworthy → converge to daemon-only; only then token economics and code
hygiene.

0. **P0 — Work Timeline transition (approved 2026-07-06; canonical design:
   `docs/work-timeline.md` — mandatory reading, includes the full rationale
   and the rejected alternatives).** Context ownership moves from tasks to a
   person-level work spine; `task` keeps its name but demotes to a work
   label/view; disambiguation happens in the agent's turn, never at ingress.
   Control plane (runs/approvals/queue/clarify/recovery) is untouched.
   Packages, one worktree each, owner verifies between packages:
   - **P1 spine: ✅ landed 2026-07-06.** Person-keyed turn-level history
     (slim entries under `kernel.SpineTrajectoryKey`; tool intermediates
     excluded), verbatim summary boundary note on every rendered compaction
     summary, ContextComposer slice formalization
     (`internal/kernel/context_composer.go`, slice ④ reserved for P2 via
     `RuntimeContextBundle.Recall`), and the read-only legacy compat chain
     (`task:<id>` → `PriorChannel` → old channel key). See the Context
     engine row above for the full contract.
   - **P2 recall v1** — ✅ shipped (2026-07-06): automatic recall slice at the
     selector layer (`httpapi/recall.go` `RecallEngine` on `Server.Recall`,
     rendered via `kernel.TaskRuntimeContext.RecallSlices`; ephemeral —
     system-prompt block only, never persisted history). Sources:
     person-partitioned session FTS + task label cards (live read-only
     `control.ListTaskCards` JOIN; artifacts/changed files ride the card's
     work line). Budget ≤3 slices / ~400-char excerpts / work-line dedupe
     (label card beats raw session fragment); control-command and
     short-message skips; `semantic_recall`-role query expansion only when
     the role is explicitly configured (`app.SemanticRecallExpander`, never
     the main model), 3s-bounded, degrades to raw terms; redacted
     `context.recall` task event. Embedding v2 interface reserved
     (`httpapi.RecallSource`). Wired identically in daemon, in-process TUI,
     and eval servers. Tests: `httpapi/recall_test.go`,
     `control/task_cards_test.go`. Details: `docs/work-timeline.md`
     "Semantic recall".
   - **P3 labels & view** — ✅ shipped (2026-07-06): `resolveTask` → harmless
     pre-label guess (explicit task_id / cue / `/resume` pin deterministic; else
     current OPEN label or a new placeholder; request workspace wins for
     guessed turns); async post-run labeler on the memory_extract role
     (`Server.Labeler`, KEEP/MOVE/TITLE one-line contract, fails safe to
     KEEP, `label.assigned` audit events, `Store.ReassignRun` transactional
     re-point + placeholder cleanup); `/tasks` aggregated open-work view with
     run counts + next-step hints (done/archived collapse to counts,
     `/tasks done|archived|all` expand); 2026-07-06: the view renders
     multi-line CARDS (simplified `[running|waiting|paused|<terminal>]`
     bracket, `last:` latest run input + age/interrupted, `file:` primary
     artifact basename, `approvals:`/`questions:` pending counts, `runs:`,
     shortened `id:`; per-card hints replaced by one trailing hint line;
     batched via `LatestRunSummaries`/`LatestHandoffFilesByPerson`/
     `PendingCountsByTask` grouped queries); `/task <id>` detail /
     `runs|rename <name>|archive` subcommands (short `task_xxxxxxxx` ids
     accepted; `archived` is a new terminal status excluded from open lists,
     `ListTaskCards`, `resolveContinueTask`, and the pre-label default —
     explicit `/resume` reopens it); implicit-continuation LLM upgrade
     REMOVED (`intent.continue_window` deprecated no-op). Tests:
     `httpapi/run_labeler_test.go`, `httpapi/task_attach_test.go`,
     `httpapi/task_view_test.go`, `control/task_labels_test.go`.
     2026-07-06 UX fixes: ordinal references everywhere the lists are numbered
     (`/task <n>`, `/resume <n>` against the /tasks open-card order via
     `resolveTaskReference`; `/workspace <n>` against the /workspaces order via
     `resolveWorkspaceReference` — display order = resolution order, approval
     resolver contract); `/resume` accepts the card-displayed short id and its
     success reply names the task's bound workspace; the workspace-escape tool
     error appends a `/resume`//`/workspace` hint; the TUI echoes typed slash
     commands into the transcript (`handleCommand`).
     2026-07-07 TUI session fixes: a successful `/workspace <n|id>` pins a
     session workspace override in the TUI (resolved id/name/path parsed from
     the gateway's control reply; `MessageRequest.WorkspaceID` rides every
     later agent/control turn, so the switch is no longer a no-op for
     subsequent CLI messages), the status bar renders the override
     `<name>:<path>` instead of the launch cwd, and client mode forwards
     `token.updated` usage events (`client.eventToStream` → `MsgTokens`) so
     run token counts tick live instead of sitting at 0 until the final
     response. Tests: `cli/workspace_override_test.go`,
     `cli/token_events_test.go`, `client/client_test.go`.
   - **P4 eval: ✅ shipped 2026-07-06.** `evalcases/timeline/` — five cases
     (iterate/new-topic/ambiguity/cross-endpoint/tasks-view) with committed
     cassettes replaying offline; scenarios 7-10 map to Go tests (see the
     suite README's scenario table). The Work Timeline transition P0-P4 is
     complete.
1. **P0 — G0: Runtime attachment model** (design:
   `docs/identity-continuity.md` "Runtime attachment model"; owner decisions
   2026-07-04, incl. origin-affinity routing). Sub-items in order:
   (0) **Weixin push reliability** — ✅ mechanism landed (2026-07-04): the
   sender DOES check iLink ret/errcode, so the observed loss is
   accepted-but-dropped on a stale context_token. Context tokens now carry
   capture timestamps (legacy files restore as age-unknown = stale);
   `delivery.SenderWithReceipt` lets the weixin adapter report push
   confidence from token freshness (30m conservative window, true window
   undocumented); doubtful sends finalize as `sent_unconfirmed` — terminal
   for the retry queue (resending on the same stale session risks
   duplicates) — with a WARN log. `sent_unconfirmed`/`failed` rows now
   surface in the attach digest (G0-c, 2026-07-04). Remaining: catch-up on
   the peer's next inbound + delivery health in `/diag` — **absorbed into the
   ACTIVE PLAN above (P0-1, 2026-07-10)**; calibrate the window empirically.
   (a) Detached run execution — ✅ resolved (2026-07-04). Runs execute on
   daemon-owned contexts: `ProcessMessage` derives the run ctx with
   `context.WithoutCancel` from the request ctx (values — stream observer,
   steering, scopes — preserved; caller deadlines re-applied as a bound on
   the run), so closing the CLI/dropping the connection mid-turn detaches a
   watcher instead of killing the run. Cancellation is owned by the
   active-run registry: `/stop`, drain `stopAllActive`, and TUI ctrl+c
   (now routed through `/stop` via `requestDaemonStop`); the idle watchdog
   and per-turn deadline still cancel. A sync run whose client vanished
   routes its result like an async one (`deliverAsyncResult` → IM fan-out
   for cli origin); connected clients get the sync answer only. Tests:
   `httpapi/detached_run_test.go`, `cli/cancel_stop_test.go`.
   (b) Two-layer routing — ✅ shipped (2026-07-04): in-memory presence
   registry (`httpapi/presence.go`, 90s TTL; touched by `/v1/message`,
   `/v1/tasks/events`, the new `GET /v1/presence/ping`, and a 30s idle ping
   loop in the client TUI; `accounts.last_seen_at` persisted throttled for
   preferred-endpoint recency). **Presence = recent user INPUT (2026-07-05):**
   an open-but-vacated TUI no longer claims attachment forever. The client
   tracks the last keystroke (`cli.SetInputActivityHook` →
   `client.InputTracker`) and stamps `active=0|1` on its presence ping and
   event polls once input age exceeds `gateway.presence_idle_timeout`
   (duration, default `5m`, `0` = old always-attached behavior; the decision
   is client-side because only the client knows input age — the daemon just
   skips `touchPresence` for `active=0`, `presenceClaimed`). `/v1/message`
   always counts as active (typing a message IS input). Effect: walk away →
   after timeout + TTL the CLI reads detached and CLI-origin approval/result
   pushes route to the preferred IM; return and type → the next beat
   re-attaches. Tests: `httpapi/presence_test.go`
   (`TestPresenceBeatWithActiveZero*`, `TestPresenceIdleBeats*`),
   `client/presence_active_test.go`, `config/presence_idle_test.go`. CLI-origin approval/result pushes: skipped
   entirely while a CLI endpoint is attached (no double notification), else
   delivered to ONE preferred IM endpoint — explicit `/notify <platform|auto>`
   preference (validated against the person's own bound accounts, stored in
   `person_settings`) or the most recently seen IM account. IM-origin
   replies unchanged (origin affinity). Follow-up: reply-endpoint override
   parsed from task text remains open.
   (c) Attach digest — ✅ shipped (2026-07-04). `GET /v1/digest`
   (`httpapi/handlers_digest.go`): person-scoped, anchored on the CLI
   account's `accounts.last_seen_at` (24h fallback when never seen); bounded
   sections — finished tasks (≤10), failed/interrupted tasks (≤10), all
   pending approvals (shared `approvalSummaryLine`), `sent_unconfirmed`/
   `failed` outbound pushes (≤5), and the person's active run. **Active-run
   progress (2026-07-05):** `api.DigestActiveRun` now carries `plan_steps`
   (the SAME `plan.updated` source `/status` uses via `latestPlanForTask`,
   pre-rendered `[x]/[>]/[ ]` lines, bounded to 6 — completed prefix
   collapses, tail truncates with "… N more steps") and `latest_activity`
   (one line from the newest tool/thinking event), rendered indented under
   the "▶ A task is running now" line — reopening the CLI shows where the
   run stands, not just that it exists. The client
   TUI fetches it BEFORE its first presence beat (the beat stamps the
   anchor) and renders one compact "While you were away" block; an empty
   digest renders nothing. Store queries:
   `control.ListTasksByStatusSince`, `control.ListUndeliveredOutbound`.
   Tests: `httpapi/handlers_digest_test.go`, `control/digest_queries_test.go`,
   `cli/attach_digest_test.go`.
   (d) Re-attach to a mid-flight run — ✅ shipped (2026-07-04). When the
   digest reports an active run, the client TUI starts
   `client.WatchActiveRun` without a user turn: baseline event probe (no
   history replay), live tool/thinking/approval events rendered exactly like
   an in-turn poll, run end detected via a fresh `run.finished`/
   `run.cancelled` event (carries the outcome summary) with a
   `/v1/tasks/current` active-run probe as fallback. The controller flips
   the run-active flags (`attachedRun`/`thinking`), so Enter steers the
   daemon run (`/v1/runs/steer`) and ctrl+c shows the background/cancel exit
   prompt — all existing paths. `cli/attach_digest.go`,
   `client.WatchActiveRun`.
2. **P0 — G1+G2: IM routing stack + queue instead of busy.** Queue-instead-of-busy
   is ✅ shipped (2026-07-04, see the Task queueing capability row): a genuinely
   new message while a run is active is enqueued with an honest acceptance and
   auto-started (sync + async drain + boot drain) when the runner frees up;
   continuations never queue (they steer). The full inbound routing priority
   order (bare y/n approval > pending question > continuation cue > NEW task) is
   now in place — the pending-question leg shipped with G3 below. Observability
   export (`selfmind doctor` / `/diag`) also shipped 2026-07-04.
3. **P0 — G3: clarify-over-IM — ✅ shipped (2026-07-04).** `gatewayClarify` is
   no longer a stub: a clarify is now a first-class DB-backed pending question
   modeled exactly on the approval waiter. It creates a `clarify_requests` row
   (`internal/control/clarifies.go`), appends the `clarify.requested` event,
   pushes a presence-aware, single-preferred-endpoint notification via the
   shared `RunCoordinator.routePendingNotification` (same routing as approvals;
   `notifyClarifyRequested` builds the "Question — reply with your answer:" body,
   `delivery.KindClarify`), then blocks polling the row for up to 30 min. An
   answer recorded from ANY endpoint (`Store.AnswerClarifyRequest`) is returned
   verbatim as the tool result; timeout / expiry returns the best-judgment
   fallback sentinel so the run never hangs. Inbound: a plain non-slash reply
   while a question is pending IS the answer (`tryHandleClarifyAnswer`, wired in
   `tryHandleControlCommand` above the new-task/queue logic and below the bare
   y/n approval leg — approvals and clarifies are mutually exclusive per run, but
   y/n-looking input still favors an approval defensively). Orphan hygiene:
   `Store.ExpireOrphanedClarifies` rides `MarkInterruptedRuns` next to the
   approval sweep, so a restart never leaves a dangling question. Surfacing:
   `/status`, `/diag`, and the attach digest (`api.DigestClarify`, CLI
   "N questions waiting") all show pending clarifies. A question now survives the
   CLI closing exactly like an approval (docs/identity-continuity.md "Runtime
   attachment model"). `internal/control/clarifies.go`,
   `internal/gateway/httpapi/{server.go,clarify_inbound.go,diag.go,handlers_digest.go}`,
   `internal/gateway/cli/attach_digest.go`. Tests:
   `control/clarifies_test.go`, `httpapi/clarify_inbound_test.go`.
4. **P2 — G4 (deferred until queues create real multi-task traffic):
   adaptive task tags + targeted messaging.** IM notifications
   carry a short task tag only when >1 task is alive; `/task <n> <text>`
   routes a message to a specific task (steer if running, next-turn input if
   parked).
5. **P0 — Task-attach semantics for parked tasks.** ✅ shipped (2026-07-05);
   **superseded by Work Timeline P3 pre-label semantics (2026-07-06)** — see
   the Task-attach semantics row and the P3 package entry above. Historical
   rule as shipped: attach ONLY on explicit continuation evidence — a caller-supplied
   `task_id`, an `IntentContinue` classification (router cue or the
   short-acceptance upgrade), or the one-shot pin written by `/resume`
   (person_settings key `resume_pin_task`, consumed by the next agent-bound
   message so a stale `/resume` can never capture unrelated work). Any other
   message that reaches the agent — sync, async dispatch, queued-task drain,
   cron — creates its OWN task carrying the request's explicit `workspace_id`
   (else the person's current workspace); parked tasks stay resumable via
   `/resume` or a later continuation cue. The channel-recency fallback
   (`Store.CurrentTaskForChannel`) was removed with it. `resolveTask` +
   `consumeResumePin` in `httpapi/server.go` / `httpapi/continue_resolver.go`.
   Tests: `httpapi/task_attach_test.go` (new-work isolation, 继续 attach,
   one-shot pin, async explicit-workspace, drained-item own task). Eval:
   `evalcases/continuity/continuity-task-attach.yaml` (`require_task_switch`)
   — needs a live cassette recording before it gates (suite README).
6. **P0 — Continuity path polish.** Surface identity: `GET /v1/accounts` +
   `selfmind accounts`, and make "bind a new endpoint → inherit tasks and
   memory" a visible moment.
7. **P1 — Finish daemon-client convergence, then delete the duplicates.**
   **SUPERSEDED by the ACTIVE PLAN above (P0-3, 2026-07-10)** — same content,
   now with deletion preconditions and eval-scope wording. The legacy TUI
   cleanup (`SELFMIND_TUI_LEGACY`, viewport, `controller_mouse.go`,
   `renderCache`, `uiModel` decomposition) and the N>1 soak
   (`SELFMIND_WORKERS`) ride along in that item.
8. **P1 — Stranger-isolation hardening (scenario 3).** Highest item: the
   Weixin owner auto-bind hazard — with `gateway.weixin.owner_person_id` set,
   EVERY sender passing the DM policy is bound to the owner person
   (`weixin/adapter.go` inbound path), and the default `dm_policy: open` +
   empty `allow_from` means any stranger DMing the account becomes the owner.
   Fix: owner auto-bind must apply only to allowlisted senders, or first-DM
   pairing-code confirmation; until then document allowlist as mandatory after
   login. Also: QQ webhook ed25519 signature verification (inbound is
   currently unverified), Feishu encrypt-envelope AES decryption, WeChat OA
   safe-mode crypto.
9. **P2 — Real `execute_code` sandbox** (namespace/seccomp/cgroup or container).
   Prerequisite for any multi-person sharing; not needed for the single-person
   scenarios.
10. **P2 — MCP `sampling/createMessage`**, IM voice STT/TTS, remaining adapter
   polish — only as scenario needs dictate.
11. **RESOLVED (2026-07-05) — Unify control-command parsing across endpoints.**
   A single canonical registry `internal/gateway/command` (leaf package, stdlib
   only, no import cycle) now owns detection/help/name-lists/async-hint/suggest.
   Rewired to read it: gateway `/help` → `command.HelpText()`;
   `suggestControlCommand` + the unknown-slash reject gate → `command.Suggest`;
   the two async-hint `isControlCommand` copies (`weixin/adapter.go`,
   `handlers_channels.go`) → `command.IsGatewayControl` (fixes the drift — now
   covers `/queue /diag /mode /notify /help /model`); the three approval-mode
   word lists → `tools.IsKnownApprovalModeWord` / `tools.CanonicalApprovalModes`;
   the TUI `slash_commands.go` now exposes every gateway command (typing
   `/approve` in the TUI relays to the daemon) and unifies its unknown-command
   message on `command.Suggest`. Drift guards: `command.TestKnownMatchesGateway`
   `Contract` + httpapi `TestEveryRegistryGatewayCommandIsHandledBySwitch`. The
   big execute switch (`tryHandleControlCommand`) stays authoritative; only
   metadata was consolidated (no execution behavior changed). Dead fork: the
   unmounted IM adapters (`gateway/telegram`, `gateway/wechat`,
   `platform/wechat` via `channel.Bridge`) route raw text through
   `router.Gateway.Handle`, which has no control detection; `Handle` is now
   documented as in-proc-TUI-only (its live caller intercepts control commands
   first) and marked `// DEAD` — physical removal of the adapters + the unused
   `GatewayDeps.Bridge` wiring is a separate low-risk cleanup left as follow-up.
Resolved from this list: **Approval UX** (was item 1, resolved 2026-07-04) —
see the Approval lifecycle row above for what shipped and the two intentional
remainders (unmounted long-poll telegram adapter; Weixin text fallback).
**H2: LLM triage for `smart` approval mode** (was item 11, resolved 2026-07-04)
— inserted as layer 4 in `SmartApprovalMiddleware`, below the hard floor and
class-grant allowlist and above the human ask; fails SAFE to ESCALATE. See the
Approval modes row for the shipped behavior.

Cron proactive delivery, user profile synthesis, CLI image input, approval
modes, and the self-check/CI gate landed with the Phase-1 work — see
`docs/phase1-modules.md`.

## How To Keep This Accurate

- Treat this file as the index of "what is real." Update the affected row in the
  same PR that changes the behavior.
- Do not add per-feature status notes to the historical roadmap docs; record state
  here instead.

## Memory Governance Reliability Closure (2026-07-11)

- Post-run maintenance is now a daemon-consumed durable job, not a detached
  best-effort goroutine only. The replay payload and frozen model proposal are
  persisted separately; crashes and apply failures reuse the same proposal.
  Fresh payload-attachment races remain pending and retries stop after a bounded
  attempt count. Empty model replies fail the job instead of becoming success.
- Canonical intake proposal items are idempotent by run, analyzer version,
  decision key, target, scope, and content. Statement lookup/status mutation is
  target+scope+hash scoped, so identical text in two workspaces cannot mutate
  across the boundary.
- `/memory history` joins explicit learning changes with automatic merge/archive
  events. New merge snapshots preserve evidence ownership and can be reverted
  with `/memory undo <event-id>`; legacy incomplete merge snapshots fail safely.
- Background governance defaults to `shadow`, uses the explicitly configured
  maintenance role only, pauses while foreground runs are active, enforces both
  global and per-workspace caps in `full`, and writes private reports with 0600
  permissions. `auto_supersede_confidence` is reserved and not yet an automatic
  consolidation apply gate.
- Remaining calibration work: run the shadow judge over real legacy history,
  inspect precision/recall, then deliberately promote `shadow -> merge-only ->
  full`. Automatic consolidation SUPERSEDE/CONFLICT application remains out of
  scope until that evidence exists.
