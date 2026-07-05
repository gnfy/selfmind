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
| LLM protocol adapters | ✅ | `openai_chat`, `openai_compatible`, `anthropic_messages`, `codex_responses` + transport registry + provider quirks. `internal/kernel/llm`. |
| Native tool calling | ✅ | Native `tool_calls` first, `[TOOL:...]` fallback, repeated-failure guardrails, secret redaction. |
| Task strategy / intent routing | ✅ | Agent-first; coarse `TaskStrategy`; rules/hybrid/llm intent. `internal/kernel/task_strategy.go`, `internal/gateway/router`. |
| Context engine | ✅ | Bounded, deterministic message window on the hot path. `internal/kernel/context_engine.go`. |
| Control store | ✅ | tenants/persons/accounts/workspaces/tasks/runs/events/handoffs/approvals/grants/notifications/outbound/person_settings/`task_queue`/etc. `internal/control/store.go`. |
| Memory + session search | ✅ | `AddFact`/`GetFacts`, FTS recall, memory fence. |
| Skills system | ✅ | list/view/manage/catalog/bundle/curator; history + undo; provenance; governance archive/restore. `internal/tools/skill_*.go`. Auto-create via `SpawnReview` (scripted-provider end-to-end: `background_review_integration_test.go`) and curator governance (pin/manual protection, archive audit + restore: `skill_curator_test.go`) now have deterministic integration coverage. Background-review change claims ("skill created/updated/patched: <name>") are now verified against the toolchain (`skill_view` through the restricted backend) before notifying — a hallucinated claim with no tool call is detected, logged, and reported as no-change instead of being forwarded. |
| Skill metrics + pruning | ✅ | `internal/kernel/skill_store.go` RecordCall/Prune. (Roadmap lists this as "to do" — it is done.) Deterministic Prune coverage in `internal/kernel/skill_store_test.go`. |
| Learning audit | ✅ | Tenant JSONL log + per-change snapshots + undo. `internal/tools/learning_audit.go`. |
| Multi-agent delegation | ✅ | Parallel, semaphore-bounded batch delegation. `internal/app/multi_agent.go`. (Roadmap lists this as serial-only — it is parallel.) |
| Extended tools | ✅ | `web_search`, `web_extract`, `execute_code`, `delegate_task`, vision, tts beyond file/terminal. |
| MCP client | 🟡 | Real stdio/HTTP JSON-RPC client, multi-server, on-demand tool registration. `sampling/createMessage` not implemented. `internal/tools/mcp_client.go`. |
| Eval loop | ✅ | Real gateway-path runs; P0 deterministic checks + state-predicate oracle (`assert_state`); VCR record/replay for free offline regression; `selfmind eval run/report/repair/scorecard/capture/clean`; day-in-the-life suite with recorded cassettes. **Data-isolated by default**: every run (record and replay) uses a throwaway temp data dir (`shared_data: true` opts out); post-case run-finalization sweep forces leftover `running` rows terminal; `selfmind eval clean [--yes]` removes historic eval residue from a real control.db. `internal/eval`, `evalcases/`. |
| Flight recorder + capture | ✅ | `SELFMIND_FLIGHT_RECORDER=1` records each real turn; `/capture` / `eval capture` promotes the last turn into a replayable eval case — everyday friction becomes a permanent regression test. `internal/kernel/llm/flight.go`, `internal/kernel/flight_recorder.go`, `internal/eval/capture.go`. |
| Telegram adapter | ✅ | Webhook + long poll, signature verify, send. |
| Personal/Enterprise WeChat (Weixin) adapter | ✅ | iLink protocol (`ilinkai.weixin.qq.com`): poll loop, AES, per-peer context_token, typing, media, group/DM policy, dedup. Built-in QR login (`selfmind weixin login`) — no external bridge needed. This is the primary multi-device WeChat path. |
| WeChat Official Account adapter | 🟡 | Inbound passive-reply + signature verify (`internal/gateway/wechat`); outbound now supported via the customer-service `custom/send` sender (`internal/gateway/delivery/wechat.go`, registered as platform `wechat`). Still no message encryption/decryption. |
| Approval lifecycle | 🟡 | DB + API + `/approve` / `/reject` + staged approval modes (`/mode`) done. Approval UX shipped (2026-07-04): all surfaces (control commands, `POST /v1/approvals/respond`, CLI, Telegram buttons) resolve references through one shared resolver (`httpapi/approval_resolver.go`) — list ordinal (`/approve 1`), unique `apr_` prefix, bare `/approve` with a single pending, `task_` ids rejected with a hint; `/approvals` shows tool + bounded args preview + reason + task title; CLI-originated approvals fan out to the person's other bound IM accounts (`notifyApprovalRequested` + `ListAccountsByPerson`); Telegram gets native inline approve/reject buttons (typed `delivery.Message.Kind`, persisted on the outbound row so retries keep buttons) with `callback_query` handled in both the telegram adapter and the generic `/v1/im/*` webhook; `selfmind approve/reject` returns one-line errors, never raw JSON; `selfmind send --mode` threads `approval_mode`. Remaining: the long-poll `internal/gateway/telegram` adapter is still not mounted by the daemon (generic webhook path is), and Weixin stays text-fallback by design. Outbound dispatch is claim-based (`ClaimDelivery`): the immediate attempt and the retry poller are mutually exclusive, fixing the live duplicate approval push. IM approvals are conversational and task-free (owner request 2026-07-04): the push is `Approval needed — reply y or n:` + the command/reason only (no task label, no apr_ id, no ordinal); a bare `y`/`n` (or 好/可以/不行) answers the single pending approval, degrading to a numbered `/approve <n>` list only when multiple runs have approvals pending in parallel. The task concept stays in the control plane, out of the IM UX. CLI-originated async results now fan out to bound IM endpoints (`deliverAsyncResult` → `fanOutToBoundIM`) so a fire-and-forget terminal run's final answer — including a rejection acknowledgment — is visible on WeChat/Telegram instead of vanishing. Watch items: (a) one live WeChat `/reject 1` got no reply, likely a message lost in a gateway-restart window (iLink getupdates canceled mid-poll); (b) two result pushes were `sent` (correct target, iLink API accepted) but never arrived on the phone ~4.7h after the user's last inbound message — suspect iLink proactive-push context_token staleness; verify the weixin sender checks the response errcode and consider marking undeliverable pushes failed for retry. |
| Clarify lifecycle (G3) | ✅ | A mid-run agent question is a first-class DB-backed pending question modeled exactly on the approval waiter (2026-07-04). `gatewayClarify` (formerly a stub) creates a `clarify_requests` row (`internal/control/clarifies.go`: `Create`/`Get`/`List`/`Answer`/`Expire`/`ExpireOrphanedClarifies`), appends the `clarify.requested` event, pushes a presence-aware, single-preferred-endpoint notification through the shared `RunCoordinator.routePendingNotification` (same routing as approvals; `notifyClarifyRequested` + `delivery.KindClarify`, body `Question — reply with your answer:`), then blocks polling the row for up to 30 min. An answer recorded from ANY endpoint (`Store.AnswerClarifyRequest`) returns verbatim as the tool result; timeout/expiry returns a best-judgment fallback sentinel so the run never hangs. Inbound: a plain non-slash reply while a question is pending IS the answer (`tryHandleClarifyAnswer`, in `tryHandleControlCommand` above new-task/queue logic and below the bare y/n approval leg). Orphan hygiene rides `MarkInterruptedRuns` next to the approval sweep. Surfaced in `/status`, `/diag`, and the attach digest (`api.DigestClarify`). A question survives the CLI closing exactly like an approval (docs/identity-continuity.md "Runtime attachment model"). Tests: `control/clarifies_test.go`, `httpapi/clarify_inbound_test.go`. |
| CLI / TUI controller | 🟡 | Components partly extracted; `uiModel` in `controller.go` is still a monolith (violates AGENTS.md guidance). |
| TUI rendering (terminal-first hybrid) | 🟡 | **Default**: history committed to native terminal scrollback (`tea.Println`), only the active region redrawn (`history_commit.go`); terminal owns scroll/select/copy. `SELFMIND_TUI_LEGACY=1` falls back to the alt-screen viewport. Colored patch diffs (`renderPatchCell`), per-message render cache, `/history` (full diffs), `/copy`. Codex-style interactive approval panel (2026-07-05): `approval.requested` arms a bordered selector in the ACTIVE region (`ui/components.ApprovalPrompt`, wired by `gateway/cli/approval_flow.go`) — ↑/↓/j/k + Enter or shortcuts y/t/a/n mapping to grant scope ""/task/person on the existing `/v1/approvals/respond` path; Esc does nothing (explicit decision required); "No" opens a deny follow-up composer (Enter = bare deny, text = deny + mid-turn guidance); queued approvals re-arm FIFO; duplicate text notice + "Preparing to run" spinner suppressed while the panel is up; transcript keeps ONE compact `notice` line per request/decision; status bar shows `⏸ waiting approval`. Renders in both hybrid and legacy modes; IM/text approval surfaces unchanged. Remaining: delete the legacy path + escape hatch once settled; write_file overwrite real diff; `/history` search + `control.db` backing. See `docs/tui-terminal-first-hybrid.md`. |
| Run execution coordinator | 🟡 | `RunCoordinator` (`httpapi/run_coordinator.go`) owns the run lifecycle (`runMessage`/`startAsyncRun`), the active-run registry, and all pre/post-run helpers (workspace/task resolution, execution scope, approval handler, context assembly, stream aggregation, outcome persistence). Server is now the HTTP/orchestration layer. Worker pool shipped behind `SELFMIND_WORKERS` (see Multi-terminal concurrency row). Async-run task visibility fixed (2026-07-04): every run (sync and async) now syncs the person's `current_task` pointer to the task it resolved (`syncCurrentTask`, same `SetCurrentTask` mechanism as `/new`/`/resume`), and `/status` prefers the active run's task over the pointer (`Server.statusReply`); regression tests in `httpapi/task_visibility_test.go`. Stuck-run recovery shipped (2026-07-04): **invariant — after any finalization or recovery sweep, no task may remain `running` with zero live runs** (`running` means "a run is executing right now"; between-turns tasks park as `in_progress`). Enforced by: `Store.FinishRun` coercing non-terminal run statuses to `done`; `Store.MarkInterruptedRuns` flipping heartbeat-stale runs *and* repairing orphaned `running` tasks; a boot sweep (threshold 0 — the `gateway.lock` flock guarantees leftover running runs are dead) plus a 60s in-daemon sweep (12× the 10s run heartbeat) that always excludes the active-run registry (`httpapi/run_recovery.go`). Recovered `interrupted`/`in_progress` tasks stay non-terminal and resumable via `继续`/`/resume`. Tests: `control/runtime_test.go`, `httpapi/run_recovery_test.go`. |
| Multi-terminal concurrency (daemon-client) | 🟡 | Decision: converge every terminal on ONE gateway daemon instead of cross-process locks. Foundation shipped: `gateway.EnsureRunning` (discover-or-autostart + health wait, race-safe via the `gateway.lock` flock); CLI client paths (`selfmind send/status/...`) auto-start a local daemon; `internal/gateway/client` daemon-backed `MessageProcessor` (sync `/v1/message` answer + best-effort event poll → ctx stream observer). Client mode is now the **default** for the TUI (`SELFMIND_TUI_INPROC=1` opts out; auto-falls-back to in-process if the daemon can't start). Chat + agent-backed slash commands (`/skills`, `/memory` incl. `list`, `/bundles`, `/checkpoint`) run on the daemon via a safelisted `/v1/dispatch` (workspace-mutating/code-exec tools refused 403); `/status`/`/tasks` route via the message processor; `/skills stats`,`/model` switch show a client-mode notice. Worker pool (`internal/runpool` + `SELFMIND_WORKERS`, default 1) runs inside that daemon. `workspaceSerialKey` serializes **write** turns only (read turns concurrent, Exclusive/SharedRead semantics). Interactive tool approval works in client mode (Codex-style TUI approval panel driven by the `approval.requested` event → `/v1/approvals/respond`, incl. grant scope; see TUI rendering row). **Remaining**: session search over the daemon (last parity gap before deleting the in-process path); soak at N>1; per-provider cap (adapter layer, deferred). See `docs/worker-pool-design.md` §8. |
| Process sandbox | 🟡 | Unix process-group isolation only; **not** a security sandbox (no namespace/seccomp/cgroup). Windows is a no-op. |
| Feishu / Lark adapter | 🟡 | Inbound via the generic `/v1/im/feishu` webhook (verification-token / encrypt-key signature, challenge); outbound via `delivery.FeishuSender` (tenant_access_token + `im/v1/messages`, chat_id/open_id routing). Config drives both. Encrypt-envelope AES decryption still TODO (use plaintext mode). |
| QQ official bot adapter | 🟡 | Inbound via `/v1/im/qq` webhook (group/C2C/guild events parsed into a `group:`/`c2c:`/`channel:` target); outbound via `delivery.QQSender` (app access token + per-target message API). Active push only — webhook ed25519 signature verify and passive `msg_id` threading are follow-ups. |
| User profile synthesis | ✅ | `ProfileSynthesizer` distills facts into a stable profile injected each turn; `pinned` authoritative facts the synthesis must not override; visible/correctable via `/memory` (+ `/memory pin`). `internal/kernel/profile_synthesizer.go`. |
| Scheduled tasks (cron) | ✅ | SQLite-backed scheduler with timezone; jobs run a real agent turn and deliver the result to their channel (e.g. daily summary → WeChat); `web` opt-in per job; built-in liveness canary; idempotent built-in jobs. `internal/kernel/task/cron`, `internal/gateway/httpapi/cron_executor.go`. |
| Self-check & CI gate | ✅ | `selfmind selfcheck` (build + test + offline eval) and `.github/workflows/ci.yml`; strict offline VCR replay (`ErrCassetteMiss`) so the gate never burns provider quota. `require_cassette: true` cases fail (not skip) when their cassette is missing; `SELFMIND_EVAL_MIN_CASES` (set to 3 in CI) fails the gate when fewer cases actually replay. `internal/cliapp/selfcheck_commands.go`. |
| Continuity eval coverage | ✅ | Cases + gate + cassettes recorded and committed; selfcheck replays 7 cases offline. `evalcases/continuity/` (cross-endpoint `/status`, `继续` resume, stranger identity isolation via per-turn `platform_user_id`), all `require_cassette: true`; record with `SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity` and commit `.vcr/continuity_*`. `continuity-task-attach.yaml` added 2026-07-05 (task-attach semantics via `require_task_switch`); its cassette is not yet recorded, so it skips offline — flip `require_cassette: true` in the same commit as the cassette. |
| CLI image input | ✅ | Image-path detection in input + clipboard screenshot paste (`/paste-image`, Ctrl+V auto-detect; WSL/macOS/Linux); routed to the `vision_analyze` tool via the attachment pipeline. Clipboard requires a local GUI (not over SSH). `internal/gateway/cli/attachments.go`, `clipboard.go`. |
| Approval modes | ✅ | Staged `on-request` / `read-only` / `auto-edit` / `full-auto` / `smart` via `/mode`, enforced in `SmartApprovalMiddleware`; on-demand y/N reuses the clarify bridge. `internal/tools/middleware.go`. **Layered approval funnel shipped (H1, 2026-07-04):** the middleware now runs (1) an unbypassable **hard floor** (`hardlineToolCall`) that fires before any mode bypass — full-auto included — for irreversible ops (recursive delete of `/`,`/home`,`$HOME`,`/etc`,`/usr`,`/var`,`/boot`; `mkfs*`; `dd of=/dev/sd*|nvme*`; redirect over a raw disk device; fork bomb; `shutdown`/`reboot`/`halt`/`poweroff`/`init 0|6`), returning `operation blocked by safety policy: …` (distinct from the user-rejection contract; kernel `isUserRejectionErr` does NOT match it); (2) mode bypass; (3) **class-level approval memory** — approving a coarse *class* (`approvalPatternKey`, e.g. any `chmod` → `exec:invokes dangerous command: chmod`) for the task (session) or person (persistent) via the durable `approval_grants` table skips later same-class asks; (4, smart mode) **LLM triage**; (5) human ask. Reply grammar: `/approve [n] task` / `/approve [n] always|person` (or bare `yt`/`ya`) records the grant scope; `/mode` is now an IM control command persisting per-person `approval_mode`. **Smart-mode LLM triage shipped (H2, 2026-07-04):** in `smart` mode a dangerous (non-hardline) op with no matching grant is triaged by a cheap role model (`background_review`, off the main run provider) before the human ask — APPROVE auto-runs AND records a task-scope class grant (judge consulted at most once per class per task), DENY blocks with the user-rejection contract (`operation rejected: blocked by safety triage`, do-not-retry), ESCALATE / no judge / any error / 15s timeout falls through to the human ask (fails SAFE, never open). The judge (`ApprovalJudge` interface in `internal/tools/approval_triage.go`, injected via `ExecutionScope.Judge`) strips shell comments and wraps the command in `<command>` delimiters with a prompt-injection defense. The hard floor stays ABOVE triage (hardline ops never reach the judge). `internal/tools/middleware.go`, `internal/tools/approval_triage.go`, `internal/app/approval_judge.go`, `internal/control/approval_grants.go`, `internal/gateway/httpapi/approval_resolver.go`. |
| Mid-turn steering | ✅ | Works in-process (`internal/kernel/steering.go` + controller `steerCh`) **and** in client mode: input typed during a daemon run is forwarded via `POST /v1/runs/steer` (`httpapi/handlers_steer.go`) into the active run's buffered steering channel (`kernel.WithSteering`, sync + async paths), leaving an auditable `run.steered` event. Daemon refusals surface honestly in the TUI (409 no active run / 429 buffer full → transcript notice, never silently dropped). Covered by unit tests (httpapi/client/controller), not VCR. |
| Task queueing (G1+G2) | ✅ | New work while a run is active is **queued, not rejected** (2026-07-04). Durable `control.task_queue` (`internal/control/queue.go`: `EnqueueQueued`/`ListQueued`/`NextQueued`/`CountQueued`/`MarkQueued`/`ClearQueued`/`ListAllQueued`/`RequeueStartedQueued`). `ProcessMessage` enqueues a genuinely-new message behind the active run with an honest acceptance (`Queued behind the running task (N ahead)…`); a continuation (`IntentContinue`) is never queued — it stays busy and steers. On every run finalization (sync + async paths) `RunCoordinator.drainQueue` auto-starts the next queued item as a normal async run (per-person `draining` re-entrancy guard; reverts the row to `queued` if a fresh run races the slot). Boot drain (`Server.DrainQueuedAtBoot`, wired in the gateway runner) requeues `started` rows and resumes pending work after a restart. Visibility: `/queue` (list) / `/queue clear`; `/stop` cancels the active run and then drains. `httpapi/queue.go`, `httpapi/run_coordinator.go`. Tests: `control/queue_test.go`, `httpapi/queue_test.go`. |
| Task-attach semantics | ✅ | Attach only on explicit continuation evidence: caller `task_id`, `IntentContinue` (router cue / short acceptance), or the one-shot `/resume` pin (`person_settings.resume_pin_task`, consumed by the next agent-bound message). Every other agent-bound message — sync, async, queued-drain, cron — creates its own task honoring the request `workspace_id`; parked tasks are never captured and stay resumable (2026-07-05, fixes the live "async request landed on an unrelated /new-created parked task" defect). `resolveTask`/`consumeResumePin` (`httpapi/server.go`, `httpapi/continue_resolver.go`); `Store.CurrentTaskForChannel` removed. Tests: `httpapi/task_attach_test.go`; eval: `evalcases/continuity/continuity-task-attach.yaml` (cassette pending). |
| Observability / diagnostics | ✅ | Self-serve diagnostics so the owner never re-describes bugs by hand (2026-07-04). `selfmind doctor [--out FILE]` (`internal/cliapp/doctor_commands.go`): a redacted bundle — gateway status (live HTTP status, else on-disk PID record), last 10 runs (status/title/elapsed/last_error), pending approvals, queued tasks, `sent_unconfirmed`/`failed` pushes, presence snapshot (durable `accounts.last_seen_at`), per-channel activity, and the last 50 gateway.log lines. Read-only; works whether or not the daemon is up (reads control.db + log directly). `/diag` control command returns a compact phone-friendly snapshot (active run + elapsed, queued count, pending approvals, last error, recent events). Content redacted via `tools.RedactSensitive`. Store queries: `control.ListRecentRunsForPerson`, `control.CountChannelMessagesByChannel` (`internal/control/doctor_queries.go`). Tests: `cliapp/doctor_test.go`, `httpapi/queue_test.go` (`/diag`). |
| Skill variant evolution / sandbox test | ❌ | Old roadmap P3 (doc removed; see git history); not started, and out of scope for the north star. |

## Highest-Value Next Work (by priority)

These are the live gaps, ordered by their distance from the north star
(`docs/identity-continuity.md` — the three continuity scenarios). This section
is the only priority list in the repo; other docs must point here.

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
   surface in the attach digest (G0-c, 2026-07-04). Remaining: optionally
   catch-up on the peer's next inbound; calibrate the window empirically.
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
   preferred-endpoint recency). CLI-origin approval/result pushes: skipped
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
   `failed` outbound pushes (≤5), and the person's active run. The client
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
5. **P0 — Task-attach semantics for parked tasks.** ✅ shipped (2026-07-05).
   Rule: attach ONLY on explicit continuation evidence — a caller-supplied
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
   Remaining parity gap: session search over the daemon. Once closed, remove
   the in-process TUI path (`SELFMIND_TUI_INPROC`), the legacy alt-screen TUI
   (`SELFMIND_TUI_LEGACY`, viewport, `controller_mouse.go`, `renderCache`), and
   decompose `uiModel` per the AGENTS.md guardrail — one simplification pass.
   Then a real N>1 soak (`SELFMIND_WORKERS`).
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
