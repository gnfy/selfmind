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
			sb.WriteString("Do not repeatedly poll external state in the model loop. When watch_external supports a proven read-only observation, it can hand the run off as waiting_external. If preparation says the watcher is unsupported, use the returned alternatives to choose a genuinely different strategy—such as one bounded observation, a provider-native wait, or an existing local process handle—and park with an actionable blocker when none is available.\n")
			// One sentence here replaces the same rule repeated across five
			// parameter descriptions. This text is conditioned on the tool
			// actually being offered; a schema description is not.
			sb.WriteString("Pick ONE watch_external completion mode: a success (and optional failure) regex; or a target state with both terminal patterns; or a typed observation adapter. Mixing modes is rejected.\n")
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
	// planStepDiscipline governs a plan that exists, under either policy. The
	// transition rules exist because a reluctant model produced its first
	// snapshot only after the work was already done, so the person's first
	// sight of the plan was several steps retroactively marked completed
	// (observed live 2026-09-03 with deepseek-v4-flash: 4/5 done on first
	// appearance). A plan that only records history is not progress the person
	// can follow.
	const planStepDiscipline = "Every call replaces the prior plan, so send the complete snapshot. Move a step to in_progress before you work on it and mark it completed before the next command: never jump a step straight from pending to completed, and never batch-complete several steps after the fact. Keep the plan current while you work, and resolve every step before a done outcome.\n"
	// planTriggers replaces an abstract test the model had to interpret with
	// the concrete situations that call for a plan.
	const planTriggers = "Plan when the work spans several actions over a long horizon, has ordered phases or dependencies, carries ambiguity worth outlining before acting, answers more than one request at once, or grows extra steps while you work. "
	switch strategy.normalized().PlanPolicy {
	case PlanPolicyDisabled:
		return "Do not call update_plan for this turn.\n"
	case PlanPolicyRequired:
		return boundary + "Use update_plan early for this multi-step work. " + planStepDiscipline
	default:
		// Neutral and time-bound rather than discouraging. The former "only
		// when …" wording read as a reason not to plan, and it said nothing
		// about WHEN. The decision stays the model's; only its timing and its
		// triggers are stated.
		return boundary + "When this work will take multiple visible steps, call update_plan BEFORE the first action tool so the person can see the shape of the work from the start; a single-step turn needs no plan. " + planTriggers + planStepDiscipline
	}
}

// planGuidanceEscalationNudge carries the PlanPolicyRequired wording into a Run
// whose system prompt was composed once, before the work proved multi-step. It
// states the observed evidence, hands over the same required wording the
// composer would have written, and keeps completion available so the escalation
// cannot turn into busywork or block a finish.
func planGuidanceEscalationNudge(strategy TaskStrategy, planEvidenceTools int) string {
	return fmt.Sprintf("SelfMind observed %d substantive tool action(s) in this run and no visible plan, so this work is multi-step in practice. ", planEvidenceTools) +
		planToolGuidance(strategy.WithPlanRequired()) +
		"Do not perform extra tool work for the plan itself: describe the work already done and what genuinely remains, then continue. If only one step remains, send a one-step snapshot and finish normally.\n"
}
