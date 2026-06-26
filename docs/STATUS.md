# SelfMind Implementation Status

> **Read this first.** This is the current-state snapshot for any AI/coding agent
> picking up work. The code is the ground truth; this page summarizes it so you do
> not re-implement something that already exists. The planning docs
> (`selfmind-evolution-roadmap.md`, `selfmind-evolution-design.md`,
> `p0-p1-development-plan.zh-CN.md`) are historical intent and are **partially
> superseded** — verify any "to do" item against this page and against the code
> before acting on it.
>
> **Snapshot date:** 2026-06-27. When you finish a change that moves a row,
> update this table in the same PR.

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
| Eval loop | ✅ | Real gateway-path replay; P0 deterministic checks; JSONL traces with content hashing; 29 cases / 5 suites. `internal/eval`, `evalcases/`. |
| Telegram adapter | ✅ | Webhook + long poll, signature verify, send. |
| Enterprise WeChat (Weixin) adapter | ✅ | Full duplex, AES decrypt, attachments. |
| WeChat Official Account adapter | 🟡 | Inbound passive-reply + signature verify (`internal/gateway/wechat`); outbound now supported via the customer-service `custom/send` sender (`internal/gateway/delivery/wechat.go`, registered as platform `wechat`). Still no message encryption/decryption. |
| Approval lifecycle | 🟡 | DB + API + `/approve` / `/reject` done. Native IM approval buttons not wired. |
| CLI / TUI controller | 🟡 | Components partly extracted; `uiModel` in `controller.go` is still a monolith (violates AGENTS.md guidance). |
| Run execution coordinator | 🟡 | Run lifecycle still embedded in `httpapi/server.go`; not extracted to a worker/coordinator. `Agent.runMu` still serializes runs. |
| Process sandbox | 🟡 | Unix process-group isolation only; **not** a security sandbox (no namespace/seccomp/cgroup). Windows is a no-op. |
| Feishu / QQ adapters | ❌ | Not started. |
| User profile synthesis | ❌ | Only loose facts; no `ProfileBuilder` / `UserProfile` / `GetProfile`. |
| Skill variant evolution / sandbox test | ❌ | Roadmap P3; not started. |

## Highest-Value Next Work (by priority)

These are the live gaps, ordered. They are derived from the table above, not from
the historical roadmaps.

1. **P0 — Extract run execution** from `httpapi/server.go` into a run coordinator /
   worker service. Unblocks a real parallel worker pool (currently gated by
   `Agent.runMu`).
2. **P0 — Decompose the CLI controller**: move `uiModel` state out of
   `controller.go` into components, per the AGENTS.md guardrail.
3. **P1 — Real `execute_code` sandbox** (namespace/seccomp/cgroup or container)
   before any untrusted multi-tenant code execution.
4. **P1 — Wire native IM approval buttons** (Telegram / Weixin); backend is ready.
5. **P2 — Feishu / QQ adapters** (WeChat Official Account outbound is done; the
   remaining IM gap is Feishu and QQ).
6. **P2 — User profile synthesis** (`ProfileBuilder`) — the "learns about you" gap.
7. **P2 — MCP `sampling/createMessage`** for servers that call back into the model.

## How To Keep This Accurate

- Treat this file as the index of "what is real." Update the affected row in the
  same PR that changes the behavior.
- Do not add per-feature status notes to the historical roadmap docs; record state
  here instead.
