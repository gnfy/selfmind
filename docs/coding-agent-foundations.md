# Coding Agent Foundations

SelfMind is an agent-first work assistant. Coding behavior must remain useful
across languages, repositories, providers, CLI sessions, and IM channels. It
must not depend on Go-specific or operations-specific keyword rules.

## Project Discovery

At the start of a run, SelfMind builds a bounded, read-only `ProjectProfile`
from the workspace root:

- It inspects the root project or at most eight immediate child projects.
- It recognizes declared manifests and lockfiles without executing repository
  code.
- It suggests only verification commands supported by detected evidence.
- It treats every suggested command as a candidate. The agent must still read
  the relevant manifest or project instructions before running it.

The initial profile supports Go, Rust, Swift, Node.js, Python, PHP, JVM,
Ruby, CMake, and .NET projects. Adding an ecosystem belongs in
`internal/kernel/project_profile.go`; it must not add routing rules to the
gateway or special cases to the main agent loop.

## Coding Loop Contract

For an ordinary coding request, the agent should:

1. Resolve the active workspace and obey applicable `AGENTS.md` instructions.
2. Inspect the smallest useful project surface before editing.
3. Use a plan only when the work is genuinely multi-step or uncertain.
4. Make scoped changes using the repository's existing conventions.
5. Run the narrowest relevant verification after the final change.
6. Treat tool errors as evidence, diagnose the environment, and adapt instead
   of repeating command variants blindly.
7. Report what changed, the verification evidence, and any remaining risk.

The runtime enforces the same recovery discipline below the prompt. Tool
attempts are correlated with the current durable plan step, target, strategy,
and environment. One changed-input correction may follow diagnostic evidence;
an identical retry, a third mutation-strategy variant, or a new mutation after
an unknown effect is refused before dispatch. Distinct proven read-only probes
remain available for diagnosis under the existing tool and resource budgets. The Agent must then observe current
state, select a genuinely different strategy, or finish with an actionable
blocker. A plan step sets `verification_required` only when executable evidence
is actually required by the user, repository instructions, or the nature of
the change; optional verification remains reportable without making direct
inspection tasks impossible to finish. Plan guidance is evidence-driven in the
same way: after a Run completes several distinct, successful, non-read-only
tool actions with no durable plan, its optional plan guidance is escalated to
the required wording for the next model step. That escalation reads the Run's
own tool evidence, never the request text, and it stays guidance — no plan is
fabricated and no completion is blocked. Plans update at meaningful progress or
scope changes; they do not require a transition around every tool call. Stable
step IDs preserve work-unit attribution without asking the model to repeat it.

Progress snapshots retain an existing step's acceptance condition and required
verification flag when omitted; cancellation remains explicit. A changed
completed criterion returns a bounded original/current comparison to Main.
Main decides whether a user scope change justifies it. Main also distinguishes
an explicit user takeover of remaining work from necessary unfinished work with
no handoff evidence. The former can conclude the agent's agreed scope; the
latter remains unfinished. Suggested next steps alone never establish takeover.
A missing final answer preserves an already known execution blocker.

`verify` accepts an optional version-1 `check` binding with a stable `criterion`
and `target`. To correct a check method, Main supplies `replaces` (the prior
verification evidence id) and a `reason`, keeping the condition and target
unchanged. The resolver requires an earlier check in the same open work unit
and working directory. Run and work-unit outcomes share one projection; the
original failed attempt remains in history. Structural checks cannot prove
semantic equivalence: Main must explain why the new method still proves the
original condition. Unbound historical checks gain no replacement authority.

New Runs opt into a versioned recovery contract. A daemon or provider
interruption with no external effect may enqueue one idempotent exact-parent
child Run. An uncertain dispatched effect instead enters verification-only
recovery: only trusted read-only tools are exposed and mutation is refused at
dispatch. Approval, clarification, and external-watch rows keep their
specialist owners, and historical Runs never gain automatic execution merely
because the binary was upgraded. Queued foreground user work has higher
priority than an automatic recovery that has not started.

Long external waits preserve the model's original goal, declared acceptance
criteria, remaining plan, corrections and exact-target evidence. The model owns
business criteria and the read-only observation method; the runtime owns
bounded execution, durable scheduling, approvals and recovery. A condition
match resumes work; it does not prove the whole task complete.

New observation receipts (version 2) require regex checks to select one bounded
scalar and match the whole value. Documents, logs and nested success markers
are rejected; the existing typed adapter remains available. The model chooses
field selection and state mapping through existing commands or an approved
observation script, without provider-specific logic in the gateway. A preflight
records the command hash, environment generation, target, deadline and effective
capabilities. Registration-time terminal observations remain durable members of
`all`/`any` groups. Handoff waits for every declared member; incomplete groups
that outlive their creating run or deadline report a check failure, never an
external failure. Aggregate resolution and continuation stay idempotent.

Version-2 continuations restore Main's ordinary tool and approval path in the
original execution scope. Main checks the evidence against the remaining
criteria, obtains missing evidence, performs authorized remaining work and uses
`verify` for required checks. It cannot silently lower criteria or replay
completed effects. Historical receipts keep their original matching and
file-only finalization policy; upgrading never grants them new authority.
Terminal shell syntax is checked before approval or execution, including before
any earlier command in a malformed compound statement can run.

Approval evidence distinguishes the user's words from daemon-generated
continuation instructions. New Runs record a versioned approval-intent snapshot
in their start event. Version 2 also freezes the proposal shown before a human
reply and carries up to six attributed human quotations through exact-parent,
same-workspace/root continuations. Later user requirements remain distinct.
The judge interprets accepted scope and effects, not an acceptance keyword list;
its reason identifies the relevant user evidence or missing execution permission.
System prose cannot invent a user deny or authorize an effect. Incomplete evidence
is labeled; missing or unsupported historical intent evidence requires
human confirmation before affected execution. This preserves current policy
without treating generic quality instructions as blanket operation bans.

Completion is evidence-based:

- `completed`: requested work is done and relevant verification passed, or no
  executable verification exists and that limitation is stated.
- `verification_partial`: implementation is complete but some verification
  could not be run or did not finish.
- `waiting_user`: progress requires approval, credentials, an unavailable
  dependency, or a user decision.
- `blocked`: a confirmed external blocker prevents meaningful progress.

The agent must not claim success solely because it wrote files or because a
tool returned output. File changes, command results, artifacts, and run events
are the evidence surface. Completion reads paged Run evidence independently of
transcript volume. File names in an answer remain prose: only observed file
changes become changed-file metadata and automatic file artifacts.

## Platform Boundary

Linux is the strongest execution target. The terminal tool can use the
configured isolated sandbox and upgrade through approval when required.

On macOS, SelfMind supports the CLI, daemon, provider adapters, workspace
tools, and LaunchAgent lifecycle. Isolated Linux sandboxing is unavailable:

- `sandbox.mode: auto` falls back to approval-controlled host execution.
- `sandbox.mode: isolated` or `sandbox.required: true` fails closed.
- `/diag` and `selfmind doctor` must make this boundary visible.

Native Windows is not an official execution target. Windows users should use
WSL. Path and shell behavior must be selected by the runtime platform rather
than inferred from user prompts.

## Operator Prompt Workspace

Deep users can refine static prompt layers without adding configuration keys.
The directory beside the active config file is the only source (normally
`~/.selfmind/prompts/`):

- `agent.md` owns foreground persona and presentation preferences. Repository
  `AGENTS.md`/`.selfmind.md` files remain lower-trust project context and are
  not a replacement for this operator layer. Replacing or disabling Progress
  Updates changes the transcript shape because each pre-tool preamble is a
  persisted assistant message. A locked response-and-interaction floor remains
  present on every primary foreground turn, including tool-free direct answers;
  it defines language following, outcome-first delivery, bounded clarification,
  terminal-oriented headings, flat lists, code/path formatting, no fixed report
  template, and no raw protocol dumps without asserting anything about the
  person's profession. The locked work-quality floor is capability neutral and
  remains present for foreground and delegated work even when no model-visible
  tool is available. Workspace implementation guidance is added only with a bound
  workspace and makes command verification conditional on an available command
  capability. Lifecycle instructions are derived from the actual tool
  definitions for the turn, so a read-only finalizer or bounded role is never
  instructed to call planning, terminal, Skill, or finalization tools it does
  not have.
- Delegated agents consume the same frozen snapshot only for applicable working
  style, verification, and semantically conditional user-facing interface
  guidance. No keyword classifier decides whether that guidance applies; the
  prompt states its applicability boundary and otherwise tells the model to
  ignore it. Delegated agents keep a dedicated
  parent-facing identity and do not inherit Persona, Progress Updates, or
  Persistent Learning. A delegation fork preserves parent cancellation,
  workspace/run authority, artifacts, and event evidence, but starts fresh
  strategy and deferred-tool state. Parent-owned plan, finalization, watch,
  memory, and Skill mutation tools are not delegated; results return as a
  structured evidence/files/tests/blockers handoff.
- `background/memory_extract.md`, `background/background_review.md`,
  `background/skill_curator.md`, `background/summarizer.md`, and
  `background/semantic_recall.md` refine only their named role/task. Background
  review does not inherit foreground `agent.md`, generic task strategy, or
  repository files. Its model-visible surface contains only memory and session
  evidence tools; verifier-only Skill readers remain internal. Conversation
  snapshots are fenced as untrusted data.
- Generated templates use exact, standalone `selfmind:section`/`selfmind:end`
  markers as the
  compatibility boundary, so Markdown headings inside a marked section remain
  ordinary custom content; marker examples inside fenced Markdown code remain
  content too. Markerless files from the first release remain supported with
  their fixed level-two headings reserved as boundaries. Opening one with
  `selfmind prompt edit` migrates it to marked grammar and keeps the original
  beside it as a timestamped backup.
  `default` inherits code-owned guidance; `off` is accepted only for explicitly
  replaceable presentation sections. Custom quality guidance is appended below
  locked schema, governance, tool, and safety contracts and cannot remove them.

The daemon loads and validates one immutable snapshot at startup; it never
reads prompt files during a turn. A valid active snapshot becomes the explicit
last-known-good revision for that exact prompt root. If the active workspace is
invalid, the daemon keeps CLI, IM, cron, and HTTP endpoints available on that
last-known-good snapshot, or on built-in defaults when no matching activation
exists. It emits a persistent, redacted degraded-status event and a log warning;
`selfmind doctor` reports the invalid file and selected source. Validation does
not become permissive: unsafe permissions or symlinks on the prompt root,
nested directories, or files, plus malformed markers and unknown sections, are
still rejected. `prompt validate` and `prompt apply`
remain strict, and an explicit `gateway restart` will not stop a healthy running
daemon while the active workspace is invalid.

Snapshot identity covers resolved section values, not comments or file
formatting. Durable maintenance payloads pin that hash and a content-addressed
local revision so a retry after restart does not change its model contract. The
daemon repairs a corrupt cache entry for the validated current snapshot and
best-effort persists a built-in fallback revision without promoting it to
last-known-good. Missing historical revisions pause affected work as
`blocked_prompt_revision` instead of being mistaken for a provider failure or
silently rebased. After restoring a historical revision, `selfmind maintenance
replay` explicitly returns the affected latest-generation work to the queue.
Prompt revisions contain static assets only, never task data, memory, project
context, or credentials.

Context compaction uses a locked summarizer system contract and a separate,
fenced data message. Its handoff preserves the active goal and plan/work unit,
verification status, failed attempts that should not be repeated, approval or
external-wait state, exact identifiers, and relevant file paths. A bounded
structured path backstop repairs model omissions and explicitly reports when
its safety limit is exceeded. Operator summary guidance can refine emphasis and
language but cannot replace those resume-critical sections.

Eval runs force embedded defaults and an empty identity regardless of the
operator workspace. Prompt loading, limits, placement, revision pinning, and
default byte stability are mechanical Go-test evidence; they do not rely on
provider replay cassettes.

## Regression Gate

Behavior changes to the coding loop require:

- focused Go tests at the owning package boundary;
- a coding eval for user-visible message-path changes;
- `go test ./...` on Linux;
- macOS build/test coverage in CI;
- npm package smoke tests for every published platform package.

Recorded eval cassettes are deterministic release evidence. Live provider
success alone is not a release gate.
