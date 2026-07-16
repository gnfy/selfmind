package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type PlanTool struct {
	BaseTool
	store *PlanStore
}

type PlanStore struct {
	mu    sync.RWMutex
	plans map[string]PlanState
}

type PlanState struct {
	Explanation string     `json:"explanation,omitempty"`
	Plan        []PlanStep `json:"plan"`
}

type PlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

func NewPlanStore() *PlanStore {
	return &PlanStore{plans: make(map[string]PlanState)}
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
								"step": {
									Type:        "string",
									Description: "A concise task step.",
								},
								"status": {
									Type:        "string",
									Description: "One of pending, in_progress, completed, cancelled.",
									Enum:        []string{"pending", "in_progress", "completed", "cancelled"},
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
	if t.store != nil {
		key := planKey(args)
		resolved := true
		for _, step := range steps {
			if step.Status != "completed" && step.Status != "cancelled" {
				resolved = false
				break
			}
		}
		if resolved {
			t.store.Delete(key)
		} else {
			t.store.Set(key, state)
		}
	}
	data, _ := json.Marshal(state)
	return string(data), nil
}

func (s *PlanStore) Set(key string, state PlanState) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans[key] = state
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
				Step:   fmt.Sprintf("%v", obj["step"]),
				Status: fmt.Sprintf("%v", obj["status"]),
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

func NewFinishRunTool() *FinishRunTool {
	return NewFinishRunToolWithStore(nil)
}

func NewFinishRunToolWithStore(store *PlanStore) *FinishRunTool {
	return &FinishRunTool{
		BaseTool: BaseTool{
			name:        "finish_run",
			description: "Record a structured task outcome before the final answer. Use when the task is done, blocked, failed, waiting on a registered external watch, or needs approval.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"status": {
						Type:        "string",
						Description: "Task status.",
						Enum:        []string{"done", "blocked", "failed", "running", "waiting_external", "needs_approval"},
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
	if status == "needs_approval" {
		status = "blocked"
		args["need_approve"] = true
	}
	summary := strings.TrimSpace(taskStringArg(args, "summary"))
	if summary == "" {
		return "", fmt.Errorf("summary is required")
	}
	key := planKey(args)
	if status == "done" && t.store != nil {
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
	data, _ := json.Marshal(out)
	if t.store != nil {
		t.store.Delete(key)
	}
	return string(data), nil
}

type ToolSearchTool struct {
	BaseTool
}

func NewToolSearchTool() *ToolSearchTool {
	return &ToolSearchTool{
		BaseTool: BaseTool{
			name:        "tool_search",
			description: "Search available tools by name, description, category, or capability.",
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
		results = append(results, result{
			Name:        name,
			Description: tool.Description(),
			Category:    meta.Category,
			RiskLevel:   meta.RiskLevel,
			ReadOnly:    meta.ReadOnly,
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
