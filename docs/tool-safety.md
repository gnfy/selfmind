# Tool Execution and Safety Contract

This document is the detailed source of truth for tool execution, workspace
scope, approval, network egress, delegation, and result presentation. Read it
before changing `internal/tools`, execution middleware, or kernel tool dispatch.

## Execution Scope

- File, terminal, patch, process, and workspace-aware memory tools execute
  inside the request's `ExecutionScope`. Scope comes from the active logical
  workspace and allowed roots, not the daemon process cwd.
- Scope checks must resolve path semantics safely, including wrapper commands
  and symlink-sensitive operations. A workspace switch must affect subsequent
  tool execution without restarting the daemon.
- `vision_analyze`'s local-path branch is a filesystem read and obeys the
  scope like `read_file` (`WorkspaceScopeMiddleware`); its http(s) branch
  stays with the tool's SSRF check. Any new tool that reads a caller-supplied
  path must join the middleware's scoped-tool list, never `os.ReadFile` raw.
- Message attachments (e.g. TUI clipboard-pasted images) enter a run only via
  the gateway import channel (`httpapi/attachments.go`): files are copied into
  the person-partitioned `<data>/attachments/<person>/<run>/` store and that
  person's partition joins the run's `AllowedRoots`. Arbitrary out-of-scope
  client paths (OS temp dirs, other users' files) stay unreadable; do not
  widen scope to accommodate an attachment path directly.
- Tools that do not need filesystem or process access must not receive broader
  scope as a convenience.

## Execution Environment

An execution environment belongs to a run, not to the channel that submitted
the message. CLI, IM, cron, and HTTP may continue the same work without
silently changing its filesystem, toolchain, credentials, or network view.
Future remote runners must preserve this rule through a durable
`EnvironmentLease`.

The lease stores policy and references only:

- workspace and environment profile identity;
- capability decisions such as shared network or host escape;
- `CredentialRef` values that identify a credential source and principal;
- a principal/source fingerprint that survives ordinary token rotation.

Raw credential bytes must never be persisted in a lease, run event, approval
grant, artifact, diagnostic response, or task metadata. A missing environment
parks work as `waiting_user` with structured reason
`environment_unavailable`; installing a compatible environment may create a
new run and lease through the existing resume path. Do not add a new top-level
task state for this condition.

### Child process environment

Every tool-owned child process, including terminal, verification, code
execution, and stdio MCP servers, constructs `cmd.Env` through
`BuildProcessEnv`. Direct `os.Environ()` inheritance at an execution callsite
is forbidden.

The current compatibility phase preserves the operator's normal toolchain and
Agent CLI login environment while removing SelfMind control-plane identity,
gateway addresses, tokens, tenant/person/task ids, and channel state. This
lets existing CLIs keep using their normal login mechanisms without exposing
the daemon's own authority. Later credential snapshots may narrow this set,
but must do so through data-driven profiles rather than per-vendor branches.

Bubblewrap receives the constructed environment through `cmd.Env`. Never put a
credential into bwrap arguments with `--setenv`: process arguments are visible
through process listings and `/proc/*/cmdline`.

### Secret redaction

One runtime `SecretRegistry` feeds the existing `RedactSensitive` boundary.
The same redactor protects new durable writes and logical read-time views such
as model context, IM, artifacts, diagnostics, and logs. Do not build a second
redaction implementation or physically rewrite historical audit evidence.

At present the registry knows daemon-configured secrets and stripped
control-plane values, plus shape-based token patterns. Operator credential
values become registrable when an execution snapshot is materialized. Values
remain registered for the process lifetime so rotation cannot reveal an older
secret in historical output. FTS may still reveal that a matching row exists
even when content is masked; treat that as a documented existence leak until
indexes support redacted projections.

Exact identifier-like values (letters, digits, `_`, or `-`) are masked only at
identifier boundaries. This prevents a short registered value such as a tool
or account name from corrupting a longer resource name that merely contains
the same characters. Structured secrets containing punctuation still use
exact-value replacement. Shape-based token rules remain the final fallback.

### Workspace trust and capabilities

Workspace trust is enforced as a durable owner-controlled boundary:

- only an authenticated local CLI may grant or revoke workspace trust; IM,
  cron, and remote HTTP cannot. Use `selfmind ws trust [id]` and
  `selfmind ws untrust [id]`;
- workspaces created by older versions are migrated to untrusted with
  `migration_review_required`; newly discovered or remote workspaces also
  start untrusted. `selfmind doctor` lists migrated paths that need review;
- untrusted workspaces default to isolated filesystem access and no network,
  but `network:shared` is an approvable, workspace-scoped, time-bounded
  capability rather than forcing users to mark the whole workspace trusted.
  A task grant lasts one hour and a person grant lasts eight hours. Marking a
  workspace untrusted revokes its active execution capabilities;
- operator credential state is reached through `credential:read`, which is
  approvable, workspace-scoped and time-bounded on the same terms as
  `network:shared`. It is asked for BEFORE execution and only when the command's
  program set matches a profile declaring operator credentials, so a trusted
  workspace and a command that touches no credential CLI are never interrupted.
  Declining leaves the command runnable with an empty state overlay;
- access is never granted to all of `~/.config`. A profile names the files and
  subdirectories it needs, bounded by `copy_in` limits, and the operator's own
  files are never modified.

Private package installation in an untrusted workspace requires a shared-network
capability, and credential-backed registries additionally require
`credential:read`. Do not bypass either boundary with a vendor-specific
exception.

Trusted workspaces may register a local observation script with
`selfmind ws observe`. This is deliberately narrower than trusting an
interpreter or shell command family: the durable approval key binds the
workspace ID, canonical in-workspace path, script content hash, argument
prefix/trailing policy, network mode, and credential mode. Symlinks resolving
outside the workspace are rejected, changed scripts stop matching, and only an
authenticated loopback CLI can create the grant. The runtime still uses the
normal execution scope, sandbox, environment lease, network, credential, and
secret-redaction layers. Treat `--all-args` as an explicit owner assertion that
the unchanged script cannot turn arbitrary arguments into mutation.

## Dispatch and Delegation

- Native tool calls preserve their provider call id through the result. The
  text tool protocol is a compatibility fallback only.
- Clearly read-only calls may run in parallel. Terminal execution, writes,
  patches, process control, memory or skill mutation, delegation, and unknown
  tools run sequentially by default.
- Delegation depth is enforced structurally. `buildDelegateSubBackend` creates
  a fresh child dispatcher and removes `delegate_task` at the configured depth
  limit. Never expose or mutate the shared parent dispatcher. Fan-out remains
  bounded by `max_subtasks` and `max_concurrent`.

### External MCP tools

MCP tools are external code and fail closed when the server has not supplied a
reviewed SelfMind policy classification. The registry injects the actual schema
origin after model-argument validation; approval must use that origin rather
than infer trust from a provider-visible name. An unclassified MCP call is
sequential and requires a once-only human decision in every approval mode,
including `full-auto`. It cannot use a stored grant or smart-model approval.
The hard safety floor still runs first.

Only provider-visible input arguments cross the MCP boundary. Dispatcher scope,
callbacks, registries, storage handles, and every other top-level
underscore-prefixed argument remain inside SelfMind. An external schema that
declares a top-level underscore parameter is quarantined because it would make
that boundary ambiguous. Nested business fields are preserved.

MCP server and tool names are normalized for provider compatibility and carry
a stable identity suffix so truncation or punctuation normalization cannot make
one server overwrite another. Registry collisions are rejected, never
last-writer-wins. Current connection and catalogue failures are exposed through
gateway status and `selfmind doctor`; they are not recoverable only from logs.

## Result Envelope and Artifacts

One tool result has three distinct surfaces:

1. bounded raw capture for diagnostics and artifact persistence;
2. model-facing content with explicit truncation and recovery instructions;
3. a concise, UTF-8-safe user preview for CLI, IM, and run events.

Do not use one truncated string for all three surfaces. Large output is spooled
through the per-run artifact sink and referenced by an artifact id. The model
uses the read-only `tool_output_view` tool to retrieve omitted ranges instead
of repeating the original command. Spooling failure degrades to a bounded
result note and must not turn a successful tool call into a failure. Aged tool
results may shrink only when an addressable artifact retains the evidence.

Failed tools preserve a separate bounded diagnostic excerpt, byte count, and
content hash in the durable event payload. This evidence is independent of the
short user preview, so a failure can be classified and reviewed without
persisting an unbounded stream. Exit status and known interface-drift failures
are recorded as structured fields where they can be derived deterministically.

Normal UI output summarizes tools, plans, and failures. Raw JSON is reserved
for an explicit protocol/debug request.

## Approval Layers

Approval evaluation is ordered. A lower layer cannot override a higher one:

1. unbypassable hard safety floor;
2. explicit approval mode and enforced execution containment;
3. matching run/task/person authorization;
4. optional smart-mode cheap-model triage;
5. human approval prompt.

An approval rejection or cancellation is a user decision, not a transient tool
failure. Kernel rejection detection and middleware error strings are a shared
contract: rejection tells the model not to repeat the operation. A hard safety
block is a policy decision and remains distinguishable from user rejection.

Approval memory uses bounded daemon-derived rules (for example a two-token
command prefix, one network host, or one write root), not opaque client-defined
classes or raw exact command strings. The proposed rule text travels alongside
its opaque key and is shown verbatim in every approval surface. Hard-floor
denials and content-level denials are never grantable. Numbered approval
references resolve in the gateway with the same order used by every client.

Containment is assessed on three independent axes: filesystem
(`isolated|host|unknown`), network (`none|shared`), and credentials
(`none|selected`). On Linux, an enforceable isolated filesystem with no network
or credentials may release an ordinary exec call in smart mode. When shared
network or selected credentials are present, only operations in the declarative
observation catalog may be released. Unknown programs, arbitrary scripts,
`execute_code`, secret reads, mutations, and unparseable command forms remain
approval-gated. Extending the catalog is a reviewed data change with focused
tests; it must not become a permissive shell heuristic.

An exec payload that is too opaque to mint a safe class authorization may offer
an exact-run decision. Its key hashes the raw action plus run, workspace,
environment, and containment metadata; raw code is never persisted. The grant
exists only in memory for that run, releases only a byte-identical repeat, and
cannot be promoted to task or person scope. `/diag` reports containment,
class/rule grant hits, exact-run hits, judge outcomes, and human asks as funnel
events rather than pretending they are unique operation counts.

Host execution, credential reads, explicit-deny overrides, and high-risk or
unclassified requests are once-only decisions. They never create a reusable
grant from the interactive prompt. Ordinary bounded operations may offer one
run-local reuse choice; the exact server-issued choice is shared by CLI and IM.
Historical task/person grants remain readable until they expire and can be
inspected or revoked with the approval and workspace grant commands, but new
interactive prompts do not mint grants at those scopes.

The model-visible `sandbox: auto|isolated|host` arguments are a compatibility
surface. Internally they may map to capability requests such as
`network:shared`, `credential:read`, or `host:escape`, but do not change the
tool schema and invalidate eval cassettes in the same patch. Any future
`with_additional_permissions` request carries a justification and the concrete
capability; it never grants an unspecified "more access".

## Hard Safety Floor

`hardlineToolCall` executes before every bypass, including full-auto, grants,
and model triage. Keep this set narrow and limited to operations that should
never be legitimate, including destructive operations against filesystem or
protected roots, filesystem formatting, raw-device overwrite, fork bombs, and
host shutdown/reboot.

The classifier inspects the executable payload used by terminal, shell, and
code-execution tools. It unwraps a bounded number of shell and common command
wrappers so an inner destructive command cannot hide behind `shell -c`,
privilege wrappers, environment wrappers, timeout helpers, or similar launchers.
An unparseable wrapped command degrades to ordinary dangerous approval rather
than being falsely hard-blocked.

Do not promote an ordinary dangerous operation into the hard floor without an
explicit product decision and focused tests.

## Arbitrary Code and Network Egress

- Arbitrary terminal or code execution asks for approval in on-request and
  smart modes, independent of a narrow dangerous-command heuristic. Full-auto
  may bypass ordinary approval but never the hard floor.
- Network egress is a first-class dangerous category. Detect common transfer,
  remote-shell, socket, and `/dev/tcp` or `/dev/udp` forms after wrapper
  unwrapping. Keep the classifier named and independently tested.
- Network egress is approvable, not hardline. Do not silently change full-auto
  semantics. Git traffic to configured remotes remains excluded to avoid
  routine approval fatigue unless the policy is explicitly revised.

## Smart Approval Triage

Smart-mode triage is below the hard floor and class grants and above the human
prompt. It uses a configured cheap role, not the run's coding model, and may
return APPROVE, DENY, or ESCALATE.

The judge receives a typed `RunIntentSnapshot`, not one blended prose intent:
raw user text is authoritative; deterministic allow/deny evidence and
control-plane workspace/source/work-key facts are separate fields; the task
summary is advisory context only. An explicit deny disables containment-based
auto-approval and forces a human decision even in full-auto or when a durable
grant exists.

A deny constrains the operation it names, not every side-effecting tool in the
run. Prohibitions are extracted deterministically (no model call), bound to the
clause they appear in, and resolved to operation classes — write, delete,
exec.in_turn, exec.delegated, network — plus any literal path or command
fragment the clause names. A pending call is compared in that vocabulary, so
"do not modify files" no longer stops a read-only probe, and a prohibition
qualified as directly/yourself/manually resolves to `exec.in_turn` and leaves a
durable delegation alone. An unqualified execution ban still covers both
shapes. Narrowing applies only to what can be read: a prohibition that cannot
be classified keeps the blanket effect, and the dangerous-op heuristic alone
never activates an unrelated deny. Prohibitions that do not match still reach
the judge as the person's stated limits — they simply no longer force the ask
by themselves. The hard floor remains unconditional, so this snapshot cannot
grant an otherwise forbidden capability.

- APPROVE from smart triage may cache its deterministic decision for the task.
  A human ask is narrower: it approves once, or records one explicitly offered
  run-local rule. Host escape, credential access, explicit-deny overrides, high
  risk, and unavailable triage never create remembered authority.
- DENY uses the user-rejection contract and must not trigger retry.
- ESCALATE asks the human.
- Missing judge, timeout, provider failure, malformed output, or an unknown
  decision always escalates. Triage never fails open.

The human ask itself is bounded, and the bound is the SMALLER of the configured
budget and whatever the caller's own deadline leaves (`agent.approval_wait`,
`agent.approval_wait_unattended`). The waiter must return its typed decision
before the caller's context expires: the timeout path parks the work as
`waiting_user`. This is a resource deadline, not an answer deadline: the row
remains `pending/parked`, releases the run slot, and stays answerable for seven
days. Answering it atomically records the decision and enqueues one task-pinned
continuation. An approval supplies a one-shot capability only for the same
regenerated action and stable workspace/environment/principal/credential
fingerprints; the safety floor and a current explicit deny still run first.

Reachability is not inferred from keyboard silence or a 24-hour account-age
window. A live process heartbeat counts as attended even when the IM route's
latest delivery is unhealthy. Without a live process, no routable IM account or
a latest IM state of `pending_session`, `failed`, or `sent_unconfirmed` selects
the unattended resource budget. Defaults are 30 minutes for
`agent.approval_wait` and 30 seconds for
`agent.approval_wait_unattended`; the caller's remaining deadline may shorten
either. A rapid park during a Weixin delivery outage is therefore intentional
resource release, not rejection or expiry: the request remains answerable for
seven days and a later answer resumes the task. With no time left to ask at all,
the ask is recorded as an `approval.skipped_no_budget` run event and the work
parks without creating a misleading row. Explicit run cancellation expires a
pending request; daemon or waiter loss parks it. On daemon restart, an approval
already answered but not yet consumed gets one idempotent queued continuation.
None of this changes the OUTCOME contract: unanswered is never rejection and
never approval.

Treat command text as untrusted data in the judge prompt. Strip irrelevant
comments and delimit the command rather than interpolating it as an instruction.

`/diag` reports the last 24 hours of triage outcomes from a durable, bounded
projection. The projection stores the run/tool-call identity, outcome, risk,
authorization assessment, grant key, provider route, latency, policy version,
rationale, and a short redacted provider error. It never stores the command,
arguments, prompt, or credentials. Records are retained for 14 days. A failure
to write diagnostics must not block approval, so the foreground write has a
short deadline and is best-effort. The judge has a five-second foreground
deadline; its 1024-token output budget accommodates hidden reasoning while the
request still asks for low reasoning and a compact structured verdict.

The declarative read-only catalog may bypass a human only when both the command
shape and the credential-bearing tool profile are recognized. Trusted workspace
status does not turn local observations such as `git diff`, `rg`, or `jq` into
credentialed operations. Unknown commands, scripts, and mutating cloud calls
remain gated.

Observation proof uses a dedicated quote-aware shell AST parser; it does not
reuse the broader dangerous-command tokenizer. Static pipelines and quoted
provider format expressions can therefore be proved without mistaking `|`,
parentheses, or redirections inside quotes for shell control operators. Dynamic
expansion, command/process substitution, heredocs, assignments, opaque scripts,
unknown global options, privilege wrappers, and writes outside `/dev/null`
remain unprovable and continue through normal approval. Tool-specific global
flags are skipped only from a fail-closed catalog, and credential-bearing files
or environment reads never become automatic merely because a command is
otherwise read-only.

## Failure Recovery

A tool error is evidence, not an automatic stop or a reason to repeat the same
call. Before retrying, inspect the relevant cwd, project markers, environment,
authentication state, runtime, and package-manager/workspace configuration.
Change the next command only after identifying a plausible cause. Generic tools
must not embed project-specific environment overrides such as forcing a Go
workspace mode for every repository.

Long-running Agent CLIs are not ordinary shell commands. `terminal`, `verify`,
and `execute_code` accept a vendor-neutral `execution_class`:

- `standard`: the ordinary tool default, capped at 15 minutes;
- `long-running`: 30-minute default, two-hour hard cap, 10-second heartbeat;
- `interactive`: one-minute default, 15-minute hard cap, one-second heartbeat.

An explicit shorter timeout still wins. The profile is generic execution
metadata; do not solve one vendor's timeout by hard-coding its binary name in
execution middleware.

## Execution engine

`docs/execution-engine.zh-CN.md` owns the mechanics: environment snapshots and
fingerprints, run scratch, the tool environment profile catalog, the five state
primitives, and the sandbox-gated failure classes. This section fixes only the
cross-cutting rules that other domains must not violate.

### Three layers, and the floor between them

Execution policy is layered, and each layer has exactly one home:

- **L1 built-in behaviour data lives in Go source**, versioned with the binary
  and reviewed in a pull request: the reusable-grant floor
  (`tools/grant_floor.go`), the hard-floor protected roots and dangerous/egress
  binaries (`tools/middleware.go`), failure-class rules
  (`tools/tool_errors.go`), and the tool environment profile catalog. These
  define behaviour, not preference. Do not move them into configuration and do
  not add a per-vendor branch in engine code — a profile is data in the
  catalog.
- **L2 remembered rules are bounded and visible.** New interactive approvals
  can reuse a bounded rule only within the current run. The daemon publishes
  both its opaque enforcement key and its human-readable rule; clients display
  that exact rule and never reconstruct authority from option prose. Durable
  `approval_grants` and `execution_capability_grants` remain the compatibility
  and administrative ledger for historical or explicitly managed grants; all
  such entries must be listable, time-bounded, and revocable.
- **L3 user configuration stays small.** `exec_sandbox` keeps `enabled`,
  `required`, and `allow_network`. New execution behaviour does not earn a new
  configuration key.

**L1 is the floor on L2.** A class may be remembered only when it actually
bounds what can run: payloads that reach an interpreter or shell, a
general-purpose exec facility, an irreversible operation, shell control flow, or
any redirection/substitution/expansion/wildcard are approvable once and never
remembered. Ordinary dangerous operations remain approvable classes — that
distinction is the same one the hard floor draws. A boot-time review re-applies
the current floor to already-stored keys and withdraws what no longer qualifies;
it only ever removes authority and is idempotent.

### One execution entry

`tools.Execute(ctx, ExecutionRequest, args)` is the only place a sandboxed
command is constructed, asserted by test. `terminal`, `verify`, `execute_code`
and `watch_external` all pass through it. `background: true` shares the same
prepared material (environment binding, scratch, tool profiles) but is not
routed through `Execute`: a detached process has no streamed output or exit code
to return. It is bounded by a ceiling so a wedged process cannot hold its
process slot, scratch, and copied credential state until the daemon restarts,
and a durable wait belongs in `watch_external` instead.

`SandboxPlan` is serializable and carries the environment binding (snapshot id,
generation, scratch handle) plus the filesystem view; `ProcessMaterial` carries
the values and has no exported fields, no `String`, and no JSON marshaller, so
an environment cannot be logged or shipped by accident. Execution policy travels
on the request (`ExecutionScope.SandboxPolicy`) rather than being read from a
process global at execution time.

Cancellation is part of the same boundary. Tools that can block on I/O or spend
meaningful time in local computation implement `tools.ContextTool`; the
dispatcher supplies the authenticated run context, never a context accepted from
model arguments. Loops must check it at bounded intervals. A tool must not use an
unbounded approximate-search fallback: `patch`, in particular, accepts a unique
exact or bounded same-line-count normalized match and otherwise fails without
writing. `/stop` records a cancellation request, while terminal run state is
owned by the goroutine after the execution body exits.

### Tool environment profiles

A profile declares what host state a command-line tool needs. The engine
understands five primitives and nothing about any vendor: `copy_in` (bounded,
selective, staged then atomically renamed), `map_ro`, `map_rw`, `map_rw_at`
(a writable state directory bound OVER a host path, for state whose location the
tool does not let us configure — the AWS SSO token cache derives from `HOME`),
and `env_redirect`. `write_back` is a reserved slot and is not implemented, so a
permanent identity change (`gcloud auth login`, `docker login`, `gh auth login`)
does not persist from inside the sandbox and belongs in approved host execution.

A dependency between profiles is either inherent (`RequiresProfiles`) or proven
(`ConditionalRequires`, which pulls a credential helper in only when the tool's
own configuration names it). Assuming a dependency is a defect in both
directions: it breaks the tools that do not need it and widens credential
exposure for commands that never touch that provider.

Credential access is a POLICY decision, never a catalog one. A trusted workspace
may use operator credential state; an untrusted one needs the `credential:read`
capability, which is asked for before execution and only when the command's
program set actually matches a profile that declares operator credentials.
Declining is not an error: the command still runs with an empty state overlay and
the tool reports its own "not logged in", which is the honest diagnosis.

### Durable execution identity

As of 2026-08-01, new watches carry a versioned, secret-free
`executionenv.Binding` derived from the creating run's lease. It freezes the
environment profile, credential references, workspace trust level, effective
capability names, and the provenance of each capability together with the
snapshot id/generation and three fingerprints. The same binding is the future
Runner request contract; node-local secret values remain in
`Snapshot`/`ProcessMaterial` and are never serialized.

Preflight and background polls resolve the same binding. An in-process exact
snapshot wins even if the daemon later samples another shell; after restart, a
snapshot id is only an index and all three fingerprints must still match before
the current environment may be rebound. Capabilities are frozen at
registration: later grants cannot widen a running watch, while trust withdrawal
or a persisted grant's revocation/expiry stops it before the next command. A
one-shot registration approval is bounded by the watch deadline and can be
withdrawn by cancelling that watch. Pre-binding watches remain on the legacy
path so an upgrade does not strand already-running work.

A `watch_external` check outlives its run and survives restarts, so it records
the environment identity it was registered under (snapshot id, generation, and
the three fingerprints — never values) and verifies it before every check. The
failure mode being prevented is not an error but a check that SUCCEEDS against a
different account. A watch whose identity no longer matches is stopped with
`environment_changed`; watches registered before identity existed are
grandfathered rather than stranded.

Registration also proves that the frozen check is runnable before ownership
moves to the daemon. The first check must exit 0 and emit a non-empty bounded
state. Non-zero exits, timeouts, missing commands, and check-definition output
such as a swallowed Python traceback are rejected in the foreground. A clean
exit whose output matches no terminal/target pattern is the normal pending
case and may register. This contract is deliberately stricter than an ordinary
interactive status probe: the background loop has no model available to repair
the command later.

### Failure classification

"Was there a sandbox" is a precondition, not one cue among many. A command that
ran isolated is classified for filesystem denial FIRST
(`sandbox_fs_denied`, `credential_state_readonly`), before the generic
taxonomy, and shell-level rejections (exit 2/126/127) are never sandbox
denials. A denied write to a tool's own state directory must never be reported
as an authentication problem, and must never carry guidance that forbids the
only correct remedy.

Event `error_category` comes from the structured class the tools layer already
appended, not from re-reading the prose hint. Do not add a second classifier
that parses enriched error text.

## Known Gaps

- A durable `EnvironmentLease` is materialized once per run and reused by
  replay of that run. It stores workspace/profile identity, capability names,
  credential references, and a non-secret principal fingerprint only. A
  continued task creates a new run and therefore a fresh lease.
- Missing local workspace environments finish the attempted run as
  `waiting_user` with reason `environment_unavailable`. Automatic wake-up when
  a compatible remote environment is later installed remains follow-up work.
- Egress classification currently covers exec tools; full-auto retains its
  documented bypass for ordinary egress.
- Workspace skills are excluded for untrusted active workspaces, skill roots
  come from `ExecutionScope` rather than daemon cwd, and credential-shaped
  environment passthrough declarations fail closed. User-installed trusted
  skills remain available.
- Credential-aware private HOME views and `credential:read` approval are
  implemented through data-driven tool profiles. Refreshed tokens written by a
  CLI stay in the run/watch overlay; permanent login changes still require an
  approved host flow because `write_back` is intentionally not implemented.
- Durable execution falls back to a private per-binding toolchain cache when a
  person-level cache cannot be materialized. Cache availability must never turn
  a valid watcher into an environment failure.
- `/diag execution` reports backend/network policy, workspace trust, active
  capability names, the current run's lease/profile, and only the count of
  hidden credential references. It never displays credential names or values.

## Required Tests

Changes in this domain need focused coverage for:

- active workspace and allowed-root enforcement;
- symlink and wrapper-command behavior;
- hard-floor precedence over every mode and grant;
- rejection versus safety-block semantics;
- code-execution approval;
- three-axis containment and the observation-only catalog;
- exact-run authorization isolation and byte-identical reuse;
- egress classification;
- child-process environment construction and daemon-secret exclusion;
- exact-value redaction without persisting raw secrets;
- local-CLI-only workspace trust and capability revocation on untrust;
- immutable per-run leases and expiry of execution capabilities;
- secret-free durable binding round trips, restart fingerprint validation,
  frozen capability sets, and revocation before the next poll;
- untrusted workspace skill fencing and credential-shaped declaration denial;
- long-running timeout bounds and heartbeat behavior;
- context cancellation of command execution and CPU-bound tool loops;
- bounded, ambiguity-safe patch misses on large files;
- host grants scoped by workspace and command family;
- smart triage fail-closed behavior;
- delegation depth and parent-backend isolation;
- UTF-8-safe previews, artifact spooling, and model truncation recovery.
