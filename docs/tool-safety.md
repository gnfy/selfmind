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

## Required Tests

Changes in this domain need focused coverage for:

- active workspace and allowed-root enforcement;
- symlink and wrapper-command behavior;
- hard-floor precedence over every mode and grant;
- rejection versus safety-block semantics;
- code-execution approval;
- egress classification;
- smart triage fail-closed behavior;
- delegation depth and parent-backend isolation;
- UTF-8-safe previews, artifact spooling, and model truncation recovery.
