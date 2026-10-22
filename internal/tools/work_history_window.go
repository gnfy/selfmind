package tools

import (
	"encoding/json"
	"fmt"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

func (t *WorkSearchTool) historyWindow(args map[string]interface{}, scope kernel.ToolInvocationScope) (string, error) {
	since, err := time.Parse(time.RFC3339, taskStringArg(args, "since"))
	if err != nil {
		return "", fmt.Errorf("since must be RFC3339 with a timezone")
	}
	until, err := time.Parse(time.RFC3339, taskStringArg(args, "until"))
	if err != nil {
		return "", fmt.Errorf("until must be RFC3339 with a timezone")
	}
	limit := intArg(args, "limit", 5)
	if limit < 1 {
		limit = 1
	}
	if limit > 8 {
		limit = 8
	}
	ctx := ContextFromArgs(args)
	page, err := control.NewWorkTimeline(t.store).QueryHistory(ctx, scope.ControlTenantID, scope.PersonID, control.WorkHistoryQuery{
		Since: since, Until: until, WorkspaceID: taskStringArg(args, "workspace_id"), Keyword: taskStringArg(args, "query"), Cursor: taskStringArg(args, "cursor"), ExcludeRunID: scope.RunID, Limit: limit,
	})
	if err != nil {
		return "", err
	}
	cards := make([]workTaskCard, 0, len(page.Runs))
	for _, run := range page.Runs {
		task, err := t.store.GetTask(ctx, scope.ControlTenantID, run.TaskID)
		if err != nil {
			return "", err
		}
		if task == nil || task.PersonID != scope.PersonID {
			return "", fmt.Errorf("history owner is unavailable")
		}
		card := workTaskCard{TaskID: task.ID, Title: workBound(task.Title, 240), WorkspaceID: run.WorkspaceID, Status: run.Status, Summary: workBound(run.InputSummary, 400), Evidence: []string{"observed_in_window"}, LastActivityAt: run.StartedAt.UTC().Format(time.RFC3339)}
		rc := workRunCard{RunID: run.ID, Status: run.Status, WorkspaceID: run.WorkspaceID, Channel: run.Channel, StartedAt: run.StartedAt.UTC().Format(time.RFC3339), ResumesRunID: run.ResumesRunID}
		if run.FinishedAt != nil {
			rc.FinishedAt = run.FinishedAt.UTC().Format(time.RFC3339)
			card.LastActivityAt = rc.FinishedAt
		}
		card.Runs = []workRunCard{rc}
		handoff, err := t.store.RunHandoff(ctx, scope.ControlTenantID, scope.PersonID, run.ID)
		if err != nil {
			return "", err
		}
		if handoff != nil {
			card.Summary = workBound(handoff.Summary, 400)
			card.NextSteps = boundedWorkStrings(handoff.NextSteps, 4, 240)
		}
		cards = append(cards, card)
	}
	coverage := "end_of_window"
	if page.HasMore {
		coverage = "more_available"
	}
	out := map[string]interface{}{"mode": "history", "query": taskStringArg(args, "query"), "since": since.UTC().Format(time.RFC3339), "until": until.UTC().Format(time.RFC3339), "results": cards, "count": len(cards), "has_more": page.HasMore, "coverage": coverage,
		"notice": "One card per observed Run, including continuation edges. Follow next_cursor until end_of_window; a page is not the full window. Status is read now; summaries are recorded findings, not fresh external verification. Reading history never resumes work."}
	if page.NextCursor != "" {
		out["next_cursor"] = page.NextCursor
	}
	data, err := json.Marshal(out)
	return string(data), err
}
