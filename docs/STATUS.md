# SelfMind Implementation Status

> **Read this first.** This is the current-state snapshot for any AI/coding agent
> picking up work. The code is the ground truth; this page summarizes it so you do
> not re-implement something that already exists. The planning docs
> (`selfmind-evolution-roadmap.md`, `selfmind-evolution-design.md`,
> `p0-p1-development-plan.zh-CN.md`) are historical intent and are **partially
> superseded** — verify any "to do" item against this page and against the code
> before acting on it.
>
> **Snapshot date:** 2026-06-28. When you finish a change that moves a row,
> update this table in the same PR. See `docs/phase1-modules.md` for the
> Phase-1 (open-source CLI + cloud daemon + WeChat) feature-module index.

## Health

- `GOWORK=off go build ./...` — passing.
- `GOWORK=off go test ./...` — passing.
- ~228 Go files, ~51.7k LOC, 49 test files.

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
| Native tool calling | ✅ | Hermes-style native `tool_calls`, `[TOOL:...]` fallback, repeated-failure guardrails, secret redaction. |
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
| Approval lifecycle | 🟡 | DB + API + `/approve` / `/reject` + codex-style approval modes (`/mode`) done. Native IM approval buttons not wired. |
| CLI / TUI controller | 🟡 | Components partly extracted; `uiModel` in `controller.go` is still a monolith (violates AGENTS.md guidance). |
| TUI rendering (terminal-first hybrid) | 🟡 | **Default**: history committed to native terminal scrollback (`tea.Println`), only the active region redrawn (`history_commit.go`); terminal owns scroll/select/copy. `SELFMIND_TUI_LEGACY=1` falls back to the alt-screen viewport. Codex-style patch diffs (`renderPatchCell`), per-message render cache, `/history` (full diffs), `/copy`. Remaining: delete the legacy path + escape hatch once settled; write_file overwrite real diff; `/history` search + `control.db` backing. See `docs/tui-terminal-first-hybrid.md`. |
| Run execution coordinator | 🟡 | `RunCoordinator` (`httpapi/run_coordinator.go`) owns the run lifecycle (`runMessage`/`startAsyncRun`), the active-run registry, and all pre/post-run helpers (workspace/task resolution, execution scope, approval handler, context assembly, stream aggregation, outcome persistence). Server is now the HTTP/orchestration layer. Worker pool shipped behind `SELFMIND_WORKERS` (see Multi-terminal concurrency row). |
| Multi-terminal concurrency (daemon-client) | 🟡 | Decision: converge every terminal on ONE gateway daemon (the codex/hermes model) instead of cross-process locks. Foundation shipped: `gateway.EnsureRunning` (discover-or-autostart + health wait, race-safe via the `gateway.lock` flock); CLI client paths (`selfmind send/status/...`) auto-start a local daemon; `internal/gateway/client` daemon-backed `MessageProcessor` (sync `/v1/message` answer + best-effort event poll → ctx stream observer). Client mode is now the **default** for the TUI (`SELFMIND_TUI_INPROC=1` opts out; auto-falls-back to in-process if the daemon can't start). Chat + agent-backed slash commands (`/skills`, `/memory` incl. `list`, `/bundles`, `/checkpoint`) run on the daemon via a safelisted `/v1/dispatch` (workspace-mutating/code-exec tools refused 403); `/status`/`/tasks` route via the message processor; `/skills stats`,`/model` switch show a client-mode notice. Worker pool (`internal/runpool` + `SELFMIND_WORKERS`, default 1) runs inside that daemon. `workspaceSerialKey` serializes **write** turns only (read turns concurrent, codex Exclusive/SharedRead). Interactive tool approval works in client mode (inline `Approve? [y/N]` driven by the `approval.requested` event → `/v1/approvals/respond`). **Remaining**: session search over the daemon (last parity gap before deleting the in-process path); soak at N>1; per-provider cap (adapter layer, deferred). See `docs/worker-pool-design.md` §8. |
| Process sandbox | 🟡 | Unix process-group isolation only; **not** a security sandbox (no namespace/seccomp/cgroup). Windows is a no-op. |
| Feishu / Lark adapter | 🟡 | Inbound via the generic `/v1/im/feishu` webhook (verification-token / encrypt-key signature, challenge); outbound via `delivery.FeishuSender` (tenant_access_token + `im/v1/messages`, chat_id/open_id routing). Config drives both. Encrypt-envelope AES decryption still TODO (use plaintext mode). |
| QQ official bot adapter | 🟡 | Inbound via `/v1/im/qq` webhook (group/C2C/guild events parsed into a `group:`/`c2c:`/`channel:` target); outbound via `delivery.QQSender` (app access token + per-target message API). Active push only — webhook ed25519 signature verify and passive `msg_id` threading are follow-ups. |
| User profile synthesis | ✅ | `ProfileSynthesizer` distills facts into a stable profile injected each turn; `pinned` authoritative facts the synthesis must not override; visible/correctable via `/memory` (+ `/memory pin`). `internal/kernel/profile_synthesizer.go`. |
| Scheduled tasks (cron) | ✅ | SQLite-backed scheduler with timezone; jobs run a real agent turn and deliver the result to their channel (e.g. daily summary → WeChat); `web` opt-in per job; built-in liveness canary; idempotent built-in jobs. `internal/kernel/task/cron`, `internal/gateway/httpapi/cron_executor.go`. |
| Self-check & CI gate | ✅ | `selfmind selfcheck` (build + test + offline eval) and `.github/workflows/ci.yml`; strict offline VCR replay (`ErrCassetteMiss`) so the gate never burns provider quota. `internal/cliapp/selfcheck_commands.go`. |
| CLI image input | ✅ | Image-path detection in input + clipboard screenshot paste (`/paste-image`, Ctrl+V auto-detect; WSL/macOS/Linux); routed to the `vision_analyze` tool via the attachment pipeline. Clipboard requires a local GUI (not over SSH). `internal/gateway/cli/attachments.go`, `clipboard.go`. |
| Approval modes | ✅ | Codex-style `on-request` / `read-only` / `auto-edit` / `full-auto` via `/mode`, enforced in `SmartApprovalMiddleware`; on-demand y/N reuses the clarify bridge. `internal/tools/middleware.go`. |
| Skill variant evolution / sandbox test | ❌ | Roadmap P3; not started. |

## Highest-Value Next Work (by priority)

These are the live gaps, ordered. They are derived from the table above, not from
the historical roadmaps.

1. **P0 — Daemon-client convergence (multi-terminal).** The worker pool shipped
   (`internal/runpool` + `SELFMIND_WORKERS`), but it only helps *within one
   process*. The real multi-terminal blocker is that the rich TUI still builds an
   in-process gateway, so two `selfmind` terminals are two processes racing on
   `control.db` / auth refresh / worker state. Fix (codex/hermes model): make the
   TUI a thin client to one shared daemon. Foundation shipped (`EnsureRunning`,
   CLI auto-start, `internal/gateway/client`, `SELFMIND_TUI_CLIENT=1`). Next:
   default the TUI to client mode and route agent-backed slash commands
   (`/model`, `/skills`, `/memory`, `/tasks`) over the gateway API so client mode
   reaches parity. Then per-write-only workspace serialization, per-provider rate
   cap, and a real N>1 soak.
2. **P0 — Decompose the CLI controller**: move `uiModel` state out of
   `controller.go` into components, per the AGENTS.md guardrail.
3. **P1 — Real `execute_code` sandbox** (namespace/seccomp/cgroup or container)
   before any untrusted multi-tenant code execution.
4. **P1 — Wire native IM approval buttons** (Telegram / Weixin); backend +
   approval modes are ready.
5. **P2 — IM adapter hardening.** Feishu and QQ are now connected (inbound
   webhook + outbound sender). Remaining polish: QQ webhook ed25519 signature
   verification + passive `msg_id` threading; Feishu encrypt-envelope AES
   decryption; inbound voice→STT and outbound TTS across platforms.
6. **P2 — MCP `sampling/createMessage`** for servers that call back into the model.

Cron proactive delivery, user profile synthesis, CLI image input, approval
modes, and the self-check/CI gate landed with the Phase-1 work — see
`docs/phase1-modules.md`.

## How To Keep This Accurate

- Treat this file as the index of "what is real." Update the affected row in the
  same PR that changes the behavior.
- Do not add per-feature status notes to the historical roadmap docs; record state
  here instead.
