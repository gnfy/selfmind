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
	Plan      PlanState              `json:"plan"`
	Version   int                    `json:"version"`
	Changed   bool                   `json:"changed"`
	WorkUnits []PlanWorkUnitIdentity `json:"work_units,omitempty"`
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
	Explanation string     `json:"explanation,omitempty"`
	Plan        []PlanStep `json:"plan"`
}

type PlanStep struct {
	StepID               string `json:"step_id,omitempty"`
	Step                 string `json:"step"`
	Status               string `json:"status"`
	SuccessCriteria      string `json:"success_criteria,omitempty"`
	VerificationRequired bool   `json:"verification_required,omitempty"`
	WorkUnitID           string `json:"work_unit_id,omitempty"`
	WorkUnit             bool   `json:"work_unit,omitempty"`
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
			description: "Replace the visible task plan with a complete current snapshot. Use only for non-trivial multi-step work; do not use for one-shot answers, small code examples, simple commands, or direct explanations. Include every step on every update, keep exactly one step in_progress while work is active, and resolve all steps before finishing successfully.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"explanation": {
						Type:        "string",
						Description: "Optional short reason for the plan update.",
					},
					"plan": {
						Type:        "array",
						Description: "The complete ordered plan snapshot, including unchanged and completed steps. This replaces the previous snapshot; it is not a partial patch.",
						Items: &PropertyDef{
							Type: "object",
							Properties: map[string]PropertyDef{
								"step_id": {
									Type:        "string",
									Description: "Stable step id returned by an earlier update_plan result. Echo it when updating or reordering that step; never invent one.",
								},
								"step": {
									Type:        "string",
									Description: "A concise task step.",
								},
								"status": {
									Type:        "string",
									Description: "One of pending, in_progress, completed, cancelled.",
									Enum:        []string{"pending", "in_progress", "completed", "cancelled"},
								},
								"success_criteria": {
									Type:        "string",
									Description: "Optional observable condition that proves this step is complete.",
								},
								"verification_required": {
									Type:        "boolean",
									Description: "True only when this step cannot be considered complete without executable verification evidence required by the user, repository instructions, or the nature of the change.",
									Default:     false,
								},
								"work_unit_id": {
									Type:        "string",
									Description: "Stable work-unit id returned by an earlier update_plan result. Echo it when updating or reordering the same independent objective; never invent one.",
								},
								"work_unit": {
									Type:        "boolean",
									Description: "True only when this step begins an independent objective or Skill boundary. Ordinary inspect/edit/verify refinements remain in the current work unit.",
									Default:     false,
								},
							},
							Required: []string{"step", "status"},
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
	steps, err := planStepsFromArgs(args["plan"])
	if err != nil {
		return "", err
	}
	inProgress := 0
	for i := range steps {
		steps[i].Step = strings.TrimSpace(steps[i].Step)
		steps[i].Status = strings.TrimSpace(steps[i].Status)
		if steps[i].Step == "" {
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
		Explanation: taskStringArg(args, "explanation"),
		Plan:        steps,
	}
	changed := true
	planVersion := 0
	var workUnits []PlanWorkUnitIdentity
	projection := runPlanProjectionFromArgs(args)
	if projection != nil {
		projected, projectionErr := projection.Project(ContextFromArgs(args), state)
		err = projectionErr
		if err != nil {
			var staleStep interface{ CurrentPlanStepIDs() []string }
			if errors.As(err, &staleStep) {
				current := staleStep.CurrentPlanStepIDs()
				safeMessage := "The supplied step_id is stale for the current run."
				if len(current) > 0 {
					safeMessage += " Current step_id values: " + strings.Join(current, ", ") + "."
				} else {
					safeMessage += " Omit step_id so this run can create its first durable plan snapshot."
				}
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
		Changed     bool                   `json:"changed"`
		PlanVersion int                    `json:"plan_version,omitempty"`
		WorkUnits   []PlanWorkUnitIdentity `json:"work_units,omitempty"`
	}{PlanState: state, Changed: changed, PlanVersion: planVersion, WorkUnits: workUnits})
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
		if a[i].StepID != b[i].StepID || a[i].Step != b[i].Step || a[i].Status != b[i].Status || a[i].SuccessCriteria != b[i].SuccessCriteria || a[i].VerificationRequired != b[i].VerificationRequired ||
			a[i].WorkUnitID != b[i].WorkUnitID || a[i].WorkUnit != b[i].WorkUnit {
			return false
		}
	}
	return true
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
				StepID:               taskStringArg(obj, "step_id"),
				Step:                 fmt.Sprintf("%v", obj["step"]),
				Status:               fmt.Sprintf("%v", obj["status"]),
				SuccessCriteria:      taskStringArg(obj, "success_criteria"),
				VerificationRequired: taskBoolArg(obj, "verification_required"),
				WorkUnitID:           taskStringArg(obj, "work_unit_id"),
				WorkUnit:             taskBoolArg(obj, "work_unit"),
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
	store *PlanStore
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
	return &FinishRunTool{
		BaseTool: BaseTool{
			name:        "finish_run",
			description: "Record a structured task outcome before the final answer. Use when the task is done, blocked, failed, waiting on a registered external watch, prepared and waiting for the user's go-ahead (waiting_user), or needs approval.",
			schema: ToolSchema{
				Type: "object",
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
						Description: "Short user-facing summary.",
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
				},
				Required: []string{"status", "summary"},
			},
			metadata: ToolMetadata{
				Category:  "task",
				RiskLevel: ToolRiskLow,
			},
		},
		store: store,
	}
}

func (t *FinishRunTool) Execute(args map[string]interface{}) (string, error) {
	status := strings.TrimSpace(taskStringArg(args, "status"))
	if status == "" {
		status = "done"
	}
	if !validFinishRunStatus(status) {
		return "", fmt.Errorf("status must be one of %s", strings.Join(finishRunStatuses, ", "))
	}
	if status == "needs_approval" {
		status = "blocked"
		args["need_approve"] = true
	}
	summary := strings.TrimSpace(taskStringArg(args, "summary"))
	if summary == "" {
		return "", fmt.Errorf("summary is required")
	}
	key := planKey(args)
	projection := runPlanProjectionFromArgs(args)
	if status == "done" && projection != nil {
		if err := projection.ValidateCompletion(ContextFromArgs(args)); err != nil {
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
				return "", fmt.Errorf("successful run still has unresolved plan steps: %s; call update_plan with the complete plan snapshot before finish_run", strings.Join(unresolved, "; "))
			}
		}
	}
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
	if reason := finishRunCompletionReason(status); reason != "" {
		out["completion_reason"] = reason
	}
	data, _ := json.Marshal(out)
	if t.store != nil {
		t.store.Purge(key)
	}
	return string(data), nil
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
		if !containsAllTerms(haystack, query) {
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
		})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})
	if len(results) > limit {
		results = results[:limit]
	}
	data, _ := json.MarshalIndent(results, "", "  ")
	return string(data), nil
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
