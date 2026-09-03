package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
)

// WorkSearchTool searches durable, structured work history. It deliberately
// does not expose channel transcripts; Main can inspect a selected Run through
// work_inspect after seeing these bounded cards.
type WorkSearchTool struct {
	store *control.Store
}

func NewWorkSearchTool(store *control.Store) *WorkSearchTool {
	return &WorkSearchTool{store: store}
}

func (t *WorkSearchTool) Name() string { return "work_search" }

func (t *WorkSearchTool) Description() string {
	return "Search the current person's retained work history, or explicitly list current attention. Returns bounded thread and run cards, never raw channel transcripts."
}

func (t *WorkSearchTool) Schema() ToolSchema {
	return ToolSchema{
		Type: "object",
		Properties: map[string]PropertyDef{
			"mode":  {Type: "string", Description: "history (default) searches by query; attention lists currently actionable or resumable work.", Enum: []string{"history", "attention"}, Default: "history"},
			"query": {Type: "string", Description: "Words, identifiers, file names, or work references to find. Required in history mode; ignored in attention mode."},
			"limit": {Type: "integer", Description: "Maximum thread cards, from 1 to 8. Default 5.", Default: 5},
		},
	}
}

func (t *WorkSearchTool) Metadata() ToolMetadata {
	return ToolMetadata{
		Exposure: ToolExposureDirect, SupportsParallel: true, ReadOnly: true,
		RiskLevel: ToolRiskLow, Category: "task",
	}
}

type workRunCard struct {
	RunID       string `json:"run_id"`
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Channel     string `json:"channel,omitempty"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

type workTaskCard struct {
	TaskID         string   `json:"task_id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	WorkspaceID    string   `json:"workspace_id,omitempty"`
	Summary        string   `json:"summary,omitempty"`
	NextSteps      []string `json:"next_steps,omitempty"`
	LastActivityAt string   `json:"last_activity_at,omitempty"`
	// Evidence says why the card is here: an explicit Attention query or a
	// literal retained-history match.
	Evidence []string      `json:"evidence,omitempty"`
	Runs     []workRunCard `json:"runs,omitempty"`
}

func (t *WorkSearchTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("work history is unavailable")
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || strings.TrimSpace(scope.ControlTenantID) == "" || strings.TrimSpace(scope.PersonID) == "" || strings.TrimSpace(scope.RunID) == "" {
		return "", fmt.Errorf("authenticated run scope is required")
	}
	query := strings.TrimSpace(taskStringArg(args, "query"))
	mode := strings.ToLower(strings.TrimSpace(taskStringArg(args, "mode")))
	if mode == "" {
		mode = "history"
	}
	if mode != "history" && mode != "attention" {
		return "", fmt.Errorf("mode must be history or attention")
	}
	if mode == "history" && query == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := intArg(args, "limit", 5)
	if limit < 1 {
		limit = 1
	}
	if limit > 8 {
		limit = 8
	}
	ctx := ContextFromArgs(args)
	cards := make([]workTaskCard, 0, limit)
	buildCard := func(task control.Task, evidence, exactRunID string) (workTaskCard, error) {
		card := workTaskCard{
			TaskID: task.ID, Title: workBound(task.Title, 240), Status: task.Status,
			WorkspaceID: task.WorkspaceID, Summary: workBound(task.CurrentSummary, 400),
			NextSteps: boundedWorkStrings(task.NextSteps, 4, 240),
			Evidence:  []string{evidence},
		}
		if !task.LastActivityAt.IsZero() {
			card.LastActivityAt = task.LastActivityAt.UTC().Format(time.RFC3339)
		}
		var runs []control.Run
		if strings.TrimSpace(exactRunID) != "" {
			run, err := t.store.GetRun(ctx, scope.ControlTenantID, exactRunID)
			if err != nil {
				return workTaskCard{}, err
			}
			if run == nil || run.PersonID != scope.PersonID || run.TaskID != task.ID {
				return workTaskCard{}, fmt.Errorf("attention run is no longer available")
			}
			runs = []control.Run{*run}
		} else {
			var err error
			runs, err = t.store.ListTaskRuns(ctx, scope.ControlTenantID, task.ID, 3)
			if err != nil {
				return workTaskCard{}, err
			}
		}
		for _, run := range runs {
			if run.PersonID != scope.PersonID {
				continue
			}
			runCard := workRunCard{
				RunID: run.ID, Status: run.Status, WorkspaceID: run.WorkspaceID,
				Channel: run.Channel, StartedAt: run.StartedAt.UTC().Format(time.RFC3339),
			}
			if run.FinishedAt != nil {
				runCard.FinishedAt = run.FinishedAt.UTC().Format(time.RFC3339)
			}
			card.Runs = append(card.Runs, runCard)
		}
		return card, nil
	}
	add := func(task control.Task, evidence, exactRunID string) error {
		// The current interaction title is derived from the user's query and is
		// therefore a guaranteed false self-hit. It is not historical evidence.
		if strings.TrimSpace(scope.TaskID) != "" && task.ID == scope.TaskID {
			return nil
		}
		card, err := buildCard(task, evidence, exactRunID)
		if err != nil {
			return err
		}
		cards = append(cards, card)
		return nil
	}
	if mode == "attention" {
		// The run that asked for a confirmation is usually on the channel the
		// confirmation arrives on, so this run's channel ranks first.
		preferChannel := ""
		if current, err := t.store.GetRun(ctx, scope.ControlTenantID, scope.RunID); err == nil && current != nil {
			preferChannel = current.Channel
		}
		items, err := control.NewWorkTimeline(t.store).AttentionForChannel(ctx, scope.ControlTenantID, scope.PersonID, preferChannel, limit)
		if err != nil {
			return "", err
		}
		for _, item := range items {
			task, err := t.store.GetTask(ctx, scope.ControlTenantID, item.Thread.ID)
			if err != nil {
				return "", err
			}
			if task == nil || task.PersonID != scope.PersonID {
				continue
			}
			evidence := "attention"
			if item.Activity == control.ThreadActivityResumable {
				evidence = "unresolved_run"
			}
			task.Status = item.Activity
			if strings.TrimSpace(item.RunSummary) != "" {
				task.CurrentSummary = item.RunSummary
			}
			if err := add(*task, evidence, item.RunID); err != nil {
				return "", err
			}
		}
	} else {
		tasks, err := t.store.SearchTasks(ctx, scope.ControlTenantID, scope.PersonID, query, limit)
		if err != nil {
			return "", err
		}
		for _, task := range tasks {
			if err := add(task, "literal_match", ""); err != nil {
				return "", err
			}
		}
	}
	encoded, _ := json.Marshal(map[string]interface{}{
		"mode": mode, "query": query, "count": len(cards), "results": cards,
		"notice": "Structured work cards only. History mode returns query matches; attention mode returns current control facts. Use work_inspect with an exact run_id for bounded details.",
	})
	return string(encoded), nil
}

// WorkInspectTool reads one exact person's Run and returns structured state
// needed for continuation decisions. Event payloads are reduced to a small
// allow-list so raw transcript or tool-result bodies never cross endpoints.
type WorkInspectTool struct {
	store *control.Store
}

func NewWorkInspectTool(store *control.Store) *WorkInspectTool {
	return &WorkInspectTool{store: store}
}

func (t *WorkInspectTool) Name() string { return "work_inspect" }

func (t *WorkInspectTool) Description() string {
	return "Inspect one exact prior run for the current person. Returns bounded status, plan, handoff, event summaries, and artifact references without raw transcripts."
}

func (t *WorkInspectTool) Schema() ToolSchema {
	return ToolSchema{
		Type: "object",
		Properties: map[string]PropertyDef{
			"run_id": {Type: "string", Description: "Exact server-issued run_id returned by work_search or a structured work hint."},
		},
		Required: []string{"run_id"},
	}
}

func (t *WorkInspectTool) Metadata() ToolMetadata {
	return ToolMetadata{
		Exposure: ToolExposureDirect, SupportsParallel: true, ReadOnly: true,
		RiskLevel: ToolRiskLow, Category: "task",
	}
}

type workEventSummary struct {
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Tool      string `json:"tool,omitempty"`
	Status    string `json:"status,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type workArtifactRef struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	URI      string `json:"uri"`
	MimeType string `json:"mime_type,omitempty"`
}

func (t *WorkInspectTool) Execute(args map[string]interface{}) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("work history is unavailable")
	}
	scope, ok := InvocationScopeFromArgs(args)
	if !ok || strings.TrimSpace(scope.ControlTenantID) == "" || strings.TrimSpace(scope.PersonID) == "" || strings.TrimSpace(scope.RunID) == "" {
		return "", fmt.Errorf("authenticated run scope is required")
	}
	runID := strings.TrimSpace(taskStringArg(args, "run_id"))
	if runID == "" {
		return "", fmt.Errorf("run_id is required")
	}
	ctx := ContextFromArgs(args)
	run, err := t.store.GetRun(ctx, scope.ControlTenantID, runID)
	if err != nil {
		return "", err
	}
	if run == nil || run.PersonID != scope.PersonID {
		return "", fmt.Errorf("run not found for the current person")
	}
	task, err := t.store.GetTask(ctx, scope.ControlTenantID, run.TaskID)
	if err != nil {
		return "", err
	}
	if task == nil || task.PersonID != scope.PersonID {
		return "", fmt.Errorf("task not found for the current person")
	}
	result := map[string]interface{}{
		"run": map[string]interface{}{
			"run_id": run.ID, "task_id": run.TaskID, "parent_run_id": run.ParentRunID,
			"status": run.Status, "workspace_id": run.WorkspaceID, "channel": run.Channel,
			"started_at": run.StartedAt.UTC().Format(time.RFC3339),
		},
		"task": map[string]interface{}{
			"task_id": task.ID, "title": workBound(task.Title, 240), "status": task.Status,
			"workspace_id": task.WorkspaceID, "summary": workBound(task.CurrentSummary, 500),
			"next_steps": boundedWorkStrings(task.NextSteps, 6, 240),
		},
	}
	if run.FinishedAt != nil {
		result["finished_at"] = run.FinishedAt.UTC().Format(time.RFC3339)
	}
	if plan, err := t.store.LatestRunPlan(ctx, scope.ControlTenantID, run.ID); err != nil {
		return "", err
	} else if plan != nil {
		planCopy := *plan
		planCopy.Explanation = workBound(planCopy.Explanation, 400)
		if len(planCopy.Steps) > 12 {
			planCopy.Steps = planCopy.Steps[:12]
		}
		for i := range planCopy.Steps {
			planCopy.Steps[i].Step = workBound(planCopy.Steps[i].Step, 300)
			planCopy.Steps[i].SuccessCriteria = workBound(planCopy.Steps[i].SuccessCriteria, 300)
		}
		result["plan"] = planCopy
	}
	if handoff, err := t.store.RunHandoff(ctx, scope.ControlTenantID, scope.PersonID, run.ID); err != nil {
		return "", err
	} else if handoff != nil {
		result["handoff"] = map[string]interface{}{
			"summary":       workBound(handoff.Summary, 600),
			"done_items":    boundedWorkStrings(handoff.DoneItems, 8, 240),
			"next_steps":    boundedWorkStrings(handoff.NextSteps, 8, 240),
			"changed_files": boundedWorkStrings(handoff.ChangedFiles, 12, 300),
			"test_status":   workBound(handoff.TestStatus, 300),
			"risks":         boundedWorkStrings(handoff.Risks, 6, 240),
		}
	}
	if events, err := t.store.ListRunEvents(ctx, scope.ControlTenantID, scope.PersonID, run.TaskID, run.ID, 20); err != nil {
		return "", err
	} else {
		summaries := make([]workEventSummary, 0, len(events))
		for _, event := range events {
			summaries = append(summaries, summarizeWorkEvent(event))
		}
		result["events"] = summaries
	}
	if artifacts, err := t.store.ListRunArtifacts(ctx, scope.ControlTenantID, scope.PersonID, run.TaskID, run.ID, 8); err != nil {
		return "", err
	} else {
		refs := make([]workArtifactRef, 0, len(artifacts))
		for _, artifact := range artifacts {
			refs = append(refs, workArtifactRef{
				ID: artifact.ID, Kind: artifact.Kind, Name: workBound(artifact.Name, 180),
				URI: workBound(artifact.URI, 500), MimeType: artifact.MimeType,
			})
		}
		result["artifacts"] = refs
	}
	encoded, _ := json.Marshal(result)
	return string(encoded), nil
}

func summarizeWorkEvent(event control.Event) workEventSummary {
	summary := workEventSummary{Type: event.Type, CreatedAt: event.CreatedAt.UTC().Format(time.RFC3339)}
	var payload map[string]interface{}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return summary
	}
	summary.Tool = workBound(stringValue(payload["tool"]), 120)
	summary.Status = workBound(stringValue(payload["status"]), 80)
	summary.Reason = workBound(stringValue(payload["reason"]), 240)
	summary.Summary = workBound(stringValue(payload["summary"]), 400)
	if outcome, ok := payload["outcome"].(map[string]interface{}); ok {
		if summary.Status == "" {
			summary.Status = workBound(stringValue(outcome["status"]), 80)
		}
		if summary.Summary == "" {
			summary.Summary = workBound(stringValue(outcome["summary"]), 400)
		}
	}
	return summary
}

func stringValue(value interface{}) string {
	valueString, _ := value.(string)
	return valueString
}

func boundedWorkStrings(values []string, limit, maxRunes int) []string {
	if len(values) > limit {
		values = values[:limit]
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = workBound(value, maxRunes); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func workBound(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if maxRunes > 0 && len(runes) > maxRunes {
		return strings.TrimSpace(string(runes[:maxRunes])) + "…"
	}
	return value
}
