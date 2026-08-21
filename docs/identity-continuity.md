# Identity & Cross-Endpoint Continuity — the Phase-1 North Star

> **Read this before any other design doc.** This is the canonical statement of
> what SelfMind Phase 1 is for. Priorities live in `docs/STATUS.md`; rules live
> in `AGENTS.md` and `docs/architecture-constraints.md`. Historical planning
> docs were removed from the tree (2026-07-03) to keep agent analysis free of
> stale narratives; retrieve them from git history if needed.

## Why SelfMind exists

Mainstream personal-agent stacks key their state per **platform account** or
per **agent instance**: pairing and allowlists are `(platform, user_id)`
tuples, sessions are keyed by channel + chat, and memory is scoped to an
agent's workspace or a per-platform user bucket. Under that data model, the
person typing in a CLI and the person messaging from WeChat are two unrelated
principals — often the CLI is not even a first-class identity (it cannot
originate scheduled jobs or share a memory bucket with an IM account). Fixing
this retroactively means re-keying pairing, sessions, memory, and scheduled
jobs at once — an architectural rewrite, not a feature. (Verified against
several widely used open-source agent stacks, 2026-07.)

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
  comparison against other coding agents.

Investment rule: when the scorecard says a scenario fails, execution-quality
work is always in scope. Surface-level parity with mainstream coding CLIs
(TUI cosmetics, command breadth, animation) is out of scope regardless.

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

> **Landed (2026-07-06, Work Timeline P1–P3):** context ownership lives on the
> person-level work spine; `task` is a work label and ingress attach is a
> harmless pre-label guess corrected by a post-run labeler.
> `docs/work-timeline.md` is the canonical description (mandatory reading
> before changing this contract); the rules below are the live behavior.

1. **Continuation detection.** Short acceptances (`ok`, `继续`, `可以`, …) are
   matched by `looksLikeAffirmativeContinuation`; richer cues come from
   `internal/gateway/router/intent*.go` (`IntentContinue`). Intent rules must
   stay high-confidence; ordinary messages go to the agent as-is. The implicit
   pre-agent "continue vs new" LLM upgrade (`intent.continue_window`,
   `router.UpgradeTaskToContinueWithLLM`) was REMOVED with Work Timeline P3:
   working context is the person-level spine, so an implicit follow-up
   ("质量太差了") keeps its context regardless of which task label the run gets
   — the call bought nothing, and its failure mode (a wrong pre-agent routing
   decision) was worse than its absence. `intent.continue_window` in existing
   config files is ignored. Do not reintroduce ingress continuation
   classifiers; see `docs/work-timeline.md` "Ingress (simplified)".
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
5. **Ordinary messages get a harmless pre-label guess** (Work Timeline P3,
   2026-07-06 — supersedes the 2026-07-05 "new work never attaches" rule). A
   message with no continuation evidence — no cue, no short acceptance, no
   explicit task id, no pin — runs under the person's current OPEN
   (non-terminal, non-archived) task label, else a fresh placeholder; async
   dispatches, queued-task drains, and cron turns follow the same rule. The
   guess is safe because labels never gate context (the spine does) and the
   EXECUTION workspace follows the REQUEST for pre-label turns — the harm of
   the old capture bug (wrong workspace, wrong context) is structurally gone.
   A cheap post-run labeler re-points a wrong guess (KEEP/MOVE/TITLE,
   `label.assigned` audit event); mislabels are display bugs. An explicit
   `/resume <task_id>` is continuation evidence for exactly the NEXT
   agent-bound message (a one-shot pin, consumed on use) and is the only way
   to reopen an ARCHIVED label (`/task <id> archive` shelves one)
   (`internal/gateway/httpapi` `resolveTask`, `run_labeler.go`).

Account binding: `POST /v1/accounts/bind` attaches a platform account to an
existing person. `/id` shows the current tenant/person/account resolution.

## Runtime attachment model (endpoints as watchers)

Owner decision, 2026-07-04. This is the runtime form of the continuity
contract; it extends the identity model above and is the design baseline for
the G-series work in `docs/STATUS.md`.

**Runs belong to the gateway; every endpoint — CLI included — is a peer
watcher.** The CLI is an `account` exactly like a bound IM account. A run's
lifetime is never tied to any endpoint's connection: closing a terminal
detaches a watcher, it must not cancel or interrupt the run (only heartbeat
death — a real crash — interrupts).

Routing has two independent layers (owner decision, 2026-07-04):

**Conversation layer — replies follow the ORIGIN endpoint, never broadcast.**
Results, approval prompts, and questions are conversation messages; they
belong to the surface where the task was dispatched (this includes scheduled
jobs, which deliver to the channel that created them). Priority:

1. The task explicitly names a reply endpoint ("结果发到 Telegram") — honored
   only if it is one of the person's OWN bound accounts (never an arbitrary
   target; this bound check is a security boundary, not a convenience).
2. Origin is an IM endpoint → reply to that endpoint (conversational
   coherence; a WeChat-dispatched task answers in that WeChat chat).
3. Origin is the CLI and it is attached → CLI.
4. Origin is the CLI and it is detached → the person's single **preferred
   notify endpoint** (default: most recently active IM account; configurable).
   One target, never a fan-out to every bound IM.

Approval requests refine rules 3/4 with a person setting. `desk-first` (the
default) shows a young request only in an attached CLI, escalates it to the
preferred IM after 15 minutes, and pushes immediately as soon as the CLI
detaches. `phone-first` mirrors CLI-origin approvals to that IM immediately.
The destination and surface policy are independent `/notify` settings. If an
approval was pushed to one endpoint and answered on another, the pushed route
receives one idempotent resolution message so it never keeps presenting a
question that is no longer true.

**Observation layer — live watching follows PRESENCE.** Any attached
interactive endpoint may hook a mid-flight run's live event stream and
steering, without changing where the conversation reply goes (watching a
WeChat-dispatched task from the CLI does not move its answer). Presence is
derived from the client process/event-stream heartbeat, never from the age of
the last keyboard input and never persisted as authority. A terminal crash is
an implicit detach identical to a graceful close.

**Delivery truth — accepted is not delivered.** A message persisted for an IM
sender may still be `pending_session` or `sent_unconfirmed`; neither state may
mark the source notification delivered. Only confirmed transport delivery does
so. Pending/unconfirmed rows remain eligible for the bounded, idempotent
session-aware catch-up path; `sent_unconfirmed` is never blindly replayed by the
approval escrow timer because the handset may already have received it.
Diagnostics aggregate this health per platform without exposing peer/channel
identifiers.

Final results that remain `pending_session` beyond the bounded automatic
catch-up window are never replayed one by one. In that exact IM peer,
`/diag delivery recover stale-results` builds one bounded, idempotent recap
grouped by durable task/run identity. Only a transport-confirmed recap dismisses
the exact rows it summarized; an unavailable or uncertain session leaves them
pending. `/diag delivery dismiss stale-results` is the explicit no-send option.
Neither operation touches approvals, clarifications, another peer, or
`sent_unconfirmed` rows.

Re-attaching (next morning, back home on the same machine → same `cli/local`
account → same person) flips rule 3/4 back and shows an **attach digest**:
what finished/failed while away, pending approvals/questions, queued tasks —
one person-scoped query. Pending approvals reconstruct the same server-issued
interactive menu, including the exact reusable rule text and whether the
original run is parked.

Approval lifetime is independent from run lifetime. A resource timeout or
daemon restart parks an unanswered approval without rejecting it; answering a
parked request starts a task-pinned continuation. A decision committed just
before a daemon crash is recovered into the same continuation path. Parked
requests are archived after seven days to bound retained interactive state. If
there is no live endpoint and the preferred IM route is currently
`pending_session`, `failed`, or `sent_unconfirmed`, the default unattended
resource wait is 30 seconds rather than 30 minutes. This releases the run slot;
it does not shorten the seven-day answer lifetime. A live CLI still selects the
full wait regardless of the latest IM delivery state.

Lifecycle: `CLI attach → work → detach (close/handoff) → IM takeover → …
→ CLI re-attach (digest + take back over)`. Crash of a terminal is an
implicit detach (heartbeat expiry), identical to a graceful close.

Quitting with a run active is an explicit choice, not an accident: the TUI
prompts — `b` quit and leave it running in the background (result pushed to
the bound IM), `c` cancel the task and stay, `esc` keep watching; a second
ctrl+c means background+quit. The prompt doubles as the moment users learn
the detached-run design (owner decision, 2026-07-04).

Derived requirements (tracked in `docs/STATUS.md`): detached run execution
(decouple runs from HTTP request contexts), presence registry + routing,
attach digest, DB-backed clarify (a pending question must be answerable from
IM after the CLI is gone — the approvals mechanism is the template), and the
conversational IM routing stack (answer-pending > continuation cue > new task,
queue instead of "busy").

## What this rules out

- No pre-agent direct-answer routers; agent-first stays (see `AGENTS.md`).
- No new IM channels, providers, or TUI features unless they serve the three
  scenarios or fix a defect on their path.
- No SaaS-motivated design work; `tenant_id` stays plumbed, nothing more.
- No cross-channel chat mirroring, ever — continuity is state, not transcript.

## Known gaps on the north-star path

Tracked with priorities in `docs/STATUS.md` ("Highest-Value Next Work"):
account visibility (`GET /v1/accounts`), stuck-run recovery, session-search
client parity, stranger-isolation hardening (QQ webhook signature, Feishu
envelope decryption). IM-native approval buttons and approval-reference UX
shipped 2026-07-04 (see the Approval lifecycle row in `docs/STATUS.md`).
