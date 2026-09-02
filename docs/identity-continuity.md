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
Code: `internal/gateway/httpapi/continuity_resolver.go`,
`turn_choices.go`, and `continue_resolver.go`.

> **Landed (2026-07-06, Work Timeline P1–P3; revised 2026-08-31):**
> context ownership lives on the person-level work spine plus the parent-run
> slice; `task` is a work label, every root run owns a fresh one, and
> `child.parent_run_id` is the only continuation authority.
> `docs/work-timeline.md` is the canonical description (mandatory reading
> before changing this contract); the rules below are the live behavior.

1. **Deterministic controls stay first.** Structured return edges, explicit
   task/run IDs, `/resume`, `/new --run`, a claimed `/choose`, and exact
   standalone continuation controls never call an ingress model. Approval and
   clarification answers retain priority over a bare numbered continuity
   choice. Daemon-originated work never uses text to steer another run.
2. **Structured return edges outrank cues.** A daemon-originated approval
   continuation binds the parked approval's origin run; platform reply
   metadata (`reply_to_run_id`, durable across the queue) binds the exact run
   it answers; a clarify answer lands structurally (one pending) or via a
   deterministic numbered pick (several pending). If a restart parked the
   asking run, the answer and its durable `clarify_id` queue edge commit in one
   transaction and the child claims that exact origin; terminal or already
   claimed origins are expired instead of falling back to another question.
3. **Natural-language run resolution is Main-owned and person-scoped.** While a
   Run is active, user-originated CLI or IM language is persisted as steer and
   acknowledged with current status before the same Main receives it at a safe
   checkpoint. Main may apply related guidance to current work, queue an
   independent request, or queue an exact historical continuation without
   changing the active execution domain. While idle, ordinary language first
   starts one normal audited Main Run. Main can search complete retained
   structured history with `work_search`, inspect an exact Run with
   `work_inspect`, and record an advisory OBSERVE/RESUME relationship with
   `work_select`. The gateway re-reads person ownership, resumability, scope,
   and effect boundaries before committing it. OBSERVE/reference interactions
   remain auditable but are projected out of ordinary task lists. A validated
   same-domain RESUME atomically claims the exact parent for the interaction
   Run; a different execution domain or checkpoint requirement transfers to a
   correctly scoped exact-parent child. One pre-effect correction is retained
   as an audit edge, while post-effect correction stops and asks. `semantic_recall`
   is optional search enrichment and may fail without blocking the turn;
   `fast_classifier` is not consulted. Exact IDs, structured reply edges, and
   standalone controls remain model-free.
4. **Resume context injection** (`withResumeContext`): the turn is prefixed
   with a bounded `[SelfMind resume context]` block selected from the PARENT
   RUN — its finalization handoff, its events, its file manifest, its plan —
   and a `run.resumed` event records the claimed edge. No exact parent means
   no resume block and no full task context (the selector downgrades to the
   bounded task card; loop-checkpoint restore is parent-gated too).
5. **Cross-channel and restart-safe by construction.** Candidate retrieval and
   pending choices key on `person_id`, not an open CLI process or current
   channel. A CLI-created task can be observed or continued from a bound IM,
   after closing/reopening the CLI, and after daemon restart. Raw transcripts
   remain local. A bare number claims a choice only when one person-wide choice
   was created within 30 minutes; `/choose <choice_id> <number>` or platform
   choice metadata remains exact for 24 hours.
6. **Messages resolved as NEW own a fresh root task** (simplification P2,
   2026-08-31 — supersedes the 2026-07-06 pre-label guess). A message with no
   prior-work match, structured edge, explicit task id, or pin creates its own
   task; async dispatches,
   queued-task drains, and cron turns follow the same rule, and
   daemon-originated text never steers the active run via cues. Grouping is
   display-only (labels never gate context), the default `/tasks` view ranks
   by derived display priority, and there is no post-run relabeling. An
   explicit `/resume <task_id>` is continuation evidence for exactly the NEXT
   agent-bound message (a one-shot pin, consumed on use) and is the only way
   to reopen an ARCHIVED label (`/task <id> archive` shelves one)
   (`internal/gateway/httpapi` `resolveTask`).

### Main-turn continuity implementation

`docs/plans/main-turn-work-continuity.md` owns rule 3's implementation and
operational evidence. Durable active steer, one idle Main turn, progressive
work-history tools, OBSERVE projection, validated continuation commits, and
explicit delivery override are the current default:

- user-originated natural language durably steers an active Run on both CLI and
  IM; the same Main decides whether to update current work or queue independent
  work at a safe checkpoint;
- idle natural language first creates an ordinary audited Main Run with a
  bounded work-spine tail and structured hints, then uses person-scoped
  `work_search` and `work_inspect` when more history is needed;
- local structured/FTS search is the reliable base, while `semantic_recall` is
  optional query expansion and cannot block or authorize a continuation;
- continuation atomically claims a same-domain interaction Run, while a scope
  or checkpoint mismatch creates a correctly scoped exact-parent child;
- active progress questions receive an immediate deterministic status at the
  asking endpoint, while the final result remains at the origin unless the user
  explicitly selects another bound endpoint through a server-issued steer.

No adapter may infer these decisions independently of the gateway.

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
original run is parked. Away events are anchored on immutable terminal-run
completion time, never the mutable task-card update time: post-run labeling or
maintenance cannot make an old interruption look newly failed. Older durable
blockers remain visible as current state under **Still needs attention**, not
under **While you were away**.

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
