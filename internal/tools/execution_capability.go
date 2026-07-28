package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/executionenv"
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
		if expiry := capabilityExpiry(decision.Scope); !expiry.IsZero() {
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
