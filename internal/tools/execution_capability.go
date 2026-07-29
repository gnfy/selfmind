package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/envprofiles"
)

// ExecutionCapabilityMiddleware provides a narrow, approval-backed escape
// hatch for isolated commands that genuinely need shared host networking. It
// does not grant host execution, extra filesystem access, or credential
// access. Model-visible sandbox arguments remain unchanged.
func ExecutionCapabilityMiddleware() Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			toolName := stringArg(args, "_tool_name")
			if !isExecTool(toolName) {
				return next(args)
			}
			requestedMode, modeErr := requestedSandboxMode(args)
			if modeErr != nil || requestedMode == SandboxHost {
				return next(args)
			}
			annotateEffectiveSandboxMode(args)
			if effectiveSandboxModeArg(args) == SandboxHost {
				// Host fallback is a broader boundary and is approval-gated by
				// SmartApprovalMiddleware. Do not create a second, narrower
				// network approval for the same execution.
				return next(args)
			}
			scope, hasScope := currentExecutionScopeAny(args)
			if !hasScope || strings.TrimSpace(scope.WorkspaceID) == "" {
				return next(args)
			}

			fingerprint := executionCapabilityFingerprint(scope.WorkspaceID, executionenv.CapabilityNetworkShared)
			networkShared := scope.TrustLevel == executionenv.TrustTrusted && ExecSandboxAllowsNetwork()
			if !networkShared && scope.CapabilityStore != nil {
				granted, err := scope.CapabilityStore.HasExecutionCapability(
					contextFromArgs(args),
					scope.TenantID,
					scope.PersonID,
					scope.WorkspaceID,
					executionenv.CapabilityNetworkShared,
					fingerprint,
				)
				if err == nil {
					networkShared = granted
				}
			}
			args["_network_shared"] = networkShared
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
			// Commands that clearly require egress are approved before their
			// first execution. This keeps the capability middleware from
			// replaying a compound command after its local side effects have
			// already run.
			if !networkShared && commandClearlyNeedsNetwork(toolName, args) {
				if err := approveNetworkCapability(args, scope, fingerprint); err != nil {
					return "", err
				}
				networkShared = true
				args["_network_shared"] = true
			}

			output, err := next(args)
			if err == nil || networkShared || !networkCapabilityFailure(toolName, err, output, args) {
				return output, err
			}
			// Unknown commands may reveal their egress requirement only after
			// failing. Grant the capability for an explicit next attempt, but
			// never replay the whole command in this middleware.
			if approveErr := approveNetworkCapability(args, scope, fingerprint); approveErr != nil {
				return output, approveErr
			}
			return output, fmt.Errorf("network:shared was approved; retry the command once explicitly with the granted capability (the failed command was not automatically replayed): %w", err)
		}
	}
}

func approveNetworkCapability(args map[string]interface{}, scope ExecutionScope, fingerprint string) error {
	if scope.Approval == nil {
		return fmt.Errorf("operation rejected: network:shared requires approval")
	}
	decision, approvalErr := scope.Approval(contextFromArgs(args), ToolApprovalRequest{
		TenantID: scope.TenantID,
		PersonID: scope.PersonID,
		TaskID:   scope.TaskID,
		RunID:    scope.RunID,
		Channel:  scope.Channel,
		ToolName: executionenv.CapabilityNetworkShared,
		Reason:   "the isolated command needs the workspace-scoped shared network capability",
		Args: map[string]interface{}{
			"capability":   executionenv.CapabilityNetworkShared,
			"workspace_id": scope.WorkspaceID,
		},
		ResourceFingerprint: fingerprint,
	})
	if approvalErr != nil {
		return approvalErr
	}
	if !decision.Approved {
		return fmt.Errorf("operation rejected: network:shared was not approved")
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

func commandClearlyNeedsNetwork(toolName string, args map[string]interface{}) bool {
	if strings.EqualFold(strings.TrimSpace(toolName), "watch_external") {
		return true
	}
	command := strings.TrimSpace(execCommandPayload(toolName, args))
	if command == "" {
		return false
	}
	segments, _ := expandCommandSegments(command, 0)
	hit, _ := egressCommand(command, segments)
	return hit
}

func networkCapabilityFailure(toolName string, err error, output string, args map[string]interface{}) bool {
	text := strings.ToLower(err.Error() + "\n" + output)
	if strings.Contains(text, "sandbox_no_network") || strings.Contains(text, "network is disabled") {
		return true
	}
	return ClassifyToolError(toolName, err, output) == "network"
}

func executionCapabilityFingerprint(workspaceID, capability string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(workspaceID) + "\n" + strings.TrimSpace(capability)))
	return fmt.Sprintf("%x", sum[:12])
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
	if scope.TrustLevel == executionenv.TrustTrusted {
		args[credentialReadArgKey] = true
		return
	}
	if scopeHasCapability(scope, executionenv.CapabilityCredentialRead) {
		args[credentialReadArgKey] = true
		return
	}
	fingerprint := executionCapabilityFingerprint(scope.WorkspaceID, executionenv.CapabilityCredentialRead)
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
	if !commandNeedsOperatorCredentials(toolName, args) {
		return
	}
	if err := approveCredentialCapability(args, scope, fingerprint); err != nil {
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

func approveCredentialCapability(args map[string]interface{}, scope ExecutionScope, fingerprint string) error {
	if scope.Approval == nil {
		return fmt.Errorf("operation rejected: credential:read requires approval")
	}
	decision, approvalErr := scope.Approval(contextFromArgs(args), ToolApprovalRequest{
		TenantID: scope.TenantID,
		PersonID: scope.PersonID,
		TaskID:   scope.TaskID,
		RunID:    scope.RunID,
		Channel:  scope.Channel,
		ToolName: executionenv.CapabilityCredentialRead,
		Reason:   "this untrusted workspace needs read access to your existing tool credentials",
		Args: map[string]interface{}{
			"capability":   executionenv.CapabilityCredentialRead,
			"workspace_id": scope.WorkspaceID,
		},
		GrantClass:          "read your existing tool credentials in this workspace",
		ResourceFingerprint: fingerprint,
	})
	if approvalErr != nil {
		return approvalErr
	}
	if !decision.Approved {
		return fmt.Errorf("operation rejected: credential:read was not approved")
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
