package tools

import (
	"fmt"
	"time"
)

type ToolExecutionClass string

const (
	ToolExecutionStandard    ToolExecutionClass = "standard"
	ToolExecutionLongRunning ToolExecutionClass = "long-running"
	ToolExecutionInteractive ToolExecutionClass = "interactive"
)

// ToolProfile is a provider- and vendor-neutral execution contract. Agent
// CLIs, compilers, deployment tools, and future command-line integrations use
// the same classes rather than accumulating per-command timeout branches.
type ToolProfile struct {
	Class             ToolExecutionClass
	Timeout           time.Duration
	MaxTimeout        time.Duration
	RequestedTimeout  time.Duration
	TimeoutClamped    bool
	HeartbeatInterval time.Duration
}

func resolveToolProfile(args map[string]interface{}, standardDefaultSeconds int) (ToolProfile, error) {
	if standardDefaultSeconds <= 0 {
		standardDefaultSeconds = 30
	}
	class := ToolExecutionClass(stringArg(args, "execution_class"))
	if class == "" {
		class = ToolExecutionStandard
	}
	profile := ToolProfile{Class: class}
	switch class {
	case ToolExecutionStandard:
		profile.Timeout = time.Duration(standardDefaultSeconds) * time.Second
		profile.MaxTimeout = 15 * time.Minute
		profile.HeartbeatInterval = time.Second
	case ToolExecutionLongRunning:
		profile.Timeout = 30 * time.Minute
		profile.MaxTimeout = 2 * time.Hour
		profile.HeartbeatInterval = 10 * time.Second
	case ToolExecutionInteractive:
		profile.Timeout = time.Minute
		profile.MaxTimeout = 15 * time.Minute
		profile.HeartbeatInterval = time.Second
	default:
		return ToolProfile{}, fmt.Errorf("unsupported execution_class %q", class)
	}
	if value, ok := args["timeout"]; ok {
		seconds := 0
		switch typed := value.(type) {
		case int:
			seconds = typed
		case float64:
			seconds = int(typed)
		}
		if seconds > 0 {
			requested := time.Duration(seconds) * time.Second
			profile.RequestedTimeout = requested
			if requested > profile.MaxTimeout {
				requested = profile.MaxTimeout
				profile.TimeoutClamped = true
			}
			profile.Timeout = requested
		}
	}
	return profile, nil
}

func timeoutSummary(profile ToolProfile) string {
	if profile.TimeoutClamped {
		return fmt.Sprintf(
			"%d seconds (requested %d seconds; clamped by %s execution_class maximum)",
			int(profile.Timeout/time.Second),
			int(profile.RequestedTimeout/time.Second),
			profile.Class,
		)
	}
	return fmt.Sprintf("%d seconds", int(profile.Timeout/time.Second))
}

func toolExecutionClassProperty() PropertyDef {
	return PropertyDef{
		Type:        "string",
		Description: "Execution duration profile: standard for ordinary commands, long-running for Agent CLIs/builds/deployments, interactive for commands that emit frequent progress",
		Enum:        []string{string(ToolExecutionStandard), string(ToolExecutionLongRunning), string(ToolExecutionInteractive)},
		Default:     string(ToolExecutionStandard),
	}
}
