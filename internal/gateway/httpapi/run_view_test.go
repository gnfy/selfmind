package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestRunViewObservesParkedPlanWithoutClaimOrTranscript(t *testing.T) {
	daemon, store, _, owner := newHumanWaitServer(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "old work", Channel: "old-session"})
	if err != nil {
		t.Fatal(err)
	}
	run := seedUnresolvedRun(t, store, owner.TenantID, task, "old-session", "prepare", "waiting_user")
	_, err = store.AppendEvent(ctx, control.Event{RunID: run.ID, TaskID: task.ID, Type: "plan.updated", Payload: json.RawMessage(`{"plan":[{"step":"inspect target","status":"completed"},{"step":"publish","status":"pending"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	ask, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, ActionType: "tool_call", RequestedChannel: "old-session", Payload: json.RawMessage(`{"tool":"write_file","args":{"path":"receipt.txt"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordChannelMessage(ctx, *owner, "old-session", task.ID, "user", "PRIVATE_TRANSCRIPT_MARKER"); err != nil {
		t.Fatal(err)
	}
	before, _ := store.ListTaskRuns(ctx, owner.TenantID, task.ID, 20)
	for _, ref := range []string{run.ID, shortRunID(run.ID)} {
		view, err := daemon.runView(ctx, owner, ref)
		if err != nil || view.RunID != run.ID || view.Status != "waiting_user" || !strings.Contains(view.Content, "inspect target") || !strings.Contains(view.Content, "/approve "+ask.ID) {
			t.Fatalf("view=%+v err=%v", view, err)
		}
		if strings.Contains(view.Content, "PRIVATE_TRANSCRIPT_MARKER") {
			t.Fatal("view copied channel transcript")
		}
		if strings.Contains(view.Content, "/resume ") {
			t.Fatal("pending approval was presented as resumable without an answer")
		}
	}
	list, err := daemon.runView(ctx, owner, "")
	if err != nil || len(list.Options) == 0 || list.Options[0].RunID != run.ID {
		t.Fatalf("parked work missing from chooser: %+v %v", list, err)
	}
	current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
	after, _ := store.ListTaskRuns(ctx, owner.TenantID, task.ID, 20)
	stillPending, _ := store.GetApprovalRequest(ctx, owner.TenantID, ask.ID)
	if len(after) != len(before) || current.Status != "waiting_user" || stillPending.Status != "pending" {
		t.Fatal("viewing claimed work or answered approval")
	}
	// The production read endpoint authenticates and projects the same view.
	req := asLocalCLI(httptest.NewRequest(http.MethodGet, "/v1/runs/view?platform=cli&platform_user_id=local&run_id="+run.ID, nil))
	rr := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rr, req)
	var response api.RunView
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &response) != nil || response.RunID != run.ID {
		t.Fatalf("endpoint=%d %s", rr.Code, rr.Body.String())
	}
	other, err := store.ResolveOrCreateAccount(ctx, owner.TenantID, "cli", "stranger", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := daemon.runView(ctx, other, run.ID)
	missing, _ := daemon.runView(ctx, other, "run_missing")
	if foreign.Content != missing.Content || foreign.RunID != "" {
		t.Fatal("foreign work revealed")
	}
}
