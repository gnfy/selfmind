package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
				"description":             {Type: "string", Description: "Short user-facing description of what is being watched"},
				"command":                 {Type: "string", Description: "Read-only command that checks the external state"},
				"success_pattern":         {Type: "string", Description: "Regular expression that marks the watch successful when matched in command output"},
				"failure_pattern":         {Type: "string", Description: "Optional regular expression that marks the watch failed when matched"},
				"cwd":                     {Type: "string", Description: "Working directory within the active workspace", Default: "."},
				"interval_seconds":        {Type: "integer", Description: "Seconds between checks (5-300)", Default: 30},
				"timeout_seconds":         {Type: "integer", Description: "Maximum total watch duration in seconds (60-86400)", Default: 7200},
				"command_timeout_seconds": {Type: "integer", Description: "Maximum duration of one check (1-120 seconds)", Default: 30},
			},
			Required: []string{"command", "success_pattern"},
		},
		metadata: ToolMetadata{
			Category:       "terminal",
			RiskLevel:      ToolRiskHigh,
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
	if command == "" || successPattern == "" {
		return "", fmt.Errorf("command and success_pattern are required")
	}
	if _, err := regexp.Compile(successPattern); err != nil {
		return "", fmt.Errorf("invalid success_pattern: %w", err)
	}
	if failurePattern != "" {
		if _, err := regexp.Compile(failurePattern); err != nil {
			return "", fmt.Errorf("invalid failure_pattern: %w", err)
		}
	}
	interval := clampInt(intArg(args, "interval_seconds", 30), 5, 300)
	totalTimeout := clampInt(intArg(args, "timeout_seconds", 7200), 60, 86400)
	commandTimeout := clampInt(intArg(args, "command_timeout_seconds", 30), 1, 120)
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
	verdict, err := preflightExternalWatch(args, command, cwd, successPattern, failurePattern, commandTimeout)
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
	identity := executionenv.DefaultRegistry().Current()
	if identity == nil {
		identity = InstallEnvironmentSnapshot(os.Environ(), "inherited")
	}
	watch, err := t.store.CreateExternalWatch(context.Background(), control.ExternalWatch{
		TenantID:              scope.TenantID,
		PersonID:              scope.PersonID,
		WorkspaceID:           scope.WorkspaceID,
		TaskID:                scope.TaskID,
		RunID:                 scope.RunID,
		Channel:               scope.Channel,
		Description:           description,
		CWD:                   cwd,
		Command:               command,
		SuccessPattern:        successPattern,
		FailurePattern:        failurePattern,
		IntervalSeconds:       interval,
		CommandTimeoutSeconds: commandTimeout,
		TimeoutAt:             time.Now().Add(time.Duration(totalTimeout) * time.Second),

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

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
