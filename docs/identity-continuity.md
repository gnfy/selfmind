# Identity & Cross-Endpoint Continuity — the Phase-1 North Star

> **Read this before any other design doc.** This is the canonical statement of
> what SelfMind Phase 1 is for. Priorities live in `docs/STATUS.md`; rules live
> in `AGENTS.md` and `docs/architecture-constraints.md`. Historical planning
> docs were removed from the tree (2026-07-03) to keep agent analysis free of
> stale narratives; retrieve them from git history if needed.

## Why SelfMind exists

Every competing personal-agent stack (hermes-agent, OpenClaw, CowAgent) keys its
state per **platform account** or per **agent instance**. None of them can
recognize that the person typing in a CLI and the person messaging from WeChat
are the same human — in hermes this is architecturally unfixable without
re-keying pairing, sessions, memory, and cron (verified 2026-07: identity is a
`(platform, user_id)` tuple everywhere; CLI cannot even originate a cron job).

SelfMind's founding insight is the inverse: **the person is the primary key.**
One human (`person_id`) binds many platform accounts; chat stays channel-local,
but work state follows the person. This is the product. Everything else —
provider breadth, TUI polish, channel count — is supporting material.

## North star

> **A 24/7 personal assistant that recognizes you as *you* on every endpoint,
> and whose work follows you across them.**

Phase 1 ships this for one person (personal edition). SaaS/multi-tenant is
deferred: keep `tenant_id` plumbing intact so the architecture does not block
it, but do **not** design for SaaS now.

## Acceptance scenarios (Phase-1 definition of done)

All three must work end-to-end, be demonstrable in under a minute, and be
covered by eval cases in `evalcases/`:

1. **Cross-endpoint task lifecycle.** Start a long coding task from the CLI.
   Leave. Receive the approval request on WeChat (or Telegram), approve it
   there. Come back: the CLI shows the completed run; WeChat got the result
   notification. One task, one run history, visible from both ends.
2. **Cross-endpoint awareness.** Ask from WeChat "how is task X going?" and get
   an answer derived from the task/run/handoff state created by CLI work —
   without the CLI chat transcript leaking into WeChat.
3. **Stranger isolation.** An unbound account messaging the same WeChat bot
   gets no access to your tasks, memory, or workspaces. Binding an account is
   an explicit act (`/v1/accounts/bind`), and unbinding revokes it.

Scenario 1 must also survive the WeChat-platform risk: keep Telegram as the
fully working fallback demo channel for every scenario.

## Two bars: differentiation and competence

Continuity is why someone picks SelfMind; task-execution quality is why they
stay. A cross-endpoint approval on a badly planned run is worthless. Phase 1
therefore carries two bars:

- **Differentiation bar** — the three scenarios above.
- **Competence bar** — the agent must handle real coding/work tasks well
  enough for daily use: planning, reliable tool calling, error diagnosis and
  recovery, bounded context, and verification. This bar is judged by the
  day-in-the-life eval scorecard (`selfmind eval scorecard`), not by feature
  comparison against Codex.

Investment rule: when the scorecard says a scenario fails, execution-quality
work is always in scope. Surface-level Codex parity (TUI cosmetics, command
breadth, animation) is out of scope regardless.

## Identity model

Implemented in `internal/control/store.go` (`control.db`):

```text
tenant_id     SaaS tenant; always "default" in the personal edition.
person_id     The same human across CLI, IM, and Web.
account_id    A bound platform account: cli/local, weixin/<id>, telegram/<id>…
workspace_id  A project directory owned by a person.
task_id       Durable task state shared across channels.
run_id        One agent execution attempt.
event_id      Auditable task state transition.
```

Rules (also in `docs/architecture-constraints.md`):

- `person_id` = same human; `account_id` = platform binding. Adapters
  authenticate accounts; only the gateway resolves accounts to persons.
- Chats are never mirrored across channels. The shared layer is durable work
  state: tasks, runs, events, handoffs, approvals, notifications, memory,
  skills.
- Approval state lives in `control.approval_requests` and gateway handlers;
  IM adapters may render buttons/callbacks but never own approval lifecycle.
- The per-person active-run guard stays until the worker pool fully replaces it.

## Continuity contract (how "continue" works)

The mechanism agents must preserve when touching resume/continuation behavior.
Code: `internal/gateway/httpapi/continue_resolver.go`.

1. **Continuation detection.** Short acceptances (`ok`, `继续`, `可以`, …) are
   matched by `looksLikeAffirmativeContinuation`; richer cues come from
   `internal/gateway/router/intent*.go` (`IntentContinue`). Intent rules must
   stay high-confidence; ordinary messages go to the agent as-is.
2. **Task resolution** (`resolveContinueTask`, person-scoped, never
   channel-scoped): the person's `CurrentTask` if set; otherwise the most
   recent non-terminal task from the last 10 (`terminalTaskStatus`: done /
   completed / cancelled / failed); otherwise, if exactly one recent task
   exists, that one; otherwise no resume — the message routes to the agent as
   normal input.
3. **Resume context injection** (`withResumeContext`): the turn is prefixed
   with a bounded `[SelfMind resume context]` block — task id/title/status,
   latest handoff (summary, done, next_steps, changed_files, test_status,
   risks), up to 8 recent events, and the live plan with per-step status — and
   a `run.resumed` event is appended. This block is selected, bounded context;
   never dump raw control rows or full tool output into it.
4. **Cross-channel by construction.** Because resolution keys on
   `person_id`, a task started from CLI resumes from WeChat and vice versa.
   The channel only affects where the reply is delivered
   (`task.LastChannel`) and the feedback style (stream vs. concise notices).

Account binding: `POST /v1/accounts/bind` attaches a platform account to an
existing person. `/id` shows the current tenant/person/account resolution.

## What this rules out

- No pre-agent direct-answer routers; agent-first stays (see `AGENTS.md`).
- No new IM channels, providers, or TUI features unless they serve the three
  scenarios or fix a defect on their path.
- No SaaS-motivated design work; `tenant_id` stays plumbed, nothing more.
- No cross-channel chat mirroring, ever — continuity is state, not transcript.

## Known gaps on the north-star path

Tracked with priorities in `docs/STATUS.md` ("Highest-Value Next Work"):
client-mode mid-turn steering loss, IM-native approval buttons, continuity
eval cassettes, session-search client parity, stranger-isolation hardening
(QQ webhook signature, Feishu envelope decryption).
