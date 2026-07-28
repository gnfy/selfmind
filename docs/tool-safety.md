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
- the planned credential-aware private-HOME view will make credential files
  override toolchain convenience when classifications overlap. `~/.npmrc`,
  `~/.docker/config.json`, `~/.netrc`, and credential helpers will require a
  credential capability even when a CLI normally reads them;
- that future view must never grant all of `~/.config`; access is
  tool-subdirectory or file scoped.

Private package installation in an untrusted workspace currently requires a
shared-network capability and uses the compatibility environment. After the
private-HOME view ships it may also require credential-read approval. Do not
bypass either boundary with a vendor-specific exception.

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

Normal UI output summarizes tools, plans, and failures. Raw JSON is reserved
for an explicit protocol/debug request.

## Approval Layers

Approval evaluation is ordered. A lower layer cannot override a higher one:

1. unbypassable hard safety floor;
2. explicit approval mode and persistent policy;
3. matching task/person class grant;
4. optional smart-mode cheap-model triage;
5. human approval prompt.

An approval rejection or cancellation is a user decision, not a transient tool
failure. Kernel rejection detection and middleware error strings are a shared
contract: rejection tells the model not to repeat the operation. A hard safety
block is a policy decision and remains distinguishable from user rejection.

Approval memory uses coarse action classes, not exact command strings. Hard
floor denials and content-level denials are never grantable. Numbered approval
references resolve in the gateway with the same order used by every client.

Host execution is a special case: a reusable host grant is additionally scoped
to a non-secret fingerprint of the active workspace and the effective command
family. If a durable workspace identity is unavailable, host execution may be
approved once but cannot create a reusable grant. Older broad host grants do
not match the resource-scoped key, so an owner may see one new approval after
upgrading. The authenticated local CLI can inspect and revoke temporary grants
with `selfmind ws grants [workspace_id]` and
`selfmind ws revoke <capability> [workspace_id]`; grants also expire
automatically.

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

- APPROVE records a bounded task-scope class grant.
- DENY uses the user-rejection contract and must not trigger retry.
- ESCALATE asks the human.
- Missing judge, timeout, provider failure, malformed output, or an unknown
  decision always escalates. Triage never fails open.

Treat command text as untrusted data in the judge prompt. Strip irrelevant
comments and delimit the command rather than interpolating it as an instruction.

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
- Credential-aware private HOME views and `credential:read` approval are not
implemented yet. Generic operator environment compatibility remains in
place behind `BuildProcessEnv`.
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
- egress classification;
- child-process environment construction and daemon-secret exclusion;
- exact-value redaction without persisting raw secrets;
- local-CLI-only workspace trust and capability revocation on untrust;
- immutable per-run leases and expiry of execution capabilities;
- untrusted workspace skill fencing and credential-shaped declaration denial;
- long-running timeout bounds and heartbeat behavior;
- host grants scoped by workspace and command family;
- smart triage fail-closed behavior;
- delegation depth and parent-backend isolation;
- UTF-8-safe previews, artifact spooling, and model truncation recovery.
