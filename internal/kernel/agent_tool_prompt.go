package kernel

import (
	"fmt"
	"sort"
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
			sb.WriteString("Use local tools for workspace facts. Read files with read_file; search text with search_files output_mode=content; list names with ls_r. Batch independent file reads with batch_read or independent native calls. Use terminal for commands and queries those tools cannot express; dependent operations wait for prior results.\n")
		}
		sb.WriteString("When a tool returns an error, treat the error as diagnostic evidence. Inspect the relevant state available to you, change the next action when evidence changes, and report a real blocker instead of repeating the same call.\n")
		if hasAny("terminal", "execute_code", "run_command") {
			sb.WriteString("For command failures, inspect the working directory, files, environment, authentication, runtime, and command help before choosing an override or retry.\n")
			sb.WriteString("Use the runtime-provided process environment and network route by default. Execution facts report the network mode and proxy projection. Choose a per-command route override when the current user request or current diagnostics establish its need.\n")
		}
		if names["verify"] {
			sb.WriteString("Use verify for checks that establish completion: it executes the command and records verification evidence. A command run through terminal is recorded as ordinary execution, not a verification check. Run the relevant check after the final change. Skip verify for pure read-only queries or direct answers when no plan step requires verification.\n")
			sb.WriteString("Command exit status is an execution fact, not a business verdict. An inspection can finish with a grounded negative finding; a required repair condition stays unresolved. Plan criteria describe the user's requested deliverable: do not turn a repair into an inspection to close it, or rerun a conclusive inspection merely to obtain exit zero.\n")
			if names["finish_run"] {
				sb.WriteString("For negative or inconclusive findings, cite verify evidence in finish_run assessments (result, evidence_refs, reason). Runtime resolves the condition and target; do not duplicate them.\n")
			}
			sb.WriteString("Verification shell commands stop on unhandled errors and failed pipelines. Use the tool's timeout instead of assuming a timeout executable is installed; do not hide a check's exit status behind an echo, output filter, or unconditional success.\n")
		}
		if names["update_plan"] {
			sb.WriteString(planToolGuidance(strategy))
			if names["skill_select"] {
				sb.WriteString("Use bound_skill_name or a listed skill_candidate for the current work unit; do not invent or activate a Skill through inspection alone.\n")
			}
		}
		if names["finish_run"] {
			sb.WriteString("For non-trivial tool-using work that creates durable task state, call finish_run after observing the work results. Supply an explicit status and write the complete final reply once in final_response, including the final changed steps in plan_patch rather than first sending a final update_plan. Omit the patch when no Plan changes. A separate summary and repeated lists are unnecessary when that body is ready. An accepted submission is saved and shown without another generation; do not send a second copy afterward. A summary or progress narration is not the complete reply. A successful finish is final. Correct refused input without rerunning work; obtain new evidence only for an actual missing or stale completion condition. Skip it for direct answers and ordinary explanations.\n")
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
				names := make([]string, 0, len(props))
				for name := range props {
					names = append(names, name)
				}
				// Sorted, so one catalog renders one prompt and a cached prefix
				// survives the next request.
				sort.Strings(names)
				for _, name := range names {
					field, ok := props[name].(map[string]interface{})
					if !ok {
						continue
					}
					line := "- " + name
					if kind, _ := field["type"].(string); kind != "" {
						line += " (" + kind + ")"
					}
					if description, _ := field["description"].(string); strings.TrimSpace(description) != "" {
						line += ": " + strings.TrimSpace(description)
					}
					sb.WriteString(line + "\n")
				}
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func planToolGuidance(strategy TaskStrategy) string {
	const boundary = "Submit the initial Plan separately so you can read its accepted identities before choosing actions. Later leading update_plan calls may share a response with work when only existing step statuses advance within the same Work Unit, goal, scope and acceptance conditions. Runtime commits the complete accepted Plan before sequential tool checks. Changed scope, reopened or cancelled work, failure, a second Plan update or pending user input holds later actions for a new decision. At completion, submit final changed steps with the complete reply in finish_run; do not spend a separate turn restating final progress. Keep work_select and queue_user_input separate from other tools. "
	const planStepDiscipline = "Use mode=patch for changed step_id/status entries after a Plan exists; omitted steps stay unchanged. First plans, adding steps and reordering use snapshot mode (the compatible default). Results always carry the full accepted Plan. Update at meaningful progress or scope changes, not around every tool call. Main states statuses explicitly, preserves acceptance conditions and resolves every necessary step before done. Existing step IDs preserve execution attribution; work-unit IDs need not be repeated.\n"
	// planTriggers replaces an abstract test the model had to interpret with
	// the concrete situations that call for a plan.
	const planTriggers = "Main decides whether a visible Plan helps manage dependencies, ambiguity or a long horizon. Tool count and read/write classification do not decide this. Preserve existing Plans and explicit user planning or acceptance requirements. "
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
		return boundary + "Use update_plan when a Plan is useful; a bounded investigation or small change may proceed directly. When you choose to plan, outline the work before its dependent actions. " + planTriggers + planStepDiscipline
	}
}

// planStaleReminder tells the model that its visible plan has not moved for a
// while. It changes nothing: the model decides whether the plan is still true.
func planStaleReminder(actionsSinceUpdate int, openSteps []string) string {
	first := ""
	if len(openSteps) > 0 {
		first = strings.TrimSpace(openSteps[0])
	}
	return fmt.Sprintf("SelfMind: the visible plan was last updated %d tool action(s) ago and still has %d open step(s), starting with %q. If the work has moved on, send update_plan with the real progress (mode=patch for changed steps) using the Plan tool's ordering contract; if the plan is still accurate, just continue. Do not do extra tool work for the plan.\n", actionsSinceUpdate, len(openSteps), first)
}
