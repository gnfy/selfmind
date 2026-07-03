# SelfMind Implementation Status

> **Read this first.** This is the current-state snapshot for any AI/coding agent
> picking up work, and the **only live priority list** in this repo. The code is
> the ground truth; this page summarizes it so you do not re-implement something
> that already exists. The north star (Phase 1 = cross-endpoint continuity) and
> the acceptance scenarios live in `docs/identity-continuity.md`. Historical
> planning docs were removed from the tree (2026-07-03; retrieve via git
> history) — never resurrect their backlog items or code samples.
>
> **Snapshot date:** 2026-07-03. When you finish a change that moves a row,
> update this table in the same PR. See `docs/phase1-modules.md` for the
> Phase-1 feature-module index.

## Health

- `GOWORK=off go build ./...` — passing.
- `GOWORK=off go test ./...` — passing.
- ~289 Go files, ~62.6k LOC, 80 test files (2026-07-03).

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
| Control store | ✅ | 15 tables: tenants/persons/accounts/workspaces/tasks/runs/events/handoffs/approvals/notifications/outbound/etc. `internal/control/store.go`. |
| Memory + session search | ✅ | `AddFact`/`GetFacts`, FTS recall, memory fence. |
| Skills system | ✅ | list/view/manage/catalog/bundle/curator; history + undo; provenance; governance archive/restore. `internal/tools/skill_*.go`. |
| Skill metrics + pruning | ✅ | `internal/kernel/skill_store.go` RecordCall/Prune. (Roadmap lists this as "to do" — it is done.) |
| Learning audit | ✅ | Tenant JSONL log + per-change snapshots + undo. `internal/tools/learning_audit.go`. |
| Multi-agent delegation | ✅ | Parallel, semaphore-bounded batch delegation. `internal/app/multi_agent.go`. (Roadmap lists this as serial-only — it is parallel.) |
| Extended tools | ✅ | `web_search`, `web_extract`, `execute_code`, `delegate_task`, vision, tts beyond file/terminal. |
| MCP client | 🟡 | Real stdio/HTTP JSON-RPC client, multi-server, on-demand tool registration. `sampling/createMessage` not implemented. `internal/tools/mcp_client.go`. |
| Eval loop | ✅ | Real gateway-path runs; P0 deterministic checks + state-predicate oracle (`assert_state`); VCR record/replay for free offline regression; `selfmind eval run/report/repair/scorecard/capture`; day-in-the-life suite with recorded cassettes. `internal/eval`, `evalcases/`. |
| Flight recorder + capture | ✅ | `SELFMIND_FLIGHT_RECORDER=1` records each real turn; `/capture` / `eval capture` promotes the last turn into a replayable eval case — everyday friction becomes a permanent regression test. `internal/kernel/llm/flight.go`, `internal/kernel/flight_recorder.go`, `internal/eval/capture.go`. |
| Telegram adapter | ✅ | Webhook + long poll, signature verify, send. |
| Personal/Enterprise WeChat (Weixin) adapter | ✅ | iLink protocol (`ilinkai.weixin.qq.com`): poll loop, AES, per-peer context_token, typing, media, group/DM policy, dedup. Built-in QR login (`selfmind weixin login`) — no external bridge needed. This is the primary multi-device WeChat path. |
| WeChat Official Account adapter | 🟡 | Inbound passive-reply + signature verify (`internal/gateway/wechat`); outbound now supported via the customer-service `custom/send` sender (`internal/gateway/delivery/wechat.go`, registered as platform `wechat`). Still no message encryption/decryption. |
| Approval lifecycle | 🟡 | DB + API + `/approve` / `/reject` + staged approval modes (`/mode`) done. Native IM approval buttons not wired. |
| CLI / TUI controller | 🟡 | Components partly extracted; `uiModel` in `controller.go` is still a monolith (violates AGENTS.md guidance). |
| TUI rendering (terminal-first hybrid) | 🟡 | **Default**: history committed to native terminal scrollback (`tea.Println`), only the active region redrawn (`history_commit.go`); terminal owns scroll/select/copy. `SELFMIND_TUI_LEGACY=1` falls back to the alt-screen viewport. Colored patch diffs (`renderPatchCell`), per-message render cache, `/history` (full diffs), `/copy`. Remaining: delete the legacy path + escape hatch once settled; write_file overwrite real diff; `/history` search + `control.db` backing. See `docs/tui-terminal-first-hybrid.md`. |
| Run execution coordinator | 🟡 | `RunCoordinator` (`httpapi/run_coordinator.go`) owns the run lifecycle (`runMessage`/`startAsyncRun`), the active-run registry, and all pre/post-run helpers (workspace/task resolution, execution scope, approval handler, context assembly, stream aggregation, outcome persistence). Server is now the HTTP/orchestration layer. Worker pool shipped behind `SELFMIND_WORKERS` (see Multi-terminal concurrency row). |
| Multi-terminal concurrency (daemon-client) | 🟡 | Decision: converge every terminal on ONE gateway daemon instead of cross-process locks. Foundation shipped: `gateway.EnsureRunning` (discover-or-autostart + health wait, race-safe via the `gateway.lock` flock); CLI client paths (`selfmind send/status/...`) auto-start a local daemon; `internal/gateway/client` daemon-backed `MessageProcessor` (sync `/v1/message` answer + best-effort event poll → ctx stream observer). Client mode is now the **default** for the TUI (`SELFMIND_TUI_INPROC=1` opts out; auto-falls-back to in-process if the daemon can't start). Chat + agent-backed slash commands (`/skills`, `/memory` incl. `list`, `/bundles`, `/checkpoint`) run on the daemon via a safelisted `/v1/dispatch` (workspace-mutating/code-exec tools refused 403); `/status`/`/tasks` route via the message processor; `/skills stats`,`/model` switch show a client-mode notice. Worker pool (`internal/runpool` + `SELFMIND_WORKERS`, default 1) runs inside that daemon. `workspaceSerialKey` serializes **write** turns only (read turns concurrent, Exclusive/SharedRead semantics). Interactive tool approval works in client mode (inline `Approve? [y/N]` driven by the `approval.requested` event → `/v1/approvals/respond`). **Remaining**: session search over the daemon (last parity gap before deleting the in-process path); soak at N>1; per-provider cap (adapter layer, deferred). See `docs/worker-pool-design.md` §8. |
| Process sandbox | 🟡 | Unix process-group isolation only; **not** a security sandbox (no namespace/seccomp/cgroup). Windows is a no-op. |
| Feishu / Lark adapter | 🟡 | Inbound via the generic `/v1/im/feishu` webhook (verification-token / encrypt-key signature, challenge); outbound via `delivery.FeishuSender` (tenant_access_token + `im/v1/messages`, chat_id/open_id routing). Config drives both. Encrypt-envelope AES decryption still TODO (use plaintext mode). |
| QQ official bot adapter | 🟡 | Inbound via `/v1/im/qq` webhook (group/C2C/guild events parsed into a `group:`/`c2c:`/`channel:` target); outbound via `delivery.QQSender` (app access token + per-target message API). Active push only — webhook ed25519 signature verify and passive `msg_id` threading are follow-ups. |
| User profile synthesis | ✅ | `ProfileSynthesizer` distills facts into a stable profile injected each turn; `pinned` authoritative facts the synthesis must not override; visible/correctable via `/memory` (+ `/memory pin`). `internal/kernel/profile_synthesizer.go`. |
| Scheduled tasks (cron) | ✅ | SQLite-backed scheduler with timezone; jobs run a real agent turn and deliver the result to their channel (e.g. daily summary → WeChat); `web` opt-in per job; built-in liveness canary; idempotent built-in jobs. `internal/kernel/task/cron`, `internal/gateway/httpapi/cron_executor.go`. |
| Self-check & CI gate | ✅ | `selfmind selfcheck` (build + test + offline eval) and `.github/workflows/ci.yml`; strict offline VCR replay (`ErrCassetteMiss`) so the gate never burns provider quota. `require_cassette: true` cases fail (not skip) when their cassette is missing; `SELFMIND_EVAL_MIN_CASES` (set to 3 in CI) fails the gate when fewer cases actually replay. `internal/cliapp/selfcheck_commands.go`. |
| Continuity eval coverage | 🟡 | cases + gate mechanism landed; cassettes pending one local recording run. `evalcases/continuity/` (cross-endpoint `/status`, `继续` resume, stranger identity isolation via per-turn `platform_user_id`), all `require_cassette: true`; record with `SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity` and commit `.vcr/continuity_*`. |
| CLI image input | ✅ | Image-path detection in input + clipboard screenshot paste (`/paste-image`, Ctrl+V auto-detect; WSL/macOS/Linux); routed to the `vision_analyze` tool via the attachment pipeline. Clipboard requires a local GUI (not over SSH). `internal/gateway/cli/attachments.go`, `clipboard.go`. |
| Approval modes | ✅ | Staged `on-request` / `read-only` / `auto-edit` / `full-auto` via `/mode`, enforced in `SmartApprovalMiddleware`; on-demand y/N reuses the clarify bridge. `internal/tools/middleware.go`. |
| Mid-turn steering | ✅ | Works in-process (`internal/kernel/steering.go` + controller `steerCh`) **and** in client mode: input typed during a daemon run is forwarded via `POST /v1/runs/steer` (`httpapi/handlers_steer.go`) into the active run's buffered steering channel (`kernel.WithSteering`, sync + async paths), leaving an auditable `run.steered` event. Daemon refusals surface honestly in the TUI (409 no active run / 429 buffer full → transcript notice, never silently dropped). Covered by unit tests (httpapi/client/controller), not VCR. |
| Skill variant evolution / sandbox test | ❌ | Old roadmap P3 (doc removed; see git history); not started, and out of scope for the north star. |

## Highest-Value Next Work (by priority)

These are the live gaps, ordered by their distance from the north star
(`docs/identity-continuity.md` — the three continuity scenarios). This section
is the only priority list in the repo; other docs must point here.

1. **P0 — Record the continuity eval cassettes.** The cross-endpoint continuity
   suite (`evalcases/continuity/`) and the gate mechanism
   (`require_cassette`, `SELFMIND_EVAL_MIN_CASES`) are landed; the only missing
   piece is one local recording run against a live provider:
   `SELFMIND_EVAL_VCR=record selfmind eval run evalcases/continuity`, then
   commit `.vcr/continuity_*`. Until then `selfmind selfcheck` (and CI) fails
   on the three `require_cassette` cases by design.
2. **P0 — Async-run task visibility.** A `selfmind send --async` run attaches
   to a workspace-matched task but does NOT update the person's `current_task`
   pointer, so `/status` on every endpoint shows an unrelated old task and the
   user's own async run is invisible (found in live use 2026-07-04). Fix:
   update `current_task` when a run resolves its task, and/or make `/status`
   prefer the active run's task. Add a regression test + eval case.
3. **P0 — Approval UX (validated live: scenario-1 works but hurts).**
   (a) `/approve` must accept the list ordinal (`/approve 1`) and a unique
   short-id prefix, not only the full `apr_` UUID (mobile-unfriendly; users
   naturally type the ordinal and get "not found");
   (b) `/approvals` and approval notifications must show WHAT is being
   approved: tool, bounded command/file preview, reason, task title — blind
   approval is an incident waiting to happen;
   (c) CLI `selfmind approve` returns a raw-JSON 500 on a wrong id and accepts
   a task id silently — friendly errors + detect `task_` prefix;
   (d) push approval requests to the person's bound IM endpoints
   (`notifyApprovalRequested` currently skips CLI-originated approvals) and
   wire Telegram inline buttons; Weixin keeps text `/approve` fallback;
   (e) `selfmind send` lacks a `--mode` flag though the API supports
   per-request `approval_mode`.
4. **P0 — Continuity path polish.** Surface identity: `GET /v1/accounts` +
   `selfmind accounts`, and make "bind a new endpoint → inherit tasks and
   memory" a visible moment.
5. **P1 — Eval isolation & run finalization.** Recording/eval against the
   default path writes eval-* persons, current_task rows, and runs left in
   `running` into the REAL `control.db` (found 2026-07-04: 20+ eval persons,
   6 stuck runs). Default eval/record runs to an isolated data dir (the
   `isolated` scenario mechanism exists); always finalize run status; provide
   a one-shot cleanup for existing eval residue.
6. **P1 — Stuck-run recovery.** Two real tasks remain `[running]` after
   interruption; the interrupted-run recovery must mark heartbeat-dead runs
   as interrupted (on daemon start and periodically), so `/tasks` never shows
   phantom running work.
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

Cron proactive delivery, user profile synthesis, CLI image input, approval
modes, and the self-check/CI gate landed with the Phase-1 work — see
`docs/phase1-modules.md`.

## How To Keep This Accurate

- Treat this file as the index of "what is real." Update the affected row in the
  same PR that changes the behavior.
- Do not add per-feature status notes to the historical roadmap docs; record state
  here instead.
