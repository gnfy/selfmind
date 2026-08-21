package kernel

import (
	"fmt"
	"strings"
)

// buildToolUsePrompt describes only capabilities present in defs. Tool schemas
// remain in the provider-native tools field when supported; fallback providers
// receive the textual catalog because that is their tool interface.
func buildToolUsePrompt(defs []map[string]interface{}, native bool, strategy TaskStrategy, profile AgentPromptProfile) string {
	if len(defs) == 0 {
		return ""
	}
	names := make(map[string]bool, len(defs))
	for _, def := range defs {
		if name := toolDefinitionName(def); name != "" {
			names[name] = true
		}
	}
	hasAny := func(candidates ...string) bool {
		for _, name := range candidates {
			if names[name] {
				return true
			}
		}
		return false
	}

	var sb strings.Builder
	sb.WriteString("\n# TOOL USE INSTRUCTIONS\n")
	sb.WriteString("Use only capabilities provided for this run. Do not invent tool names or imply that an unavailable action was performed.\n")
	if profile != PromptProfileBackgroundReview {
		if hasAny("read_file", "write_file", "patch", "apply_patch", "search_files", "list_files", "ls_r", "terminal", "execute_code", "run_command") {
			sb.WriteString("Use the available local tools when the request requires workspace files, command output, project state, or system status.\n")
		}
		sb.WriteString("When a tool returns an error, treat the error as diagnostic evidence. Inspect the relevant state available to you, change the next action when evidence changes, and report a real blocker instead of repeating the same call.\n")
		if hasAny("terminal", "execute_code", "run_command") {
			sb.WriteString("For command failures, inspect the working directory, files, environment, authentication, runtime, and command help before choosing an override or retry.\n")
		}
		if names["update_plan"] {
			sb.WriteString(planToolGuidance(strategy))
			if names["skill_select"] {
				sb.WriteString("When a new top-level work unit is returned, echo its work_unit_id in later plan snapshots. Use bound_skill_name or a listed skill_candidate; do not invent or activate a Skill through inspection alone.\n")
			}
		}
		if names["finish_run"] {
			sb.WriteString("For non-trivial tool-using work that creates durable task state, call finish_run once with the structured outcome after the work and plan are resolved. Skip it for direct answers and ordinary explanations.\n")
		}
		if names["tool_search"] {
			sb.WriteString("Use tool_search when you need a registered capability that is not currently active. A result activates matching deferred tools for later calls in this run.\n")
		}
		if names["watch_external"] {
			sb.WriteString("When an external operation will outlive one short check, register watch_external with explicit success and failure conditions instead of polling. Registration hands the run off as waiting_external; do not continue foreground work afterward.\n")
			if names["finish_run"] {
				sb.WriteString("After watch_external registration, do not call finish_run.\n")
			}
		}
	} else {
		sb.WriteString("This is a bounded background role. Follow its role contract and use its listed tools only for the evidence needed by that contract.\n")
	}

	if native {
		sb.WriteString("Tools are provided through the native tool-calling interface.\n")
		return sb.String()
	}
	sb.WriteString("If native tool calls are unavailable, use the exact fallback format: [TOOL:tool_name:{\"arg\": \"val\"}]\n")
	sb.WriteString("Do not emit XML-style tool tags. The only valid tool names are: ")
	for i, def := range defs {
		sb.WriteString(fmt.Sprintf("'%s'", toolDefinitionName(def)))
		if i < len(defs)-1 {
			sb.WriteString(", ")
		}
	}
	sb.WriteString(". Output only the [TOOL:...] tag for a fallback tool step.\n\n## Available Tools\n")
	for _, def := range defs {
		sb.WriteString(fmt.Sprintf("### %s\n%s\n", toolDefinitionName(def), toolDefinitionDescription(def)))
		if params := toolDefinitionParameters(def); params != nil {
			if props, ok := params["properties"].(map[string]interface{}); ok {
				sb.WriteString("Parameters:\n")
				for name, raw := range props {
					if field, ok := raw.(map[string]interface{}); ok {
						sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", name, field["type"], field["description"]))
					}
				}
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func planToolGuidance(strategy TaskStrategy) string {
	const boundary = "Call update_plan by itself; do not batch it with reads or other tools because it changes the work-unit boundary. "
	switch strategy.normalized().PlanPolicy {
	case PlanPolicyDisabled:
		return "Do not call update_plan for this turn.\n"
	case PlanPolicyRequired:
		return boundary + "Use update_plan early for this multi-step work. Every call replaces the prior plan, so send the complete snapshot, update it after meaningful transitions, and resolve every step before a done outcome.\n"
	default:
		return boundary + "Use update_plan only when the work genuinely needs multiple visible steps. Every call replaces the prior plan, so send the complete snapshot and resolve every step before a done outcome.\n"
	}
}
