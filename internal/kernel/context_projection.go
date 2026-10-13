package kernel

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The selector has already bounded and scoped these facts. Never cut their
// meaning to make a local slice fit: PrepareRequest owns the hard token limit.
func (r TaskRuntimeContext) requiredPrompt() string {
	var b strings.Builder
	b.WriteString("\n# DURABLE TASK CONTEXT\nSelected state, not a new request or execution permission. Later accepted tool receipts supersede this start-of-Run snapshot.\n")
	for _, kv := range []struct{ key, value string }{
		{"task_id", r.TaskID}, {"run_id", r.RunID}, {"title", r.Title}, {"status", r.Status},
		{"channel", r.Channel}, {"workspace_id", r.WorkspaceID}, {"workspace_root", r.Workspace},
	} {
		writeKV(&b, kv.key, kv.value)
	}
	if len(r.UserRequirements) > 0 {
		b.WriteString("\n## User updates to the continued work\nRecorded additions and corrections, oldest first; current user input wins. No new permissions.\n")
		for _, line := range uniqueContextLines(r.UserRequirements) {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	if len(r.Plan) > 0 {
		if r.PlanVersion > 0 {
			fmt.Fprintf(&b, "plan_version: %d\n", r.PlanVersion)
		}
		b.WriteString("\n## Current Run Plan\nCurrent Run step IDs. Preserve criteria and independent unfinished work; update_plan takes a complete snapshot that lists every step_id with its status and sends text or success_criteria only when changed. Bind verify to step_id. Explicitly justify cancellations or replacements.\n")
		for _, step := range r.Plan {
			fmt.Fprintf(&b, "- step_id=%s [%s] %s; success_criteria=%q verification_required=%t\n", step.StepID, step.Status, step.Step, step.SuccessCriteria, step.VerificationRequired)
			if step.CancellationDisposition != "" {
				fmt.Fprintf(&b, "  cancellation_disposition=%s reason=%q user_takeover_quote=%q replacement_step_id=%s scope_change_quote=%q\n", step.CancellationDisposition, step.CancellationReason, step.UserTakeoverQuote, step.ReplacementStepID, step.ScopeChangeQuote)
			}
		}
	}
	if r.Handoff != nil && (r.Handoff.TestStatus != "" || len(r.Handoff.Risks) > 0) {
		b.WriteString("\n## Verification and Risks\n")
		if r.Handoff.TestStatus != "" {
			fmt.Fprintf(&b, "- Tests: %s\n", r.Handoff.TestStatus)
		}
		for _, risk := range r.Handoff.Risks {
			fmt.Fprintf(&b, "- Risk: %s\n", risk)
		}
	}
	b.WriteString(externalEffectContextPrompt(r.ExternalEffects))
	if len(r.InheritedEvidence) > 0 {
		b.WriteString("\n## Prior Step Evidence\nHistorical observations, not automatic current verification. Main may request reuse_prior_verification=true with reuse_reason for unchanged targets and criteria; runtime checks exact provenance and scope. Refresh mutable or incomplete evidence.\n")
		for _, item := range r.InheritedEvidence {
			fmt.Fprintf(&b, "- step_id=%s source_run_id=%s source_status=%s prior_verification=%s latest_check=%s target=%q", item.StepID, item.SourceRunID, item.SourceStatus, item.PriorVerification, item.LatestCheck, item.Target)
			if item.CriterionChanged {
				fmt.Fprintf(&b, " criterion_changed=true source_criterion=%q", item.SourceCriterion)
			}
			if item.ObservedCriterion != "" && item.ObservedCriterion != item.SourceCriterion {
				fmt.Fprintf(&b, " checked_criterion=%q", item.ObservedCriterion)
			}
			if item.ReuseBlockedReason != "" {
				fmt.Fprintf(&b, " reuse_blocked=%q", item.ReuseBlockedReason)
			}
			if !item.CheckedAt.IsZero() {
				fmt.Fprintf(&b, " checked_at=%s", item.CheckedAt.Format(time.RFC3339))
				if item.Age > 0 {
					fmt.Fprintf(&b, " checked_ago=%s", EvidenceAge(item.Age))
				}
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString(r.RuntimeObservation.Prompt())
	return b.String()
}

// Optional history is admitted as complete lines, never a byte-cut prefix.
// Reserving the notice makes omitted context visible without losing the core.
func appendOptionalContext(core, optional string, budget int) string {
	if strings.TrimSpace(optional) == "" {
		return core
	}
	if len(core)+len(optional) <= budget {
		return core + optional
	}
	const notice = "\n[Optional context omitted by budget; inspect the exact Run for details.]\n"
	var b strings.Builder
	b.WriteString(core)
	for _, line := range strings.SplitAfter(optional, "\n") {
		if b.Len()+len(line)+len(notice) > budget {
			break
		}
		b.WriteString(line)
	}
	if b.Len()+len(notice) <= budget {
		b.WriteString(notice)
	}
	return b.String()
}

func selectedPlanState(ctx context.Context) (bool, []string) {
	runtime, ok := TaskRuntimeContextFromContext(ctx)
	if !ok || len(runtime.Plan) == 0 {
		return false, nil
	}
	var unresolved []string
	for _, step := range runtime.Plan {
		if step.Status == "completed" || step.Status == "cancelled" {
			continue
		}
		name := strings.TrimSpace(step.Step)
		if name == "" {
			name = "unnamed step"
		}
		unresolved = append(unresolved, name)
	}
	return true, unresolved
}
