package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"selfmind/internal/kernel"
	"selfmind/internal/verification"
)

type planProjectionSinkContextKey struct{}

type PlanWorkUnitIdentity struct {
	ID             string `json:"id"`
	Sequence       int    `json:"sequence"`
	Goal           string `json:"goal"`
	PlanStatus     string `json:"plan_status"`
	BoundSkillName string `json:"bound_skill_name,omitempty"`
	// SkillCatalog is already rendered by the kernel's canonical byte/token
	// allocator. Keeping it as one bounded string prevents update_plan from
	// duplicating every full description in structured JSON.
	SkillCatalog string `json:"skill_catalog,omitempty"`
}

type PlanProjectionResult struct {
	ClosureFacts     *verification.ClosureFacts `json:"closure_facts,omitempty"`
	AcceptanceReview []string                   `json:"acceptance_review,omitempty"`
	Plan             PlanState                  `json:"plan"`
	Version          int                        `json:"version"`
	Changed          bool                       `json:"changed"`
	WorkUnits        []PlanWorkUnitIdentity     `json:"work_units,omitempty"`
}

// RunPlanProjection is the deep persistence seam for plan versioning,
// work-unit attribution, and completion validation. Production uses the
// control-backed adapter; focused tool tests may use an in-memory adapter.
type RunPlanProjection interface {
	Project(context.Context, PlanState) (PlanProjectionResult, error)
	ValidateCompletion(context.Context) error
}

type PlanProjectionSink func(context.Context, []PlanStep) ([]PlanWorkUnitIdentity, error)

type legacyPlanProjection struct{ sink PlanProjectionSink }

func (l legacyPlanProjection) Project(ctx context.Context, state PlanState) (PlanProjectionResult, error) {
	if state.Mode == "patch" {
		return PlanProjectionResult{}, fmt.Errorf("patch mode requires the durable versioned Plan adapter")
	}
	units, err := l.sink(ctx, state.Plan)
	return PlanProjectionResult{Plan: state, Changed: true, WorkUnits: units}, err
}

func (legacyPlanProjection) ValidateCompletion(context.Context) error { return nil }

func WithPlanProjectionSink(ctx context.Context, sink PlanProjectionSink) context.Context {
	if ctx == nil || sink == nil {
		return ctx
	}
	return context.WithValue(ctx, planProjectionSinkContextKey{}, RunPlanProjection(legacyPlanProjection{sink: sink}))
}

func WithRunPlanProjection(ctx context.Context, projection RunPlanProjection) context.Context {
	if ctx == nil || projection == nil {
		return ctx
	}
	return context.WithValue(ctx, planProjectionSinkContextKey{}, projection)
}

func runPlanProjectionFromArgs(args map[string]interface{}) RunPlanProjection {
	ctx := ContextFromArgs(args)
	if ctx == nil {
		return nil
	}
	projection, _ := ctx.Value(planProjectionSinkContextKey{}).(RunPlanProjection)
	return projection
}

type PlanTool struct {
	BaseTool
	store *PlanStore
}

type PlanStore struct {
	mu    sync.RWMutex
	plans map[string]PlanState
	last  map[string]PlanState
}

type PlanState struct {
	// Mode controls input expansion only; every receipt remains a full snapshot.
	Mode        string     `json:"-"`
	Explanation string     `json:"explanation,omitempty"`
	Plan        []PlanStep `json:"plan"`
}

type PlanStep struct {
	CancellationDisposition string `json:"cancellation_disposition,omitempty"`
	CancellationReason      string `json:"cancellation_reason,omitempty"`
	UserTakeoverQuote       string `json:"user_takeover_quote,omitempty"`
	ReplacementStepID       string `json:"replacement_step_id,omitempty"`
	ScopeChangeQuote        string `json:"scope_change_quote,omitempty"`
	// Result-only: the input a quote came from (plan_provenance.go).
	CancellationAuthorizedBy *kernel.PlanQuoteOrigin `json:"cancellation_authorized_by,omitempty"`

	StepID                 string `json:"step_id,omitempty"`
	Step                   string `json:"step"`
	Status                 string `json:"status"`
	SuccessCriteria        string `json:"success_criteria,omitempty"`
	VerificationRequired   bool   `json:"verification_required,omitempty"`
	ReusePriorVerification bool   `json:"reuse_prior_verification,omitempty"`
	ReuseReason            string `json:"reuse_reason,omitempty"`
	WorkUnitID             string `json:"work_unit_id,omitempty"`
	WorkUnit               bool   `json:"work_unit,omitempty"`
}

func NewPlanStore() *PlanStore {
	return &PlanStore{
		plans: make(map[string]PlanState),
		last:  make(map[string]PlanState),
	}
}

func NewUpdatePlanTool() *PlanTool {
	return NewUpdatePlanToolWithStore(NewPlanStore())
}

func NewUpdatePlanToolWithStore(store *PlanStore) *PlanTool {
	if store == nil {
		store = NewPlanStore()
	}
	return &PlanTool{
		BaseTool: BaseTool{
			name:        "update_plan",
			description: "Maintain an outcome plan for multi-step work. Use mode=patch with changed step_id/status entries; omitted steps stay unchanged. First plans, adding or reordering steps use snapshot (compatible default). Submit the first snapshot alone and use server-issued ids. Receipts return the full accepted Plan. Main judges status, cancellation and evidence sufficiency; keep necessary work open. Use declared cancellation/reuse fields; resolve necessary work before done.",
			schema: ToolSchema{
				Type:                 "object",
				AdditionalProperties: rejectAdditionalProperties(),
				Properties: map[string]PropertyDef{
					"mode": {Type: "string", Enum: []string{"snapshot", "patch"}, Description: "snapshot (default): list every step; patch: change named current steps and preserve the rest. Runtime checks the selected version."},
					"explanation": {
						Type:        "string",
						Description: "Optional short reason for the plan update.",
					},
					"plan": {
						Type:        "array",
						Description: "Snapshot mode: all steps in order. Patch mode: changed current steps only, each with step_id and explicit status; omission preserves other steps. First plans and new steps require text.",
						Items: &PropertyDef{
							Type:                 "object",
							AdditionalProperties: rejectAdditionalProperties(),
							Properties: map[string]PropertyDef{
								"step_id": {
									Type:        "string",
									Description: "Stable step id returned by an earlier update_plan result. Echo it when updating or reordering that step; never invent one. The first plan has none: leave it out (ids sent in a first plan are ignored).",
								},
								"step": {
									Type:        "string",
									Description: "A concise task step. Required for every step of the first plan and for any new step. Omit it only when echoing a step_id that an earlier update_plan result issued and the wording is unchanged; omission then preserves the stored text.",
								},
								"cancellation_disposition": {Type: "string", Enum: []string{"not_required", "user_takeover", "unfinished"}, Description: "Main's assessment against the original goal and user corrections. Cancel only work no longer necessary (not_required), or explicitly taken over by the user (user_takeover). A blocked necessary step is unfinished and remains pending, not resolved. Omission also keeps a cancelled obligation pending."},
								"cancellation_reason":      {Type: "string", Description: "Why this step is no longer necessary or within the user's explicit takeover scope. Suggesting the user do the work is not a takeover."},
								"replacement_step_id":      {Type: "string", Description: "For not_required: an exact step id in this Plan that is completed and covers this step. The replacement repeats this step's established success_criteria exactly and keeps its required verification; for a step planned without success_criteria, the replacement states its own. A preparation or failed check does not replace the requested result."},
								"scope_change_quote":       {Type: "string", Description: "For not_required without a completed replacement: actual user text establishing the changed or limited scope, copied exactly from one user input, never including any part of a question: the whole user message, one whole sentence of it, or a span of at least two words and eight letters (a CJK character counts twice). Explain why that instruction makes this step unnecessary. Runtime verifies quote provenance; Main judges its meaning."},
								"user_takeover_quote":      {Type: "string", Description: "Exact quote of the actual user's explicit takeover instruction, required for user_takeover, never including any part of a question: the whole user message, one whole sentence of it, or a span of at least two words and eight letters (a CJK character counts twice). Never quote your own suggestion, a tool result or daemon-originated text."},
								"status": {
									Type:        "string",
									Description: "One of pending, in_progress, completed, cancelled.",
									Enum:        []string{"pending", "in_progress", "completed", "cancelled"},
								},
								"success_criteria": {
									Type:        "string",
									Description: "Observable condition that proves this step is complete. State what would be true, not what you will do. For an existing step omit it unless the criterion changes; omission preserves the stored criterion. Judge completion against the original condition; explain a user-authorized scope change instead of weakening it to fit the result.",
								},
								// The wording used to be "True ONLY when this step
								// cannot be considered complete without executable
								// verification evidence required by the user,
								// repository instructions, or the nature of the
								// change" — an exception with three qualifiers,
								// which reads as rare. It was left false on every
								// step of every plan, including one whose own step
								// was "verify and report" with the criterion
								// "re-check the actual state with gh api". The
								// completion gate that reads this flag
								// (Store.ValidateRunCompletion) was therefore never
								// armed. Tie the flag to the criterion the model has
								// already written instead of asking it to classify
								// the step.
								"verification_required": {
									Type:        "boolean",
									Description: "True only when this milestone requires a successful executed check. verify or an explicitly associated confirmed command observation can supply it. Static inspection/reporting uses false and may complete with a grounded negative finding. Do not drop a required repair gate or create a retry milestone to escape it. On an existing step omit this field to keep the stored requirement.",
									Default:     false,
								},
								"reuse_prior_verification": {
									Type:        "boolean",
									Description: "On an exact Run continuation only: Main may adopt an unchanged successful prior check with a reason. The runtime checks direct source provenance, criterion, scope and recorded changes to the check's declared inputs across waits; unrelated writes do not invalidate explicitly independent checks. Omit success_criteria or repeat it exactly. Otherwise run verify with this step_id again.",
									Default:     false,
								},
								"reuse_reason": {
									Type:        "string",
									Description: "When reusing prior verification, briefly explain why the prior observation is still sufficient for this criterion and target.",
								},
								"work_unit_id": {
									Type:        "string",
									Description: "Optional existing work-unit identity. Omit it for ordinary step updates: the runtime preserves attribution from step_id. Never invent an id.",
								},
								"work_unit": {
									Type:        "boolean",
									Description: "True only when this step begins an independent objective or Skill boundary. Ordinary inspect/edit/verify refinements remain in the current work unit.",
									Default:     false,
								},
							},
							Required: []string{"status"},
						},
					},
				},
				Required: []string{"plan"},
			},
			metadata: ToolMetadata{
				Category:  "task",
				RiskLevel: ToolRiskLow,
			},
		},
		store: store,
	}
}

func (t *PlanTool) Execute(args map[string]interface{}) (string, error) {
	mode := taskStringArg(args, "mode")
	if mode != "" && mode != "snapshot" && mode != "patch" {
		return "", fmt.Errorf("mode must be snapshot or patch")
	}
	steps, err := planStepsFromArgs(args["plan"])
	if err != nil {
		return "", err
	}
	projection := runPlanProjectionFromArgs(args)
	if mode == "patch" && projection == nil {
		return "", fmt.Errorf("patch mode requires a durable current Run Plan")
	}
	inProgress := 0
	for i := range steps {
		steps[i].Step = strings.TrimSpace(steps[i].Step)
		steps[i].Status = strings.TrimSpace(steps[i].Status)
		// With a durable plan the runtime decides: only it knows whether a step
		// id can keep this entry's text, so it names the repair (see below).
		if steps[i].Step == "" && projection == nil {
			return "", fmt.Errorf("plan[%d].step is required", i)
		}
		switch steps[i].Status {
		case "pending", "in_progress", "completed", "cancelled":
		default:
			return "", fmt.Errorf("plan[%d].status must be pending, in_progress, completed, or cancelled", i)
		}
		if steps[i].Status == "in_progress" {
			inProgress++
		}
	}
	if inProgress > 1 {
		return "", fmt.Errorf("only one plan step can be in_progress")
	}

	state := PlanState{
		Mode:        mode,
		Explanation: taskStringArg(args, "explanation"),
		Plan:        steps,
	}
	changed := true
	planVersion := 0
	var workUnits []PlanWorkUnitIdentity
	var acceptanceReview []string
	var closureFacts *verification.ClosureFacts
	if projection != nil {
		projected, projectionErr := projection.Project(ContextFromArgs(args), state)
		err = projectionErr
		if err != nil {
			var version interface{ CurrentPlanVersion() int }
			if errors.As(err, &version) {
				return "", newStableToolError(err, "plan_version_changed", "stale_precondition", err.Error(), "Inspect the current Plan, then reconcile it with a complete snapshot. Version matching is runtime-owned.")
			}
			var cancellation interface{ PlanCancellationPrecondition() bool }
			if errors.As(err, &cancellation) {
				return "", newStableToolError(err, "plan_cancellation_unresolved", "stale_precondition", err.Error(), "Keep necessary unfinished work pending. For a legitimate cancellation supply Main's disposition and reason; user_takeover also requires an exact quote from the actual user's instruction.")
			}
			var verification interface{ PlanVerificationPrecondition() bool }
			if errors.As(err, &verification) && verification.PlanVerificationPrecondition() {
				return "", newStableToolError(err, "plan_verification_required", "stale_precondition", err.Error(),
					"Keep the current work unit open and follow each step's next action. verify can bind directly to an owned step_id without a Plan transition. When prior reuse is offered, Main must assess sufficiency and submit reuse_prior_verification=true with reuse_reason on that completed step; explanation alone is not adoption. For a replacement, preserve criterion and target, cite the evidence id in replaces and explain the method correction in reason. Then submit the complete plan snapshot.")
			}
			var textRequired interface{ PlanStepTextRequired() (int, bool) }
			if errors.As(err, &textRequired) {
				index, firstPlan := textRequired.PlanStepTextRequired()
				return "", planStepTextRequiredError(err, index, firstPlan)
			}
			var staleStep interface{ CurrentPlanStepIDs() []string }
			if errors.As(err, &staleStep) {
				safeMessage := staleStepReceipt(err, staleStep.CurrentPlanStepIDs())
				return "", newStableToolError(err, "stale_plan_step", "stale_precondition", safeMessage,
					"Replace the stale id with the matching server-issued id, or omit it only for a genuinely new step.")
			}
			var stale interface{ CurrentWorkUnitIDs() []string }
			if errors.As(err, &stale) {
				current := stale.CurrentWorkUnitIDs()
				safeMessage := "The supplied work_unit_id is stale for the current run."
				if len(current) > 0 {
					safeMessage += " Current work_unit_id values: " + strings.Join(current, ", ") + "."
				} else {
					safeMessage += " Omit work_unit_id so this run can create its first work unit."
				}
				return "", newStableToolError(
					err,
					"stale_work_unit",
					"stale_precondition",
					safeMessage,
					"Replace the stale id with the matching current id returned above, or omit it only when beginning a new work unit.",
				)
			}
			return "", err
		}
		state = projected.Plan
		steps = state.Plan
		changed = projected.Changed
		planVersion = projected.Version
		workUnits = projected.WorkUnits
		acceptanceReview = projected.AcceptanceReview
		closureFacts = projected.ClosureFacts
		currentStepID := ""
		for _, step := range steps {
			if step.Status == "in_progress" {
				currentStepID = step.StepID
				break
			}
		}
		kernel.UpdateRunExecutionPlan(ContextFromArgs(args), planVersion, currentStepID)
	}
	if t.store != nil {
		key := planKey(args)
		localChanged := t.store.Set(key, state)
		if projection == nil {
			changed = localChanged
		}
		resolved := true
		for _, step := range steps {
			if step.Status != "completed" && step.Status != "cancelled" {
				resolved = false
				break
			}
		}
		if resolved {
			t.store.Delete(key)
		}
	}
	data, _ := json.Marshal(struct {
		PlanState
		Changed          bool                   `json:"changed"`
		PlanVersion      int                    `json:"plan_version,omitempty"`
		AcceptanceReview []string               `json:"acceptance_review,omitempty"`
		WorkUnits        []PlanWorkUnitIdentity `json:"work_units,omitempty"`
		// Append derived hints after the canonical Plan: legacy cassette
		// identities are bound in first-occurrence order within this receipt.
		ClosureFacts *verification.ClosureFacts `json:"closure_facts,omitempty"`
	}{ClosureFacts: closureFacts, PlanState: state, Changed: changed, PlanVersion: planVersion, WorkUnits: workUnits, AcceptanceReview: acceptanceReview})
	return string(data), nil
}

// Set records the complete snapshot and reports whether the visible plan
// changed. Explanations are intentionally not part of the comparison: changing
// narration without changing a step or status must not repaint the UI or add a
// duplicate plan.updated event.
func (s *PlanStore) Set(key string, state PlanState) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, ok := s.last[key]
	changed := !ok || !samePlanSteps(previous.Plan, state.Plan)
	s.last[key] = state
	s.plans[key] = state
	return changed
}

func (s *PlanStore) Get(key string) (PlanState, bool) {
	if s == nil {
		return PlanState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.plans[key]
	return state, ok
}

func (s *PlanStore) Delete(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.plans, key)
}

// Purge removes both the active plan and its final deduplication snapshot.
// UpdatePlan keeps the final snapshot briefly so repeated completion updates
// do not repaint the UI; FinishRun ends that lifetime explicitly.
func (s *PlanStore) Purge(key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.plans, key)
	delete(s.last, key)
}

func samePlanSteps(a, b []PlanStep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].StepID != b[i].StepID || a[i].Step != b[i].Step || a[i].Status != b[i].Status || a[i].SuccessCriteria != b[i].SuccessCriteria || a[i].VerificationRequired != b[i].VerificationRequired || a[i].ReusePriorVerification != b[i].ReusePriorVerification || a[i].ReuseReason != b[i].ReuseReason ||
			a[i].CancellationDisposition != b[i].CancellationDisposition || a[i].CancellationReason != b[i].CancellationReason || a[i].UserTakeoverQuote != b[i].UserTakeoverQuote || a[i].ReplacementStepID != b[i].ReplacementStepID || a[i].ScopeChangeQuote != b[i].ScopeChangeQuote || a[i].WorkUnitID != b[i].WorkUnitID || a[i].WorkUnit != b[i].WorkUnit {
			return false
		}
	}
	return true
}

// planStepTextRequiredError words the refusal of an entry that has no text. In a
// Run's first plan no step_id can refer to a stored step (the runtime ignores
// the ids it was sent), so the repair is the whole plan with every step's text;
// later, an entry without a step_id is a new step and has no stored text to keep.
func planStepTextRequiredError(err error, index int, firstPlan bool) error {
	if firstPlan {
		return newStableToolError(err, "plan_step_text_required", "invalid_input",
			fmt.Sprintf("plan[%d].step is required: this run has no plan yet, so a step_id cannot refer to a stored step (ids are ignored in a first plan).", index),
			"Send the complete plan again: every step with its text, and without step_id.")
	}
	return newStableToolError(err, "plan_step_text_required", "invalid_input",
		fmt.Sprintf("plan[%d].step is required: with no step_id the entry is a new step and has no stored text to keep.", index),
		"Give a new step its text, or echo the exact step_id an earlier update_plan result issued for an existing step.")
}

// staleStepReceipt words the refusal for a step_id this Run never issued. With
// the Run's step listing it says which legal id belongs to which step, so the
// model maps by what a step is rather than guessing among opaque ids; an error
// that carries only ids falls back to listing them.
func staleStepReceipt(err error, current []string) string {
	message := "The supplied step_id is stale for the current run."
	var located interface{ StalePlanEntry() (int, string) }
	if errors.As(err, &located) {
		index, supplied := located.StalePlanEntry()
		message = fmt.Sprintf("plan[%d].step_id %q is stale for the current run.", index, supplied)
	}
	if len(current) == 0 {
		return message + " Omit step_id so this run can create its first durable plan snapshot."
	}
	var described interface{ CurrentPlanStepListing() string }
	if errors.As(err, &described) {
		if listing := described.CurrentPlanStepListing(); listing != "" {
			return message + " Current steps (step_id [status] step): " + listing + "."
		}
	}
	return message + " Current step_id values: " + strings.Join(current, ", ") + "."
}

func planStepsFromArgs(raw interface{}) ([]PlanStep, error) {
	switch v := raw.(type) {
	case []interface{}:
		steps := make([]PlanStep, 0, len(v))
		for _, item := range v {
			obj, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("plan items must be objects")
			}
			steps = append(steps, PlanStep{
				StepID:                  taskStringArg(obj, "step_id"),
				CancellationDisposition: taskStringArg(obj, "cancellation_disposition"), CancellationReason: taskStringArg(obj, "cancellation_reason"), UserTakeoverQuote: taskStringArg(obj, "user_takeover_quote"),
				ReplacementStepID: taskStringArg(obj, "replacement_step_id"), ScopeChangeQuote: taskStringArg(obj, "scope_change_quote"),
				Step:                   taskStringArg(obj, "step"),
				Status:                 taskStringArg(obj, "status"),
				SuccessCriteria:        taskStringArg(obj, "success_criteria"),
				VerificationRequired:   taskBoolArg(obj, "verification_required"),
				ReusePriorVerification: taskBoolArg(obj, "reuse_prior_verification"),
				ReuseReason:            taskStringArg(obj, "reuse_reason"),
				WorkUnitID:             taskStringArg(obj, "work_unit_id"),
				WorkUnit:               taskBoolArg(obj, "work_unit"),
			})
		}
		return steps, nil
	case []PlanStep:
		return v, nil
	default:
		return nil, fmt.Errorf("plan must be an array")
	}
}

func planKey(args map[string]interface{}) string {
	if scope, ok := currentExecutionScopeAny(args); ok {
		if scope.RunID != "" {
			return scope.RunID
		}
		if scope.TaskID != "" {
			return scope.TaskID
		}
	}
	if tenantID, _ := args["_tenant_id"].(string); tenantID != "" {
		return tenantID
	}
	return "default"
}

type FinishRunTool struct {
	BaseTool
	store        *PlanStore
	legacySchema ToolSchema
}

var finishRunStatuses = []string{
	"done", "blocked", "failed", "running", "waiting_external", "waiting_user", "needs_approval",
}

func validFinishRunStatus(status string) bool {
	for _, allowed := range finishRunStatuses {
		if status == allowed {
			return true
		}
	}
	return false
}

func NewFinishRunTool() *FinishRunTool {
	return NewFinishRunToolWithStore(nil)
}

func NewFinishRunToolWithStore(store *PlanStore) *FinishRunTool {
	tool := &FinishRunTool{
		BaseTool: BaseTool{
			name:        "finish_run",
			description: "Finish after observing work results. Submit status and final_response once; include final changed steps in plan_patch instead of a separate final update_plan. Omit plan_patch if no steps change. assessments are Main's condition judgments; step_id with evidence_refs can associate existing observations. Static reads cannot prove executable gates. Unmet necessary work stays open. No new work in the finish batch.",
			schema: ToolSchema{
				Type:                 "object",
				AdditionalProperties: rejectAdditionalProperties(),
				Properties: map[string]PropertyDef{
					"status": {
						Type: "string",
						// User-owned lifecycle states such as cancelled and archived
						// deliberately do not belong here. They are available only
						// through explicit gateway controls, never a model tool call.
						Description: "Task status. Cancellation and archival are user controls and are not valid finish_run outcomes.",
						Enum:        append([]string(nil), finishRunStatuses...),
					},
					"summary": {
						Type:        "string",
						Description: "Short user-facing summary. Optional when final_response is supplied; otherwise required for legacy summary-only outcomes.",
					},
					"final_response": {
						Type:        "string",
						Description: "Complete final reply, saved and shown without another generation after acceptance. Optional for legacy summary-only outcomes. Text alone proves neither completion nor authorization.",
					},
					"done": {
						Type:        "array",
						Description: "Completed items.",
						Items:       &PropertyDef{Type: "string"},
					},
					"next_steps": {
						Type:        "array",
						Description: "Remaining or suggested next steps.",
						Items:       &PropertyDef{Type: "string"},
					},
					"files": {
						Type:        "array",
						Description: "Files or artifacts touched.",
						Items:       &PropertyDef{Type: "string"},
					},
					"tests": {
						Type:        "array",
						Description: "Verification performed.",
						Items:       &PropertyDef{Type: "string"},
					},
					"risks": {
						Type:        "array",
						Description: "Known risks or blockers.",
						Items:       &PropertyDef{Type: "string"},
					},
					"need_approve": {
						Type:        "boolean",
						Description: "Whether user approval is needed.",
					},
					"assessments": conditionAssessmentsProperty(),
					"plan_patch":  finalPlanPatchProperty(),
				},
				Required: []string{"status"},
			},
			metadata: ToolMetadata{
				Category:  "task",
				RiskLevel: ToolRiskLow,
			},
		},
		store: store,
	}
	tool.legacySchema = tool.schema
	tool.schema = finishModelSchema(tool.schema)
	return tool
}

func (t *FinishRunTool) Execute(args map[string]interface{}) (string, error) {
	status := strings.TrimSpace(taskStringArg(args, "status"))
	if status == "" {
		return "", finishInputError(fmt.Errorf("status is required; choose one of %s", strings.Join(finishRunStatuses, ", ")),
			"tool_arguments_invalid", "Resend finish_run with an explicit status.")
	}
	if !validFinishRunStatus(status) {
		return "", finishInputError(fmt.Errorf("status must be one of %s", strings.Join(finishRunStatuses, ", ")),
			"tool_arguments_invalid", "Resend finish_run with one of the listed statuses.")
	}
	if status == "needs_approval" {
		status = "blocked"
		args["need_approve"] = true
	}
	summary := strings.TrimSpace(taskStringArg(args, "summary"))
	response := strings.TrimSpace(taskStringArg(args, "final_response"))
	if summary == "" {
		if response == "" {
			return "", finishInputError(fmt.Errorf("final_response or summary is required"),
				"tool_arguments_invalid", "Resend finish_run with final_response (or a summary).")
		}
		// This is a display index, never completion evidence or a replacement
		// for the full final response, whose paragraphs remain intact below.
		summary = truncateRunes(toSingleLine(response), 240)
	}
	out := finishOutcomeFields(args, status, summary, response)
	if handled, err := prepareAtomicFinish(args, out); handled || err != nil {
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(out)
		return string(data), err
	}
	key := planKey(args)
	projection := runPlanProjectionFromArgs(args)
	assessments, assessed, err := prepareOutcomeAssessments(args, status, projection)
	if err != nil {
		var inputError interface{ ConditionAssessmentInput() bool }
		if errors.As(err, &inputError) && inputError.ConditionAssessmentInput() {
			return "", finishInputError(err, "condition_assessment_invalid",
				"Correct the finding or its evidence references and send finish_run again. Observe again only if the existing evidence is stale or unavailable.")
		}
		var precondition interface{ CompletionPrecondition() bool }
		if errors.As(err, &precondition) && precondition.CompletionPrecondition() {
			return "", completionPreconditionFailure(err)
		}
		return "", err
	}
	if status == "done" && projection != nil && !assessed {
		if err := projection.ValidateCompletion(ContextFromArgs(args)); err != nil {
			var precondition interface{ CompletionPrecondition() bool }
			if errors.As(err, &precondition) && precondition.CompletionPrecondition() {
				return "", completionPreconditionFailure(err)
			}
			return "", err
		}
	}
	if status == "done" && projection == nil && t.store != nil {
		if plan, ok := t.store.Get(key); ok {
			var unresolved []string
			for _, step := range plan.Plan {
				if step.Status != "completed" && step.Status != "cancelled" {
					unresolved = append(unresolved, step.Step)
				}
			}
			if len(unresolved) > 0 {
				return "", completionPreconditionFailure(fmt.Errorf("successful run still has unresolved plan steps: %s; call update_plan with the complete plan snapshot before finish_run", strings.Join(unresolved, "; ")))
			}
		}
	}
	if len(assessments) > 0 {
		out["assessments"] = assessments
	}
	if reason := finishRunCompletionReason(status); reason != "" {
		out["completion_reason"] = reason
	}
	data, _ := json.Marshal(out)
	if t.store != nil {
		t.store.Purge(key)
	}
	return string(data), nil
}

func finishOutcomeFields(args map[string]interface{}, status, summary, response string) map[string]interface{} {
	out := map[string]interface{}{
		"status":       status,
		"summary":      summary,
		"done":         taskStringSliceArg(args, "done"),
		"next_steps":   taskStringSliceArg(args, "next_steps"),
		"files":        taskStringSliceArg(args, "files"),
		"tests":        taskStringSliceArg(args, "tests"),
		"risks":        taskStringSliceArg(args, "risks"),
		"need_approve": taskBoolArg(args, "need_approve"),
	}
	if response != "" {
		out["final_response"] = response
	}
	return out
}

// finishInputError reports a finish_run call refused before any effect because
// its own arguments are wrong. finish_run is capped at one call per Run, so the
// recovery contract (preparation, corrected_input, not_dispatched) is what lets
// the kernel return the uncommitted attempt; without it a correctable mistake
// would cost the Run its only way to close.
func finishInputError(cause error, code, hint string) error {
	return newStableToolRecoveryError(cause, code, "invalid_input", cause.Error(), hint,
		"preparation", "corrected_input", "not_dispatched", false)
}

func completionPreconditionFailure(err error) error {
	return newStableToolRecoveryError(err, "completion_precondition", "stale_precondition", err.Error(),
		"Resolve the outstanding plan or verification requirement. One corrected finish_run attempt is available after a changed plan or successful verification; unchanged retries are refused.",
		"preparation", "after_state_change", "not_dispatched", false)
}

func finishRunCompletionReason(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed":
		return "completed"
	case "waiting_external":
		return "waiting_external"
	case "waiting_user":
		return "waiting_user"
	case "blocked":
		return "blocked"
	case "failed":
		return "failed"
	default:
		return ""
	}
}

type ToolSearchTool struct {
	BaseTool
}

func NewToolSearchTool() *ToolSearchTool {
	return &ToolSearchTool{
		BaseTool: BaseTool{
			name:        "tool_search",
			description: "Search available tools by name, description, category, or capability. Matching deferred tools are activated for subsequent calls in this run.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"query": {
						Type:        "string",
						Description: "Search terms.",
					},
					"limit": {
						Type:        "integer",
						Description: "Maximum number of tools to return.",
						Default:     8,
					},
				},
				Required: []string{"query"},
			},
			metadata: ToolMetadata{
				Category:         "task",
				RiskLevel:        ToolRiskLow,
				SupportsParallel: true,
				ReadOnly:         true,
			},
		},
	}
}

func (t *ToolSearchTool) Execute(args map[string]interface{}) (string, error) {
	reg, _ := args["_registry"].(*Registry)
	if reg == nil {
		return "", fmt.Errorf("tool registry is unavailable")
	}
	query := strings.ToLower(strings.TrimSpace(taskStringArg(args, "query")))
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := taskIntArg(args, "limit", 8)
	if limit <= 0 || limit > 50 {
		limit = 8
	}

	type result struct {
		Name        string        `json:"name"`
		Description string        `json:"description"`
		Category    string        `json:"category,omitempty"`
		RiskLevel   ToolRiskLevel `json:"risk_level,omitempty"`
		ReadOnly    bool          `json:"read_only,omitempty"`
		Exposure    ToolExposure  `json:"exposure"`
		Activated   bool          `json:"activated"`
		Score       int           `json:"-"`
	}
	var results []result
	for _, name := range reg.List() {
		tool, ok := reg.Get(name)
		if !ok {
			continue
		}
		meta := ToolMetadataFor(tool)
		if meta.Exposure == ToolExposureHidden {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{
			name,
			tool.Description(),
			meta.Category,
			meta.SearchText,
			string(meta.RiskLevel),
		}, " "))
		score := toolSearchScore(name, haystack, query)
		if score == 0 {
			continue
		}
		exposure := reg.EffectiveToolExposure(name)
		results = append(results, result{
			Name:        name,
			Description: tool.Description(),
			Category:    meta.Category,
			RiskLevel:   meta.RiskLevel,
			ReadOnly:    meta.ReadOnly,
			Exposure:    exposure,
			Activated:   exposure == ToolExposureDeferred,
			Score:       score,
		})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Name < results[j].Name
	})
	if len(results) > limit {
		results = results[:limit]
	}
	data, _ := json.MarshalIndent(results, "", "  ")
	return string(data), nil
}

func toolSearchScore(name, haystack, query string) int {
	name = strings.ToLower(strings.TrimSpace(name))
	query = strings.ToLower(strings.TrimSpace(query))
	if name == "" || query == "" {
		return 0
	}
	score := 0
	if query == name {
		score += 1000
	}
	for _, term := range strings.Fields(query) {
		if term == name {
			score += 500
		}
		if strings.Contains(name, term) {
			score += 50
		} else if strings.Contains(haystack, term) {
			score += 10
		}
	}
	return score
}

func taskStringArg(args map[string]interface{}, key string) string {
	if value, ok := args[key].(string); ok {
		return value
	}
	return ""
}

func taskIntArg(args map[string]interface{}, key string, fallback int) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return fallback
	}
}

func taskBoolArg(args map[string]interface{}, key string) bool {
	value, _ := args[key].(bool)
	return value
}

func taskStringSliceArg(args map[string]interface{}, key string) []string {
	raw, ok := args[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text := strings.TrimSpace(fmt.Sprintf("%v", item))
			if text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func containsAllTerms(haystack, query string) bool {
	for _, term := range strings.Fields(query) {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}
