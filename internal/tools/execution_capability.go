package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/envprofiles"
)

// ExecutionCapabilityMiddleware provides a narrow, approval-backed escape
// hatch for isolated commands that genuinely need shared host networking. It
// does not grant host execution, extra filesystem access, or credential
// access. Model-visible sandbox arguments remain unchanged.
//
// A network permission is not a reason to share the network. Workspace trust
// under an operator policy that allows egress, a stored or run network:shared
// grant, and a one-time approval each PERMIT it; the call receives it only when
// it also needs it, because its command names a remote service
// (commandPlausiblyNeedsEgress) or the model asked with `network: true`. Every
// call records which way this went (`_network_decision`).
func ExecutionCapabilityMiddleware(prechecks ...func(map[string]interface{}) error) Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			toolName := stringArg(args, "_tool_name")
			if !isExecTool(toolName) {
				return next(args)
			}
			// The decision is the runtime's. Whatever a caller left in its keys is
			// not evidence of anything.
			recordNetworkDecision(args, "", "")
			if !networkRequestDeclaredBy(toolName) {
				delete(args, "network")
			}
			requestedMode, modeErr := requestedSandboxMode(args)
			if modeErr != nil {
				return next(args)
			}
			if requestedMode == SandboxHost {
				recordNetworkDecision(args, executionenv.NetworkDecisionShared, "")
				return next(args)
			}
			annotateEffectiveSandboxMode(args)
			if effectiveSandboxModeArg(args) == SandboxHost {
				// Host fallback is a broader boundary and is approval-gated by
				// SmartApprovalMiddleware. Do not create a second, narrower
				// network approval for the same execution.
				recordNetworkDecision(args, executionenv.NetworkDecisionShared, "")
				return next(args)
			}
			if strings.TrimSpace(stringArg(args, sandboxPlanRefusalArg)) != "" {
				// Isolation is demanded and cannot be enforced: no capability is
				// asked for a sandbox that will not exist. The approval funnel
				// refuses the call after its hard floor.
				return next(args)
			}
			scope, hasScope := currentExecutionScopeAny(args)
			if !hasScope || strings.TrimSpace(scope.WorkspaceID) == "" {
				// No workspace to grant anything to: the operator policy alone
				// decides, as the sandbox does when no decision is recorded.
				if _, _, allowed := execSandboxPolicyForArgs(args); allowed {
					recordNetworkDecision(args, executionenv.NetworkDecisionShared, "")
				} else {
					recordNetworkDecision(args, executionenv.NetworkWithheldPolicyDisabled, "")
				}
				return next(args)
			}
			for _, check := range prechecks {
				if err := check(args); err != nil {
					return "", err
				}
			}
			explicit := networkRequestedByCall(toolName, args)
			if isolatedExecutionView(scope) && !scope.ParallelWork {
				// A legacy view without the parallel effect-claim contract remains
				// file-only. Parallel views continue through the normal capability
				// path; approval and the durable external-effect lane constrain
				// their remote operations before dispatch.
				if commandClearlyNeedsNetwork(toolName, args) || explicit {
					return "", fmt.Errorf("networked execution is unavailable in this isolated view until its external target can be claimed")
				}
				args["_network_shared"] = false
				recordNetworkDecision(args, executionenv.NetworkWithheldPolicyDisabled, "")
				return next(args)
			}

			fingerprint := executionCapabilityFingerprint(scope.WorkspaceID, executionenv.CapabilityNetworkShared)
			resolution := resolveNetworkForCall(args, scope, toolName, fingerprint)
			args["_network_shared"] = resolution.shared
			recordNetworkDecision(args, resolution.decision, resolution.permission)
			// The hard floor is the approval funnel's first layer, and nothing a
			// person or a grant says answers it. A call it blocks is not asked about
			// any capability first: asking whether `rm -rf /` may have the network
			// (or the person's credentials) would put a question to a human whose
			// answer cannot matter. The funnel returns the block.
			if blocked, _ := hardlineToolCall(approvalProjectRoot("", scope, args), toolName, args); blocked {
				return next(args)
			}
			// Credential access is resolved here too, and BEFORE execution.
			//
			// Without this, `credential:read` existed as a constant that only
			// ever got READ: an untrusted workspace could never obtain operator
			// credentials, so the only way to run a credential CLI was to trust
			// the whole workspace — exactly the all-or-nothing escalation this
			// capability exists to avoid. Deciding after the failure is not an
			// option either: the tool would already have reported "not logged
			// in", and the model would chase a login problem that does not
			// exist.
			resolveCredentialCapability(args, scope, toolName)
			// Commands that clearly require egress, and calls that asked for the
			// network, are approved before their first execution when nothing
			// permits it yet. This keeps the capability middleware from replaying
			// a compound command after its local side effects have already run.
			if !resolution.shared && (commandClearlyNeedsNetwork(toolName, args) || explicit) {
				if err := approveNetworkCapability(args, scope, fingerprint, explicit); err != nil {
					return "", err
				}
				resolution = networkResolution{shared: true, decision: executionenv.NetworkDecisionShared,
					permission: executionenv.NetworkPermissionApproval, permitted: true}
				args["_network_shared"] = true
				recordNetworkDecision(args, resolution.decision, resolution.permission)
			}

			output, err := next(args)
			if err == nil || resolution.shared || !networkCapabilityFailure(toolName, err, output, args) {
				return output, err
			}
			if resolution.permitted {
				// The workspace already permits network; the command ran offline
				// because it does not name a remote service and did not ask. Asking
				// again would be a question with a known answer. Say what happened
				// and how to run it networked, without replaying it.
				return output, newNetworkWithheldError(err)
			}
			// Unknown commands may reveal their egress requirement only after
			// failing. Grant the capability for an explicit next attempt, but
			// never replay the whole command in this middleware.
			if approveErr := approveNetworkCapability(args, scope, fingerprint, false); approveErr != nil {
				return output, approveErr
			}
			return output, fmt.Errorf("network:shared was approved; retry the command once with network: true (the failed command was not automatically replayed). %s\n%w", networkRetryCaution, err)
		}
	}
}

// Runtime-owned record of the network decision for one call. The kernel removes
// every underscore argument a model supplies, and the middleware overwrites
// both on every execution call regardless.
const (
	networkDecisionArg   = "_network_decision"
	networkPermissionArg = "_network_permission"
)

func recordNetworkDecision(args map[string]interface{}, decision, permission string) {
	if decision == "" {
		delete(args, networkDecisionArg)
	} else {
		args[networkDecisionArg] = decision
	}
	if permission == "" {
		delete(args, networkPermissionArg)
	} else {
		args[networkPermissionArg] = permission
	}
}

// networkResolution is the capability decision for one call.
type networkResolution struct {
	// shared: the daemon's network is shared with the call.
	shared bool
	// decision is the evidence string (executionenv.NetworkDecision* and
	// NetworkWithheld*), permission what permitted network at the time.
	decision   string
	permission string
	// permitted: something permitted network for this workspace. A call that
	// was not shared although permitted was withheld because it did not need
	// the network, never because no one allowed it.
	permitted bool
}

// resolveNetworkForCall decides the network for one call from two independent
// questions. PERMISSION: does anything allow the daemon's network to be shared
// with this workspace's commands (trust under an operator policy that allows
// egress, a run grant, a stored grant). NEED: does the call reach a remote
// service by name, or did it ask. The network is shared only when both hold.
//
// The first version of this decision let a permission stand in for need, which
// networked every command in a workspace with a grant — a build, a test, `ls` —
// and made every one of them, at parallel capacity, claim the person-wide
// unknown external target. A network-shared call is also not contained, so
// purely local work lost the sandbox's own approval release.
func resolveNetworkForCall(args map[string]interface{}, scope ExecutionScope, toolName, fingerprint string) networkResolution {
	_, _, policyAllows := execSandboxPolicyForArgs(args)
	permission := networkPermissionFor(args, scope, policyAllows, fingerprint)
	switch {
	case permission == "" && scope.TrustLevel == executionenv.TrustTrusted && !policyAllows:
		return networkResolution{decision: executionenv.NetworkWithheldPolicyDisabled}
	case permission == "":
		return networkResolution{decision: executionenv.NetworkWithheldNoGrant}
	case commandWantsNetwork(toolName, args):
		return networkResolution{shared: true, decision: executionenv.NetworkDecisionShared, permission: permission, permitted: true}
	}
	return networkResolution{decision: executionenv.NetworkWithheldNotBound, permission: permission, permitted: true}
}

// networkPermissionFor says what, if anything, permits the network for this
// workspace: trust under an operator policy that allows egress, else a run
// grant, else a stored grant. exec_sandbox.allow_network: false closes the
// trust path; only a grant or an approval a person gave opens it again, as it
// always has.
func networkPermissionFor(args map[string]interface{}, scope ExecutionScope, policyAllows bool, fingerprint string) string {
	if scope.TrustLevel == executionenv.TrustTrusted && policyAllows {
		return executionenv.NetworkPermissionTrust
	}
	if scope.runGrants != nil && scope.runGrants.has(executionCapabilityRunGrantKey(executionenv.CapabilityNetworkShared, fingerprint)) {
		return executionenv.NetworkPermissionGrant
	}
	if scope.CapabilityStore != nil {
		granted, err := scope.CapabilityStore.HasExecutionCapability(
			contextFromArgs(args),
			scope.TenantID,
			scope.PersonID,
			scope.WorkspaceID,
			executionenv.CapabilityNetworkShared,
			fingerprint,
		)
		if err == nil && granted {
			return executionenv.NetworkPermissionGrant
		}
	}
	return ""
}

// networkWithheldCode is the typed failure of a command that ran offline in a
// workspace that permits network, then failed for want of it.
const networkWithheldCode = "network_withheld_by_policy"

const networkWithheldNote = "Network was withheld by policy for this command, although this workspace permits network access: " +
	"it does not name a remote service, so it ran offline, and it was not replayed. " +
	"Retry with network: true to run it with network access. " + networkRetryCaution

// networkRetryCaution is said wherever a retry is offered. The command ran, and
// ran only as far as its first network step: anything it did before that point
// (bumped a version, wrote files, committed) is done, and a retry does it again.
const networkRetryCaution = "If the command changes state before it reaches the network (a version bump, a commit, written files), " +
	"inspect that state first: those steps already ran, and a retry runs them again."

// networkWithheldError carries a network-class failure of a call that ran
// offline although the workspace permits network. It adds the reason and the
// way to retry, and keeps everything the original failure said: its text, its
// class (it sets no category of its own, so the `error_class` the failure
// already carries, such as sandbox_no_network, stays what events and reports
// group it by), and its effect certainty. The command ran and failed; that is
// neither "not dispatched" nor an unknown effect, and claiming either would
// change the failure's meaning and block the very retry it describes.
type networkWithheldError struct{ cause error }

func newNetworkWithheldError(cause error) error { return &networkWithheldError{cause: cause} }

func (e *networkWithheldError) Error() string         { return e.cause.Error() + "\n" + networkWithheldNote }
func (e *networkWithheldError) Unwrap() error         { return e.cause }
func (e *networkWithheldError) ToolErrorCode() string { return networkWithheldCode }
func (e *networkWithheldError) ToolErrorCategory() string {
	var typed interface{ ToolErrorCategory() string }
	if errors.As(e.cause, &typed) {
		return typed.ToolErrorCategory()
	}
	return ""
}
func (e *networkWithheldError) ModelSafeMessage() string { return e.Error() }
func (e *networkWithheldError) ToolRecoveryHint() string {
	return "Retry the command once with network: true. Permission comes from this workspace's network grant; the flag only says this command needs it. " + networkRetryCaution
}
func (e *networkWithheldError) ToolFailurePhase() string { return "execution" }
func (e *networkWithheldError) ToolRetryability() string { return "corrected_input" }
func (e *networkWithheldError) ToolEffectState() string  { return "" }
func (e *networkWithheldError) ToolStateChanged() bool   { return false }
func (e *networkWithheldError) ToolAlternatives() []string {
	return []string{"retry_with_network_true"}
}

func isolatedExecutionView(scope ExecutionScope) bool {
	for _, binding := range scope.RootBindings {
		if binding.Source == executionenv.RootSourceExecutionView {
			return true
		}
	}
	return false
}

func approveNetworkCapability(args map[string]interface{}, scope ExecutionScope, fingerprint string, requested bool) error {
	if scope.Approval == nil {
		return rejectOperation(rejectionCodeCapability, "operation rejected: network:shared requires approval")
	}
	reason := "the isolated command needs the workspace-scoped shared network capability"
	if requested {
		reason = "the command asked for network access (network: true) and needs the workspace-scoped shared network capability"
	}
	decision, approvalErr := scope.Approval(contextFromArgs(args), ToolApprovalRequest{
		TenantID: scope.TenantID,
		PersonID: scope.PersonID,
		TaskID:   scope.TaskID,
		RunID:    scope.RunID,
		Channel:  scope.Channel,
		ToolName: executionenv.CapabilityNetworkShared,
		Reason:   reason,
		Args: map[string]interface{}{
			"capability":   executionenv.CapabilityNetworkShared,
			"workspace_id": scope.WorkspaceID,
		},
		ResourceFingerprint: fingerprint,
		GrantClass:          "shared network access for this workspace",
	})
	if approvalErr != nil {
		return approvalErr
	}
	if !decision.Approved {
		return rejectOperation(rejectionCodeCapability, "operation rejected: network:shared was not approved")
	}
	if decision.Scope != "" && decision.Scope != "run" {
		return rejectOperation(rejectionCodeScope, fmt.Sprintf("operation rejected: approval scope %q was not offered for network:shared", decision.Scope))
	}
	if decision.Scope == "run" && scope.runGrants != nil {
		scope.runGrants.add(executionCapabilityRunGrantKey(executionenv.CapabilityNetworkShared, fingerprint))
	}

	if scope.CapabilityStore != nil {
		if expiry := capabilityExpiryFromDecision(decision); !expiry.IsZero() {
			if err := scope.CapabilityStore.GrantExecutionCapability(
				context.Background(),
				scope.TenantID,
				scope.PersonID,
				scope.WorkspaceID,
				executionenv.CapabilityNetworkShared,
				fingerprint,
				"human:"+fallbackCapabilitySource(scope.Channel),
				expiry,
			); err != nil {
				return fmt.Errorf("persist network capability: %w", err)
			}
		}
	}
	return nil
}

// commandPlausiblyNeedsEgress reports whether sharing the daemon's network with
// this command is warranted at all: whether the command reaches a remote
// service by name. It is the single predicate every network decision starts
// from, for a trusted workspace and for a grant alike.
//
// It is deliberately WIDER than commandClearlyNeedsNetwork. That detector
// recognises explicit egress programs and matched 55 of 666 real commands,
// while 388 of them invoke a credential-bearing tool profile — gcloud, aws,
// kubectl and friends exist to talk to a remote service, so withholding the
// network from them would turn an approval into a failed first attempt that
// only the post-failure recovery path could rescue. Trading one approval for
// one wasted execution is not an improvement.
//
// The union has three parts, each independent: explicit egress programs, the
// remote-reaching subcommands of package managers, VCS remotes and
// infrastructure tools (network_commands.go), and credential-bearing service
// CLIs. It keeps that cost at zero while still denying egress to the commands
// that plainly never leave the host — builds, tests, linters, `git status` — which
// is what lets those take the sandbox's contained fast path.
//
// Each part sets aside an invocation that is an explicit offline or local form
// (network_offline_forms.go): `npm install --offline`, a clone of a path on this
// machine, `kubectl --help`. A form that names a remote target is not one.
func commandPlausiblyNeedsEgress(toolName string, args map[string]interface{}) bool {
	return commandClearlyNeedsNetwork(toolName, args) ||
		commandNamesRemoteService(toolName, args) ||
		commandReachesCredentialedService(toolName, args)
}

// networkCapabilityFailure reports a command that ran offline and failed for
// want of network. It reads the failure's text, which is what a command's own
// failure offers, and therefore must never read the text of an error that is not
// the command's: a refusal, a safety block or a parked approval says why a call
// did not run, and a rationale in it that mentions the sandbox network is
// prose, not an observation of a command that tried to connect.
func networkCapabilityFailure(toolName string, err error, output string, args map[string]interface{}) bool {
	if errorIsDecisionOrWait(err) {
		return false
	}
	text := strings.ToLower(err.Error() + "\n" + output)
	if strings.Contains(text, "sandbox_no_network") || strings.Contains(text, "network is disabled") {
		return true
	}
	return ClassifyToolError(toolName, err, output) == "network"
}

// errorIsDecisionOrWait reports an error that already says the call did not run
// or stopped on a person: it was not dispatched, it is a decision for a person
// to make, it was rejected (by a person, safety triage or a capability policy),
// the Run is parked on a human, or the hard floor blocked it. Such an error is
// an outcome with its own meaning and its own recovery. Re-typing it as a
// withheld network would tell the model to retry what a person or a policy just
// refused.
func errorIsDecisionOrWait(err error) bool {
	if err == nil {
		return false
	}
	var pause interface{ ToolRunPause() (string, string, bool) }
	if errors.As(err, &pause) {
		return true
	}
	var effect interface{ ToolEffectState() string }
	if errors.As(err, &effect) && effect.ToolEffectState() == "not_dispatched" {
		return true
	}
	var retry interface{ ToolRetryability() string }
	if errors.As(err, &retry) && retry.ToolRetryability() == "user_decision" {
		return true
	}
	var category interface{ ToolErrorCategory() string }
	if errors.As(err, &category) {
		switch category.ToolErrorCategory() {
		case toolErrorCategoryRejected, toolErrorCategoryHumanWait:
			return true
		}
	}
	// The untyped spellings of the same outcomes: the rejection contract kernel
	// recognises by prefix, and the hard floor's block.
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.HasPrefix(message, "operation rejected") || strings.HasPrefix(message, "operation blocked by safety policy")
}

func executionCapabilityFingerprint(workspaceID, capability string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(workspaceID) + "\n" + strings.TrimSpace(capability)))
	return fmt.Sprintf("%x", sum[:12])
}

func executionCapabilityRunGrantKey(capability, fingerprint string) string {
	return "capability:" + strings.TrimSpace(capability) + ":" + strings.TrimSpace(fingerprint)
}

// capabilityExpiryFromDecision prefers the deadline the CONTROL PLANE returned.
// The execution layer keeps a conservative fallback for a decision that carries
// none, but it must not be the authority on how long its own authorization
// lasts — that ruling belongs where approvals are decided.
func capabilityExpiryFromDecision(decision ToolApprovalDecision) time.Time {
	if !decision.ExpiresAt.IsZero() {
		return decision.ExpiresAt
	}
	return capabilityExpiry(decision.Scope)
}

func capabilityExpiry(scope string) time.Time {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "task":
		return time.Now().Add(time.Hour)
	case "person":
		return time.Now().Add(8 * time.Hour)
	default:
		return time.Time{}
	}
}

func fallbackCapabilitySource(channel string) string {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return "unknown"
	}
	return channel
}

// resolveCredentialCapability decides whether this call may use the operator's
// credential state, asking the human exactly once when it would change the
// outcome.
//
// It asks only when all of these hold, so a trusted workspace and a command that
// touches no credential CLI are never interrupted:
//
//   - the workspace is untrusted (a trusted one already has access);
//   - the command's programs match a profile that declares operator credentials;
//   - no live grant already covers this workspace.
//
// A refusal is not an error: the command still runs, the state overlay stays
// empty, and the tool reports its own "not logged in" — which is the honest
// diagnosis and leaves the decision with the person.
func resolveCredentialCapability(args map[string]interface{}, scope ExecutionScope, toolName string) {
	// A trusted workspace may use operator credentials without another human
	// decision, but only when the command actually invokes a credential-bearing
	// tool profile. Marking every trusted command as credentialed made harmless
	// local observations (git diff, rg, jq, ...) lose sandbox auto-approval.
	if !commandNeedsOperatorCredentials(toolName, args) {
		return
	}
	if scope.TrustLevel == executionenv.TrustTrusted {
		args[credentialReadArgKey] = true
		return
	}
	if scopeHasCapability(scope, executionenv.CapabilityCredentialRead) {
		args[credentialReadArgKey] = true
		return
	}
	// On a backend that cannot mount, granting access is what makes the call
	// write the operator's real credential cache in place, and a contained
	// safe observation is then released with no second ask. This ask is the
	// person's only decision about that write, so it leads with the paths and
	// the run grant it leaves covers exactly that write set.
	hostWrites := grantableHostCredentialWrites(toolName, args)
	fingerprint := credentialCapabilityFingerprint(scope.WorkspaceID, toolName, args, hostWrites)
	if scope.runGrants != nil && scope.runGrants.has(executionCapabilityRunGrantKey(executionenv.CapabilityCredentialRead, fingerprint)) {
		args[credentialReadArgKey] = true
		return
	}
	if scope.CapabilityStore != nil {
		granted, err := scope.CapabilityStore.HasExecutionCapability(
			contextFromArgs(args), scope.TenantID, scope.PersonID, scope.WorkspaceID,
			executionenv.CapabilityCredentialRead, fingerprint,
		)
		if err == nil && granted {
			args[credentialReadArgKey] = true
			return
		}
	}
	if err := approveCredentialCapability(args, scope, toolName, fingerprint, hostWrites); err != nil {
		// Declining leaves the command runnable without credentials. Turning a
		// refusal into a hard failure would remove the option of running the
		// read-only part of a command, and a rejection is a decision, not a
		// fault.
		return
	}
	args[credentialReadArgKey] = true
}

// credentialReadArgKey carries the resolved decision to material preparation.
const credentialReadArgKey = "_credential_read"

// credentialReadResolved reports the credential:read decision made for a
// credential CLI this call names. It is narrower than the containment
// assessment's credential axis, which also counts the host inventory an
// undecidable payload is prepared with (receivesOperatorCredentials).
func credentialReadResolved(args map[string]interface{}) bool {
	allowed, _ := args[credentialReadArgKey].(bool)
	return allowed
}

// commandNeedsOperatorCredentials reports whether any program in the command is
// covered by a profile that declares operator credential access. The program set
// comes from the shell AST, so a compound command is judged by what it will
// actually run.
func commandNeedsOperatorCredentials(toolName string, args map[string]interface{}) bool {
	for _, profile := range envprofiles.Match(execCommandPrograms(toolName, args)) {
		if profile != nil && profile.CredentialAccess == envprofiles.CredentialAccessOperator {
			return true
		}
	}
	return false
}

func approveCredentialCapability(args map[string]interface{}, scope ExecutionScope, toolName, fingerprint string, hostWrites []string) error {
	if scope.Approval == nil {
		return rejectOperation(rejectionCodeCapability, "operation rejected: credential:read requires approval")
	}
	safeObservation := scope.runGrants != nil && credentialSafeObservation(toolName, args)
	decisionPolicy := ApprovalDecisionPolicyOnceOnly
	grantClass := ""
	if safeObservation {
		decisionPolicy = ""
		grantClass = describeHostCredentialWritesGrant("read existing tool credentials for safe observations in this run",
			hostCredentialWritesBinding(hostWrites), compactHostPaths(args, hostWrites))
	}
	reason := prependApprovalReason(hostCredentialWritesFact(args, hostWrites),
		"this untrusted workspace needs read access to your existing tool credentials")
	decision, approvalErr := scope.Approval(contextFromArgs(args), ToolApprovalRequest{
		TenantID: scope.TenantID,
		PersonID: scope.PersonID,
		TaskID:   scope.TaskID,
		RunID:    scope.RunID,
		Channel:  scope.Channel,
		ToolName: executionenv.CapabilityCredentialRead,
		Reason:   reason,
		Args: map[string]interface{}{
			"capability":   executionenv.CapabilityCredentialRead,
			"workspace_id": scope.WorkspaceID,
		},
		GrantClass:          grantClass,
		ResourceFingerprint: fingerprint,
		DecisionPolicy:      decisionPolicy,
	})
	if approvalErr != nil {
		return approvalErr
	}
	if !decision.Approved {
		return rejectOperation(rejectionCodeCapability, "operation rejected: credential:read was not approved")
	}
	if strings.TrimSpace(decision.GrantKey) != "" {
		return rejectOperation(rejectionCodeScope, "operation rejected: credential access does not accept a rule grant")
	}
	if decision.Scope != "" && (!safeObservation || decision.Scope != "run") {
		return rejectOperation(rejectionCodeScope, fmt.Sprintf("operation rejected: approval scope %q was not offered for credential access", decision.Scope))
	}
	if decision.Scope == "run" && scope.runGrants != nil {
		scope.runGrants.add(executionCapabilityRunGrantKey(executionenv.CapabilityCredentialRead, fingerprint))
	}
	if scope.CapabilityStore != nil {
		if expiry := capabilityExpiryFromDecision(decision); !expiry.IsZero() {
			if err := scope.CapabilityStore.GrantExecutionCapability(
				context.Background(), scope.TenantID, scope.PersonID, scope.WorkspaceID,
				executionenv.CapabilityCredentialRead, fingerprint,
				"human:"+fallbackCapabilitySource(scope.Channel), expiry,
			); err != nil {
				return fmt.Errorf("persist credential capability: %w", err)
			}
		}
	}
	return nil
}

func credentialSafeObservation(toolName string, args map[string]interface{}) bool {
	copyArgs := make(map[string]interface{}, len(args)+1)
	for key, value := range args {
		copyArgs[key] = value
	}
	copyArgs[credentialReadArgKey] = true
	return observationOnlyExec(toolName, copyArgs)
}

// credentialCapabilityFingerprint scopes a credential:read grant to the
// workspace, the credential profiles the command names and the in-place
// credential writes granting it would enable. A call that writes nothing keeps
// the fingerprint it always had.
func credentialCapabilityFingerprint(workspaceID, toolName string, args map[string]interface{}, hostWrites []string) string {
	profiles := envprofiles.Match(execCommandPrograms(toolName, args))
	ids := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if profile != nil && profile.CredentialAccess == envprofiles.CredentialAccessOperator {
			ids = append(ids, profile.ID)
		}
	}
	sort.Strings(ids)
	material := strings.TrimSpace(workspaceID) + "\n" + executionenv.CapabilityCredentialRead + "\n" + strings.Join(ids, ",")
	if binding := hostCredentialWritesBinding(hostWrites); binding != "" {
		material += "\n" + binding
	}
	sum := sha256.Sum256([]byte(material))
	return fmt.Sprintf("%x", sum[:12])
}

// grantableHostCredentialWrites is the in-place credential write set this call
// makes once its credential access is granted: the plan preparation makes when
// an enforced sandbox cannot mount tool state, or nothing. A decision taken
// before the call (the credential:read ask, a phase bundle) is bound to it.
func grantableHostCredentialWrites(toolName string, args map[string]interface{}) []string {
	if effectiveSandboxModeArg(args) != SandboxIsolated || SandboxBackendForMode(SandboxIsolated).MountsToolState() {
		return nil
	}
	if enabled, _, _ := execSandboxPolicyForArgs(args); !enabled || !ExecSandboxAvailable() {
		return nil
	}
	plan, err := previewHostStateWrites(toolName, args, true)
	if err != nil {
		// An unexpressible plan is refused before any approval.
		return nil
	}
	return plan.Paths()
}
