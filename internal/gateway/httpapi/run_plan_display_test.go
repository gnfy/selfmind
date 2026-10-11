package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func TestRunViewPlanSurvivesEventTail(t *testing.T) {
	for _, count := range []int{0, 49, 50} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			daemon, store, _, owner := newHumanWaitServer(t)
			ctx := context.Background()
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "review work", Channel: "review-session"})
			if err != nil {
				t.Fatal(err)
			}
			run := seedUnresolvedRun(t, store, owner.TenantID, task, "review-session", "request", "waiting_user")
			projected, err := store.SyncRunPlan(ctx, owner.TenantID, run.ID, "two accepted steps", []control.RunPlanStepInput{{Step: "review inspect marker", Status: "completed"}, {Step: "review deliver marker", Status: "pending"}})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"plan": projected.Plan.Steps, "version": projected.Plan.Version})
			if _, err = store.AppendEvent(ctx, control.Event{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Channel: "review-session", Type: "plan.updated", Payload: raw}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < count; i++ {
				if _, err = store.AppendEvent(ctx, control.Event{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Channel: "review-session", Type: "tool.finished", Payload: json.RawMessage("{}")}); err != nil {
					t.Fatal(err)
				}
			}
			durable, err := store.LatestRunPlan(ctx, owner.TenantID, run.ID)
			if err != nil || durable == nil || len(durable.Steps) != 2 {
				t.Fatalf("durable plan missing: %+v %v", durable, err)
			}
			view, err := daemon.runView(ctx, owner, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.PlanJSON == "" || !strings.Contains(view.Content, "review deliver marker") {
				t.Fatalf("accepted plan hidden after %d later events: durable_steps=%d view_plan=%q content=%q", count, len(durable.Steps), view.PlanJSON, view.Content)
			}
		})
	}
}

func TestDurablePlanDisplayPreservesAuthority(t *testing.T) {
	ctx := context.Background()
	daemon, store, identity := newTaskViewServer(t)
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "active work", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "work")
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"plan":[{"step":"stale event step","status":"pending"}]}`
	if _, err = store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "plan.updated", Payload: json.RawMessage(legacy)}); err != nil {
		t.Fatal(err)
	}
	if got := daemon.latestPlanPayloadForRun(ctx, identity, task.ID, run.ID); got != legacy {
		t.Fatal("legacy event fallback lost")
	}
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "current accepted plan", []control.RunPlanStepInput{{Step: "current accepted step", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "plan.updated", Payload: json.RawMessage(legacy)}); err != nil {
		t.Fatal(err)
	}
	got := daemon.latestPlanPayloadForRun(ctx, identity, task.ID, run.ID)
	if strings.Contains(got, "stale event step") || !strings.Contains(got, "current accepted step") || !strings.Contains(got, `"plan_version":1`) {
		t.Fatalf("accepted plan replaced by stale event: %s", got)
	}
	stranger := *identity
	stranger.PersonID = "stranger"
	if got := daemon.latestPlanPayloadForRun(ctx, &stranger, task.ID, run.ID); got != "" {
		t.Fatal("another person's plan leaked")
	}
	if got := daemon.latestPlanPayloadForRun(ctx, identity, "other-thread", run.ID); got != "" {
		t.Fatal("mismatched run/thread leaked a plan")
	}
}
