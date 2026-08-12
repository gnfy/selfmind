# Phase 1 Feature Modules

Phase 1 delivers SelfMind as an **open-source CLI assistant that is good enough
for daily personal work, deployable as a 24/7 cloud daemon, commandable from
personal WeChat, and that learns about its user over time** — with UX measured
against the best mainstream coding CLIs.

This page is the module-level index for that work: what each module does and
where it lives. For per-capability status (Done/Partial/Missing) see
`docs/STATUS.md`; for the rules behind these modules see `AGENTS.md`.

> **North star note (2026-07-03):** the Phase-1 acceptance bar is
> **cross-endpoint continuity** (`docs/identity-continuity.md`). Pillar 3
> (cross-channel handoff) is the primary pillar; Pillars 1, 2, and 4 exist to
> support it. Priorities live in `docs/STATUS.md`.

---

## Pillar 1 — CLI daily use (daily-driver quality)

| Module | What it does | Lives in |
|---|---|---|
| Agent loop + native tool calling | Native `tool_calls` first with `[TOOL:]` fallback; safe parallel-vs-sequential batch policy; structured result envelope. | `internal/kernel/native_tool_call.go`, `tool_result.go` |
| Task strategy (agent-first) | Coarse per-turn guardrails; exposes the config-allowed tool surface and lets the model decide (no keyword tool-hiding); web off unless asked; `WithWebEnabled` opt-in. | `internal/kernel/task_strategy.go` |
| Project context loading | Auto-injects `AGENTS.md`/`CLAUDE.md`/`.selfmind.md` into the system prompt. | `internal/kernel/context_scanner.go` |
| Work-quality discipline | Language-agnostic explore→precise-edit→verify guidance; conditional frontend/UI guidance only for UI tasks. | `internal/kernel/prompt_guidance.go` |
| Diff rendering | Colored (+/−) diff for `patch`/`write_file` results in the transcript. | `internal/gateway/cli/transcript_renderer.go` |
| Slash-command palette | Arrow-key navigable popup with selection highlight + Tab completion. | `internal/ui/components/editor.go`, `internal/gateway/cli/controller.go` |
| Approval modes | Staged `/mode`: `on-request` / `read-only` / `auto-edit` / `full-auto`, enforced in tool middleware; on-demand y/N via the clarify bridge. | `internal/tools/middleware.go`, `internal/gateway/cli/command_handlers.go` |
| Image input | Image-path detection + clipboard screenshot paste (`/paste-image`, Ctrl+V auto-detect); routed to `vision_analyze`. Clipboard needs a local GUI (not over SSH). | `internal/gateway/cli/attachments.go`, `clipboard.go` |
| Context compaction | `/compact` shrinks the visible transcript to free context (deterministic, no model call). | `internal/gateway/cli/command_handlers.go` |
| Mid-turn steering | Input typed while a run is in flight is injected into the SAME turn as user guidance at the next iteration boundary — not rejected as "busy" or dropped. In client mode the guidance is forwarded to the daemon's active run via `POST /v1/runs/steer`. | `internal/kernel/steering.go`, `internal/gateway/cli/controller.go`, `internal/gateway/httpapi/handlers_steer.go`, `internal/gateway/client/client.go` |
| Session resume / continuation | Short-acceptance continuation cues resume the same task; handoff + live plan re-injected. | `internal/gateway/httpapi/continue_resolver.go` |

## Pillar 2 — Cloud 24/7 daemon

| Module | What it does | Lives in |
|---|---|---|
| Gateway daemon | `selfmind gateway run` lifecycle (PID/addr/start/stop/restart/status); systemd unit. | `internal/runtime/gateway/`, `packaging/linux/selfmind.service` |
| Fail-closed bind guard | Refuses to bind a non-loopback address without an auth token; default bind is `127.0.0.1:8765` (nothing exposed). | `internal/runtime/gateway/paths.go` (`guardPublicBind`) |
| Run coordinator | Owns the run lifecycle, active-run registry, execution scope, outcome persistence. | `internal/gateway/httpapi/run_coordinator.go` |
| Scheduled tasks (cron) | SQLite-backed scheduler (timezone-aware); a job runs a real agent turn and delivers the result to its channel; per-job `web` opt-in; idempotent built-in jobs. | `internal/kernel/task/cron/`, `internal/gateway/httpapi/cron_executor.go` |

## Pillar 3 — Personal WeChat / IM command

| Module | What it does | Lives in |
|---|---|---|
| Weixin (iLink) adapter | Outbound poll loop (no inbound port needed), AES context_token, typing, media + voice transcript, group/DM policy, dedup, built-in QR login. | `internal/gateway/weixin/` |
| Delivery senders | Channel-local outbound (Weixin/Telegram/Feishu/QQ/WeChat OA); async result delivery + 30s "still working" heartbeat for IM. | `internal/gateway/delivery/`, `internal/gateway/httpapi/run_events.go` |
| Cross-channel handoff | Task state shared via `control.db`; a task started on CLI continues from WeChat (chat is channel-local, task is shared). | `internal/gateway/httpapi/continue_resolver.go`, `internal/control/store.go` |

## Pillar 4 — Learns about you

| Module | What it does | Lives in |
|---|---|---|
| Fact extraction | Auto-extracts durable facts from turns (background, non-blocking). | `internal/kernel/turn_extractor.go`, `fact_extractor.go` |
| Profile synthesis | Distills scattered facts into one stable, always-injected user profile; honors `pinned` authoritative facts (never overridden). | `internal/kernel/profile_synthesizer.go` |
| Profile visibility / veto | `/memory` shows the synthesized profile + pinned facts; `/memory pin` marks ground truth; entries are editable/removable. | `internal/gateway/cli/command_handlers.go`, `internal/tools/memory.go` |
| Reflection / background review | Periodic reflection + memory-fenced background review; skill curator governance. | `internal/kernel/reflection.go`, `background_review.go`, `internal/tools/skill_curator.go` |
| Session recall | FTS history search + searchable session browser. | `internal/tools/session_search.go`, `internal/ui/components/session_browser.go` |

## Cross-cutting — Automated test & self-repair safety net

| Module | What it does | Lives in |
|---|---|---|
| Self-check gate | `selfmind selfcheck` = build + test + offline eval, one pass/fail. | `internal/cliapp/selfcheck_commands.go` |
| CI gate | `vet + build + test + offline eval` on push/PR; never makes live calls. | `.github/workflows/ci.yml` |
| Eval loop + state oracle | Real gateway-path runs; deterministic P0 checks + `assert_state` world-state predicates. | `internal/eval/` |
| VCR record/replay | Record live runs once, replay offline forever (sequence-keyed); strict-offline `ErrCassetteMiss` so CI/selfcheck can't burn quota. | `internal/kernel/llm/vcr.go` |
| Bounded self-repair | `selfmind eval repair` emits a structured repair brief (failed checks + trace) + optional isolated worktree; apply stays human-gated. | `internal/cliapp/repair_commands.go` |
| Scorecard | `selfmind eval scorecard` runs the day-in-the-life suite and emits a per-scenario daily-driver readiness report. | `internal/cliapp/scorecard_commands.go` |
| Flight recorder | Configured via `flight_recorder.enabled` in `config.yaml` (env overrides): records each real user turn's model interaction (bounded, auto-pruned) for later promotion. Free — it saves what already streamed. | `internal/kernel/llm/flight.go`, `internal/kernel/flight_recorder.go`, `internal/platform/config/loader.go` |
| Capture (friction → test) | `/capture` (TUI) / `selfmind eval capture` promotes the last recorded turn into a replayable eval case + copies its cassette, seeding a P0-check + `assert_state` draft. Everyday friction becomes a permanent offline regression test. | `internal/eval/capture.go`, `internal/cliapp/eval_commands.go` |
| Liveness canary | Optional periodic self-check job that alerts the channel only on failure. | `internal/kernel/task/cron/`, `internal/gateway/httpapi/cron_executor.go` |
| Day-in-the-life suite | Representative daily scenarios with recorded cassettes, replayed by the gate. | `evalcases/dayinlife/`, `evalcases/continuity/` |

---

## Known limitations (carried to Phase 2)

- **Provider concurrency is only partially governed** — the daemon worker pool exists, but per-provider caps and tenant-level fairness remain Phase 2 work.
- **`controller.go` still monolithic** — violates the AGENTS.md decomposition guardrail (code hygiene).
- **The Linux sandbox is single-user, not multi-tenant isolation** — bubblewrap protects the daily-driver boundary, while SaaS still needs containers or equivalent namespace/seccomp/cgroup and quota isolation.
- **Clipboard image paste is local-GUI only** — not reachable over SSH (use a file path or send via WeChat there).
- **Native IM approval buttons** not wired; **MCP `sampling/createMessage`** not implemented.
- **Write+verify reliability** is provider-dependent (see the `dayinlife` scenario-5 provider-resilience probe).
