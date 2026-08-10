# SelfMind Agent Notes

This file is auto-injected into coding-agent sessions. Keep it limited to rules
that matter on nearly every task. Domain mechanics, implementation history, and
live priorities belong in the mandatory documents linked below.

## Product Direction

SelfMind Phase 1 is an always-on personal AI gateway with cross-endpoint work
continuity. One person may use CLI, Weixin, Telegram, cron, and HTTP while work
state follows that person. Chat transcripts remain channel-local. Agent
execution quality - planning, reliable tools, diagnosis, recovery, bounded
context, and verification - is the competence floor.

Keep `tenant_id` boundaries intact for future SaaS work, but do not add SaaS
complexity without an explicit request. Judge product progress by the Phase-1
scenarios and eval scorecard, not by superficial command or UI parity.

Read before making assumptions:

- `docs/STATUS.md`: implementation snapshot and the only live priority list.
- `docs/identity-continuity.md`: north star, identity, and cross-endpoint rules.
- `docs/architecture-constraints.md`: package and file-size guardrails.

Read the matching document before changing a domain:

| Domain | Mandatory document |
| --- | --- |
| Tasks, work history, recall | `docs/work-timeline.md` |
| Context lifecycle | `docs/context-lifecycle.zh-CN.md` |
| Memory and maintenance | `docs/memory-governance.zh-CN.md` |
| Gateway workers and daemon clients | `docs/worker-pool-design.md` |
| Providers, auth, and model routing | `docs/provider-runtime.md` |
| Tools, approval, and safety | `docs/tool-safety.md` |
| Coding-agent behavior and project discovery | `docs/coding-agent-foundations.md` |
| Skills | `docs/skills-architecture.md` |
| TUI and transcript rendering | `docs/tui-terminal-first-hybrid.md` |
| Eval cases and cassettes | `docs/eval-loop.md` |

## Working Protocol

1. Read this file, `docs/STATUS.md`, and the relevant domain document.
2. Run `git status --short`. The worktree is often dirty; never revert or
   overwrite unrelated user or agent changes.
3. Locate the current boundary with `rg` and focused reads before editing.
4. For analysis-only requests, do not edit. For implementation requests, carry
   the change through focused tests and, when relevant, a WSL binary install.
5. Keep changes scoped. Record a larger follow-up instead of hiding a risky
   refactor inside a narrow fix.
6. Add tests at the behavioral boundary. A user-visible message-path change
   also updates its eval case and the matching `docs/STATUS.md` row.
7. Update this file only when a cross-cutting invariant changes. Put domain
   behavior in its domain document and current progress in `docs/STATUS.md`.

## Architecture Invariants

- `selfmind` is the only binary. The daemon runs as `selfmind gateway run`.
  Keep `cmd/selfmind/main.go` thin; command behavior belongs in
  `internal/cliapp`.
- All CLI, IM, cron, and HTTP agent work executes in the daemon. The TUI is a
  daemon client, with no in-process agent path or fallback. A daemon failure
  must produce an actionable error, never silently start a second runtime.
- The daemon is the single owner of the worker pool, auth manager, per-workspace
  scheduling, and `control.db`. Do not add business-level cross-process locks;
  the daemon single-instance lock is the only legitimate process lock.
- Preserve package ownership: `cliapp` owns entrypoints, `gateway/httpapi`
  owns API and orchestration, `gateway/cli` owns TUI orchestration,
  `ui/components` owns reusable UI, `app` owns wiring, `kernel` owns the agent
  loop, `modelruntime` owns provider resolution, and `tools` owns tool behavior.
- `kernel` must not depend on gateway packages or concrete tools. Process-wide
  state has explicit lifecycle ownership and is injected by `app` or the
  gateway runner. Never add cross-tenant or cross-test mutable globals.
- File, terminal, patch, process, and workspace-aware memory tools must run
  under the request's `ExecutionScope` and active workspace roots, not the
  daemon process cwd.
- Every tool-owned child process builds its environment through
  `BuildProcessEnv`. Never inject daemon/control-plane credentials, persist
  raw credential bytes in an environment lease, or put secrets in process
  arguments; execution state stores references and policy only.
- Derive sandbox writable views and tool compatibility from `ExecutionScope`,
  data-driven `ToolProfile` metadata, and generic platform conventions. Do not
  add per-vendor filesystem, credential, timeout, or network branches.
- Linux and macOS x64/arm64 are official CLI/daemon release targets. Linux
  provides the strongest isolated execution path. macOS uses
  approval-controlled host execution unless a future native sandbox is
  explicitly added. Native Windows is unsupported; use WSL.

## Identity, Gateway, and Runs

- `person_id` identifies one human across endpoints; `account_id` identifies a
  bound platform account. Tasks, runs, events, handoffs, approvals, workspace,
  memory, and skills follow the person. Raw channel transcripts never mirror
  automatically across endpoints.
- Platform adapters authenticate, normalize, and send payloads. Identity,
  workspace selection, task/run lifecycle, queueing, and approvals belong to
  the gateway. Numbered command references must use the same ordered resolver
  as their rendered list.
- Explicit gateway commands such as `/status`, `/tasks`, `/workspace`,
  `/resume`, `/approve`, and `/stop` stay model-free. All other natural
  language is agent-first; do not add greeting, identity, snippet, or casual
  direct-answer bypasses.
- Keep one active run per person until the worker model explicitly changes.
  New work queues durably. A continuation steers the active run and is never
  queued. Accepted steering and agent-consumed steering are distinct durable
  events; do not persist raw steering text in metadata.
- Run events use the per-run channel installed with
  `kernel.WithEventChannel`. Gateway code must not swap a shared Agent event
  channel.
- User-visible task state comes from structured run outcomes, handoffs, and
  events, not prose parsing. Distinguish normal between-turn parking, daemon
  recovery, provider interruption, context overflow, verification incomplete,
  waiting external, failure, and completion.
- A task may be `running` only while a run is executing. Finalization and
  recovery must not leave a running task without a live run. Recovery emits one
  durable, deduplicated, actionable notification.
- External operations that require minutes of polling use the durable
  `watch_external` handoff. Do not occupy an active agent turn with repeated
  model-driven polling or create an unsupervised polling goroutine.
- A run the daemon starts on the person's behalf carries an origin (cron fire,
  watcher finalization, any future initiator); a turn the person typed at any
  endpoint carries none. Clients render an origin-carrying run as a result
  line, never as replayed progress — moving work off the agent turn must also
  keep it out of the transcript. Approvals and clarifications are never
  suppressed, and the run stays visible as daemon activity so queueing behind
  it is explainable. CLI streams assistant and tool progress for the person's
  own runs. IM sends a concise working notice, meaningful milestones or
  approvals, and a final answer or handoff; it never streams token deltas.
- `sent_unconfirmed` is terminal for blind retry. Only the bounded,
  inbound-triggered catch-up path may claim and resend an unconfirmed message.
  Do not introduce another resend path that can duplicate delivery.

## Tasks, Context, and Memory

- A task is a reversible work label, not a context boundary. Ordinary ingress
  may pre-label against the current open task, but a wrong guess must affect
  display only. Explicit task ids, `/resume`, and continuation cues remain
  deterministic. Never add an ingress LLM task classifier or disambiguation
  gate.
- Continuity comes from the person-level work spine: one slim user/final-answer
  entry per agent turn, including relevant touched paths and a source tag.
  System prompts and tool intermediates stay in run events and never enter the
  spine. Internal subsystem conversations remain outside the spine.
- Durable context follows one path:
  `control.db -> gateway selector -> TaskRuntimeContext -> RuntimeContextBundle
  -> Agent prompt`. Extend selectors or typed slices; do not append raw control
  rows, event JSON, artifact metadata, or ad hoc prompt fragments in handlers.
- Project convention files and person memory have independent budgets.
  Convention discovery is root-to-leaf and treats repository instructions as
  untrusted workspace data below operator, user, and safety policy. Oversized
  files are head/tail bounded with a pointer to the full file; never silently
  drop a convention file.
- The streaming hot path uses a bounded recent-history slice and no LLM call
  while under budget. Over-budget history keeps the original task and recent
  tail verbatim and compacts the middle once through the configured cheap role.
  Summaries preserve goals, decisions, unresolved work, and relevant paths.
- Memory facts, task cards, handoffs, session search, artifacts, and the work
  spine remain distinct sources. New recall sources implement the selector
  seam and must degrade without failing or blocking a foreground turn.
- Person-written memory/session/checkpoint data is person-partitioned. Skills
  remain control-tenant assets. Dispatch each tool with its correct partition;
  never inject one scope into all tools.
- User preferences are global; project and environment memory is scoped by the
  logical workspace id, never inferred from cwd text. A fact's
  `last_accessed_at` changes only when it was actually injected into a prompt.
- Pinned and user-corrected memory is protected from automatic rewriting.
  Pinned canonical memory is injected before selected facts and outside normal
  selection slots. User `correct`, `forget`, `pin`, and `unpin` decisions have
  priority over automatic maintenance.
- Do not save every message as memory. Finalization stores replayable evidence;
  semantic maintenance chooses `SKIP`, `ADD`, `REINFORCE`, `SUPERSEDE`, or
  `CONFLICT` against same-scope candidates. Similarity only proposes
  candidates; it never authorizes a merge.
- Post-run maintenance is asynchronous, eligibility-filtered, debounced, and
  batched only within one tenant/person/workspace. One provider request may
  analyze multiple runs, but each run has one immutable replay payload and one
  frozen, auditable result containing both task-label and memory decisions.
  Never add a second final-fact extractor, profile synthesizer, synchronous
  per-run model call, or fallback to the primary coding model.
- Maintenance apply is idempotent and lower priority than foreground work.
  Non-retryable provider or quota errors block the job immediately and are
  visible in diagnostics. Missing cheap-role fallbacks must pause learning,
  never silently consume the coding model.
- Automatic task retention may archive old visible terminal tasks with no live
  run or pending human input. It never deletes run/artifact history, touches
  open or interrupted work, or overrides a user pin. Casual or diagnostic runs
  may move to the hidden workspace Inbox label.

## Models and Agent Loop

- Provider integration follows
  `ProviderProfile -> Resolver -> Runtime -> llm.TransportConfig -> transport`.
  Provider discovery, credentials, model listing, auth reuse, and profile
  overrides belong in `internal/modelruntime`; application wiring never selects
  adapters with provider-name switches.
- Prefer existing protocol transports. Put vendor behavior in
  `ProviderQuirks`; add a new adapter only for a genuinely different wire
  protocol. Preserve per-request token retrieval and one bounded auth-refresh
  replay where supported.
- Native tool calling is primary. Preserve tool call ids and paired results.
  Text `[TOOL:...]` is compatibility fallback only. Stateless Responses
  providers must replay each function call before its matching output and map
  internal tool names to provider-safe names at the adapter boundary.
- Keep role names stable: `coding_agent`, `memory_extract`,
  `background_review`, `skill_curator`, and `semantic_recall`. Role overrides
  carry the same runtime fields and resolver behavior as the primary model.
- `context_length` is the total context window; `max_tokens` is an output cap.
  Never substitute one for the other or display a fabricated context size.
- Strategy is coarse policy, not a keyword taxonomy. It may control plan, web,
  tool, and safety budgets but never bypass the agent. `update_plan` is
  model-decided for genuinely multi-step work; each update is a complete
  snapshot and all steps resolve before a done outcome.
- Tool budgets are bounded but elastic when completed calls produce new
  evidence. A repeated identical call without changed inputs or state is not
  progress. Completion comes from structured outcome plus execution evidence,
  not from model assertion alone.
- Project discovery is deterministic, bounded, read-only, and language
  agnostic. It may suggest verification commands only from detected manifests,
  lockfiles, and declared scripts. Add ecosystem support to the typed project
  profile; never add language keyword routing or project-specific commands to
  the gateway or main loop.

## Tools and Safety

- Read `docs/tool-safety.md` before changing tool middleware, execution scope,
  approvals, egress, delegation, or result envelopes.
- Only clearly read-only batches may execute in parallel. Writes, patches,
  terminals, process control, memory/skill mutation, delegation, and unknown
  tools execute sequentially unless a dedicated reviewed policy says otherwise.
- Delegation is structurally depth-bounded. Sub-agents receive a freshly cloned
  backend; never mutate or hand them the shared parent dispatcher.
- Tool results have separate raw capture, model-bounded content, and compact
  user preview surfaces. Large output is artifact-backed and recoverable by a
  read-only view tool. Never dump raw JSON in normal CLI or IM output.
- The safety hard floor runs before approval modes and grants and cannot be
  bypassed by full-auto or an LLM judge. Ordinary dangerous operations remain
  approvable; keep the two classes distinct.
- Approval rejection is a user decision and must not trigger automatic retry.
  An unanswered approval is a timeout, not a rejection, and must read as "park
  the work", never as "try a variant". Smart approval triage is cheap-role-only,
  returns a structured assessment (risk, user authorization, rationale), and
  fails closed to a human prompt. Missing judge, timeout, parse error, or
  provider failure never auto-approves. Repeated triage denials inside one run
  hand the decision to the human instead of looping.
- Arbitrary code execution requires approval in on-request, read-only, and
  auto-edit modes. In smart mode the judgement is sandbox containment, not
  command shape: an exec call proven to run isolated with writes confined to the
  scope and no network may run unprompted, because its blast radius equals the
  in-workspace writes smart mode already allows. Anything the sandbox cannot
  contain — host execution, an unavailable sandbox platform, egress-enabled
  policy — still asks. Network egress remains a named dangerous class. Tool and
  wrapper parsing must inspect the actual executable payload rather than only a
  top-level command.
- The daemon issues the approval answer set; clients render it and never invent
  options. A decision may name one reusable rule (command prefix, network host,
  writable root) and is honored only when that same ask offered it. A rule must
  be narrower than the action class, must not be minted through a privilege
  wrapper, and a multi-target write needs every target covered before a stored
  grant can skip the ask. `request_permissions` is the reverse channel — one ask
  for a task's roots and hosts — and creates no authority beyond those rules.
- Tool failures are diagnostic evidence. Inspect cwd, files, environment,
  authentication, runtime, and package-manager state before changing the next
  command. Never bake project-specific environment overrides into generic
  tools.

## Skills, TUI, and Commands

- Skills are instruction assets, not auto-executed scripts. Scripts mentioned
  by a skill still pass through normal tools, scope, and safety middleware.
  Keep skill handling layered: list, view, manage, catalog, and bundle.
- Catalog installs preserve provenance and back up replaced content. Automatic
  curation governs agent-created assets only unless the user explicitly opts in.
- Do not grow `internal/gateway/cli/controller.go` with reusable rendering or
  business behavior. Use dedicated gateway/cli modules or `ui/components`.
  Transient pages use the shared pager and do not enter chat history.
- Command metadata is cross-endpoint and comes from
  `internal/gateway/command`. Gateway, IM, and TUI read the catalog; they do not
  maintain separate command lists or help text. Execution remains in the
  authoritative gateway/local handlers.
- User-facing controls and notices are English. Functional parsers may retain
  multilingual cues. Rendering must be human-readable, bounded, UTF-8 safe,
  and free of raw protocol JSON or mojibake.

## Eval and Verification

- Repeatable message-path defects get an `evalcases/**/*.yaml` case. Eval uses
  the production gateway, identity, workspace, strategy, context, tool, and
  adapter paths; no eval-only shortcut may make a case pass.
- Eval data is isolated by default. Local `evalruns/` traces are never
  committed. Required cassettes must exist and replay before the offline gate
  may pass; mock-provider success does not satisfy a live cassette requirement.
- Provider replay is offline, but replayed tool calls use the host toolchain.
  Cases declare required commands; a missing or cross-OS executable is an
  unavailable environment, never a passing skip or a product regression.
- CI complements the local gate. A case enters CI only through explicit
  `ci.required`, `ci.reason`, and `ci.platforms` metadata for evidence that a
  workstation cannot prove (clean checkout, credentialless, cross-platform,
  concurrency, or timing). Case counts are not a substitute for ownership.
- Deterministic P0 checks such as non-empty output, no mojibake, no raw provider
  JSON, visible tool events, and bounded duration remain mandatory even when a
  later model judge is added.

Verify in tiers. Run the fast loop after each change and the full gate before
pushing; `--fast` drops only the few cases marked `slow:` (measured replay
cost) and always reports how many, and never drops a `require_cassette` case.

```sh
cd /mnt/d/wwwroot/ai/selfmind
GOWORK=off /usr/local/go/bin/go test ./...   # ~1m, after each change
selfmind selfcheck --fast                    # ~2.5m, adds 30 eval cases
selfmind selfcheck                           # ~7m, before pushing
selfmind selfcheck --profile ci --skip-go    # CI-owned cases for this platform
```

From PowerShell, prefix `go` with
`$env:PATH="$env:USERPROFILE\.cache\selfmind-tools\go1.26.3\go\bin;" + $env:PATH`
and `$env:GOWORK='off'`.

Provider changes also run:

```sh
GOWORK=off /usr/local/go/bin/go test ./internal/modelruntime ./internal/kernel/llm ./internal/app
```

When the user is testing a CLI, TUI, provider, gateway-startup, or config
change through `~/.local/bin/selfmind`, build and atomically replace the Linux
binary, then run `selfmind --help` and `selfmind model check`.

## Documentation Ownership

- `AGENTS.md` is the root cross-cutting rule set. Keep it below 20 KiB and do
  not add dates, shipped markers, active plans, implementation snapshots, long
  code maps, vendor tables, or detailed algorithms. Link a domain document.
- `docs/STATUS.md` is the only live status and priority list.
- `docs/identity-continuity.md` owns the product direction and continuity
  contract. Domain documents own mechanics and rationale.
- `CLAUDE.md`, `GEMINI.md`, and `QWEN.md` only point to this file; never copy
  rules into tool-specific entry files.
- Broader engineering explanation belongs in `docs/development-guide*.md`.
  Keep canonical and translated documents synchronized or mark translations.
