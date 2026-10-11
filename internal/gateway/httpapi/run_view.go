package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/textutil"
)

func (d *Server) handleRunView(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	view, err := d.runView(r.Context(), identity, strings.TrimSpace(r.URL.Query().Get("run_id")))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (d *Server) runView(ctx context.Context, identity *control.IdentityContext, ref string) (api.RunView, error) {
	if ref == "" {
		return d.runViewList(ctx, identity)
	}
	run, err := d.Control.GetRun(ctx, identity.TenantID, ref)
	if err != nil {
		return api.RunView{}, err
	}
	if run == nil && strings.HasPrefix(ref, "run_") && len(ref) >= 12 {
		recent, err := d.Control.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, 100)
		if err != nil {
			return api.RunView{}, err
		}
		for _, item := range recent {
			if !strings.HasPrefix(item.RunID, ref) {
				continue
			}
			if run != nil {
				return api.RunView{Content: "Run ID is ambiguous; use the full ID."}, nil
			}
			run, err = d.Control.GetRun(ctx, identity.TenantID, item.RunID)
			if err != nil {
				return api.RunView{}, err
			}
		}
	}
	if run == nil || run.PersonID != identity.PersonID || run.ExecutionClass == "coordination" {
		return api.RunView{Content: "That run is not active or not yours."}, nil
	}
	// Follow only the runtime-owned continuation edge, never the newest Run
	// of a Thread. This observes the current leaf without claiming it.
	leaf, err := d.waitingRunDisplayLeaf(ctx, identity, run)
	if err != nil {
		return api.RunView{}, err
	}
	run = leaf
	task, err := d.Control.GetTask(ctx, identity.TenantID, run.TaskID)
	if err != nil {
		return api.RunView{}, err
	}
	if task == nil || task.PersonID != identity.PersonID {
		return api.RunView{Content: "That run is not active or not yours."}, nil
	}
	copy := *task
	copy.Status = run.Status
	copy.CurrentSummary, copy.NextSteps = "", nil
	handoff, err := d.Control.RunHandoff(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil {
		return api.RunView{}, err
	}
	active := d.coordinator().activeForRun(identity.PersonID, run.ID)
	planJSON := d.latestPlanPayloadForRun(ctx, identity, run.TaskID, run.ID)
	plan := taskPlanStepsFromPayload(planJSON)
	content := fmt.Sprintf("Run %s\n", run.ID) + formatTaskStatus(&copy, handoff, active, plan)
	if active != nil {
		content += "\n\n" + d.activeProgress(ctx, identity, active)
	}
	approvals, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil {
		return api.RunView{}, err
	}
	humanPending := false
	for _, ask := range approvals {
		if ask.RunID == run.ID {
			humanPending = true
			content += "\n\n" + approvalSummaryLine(ask, "") + "\n/approve " + ask.ID + "  ·  /reject " + ask.ID
		}
	}
	questions, err := d.Control.ListClarifyRequests(ctx, identity.TenantID, identity.PersonID, "pending", 100)
	if err != nil {
		return api.RunView{}, err
	}
	for _, ask := range questions {
		if ask.RunID == run.ID {
			humanPending = true
			content += "\n\nQuestion:\n" + ask.Question
			for i, option := range clarifyOptions(ask) {
				content += fmt.Sprintf("\n%d. %s", i+1, option)
			}
			content += "\nAnswer in the session that asked this question. /status shows pending questions."
		}
	}
	content += "\n\nViewing does not resume this work."
	if continuityRunResumable(run.Status) && !humanPending {
		content += " /resume " + run.ID + " selects it for your next message."
	}
	if run.Status == "running" || continuityRunResumable(run.Status) || run.Status == "waiting_external" {
		content += " /stop " + run.ID + " cancels it."
	}
	view := api.RunView{RunID: run.ID, Title: textutil.Truncate(toOneLine(task.Title), 96), Status: run.Status, Content: content,
		CanContinue: !humanPending && (run.Status == "running" || continuityRunResumable(run.Status))}
	if handoff != nil {
		view.Summary = handoff.Summary
	}
	view.PlanJSON = planJSON
	return view, nil
}

func (d *Server) runViewList(ctx context.Context, identity *control.IdentityContext) (api.RunView, error) {
	attention, err := control.NewWorkTimeline(d.Control).Attention(ctx, identity.TenantID, identity.PersonID, 20)
	if err != nil {
		return api.RunView{}, err
	}
	view := api.RunView{Content: "Choose work to view. This does not resume or answer it."}
	seen := map[string]bool{}
	for _, item := range attention {
		view.Options = append(view.Options, api.RunViewOption{RunID: item.RunID, Title: textutil.Truncate(toOneLine(item.Thread.Title), 96), Status: item.RunStatus})
		seen[item.RunID] = true
	}
	recent, err := d.Control.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, 5)
	if err != nil {
		return api.RunView{}, err
	}
	for _, item := range recent {
		if seen[item.RunID] {
			continue
		}
		run, err := d.Control.GetRun(ctx, identity.TenantID, item.RunID)
		if err != nil {
			return api.RunView{}, err
		}
		if run == nil || run.ExecutionClass == "coordination" {
			continue
		}
		task, err := d.Control.GetTask(ctx, identity.TenantID, run.TaskID)
		if err != nil {
			return api.RunView{}, err
		}
		if task == nil {
			continue
		}
		view.Options = append(view.Options, api.RunViewOption{RunID: run.ID, Title: textutil.Truncate(toOneLine(task.Title), 96), Status: run.Status})
	}
	if len(view.Options) == 0 {
		view.Content = "No work to view. /queue lists requests that have not started."
	}
	return view, nil
}
