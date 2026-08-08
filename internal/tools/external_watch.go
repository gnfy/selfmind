package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

// ExternalWatchTool registers a durable daemon-side condition check. The
// registration goes through the normal workspace, guardrail, and approval
// middleware; later checks execute the frozen command without holding an agent
// turn or consuming model tokens.
type ExternalWatchTool struct {
	BaseTool
	store *control.Store
}

func NewExternalWatchTool(store *control.Store) *ExternalWatchTool {
	t := &ExternalWatchTool{store: store}
	t.BaseTool = BaseTool{
		name:        "watch_external",
		description: "Register a durable daemon-side check for external CI/CD or deployment state, then end the current run with status waiting_external",
		schema: ToolSchema{
			Type: "object",
			Properties: map[string]PropertyDef{
				"description":              {Type: "string", Description: "Short user-facing description of what is being watched"},
				"command":                  {Type: "string", Description: "Read-only command that checks the external state"},
				"success_pattern":          {Type: "string", Description: "V1 regular expression that marks the watch successful"},
				"failure_pattern":          {Type: "string", Description: "V1 optional regular expression that marks the watch failed"},
				"target_pattern":           {Type: "string", Description: "V2 desired handoff state, such as PENDING_APPROVAL; when set, both terminal patterns are required because an external system may skip this state"},
				"terminal_success_pattern": {Type: "string", Description: "V2 terminal success state; required with target_pattern so a skipped target can still finish successfully"},
				"terminal_failure_pattern": {Type: "string", Description: "V2 terminal failure state; required with target_pattern and evaluated before success and target"},
				"cwd":                      {Type: "string", Description: "Working directory within the active workspace", Default: "."},
				"interval_seconds":         {Type: "integer", Description: "Seconds between checks (5-300)", Default: 30},
				"timeout_seconds":          {Type: "integer", Description: "Maximum total watch duration in seconds (60-86400)", Default: 7200},
				"command_timeout_seconds":  {Type: "integer", Description: "Maximum duration of one check (1-120 seconds)", Default: 30},
			},
			Required: []string{"command"},
		},
		metadata: ToolMetadata{
			Category:  "terminal",
			RiskLevel: ToolRiskHigh,
			// Registration itself is a database write, but it now runs the check
			// once first (see preflightExternalWatch), so the tool's own budget
			// must cover one bounded check plus the write.
			TimeoutSeconds: 45,
		},
	}
	return t
}

func (t *ExternalWatchTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("external watch storage is unavailable")
	}
	scope, ok := currentExecutionScopeAny(args)
	if !ok || strings.TrimSpace(scope.PersonID) == "" || strings.TrimSpace(scope.TaskID) == "" {
		return "", fmt.Errorf("external watch requires an active task execution scope")
	}
	command := strings.TrimSpace(stringArg(args, "command"))
	successPattern := strings.TrimSpace(stringArg(args, "success_pattern"))
	failurePattern := strings.TrimSpace(stringArg(args, "failure_pattern"))
	targetPattern := strings.TrimSpace(stringArg(args, "target_pattern"))
	terminalSuccessPattern := strings.TrimSpace(stringArg(args, "terminal_success_pattern"))
	terminalFailurePattern := strings.TrimSpace(stringArg(args, "terminal_failure_pattern"))
	specVersion := 1
	if targetPattern != "" || terminalSuccessPattern != "" || terminalFailurePattern != "" {
		specVersion = 2
	}
	if command == "" {
		return "", fmt.Errorf("command is required")
	}
	if specVersion == 1 && successPattern == "" {
		return "", fmt.Errorf("success_pattern is required for watch spec v1")
	}
	if err := control.ValidateExternalWatchSpec(control.ExternalWatch{
		SpecVersion:            specVersion,
		SuccessPattern:         successPattern,
		TargetPattern:          targetPattern,
		TerminalSuccessPattern: terminalSuccessPattern,
		TerminalFailurePattern: terminalFailurePattern,
	}); err != nil {
		return "", err
	}
	for name, pattern := range map[string]string{
		"success_pattern": successPattern, "failure_pattern": failurePattern,
		"target_pattern": targetPattern, "terminal_success_pattern": terminalSuccessPattern,
		"terminal_failure_pattern": terminalFailurePattern,
	} {
		if pattern != "" {
			if _, err := regexp.Compile(pattern); err != nil {
				return "", fmt.Errorf("invalid %s: %w", name, err)
			}
		}
	}
	interval := clampInt(intArg(args, "interval_seconds", 30), 5, 300)
	totalTimeout := clampInt(intArg(args, "timeout_seconds", 7200), 60, 86400)
	commandTimeout := clampInt(intArg(args, "command_timeout_seconds", 30), 1, 120)
	timeoutAt := time.Now().Add(time.Duration(totalTimeout) * time.Second)
	cwd := strings.TrimSpace(stringArg(args, "cwd"))
	if cwd == "" {
		cwd = scope.WorkspaceRoot
	}
	description := strings.TrimSpace(stringArg(args, "description"))
	if description == "" {
		description = "External operation"
	}

	// Preflight: run the frozen check ONCE, here, with this run's material.
	//
	// Both live watcher failures were unrecoverable in the background — the
	// durable path has no host escape hatch and no model to diagnose it — and
	// both were detectable at registration. Running the check while the agent is
	// still in its turn converts "two hours of retries plus a misleading verdict"
	// into an error the model can act on immediately.
	//
	// It adds no approval surface: registration already passed approval, and the
	// same command was going to run unattended seconds later.
	patterns := externalWatchPreflightPatterns{Success: successPattern, Failure: failurePattern}
	if specVersion >= 2 {
		patterns = externalWatchPreflightPatterns{
			Target: targetPattern, TerminalSuccess: terminalSuccessPattern, TerminalFailure: terminalFailurePattern,
		}
	}
	verdict, err := preflightExternalWatchPatterns(args, command, cwd, patterns, commandTimeout)
	if err != nil {
		return "", err
	}
	if verdict != "" {
		return verdict, nil
	}

	// Record the environment this watch was registered under. A watch outlives
	// its run and survives restarts, so without its own identity it would
	// silently adopt whatever account the daemon has later — the check would
	// still "succeed", against a different project.
	lease, err := t.store.GetExecutionLeaseByRun(context.Background(), scope.TenantID, scope.RunID)
	if err != nil {
		return "", fmt.Errorf("load execution lease for external watch: %w", err)
	}
	if lease == nil {
		return "", fmt.Errorf("external watch requires the creating run's execution lease")
	}
	leaseBinding := executionenv.BindingFromLease(lease.ID, *lease, scope.TrustLevel, nil, nil)
	identity, err := executionenv.ResolveBinding(executionenv.DefaultRegistry(), leaseBinding)
	if err != nil {
		return "", fmt.Errorf("resolve external watch execution environment: %w", err)
	}
	// Freeze only capabilities that were effective for THIS registration call.
	// A lease records what was available when its run started; copying that list
	// could revive a grant that expired or was revoked before the watch was
	// registered. The middleware resolves these booleans immediately before
	// Execute, after consulting current trust, grants, and one-shot approvals.
	capabilities := externalWatchEffectiveCapabilities(args)
	binding := executionenv.BindingFromLease("", *lease, scope.TrustLevel, capabilities, identity)
	activeGrants, err := t.store.ListActiveExecutionCapabilities(
		context.Background(), scope.TenantID, scope.PersonID, scope.WorkspaceID)
	if err != nil {
		return "", fmt.Errorf("freeze external watch capabilities: %w", err)
	}
	for _, capability := range binding.ExecutionCapabilities {
		bound := executionenv.CapabilityBinding{
			Capability: capability,
			Source:     executionenv.CapabilitySourceRegistration,
			ExpiresAt:  timeoutAt,
		}
		for _, grant := range activeGrants {
			if grant.Capability == capability {
				bound.Source = executionenv.CapabilitySourceGrant
				bound.GrantID = grant.ID
				bound.ResourceFingerprint = grant.ResourceFingerprint
				bound.ExpiresAt = grant.ExpiresAt
				break
			}
		}
		if bound.Source == executionenv.CapabilitySourceRegistration && scope.TrustLevel == executionenv.TrustTrusted {
			if capability == executionenv.CapabilityCredentialRead ||
				(capability == executionenv.CapabilityNetworkShared && ExecSandboxAllowsNetwork()) {
				bound.Source = executionenv.CapabilitySourceTrust
				bound.ExpiresAt = time.Time{}
			}
		}
		binding.CapabilityBindings = append(binding.CapabilityBindings, bound)
	}
	watch, err := t.store.CreateExternalWatch(context.Background(), control.ExternalWatch{
		TenantID:               scope.TenantID,
		PersonID:               scope.PersonID,
		WorkspaceID:            scope.WorkspaceID,
		TaskID:                 scope.TaskID,
		RunID:                  scope.RunID,
		Channel:                scope.Channel,
		Description:            description,
		CWD:                    cwd,
		Command:                command,
		SuccessPattern:         successPattern,
		FailurePattern:         failurePattern,
		SpecVersion:            specVersion,
		TargetPattern:          targetPattern,
		TerminalSuccessPattern: terminalSuccessPattern,
		TerminalFailurePattern: terminalFailurePattern,
		IntervalSeconds:        interval,
		CommandTimeoutSeconds:  commandTimeout,
		TimeoutAt:              timeoutAt,
		ExecutionBinding:       binding,

		EnvironmentSnapshotID:  identity.ID,
		EnvironmentGeneration:  identity.Generation,
		PrincipalFingerprint:   identity.PrincipalFingerprint,
		EnvironmentFingerprint: identity.EnvironmentFingerprint,
		CredentialSourceHash:   identity.CredentialSourceHash,
	})
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"watch_id":         watch.ID,
		"description":      watch.Description,
		"interval_seconds": watch.IntervalSeconds,
		"timeout_at":       watch.TimeoutAt.Format(time.RFC3339),
	})
	_, _ = t.store.AppendEvent(context.Background(), control.Event{
		TaskID:     watch.TaskID,
		RunID:      watch.RunID,
		Type:       "external_watch.created",
		Visibility: "task",
		Channel:    watch.Channel,
		Payload:    payload,
	})
	return fmt.Sprintf("External watch registered: %s (%s). End this turn with finish_run status waiting_external. The daemon will notify the user when it completes, fails, or times out.", watch.ID, watch.Description), nil
}

func externalWatchEffectiveCapabilities(args map[string]interface{}) []string {
	capabilities := make([]string, 0, 2)
	if enabled, _ := args["_network_shared"].(bool); enabled {
		capabilities = append(capabilities, executionenv.CapabilityNetworkShared)
	}
	if enabled, _ := args[credentialReadArgKey].(bool); enabled {
		capabilities = append(capabilities, executionenv.CapabilityCredentialRead)
	}
	return capabilities
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
