package tools

import (
	"context"
	"crypto/sha256"
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
	store     *control.Store
	planStore *PlanStore
}

func NewExternalWatchTool(store *control.Store) *ExternalWatchTool {
	return NewExternalWatchToolWithPlanStore(store, nil)
}

func NewExternalWatchToolWithPlanStore(store *control.Store, planStore *PlanStore) *ExternalWatchTool {
	t := &ExternalWatchTool{store: store, planStore: planStore}
	t.BaseTool = BaseTool{
		name:        "watch_external",
		description: "Register a durable daemon-side read-only check for external CI/CD or deployment state. Successful registration automatically hands off the current run as waiting_external; do not call finish_run afterward.",
		schema: ToolSchema{
			Type: "object",
			Properties: map[string]PropertyDef{
				"description": {Type: "string", Description: "Short user-facing description of what is being watched"},
				"command":     {Type: "string", Description: "Read-only command that checks the external state"},
				// A description says what the value IS. Which combination is
				// legal is enforced by ValidateExternalWatchSpec, which returns
				// a precise error, so repeating it here bought nothing and cost
				// it in every request. The internal "V1/V2/V3" spec vocabulary
				// was leaking too: the model never needed to know which
				// generation of the evaluator it was addressing.
				"success_pattern":          {Type: "string", Description: "Regular expression that marks the watch successful"},
				"failure_pattern":          {Type: "string", Description: "Regular expression that marks the watch failed"},
				"target_pattern":           {Type: "string", Description: "Intermediate state to watch for, such as PENDING_APPROVAL"},
				"terminal_success_pattern": {Type: "string", Description: "Terminal success state"},
				"terminal_failure_pattern": {Type: "string", Description: "Terminal failure state"},
				"observation_adapter":      {Type: "string", Description: "Typed observation adapter", Enum: []string{ExternalWatchAdapterStatusJSON}},
				"cwd":                      {Type: "string", Description: "Working directory within the active workspace", Default: "."},
				"interval_seconds":         {Type: "integer", Description: "Seconds between checks (5-300)", Default: 30},
				"timeout_seconds":          {Type: "integer", Description: "Maximum total watch duration in seconds (60-86400)", Default: 7200},
				"command_timeout_seconds":  {Type: "integer", Description: "Maximum duration of one check (1-120 seconds)", Default: 30},
				"wait_group":               {Type: "string", Description: "Optional run-local key shared by 2-8 independent watchers"},
				"wait_group_mode":          {Type: "string", Description: "Aggregate completion rule for a wait group", Enum: []string{control.ExternalWatchGroupAll, control.ExternalWatchGroupAny}, Default: control.ExternalWatchGroupAll},
				"wait_group_size":          {Type: "integer", Description: "Exact number of watchers in the group (2-8)"},
			},
			Required: []string{"command"},
		},
		metadata: ToolMetadata{
			Category:  "terminal",
			RiskLevel: ToolRiskHigh,
			// Registration itself is a database write, but it now runs the check
			// once first (see preflightExternalWatch), so the tool's own budget
			// must cover one bounded check plus the write.
			TimeoutSeconds: 135,
		},
	}
	return t
}

// ExternalWatchStaticValidationMiddleware rejects watcher specifications that
// cannot possibly register before approval is requested. It performs no
// external observation and consumes no network or credential capability; the
// bounded real preflight remains inside Execute, after approval.
func ExternalWatchStaticValidationMiddleware() Middleware {
	return func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			toolName, _ := args["_tool_name"].(string)
			if !strings.EqualFold(strings.TrimSpace(toolName), "watch_external") {
				return next(args)
			}
			if err := validateExternalWatchStatic(args); err != nil {
				return "", err
			}
			return next(args)
		}
	}
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
	observationAdapter := strings.TrimSpace(stringArg(args, "observation_adapter"))
	specVersion := 1
	if observationAdapter != "" {
		specVersion = 3
	} else if targetPattern != "" || terminalSuccessPattern != "" || terminalFailurePattern != "" {
		specVersion = 2
	}
	if command == "" {
		return "", fmt.Errorf("command is required")
	}
	if err := validateExternalWatchStatic(args); err != nil {
		return "", err
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
	waitGroupKey := strings.TrimSpace(stringArg(args, "wait_group"))
	waitGroupMode := strings.TrimSpace(stringArg(args, "wait_group_mode"))
	waitGroupSize := intArg(args, "wait_group_size", 0)

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
	if specVersion >= 3 {
		patterns = externalWatchPreflightPatterns{Adapter: observationAdapter}
	} else if specVersion >= 2 {
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
	// registered, so the effective set is resolved here from current trust,
	// current grants, and this call's one-shot approvals.
	activeGrants, err := t.store.ListActiveExecutionCapabilities(
		context.Background(), scope.TenantID, scope.PersonID, scope.WorkspaceID)
	if err != nil {
		return "", fmt.Errorf("freeze external watch capabilities: %w", err)
	}
	capabilities := externalWatchEffectiveCapabilities(args, scope, activeGrants)
	binding := executionenv.BindingFromLease("", *lease, scope.TrustLevel, capabilities, identity)
	binding.CapabilityBindings = externalWatchCapabilityBindings(
		binding.ExecutionCapabilities, scope, activeGrants, timeoutAt)
	waitGroupID := ""
	if waitGroupKey != "" {
		group, groupErr := t.store.ResolveOrCreateExternalWatchGroup(context.Background(), scope.TenantID, scope.PersonID,
			scope.TaskID, scope.RunID, waitGroupKey, waitGroupMode, waitGroupSize)
		if groupErr != nil {
			return "", groupErr
		}
		waitGroupID = group.ID
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
		ObservationAdapter:     observationAdapter,
		PreflightReceipt: control.ExternalWatchPreflightReceipt{
			Version: 1, CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command))),
			EnvironmentGeneration: identity.Generation, Adapter: observationAdapter,
			Target: firstNonEmptyPreflight(targetPattern, description), DeadlineUnix: timeoutAt.Unix(),
			Capabilities: append([]string(nil), capabilities...),
		},
		WaitGroupID:           waitGroupID,
		IntervalSeconds:       interval,
		CommandTimeoutSeconds: commandTimeout,
		TimeoutAt:             timeoutAt,
		ExecutionBinding:      binding,

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
	// Successful registration ends this run without a second model-authored
	// finish_run call. Purge the shared in-memory plan here so the automatic
	// lifecycle handoff has the same bounded plan-store lifetime as finish_run.
	if t.planStore != nil {
		t.planStore.Purge(planKey(args))
	}
	message := fmt.Sprintf("Watcher %s is running in the background for %s. You can start another task now; SelfMind will notify you when it reaches a terminal state.", watch.ID, watch.Description)
	result, err := json.Marshal(map[string]interface{}{
		"watch_id":    watch.ID,
		"description": watch.Description,
		"registered":  true,
		"message":     message,
		"lifecycle_handoff": map[string]interface{}{
			"status":       "waiting_external",
			"summary":      fmt.Sprintf("Watching %s in the background.", watch.Description),
			"done":         []string{fmt.Sprintf("Registered durable watcher %s.", watch.ID)},
			"next_steps":   []string{"SelfMind will notify you when the watcher reaches a terminal state."},
			"need_approve": false,
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode external watch registration: %w", err)
	}
	return string(result), nil
}

func validateExternalWatchObservation(args map[string]interface{}) error {
	if observationOnlyExec("watch_external", args) {
		return nil
	}
	return fmt.Errorf("watch not registered: command is not a proven read-only observation; use a supported status command or an operator-approved hash-pinned observation script")
}

func validateExternalWatchStatic(args map[string]interface{}) error {
	command := strings.TrimSpace(stringArg(args, "command"))
	if command == "" {
		return invalidExternalWatchSpec(fmt.Errorf("command is required"))
	}
	if err := validateExternalWatchObservation(args); err != nil {
		return newStableToolRecoveryError(
			err,
			"watch_observation_unsupported", "capability_unavailable",
			"The durable watcher cannot prove this command is a read-only observation, so registration was not attempted.",
			"Choose a genuinely different observation strategy; do not retry cosmetic command variants.",
			"preparation", "different_strategy", "not_dispatched", false,
			"one_shot_observation", "provider_native_wait", "local_process_handle", "hash_pinned_observation_script", "actionable_blocker",
		)
	}
	successPattern := strings.TrimSpace(stringArg(args, "success_pattern"))
	targetPattern := strings.TrimSpace(stringArg(args, "target_pattern"))
	terminalSuccessPattern := strings.TrimSpace(stringArg(args, "terminal_success_pattern"))
	terminalFailurePattern := strings.TrimSpace(stringArg(args, "terminal_failure_pattern"))
	observationAdapter := strings.TrimSpace(stringArg(args, "observation_adapter"))
	waitGroupKey := strings.TrimSpace(stringArg(args, "wait_group"))
	waitGroupMode := strings.TrimSpace(stringArg(args, "wait_group_mode"))
	waitGroupSize := intArg(args, "wait_group_size", 0)
	specVersion := 1
	if observationAdapter != "" {
		specVersion = 3
	} else if targetPattern != "" || terminalSuccessPattern != "" || terminalFailurePattern != "" {
		specVersion = 2
	}
	if specVersion == 1 && successPattern == "" {
		return invalidExternalWatchSpec(fmt.Errorf("success_pattern is required for watch spec v1"))
	}
	if waitGroupKey == "" && (waitGroupMode != "" || waitGroupSize != 0) {
		return invalidExternalWatchSpec(fmt.Errorf("wait_group_mode and wait_group_size require wait_group"))
	}
	if waitGroupKey != "" {
		if waitGroupMode != "" && waitGroupMode != control.ExternalWatchGroupAll && waitGroupMode != control.ExternalWatchGroupAny {
			return invalidExternalWatchSpec(fmt.Errorf("wait_group_mode must be all or any"))
		}
		if waitGroupSize < 2 || waitGroupSize > 8 {
			return invalidExternalWatchSpec(fmt.Errorf("wait_group_size must be between 2 and 8"))
		}
	}
	if err := control.ValidateExternalWatchSpec(control.ExternalWatch{
		SpecVersion:            specVersion,
		ObservationAdapter:     observationAdapter,
		SuccessPattern:         successPattern,
		TargetPattern:          targetPattern,
		TerminalSuccessPattern: terminalSuccessPattern,
		TerminalFailurePattern: terminalFailurePattern,
	}); err != nil {
		return invalidExternalWatchSpec(err)
	}
	for name, pattern := range map[string]string{
		"success_pattern":          successPattern,
		"failure_pattern":          strings.TrimSpace(stringArg(args, "failure_pattern")),
		"target_pattern":           targetPattern,
		"terminal_success_pattern": terminalSuccessPattern,
		"terminal_failure_pattern": terminalFailurePattern,
	} {
		if pattern == "" {
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return invalidExternalWatchSpec(fmt.Errorf("invalid %s: %w", name, err))
		}
	}
	return nil
}

func invalidExternalWatchSpec(err error) error {
	return newStableToolRecoveryError(
		err,
		"watch_spec_invalid", "invalid_request",
		"The durable watcher specification is invalid, so registration was not attempted.",
		"Correct the watcher command or state patterns once before choosing another strategy.",
		"preparation", "same_strategy_after_correction", "not_dispatched", false,
	)
}

// externalWatchEffectiveCapabilities resolves the capabilities that are
// effective for THIS registration call, the same way the foreground middleware
// resolves them for an ordinary command.
//
// It cannot rely on `_network_shared` alone. ExecutionCapabilityMiddleware
// returns before it decides anything whenever the EFFECTIVE sandbox mode is
// `host` — the normal mode on macOS, and on any Linux host without an available
// sandbox — because host networking is part of the broader, separately approved
// host boundary. The arg then never arrived, so every such watch froze an EMPTY
// capability set and every later poll refused with "does not include
// network:shared", moments after the registration preflight had proven the same
// command works with egress.
//
// Resolution adds no authority. network:shared comes from workspace trust plus
// the operator sandbox policy (exactly the middleware's own condition), from a
// live workspace grant, or from the one-shot approval the middleware recorded
// for this call. credential:read keeps its single existing source: the decision
// the middleware already made for this call.
func externalWatchEffectiveCapabilities(
	args map[string]interface{},
	scope ExecutionScope,
	activeGrants []executionenv.CapabilityGrant,
) []string {
	granted := func(capability string) bool {
		for _, grant := range activeGrants {
			if grant.Capability == capability {
				return true
			}
		}
		return false
	}
	networkShared, _ := args["_network_shared"].(bool)
	if !networkShared {
		networkShared = scope.TrustLevel == executionenv.TrustTrusted && ExecSandboxAllowsNetwork()
	}
	if !networkShared {
		networkShared = granted(executionenv.CapabilityNetworkShared)
	}
	capabilities := make([]string, 0, 2)
	if networkShared {
		capabilities = append(capabilities, executionenv.CapabilityNetworkShared)
	}
	if enabled, _ := args[credentialReadArgKey].(bool); enabled {
		capabilities = append(capabilities, executionenv.CapabilityCredentialRead)
	}
	return capabilities
}

// externalWatchCapabilityBindings records WHY each frozen capability was
// available, because provenance is what a later poll re-validates: a
// trust-derived capability must disappear when workspace trust is withdrawn (and
// network:shared additionally when the operator sandbox policy stops allowing
// egress), a grant-derived one dies with its grant, and a one-shot registration
// approval is bounded by the watch deadline. Recording a trust-derived
// capability as a registration approval instead would let it outlive the trust
// decision it came from.
func externalWatchCapabilityBindings(
	capabilities []string,
	scope ExecutionScope,
	activeGrants []executionenv.CapabilityGrant,
	deadline time.Time,
) []executionenv.CapabilityBinding {
	bindings := make([]executionenv.CapabilityBinding, 0, len(capabilities))
	for _, capability := range capabilities {
		bound := executionenv.CapabilityBinding{
			Capability: capability,
			Source:     executionenv.CapabilitySourceRegistration,
			ExpiresAt:  deadline,
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
		bindings = append(bindings, bound)
	}
	return bindings
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
