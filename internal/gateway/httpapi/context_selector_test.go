package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
)

func TestSelectedTaskRuntimeContextReadsControlSlices(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "repo",
		LocalPath:     "/tmp/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID:    identity.TenantID,
		PersonID:    identity.PersonID,
		WorkspaceID: ws.ID,
		Title:       "Context task",
		Channel:     "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The parent run this continuation resumes: its handoff, artifacts, and
	// events are the run-scoped slice the selector must load (P0). A second,
	// unrelated finished run proves other runs' slices stay out.
	parent, err := store.StartRun(ctx, task, "cli", "input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{
		ID:        "handoff_run_" + parent.ID,
		TaskID:    task.ID,
		Summary:   "Latest handoff summary",
		NextSteps: []string{"finish selector"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveArtifact(ctx, control.Artifact{
		TaskID: task.ID,
		RunID:  parent.ID,
		Kind:   "file",
		Name:   "agent.go",
		URI:    "internal/kernel/agent.go",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID:  task.ID,
		RunID:   parent.ID,
		Type:    "tool.completed",
		Channel: "cli",
		Payload: mustJSON(map[string]string{"result": "read agent.go"}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID:  task.ID,
		Type:    "tool.completed",
		Channel: "cli",
		Payload: mustJSON(map[string]string{"result": "unrelated task-wide event"}),
	}); err != nil {
		t.Fatal(err)
	}

	server := &Server{Control: store}
	selected := server.coordinator().selectedTaskRuntimeContext(ctx, task, nil, ws, "cli", "cli", "continue the context selector work", false)
	prompt := selected.Prompt(10000)
	for _, want := range []string{
		task.ID,
		"Latest handoff summary",
		"internal/kernel/agent.go",
		"tool.completed",
		"read agent.go",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("selected prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "unrelated task-wide event") {
		t.Fatalf("run-scoped selection leaked a task-wide event:\n%s", prompt)
	}
	if selected.PriorChannel != parent.Channel {
		t.Fatalf("prior channel must come from the parent run, got %q", selected.PriorChannel)
	}
}

// TestFullContextWithoutExactParentDowngrades pins the P0 gate: a full-mode
// attach on a task with no (or more than one) unclaimed resumable run must
// fall back to the bounded task card — no handoff, no artifacts, no events.
func TestFullContextWithoutExactParentDowngrades(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "downgrade", "User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "mixed history", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store}
	seedRun := func(summary, status string) *control.Run {
		run, err := store.StartRun(ctx, task, "cli", summary)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SaveHandoff(ctx, control.Handoff{
			ID: "handoff_run_" + run.ID, TaskID: task.ID, Summary: "handoff of " + summary,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendEvent(ctx, control.Event{
			TaskID: task.ID, RunID: run.ID, Type: "tool.completed",
			Payload: mustJSON(map[string]string{"result": "event of " + summary}),
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
			t.Fatal(err)
		}
		return run
	}
	seedRun("first waiting work", "waiting_user")
	seedRun("second waiting work", "interrupted")
	task, _ = store.GetTask(ctx, identity.TenantID, task.ID)

	selected := server.coordinator().selectedTaskRuntimeContext(ctx, task, nil, nil, "cli", "cli", "继续", false)
	if selected.Handoff != nil || len(selected.Events) != 0 || len(selected.Artifacts) != 0 || selected.PriorChannel != "" {
		t.Fatalf("ambiguous parent must downgrade to the task card, got %+v", selected)
	}
	prompt := selected.Prompt(10000)
	for _, banned := range []string{"handoff of first", "handoff of second", "event of first", "event of second"} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("downgraded prompt leaked run slice %q:\n%s", banned, prompt)
		}
	}
}

func TestBoundedTaskContextOmitsEventAndCompatibilityHistory(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "bounded-context", "User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Referenced work", Channel: "old-channel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "in_progress", "bounded summary", []string{"bounded next"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{
		TaskID: task.ID, Summary: "bounded handoff", NextSteps: []string{"bounded next"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, Type: "tool.completed", Payload: mustJSON(map[string]string{"result": "must stay out"})}); err != nil {
		t.Fatal(err)
	}
	task, _ = store.GetTask(ctx, identity.TenantID, task.ID)
	server := &Server{Control: store}
	selected := server.coordinator().selectedTaskRuntimeContextWithMode(ctx, task, nil, nil, "cli", "cli", "mention referenced work", attachContextBounded, nil)
	prompt := selected.Prompt(10000)
	for _, want := range []string{"bounded summary", "bounded next"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("bounded prompt missing %q:\n%s", want, prompt)
		}
	}
	// The bounded task card carries summary/next steps/blockers only: a
	// task-wide handoff would be another run's slice (P0).
	if selected.Handoff != nil || strings.Contains(prompt, "bounded handoff") {
		t.Fatalf("bounded card must not carry a handoff: %+v\n%s", selected, prompt)
	}
	if selected.PriorChannel != "" || len(selected.Events) != 0 || strings.Contains(prompt, "must stay out") {
		t.Fatalf("bounded context leaked full history: %+v\n%s", selected, prompt)
	}
}

// TestPreLabelContextIsMinimal pins the Work Timeline boundary: a soft
// pre-label GUESS must not bias the prompt with the guessed task's rich
// context. Only id/title/status metadata may appear; summary, handoff,
// events, and artifacts are withheld (the spine tail + recall cover the real
// work). An explicit attach (preLabel=false) keeps the full context — covered
// by TestSelectedTaskRuntimeContext above.
func TestPreLabelContextIsMinimal(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Sel")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "guessed label", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "in_progress", "old summary that must not leak", []string{"stale next step"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{TaskID: task.ID, Summary: "handoff that must not leak", ChangedFiles: []string{"secret/file.go"}}); err != nil {
		t.Fatal(err)
	}
	task, err = store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Control: store}
	selected := server.coordinator().selectedTaskRuntimeContext(ctx, task, nil, nil, "cli", "cli", "an ordinary new message", true)

	if selected.TaskID != task.ID || selected.Title == "" || selected.Status == "" {
		t.Fatalf("minimal metadata must survive: %+v", selected)
	}
	if selected.Summary != "" || selected.Handoff != nil || len(selected.Events) > 0 || len(selected.Artifacts) > 0 || len(selected.NextSteps) > 0 {
		t.Fatalf("pre-label turn leaked rich task context: %+v", selected)
	}
	prompt := selected.Prompt(10000)
	for _, banned := range []string{"old summary that must not leak", "handoff that must not leak", "secret/file.go", "stale next step"} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("pre-label prompt contains banned content %q:\n%s", banned, prompt)
		}
	}
}

func TestExplicitCLIAttachIncludesPossiblyMissedIMFinalResult(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Sel")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "weixin", "publish")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueDelivery(ctx, control.Delivery{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "weixin", Channel: "wx-chat",
		TaskID: task.ID, RunID: run.ID, Kind: delivery.KindFinalResult,
		Content: "Production release succeeded; build id abc-123.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliverySentUnconfirmed(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := store.EnqueueDelivery(ctx, control.Delivery{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "weixin", Channel: "wx-chat",
		TaskID: task.ID, RunID: run.ID, Kind: delivery.KindFinalResult,
		Content: "Pending-session final result: deployment health is green.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryPendingSession(ctx, pending.ID, "fresh session required"); err != nil {
		t.Fatal(err)
	}
	// Full context is run-scoped (P0): the prior run must be an unclaimed
	// resumable parent for the continuation to see its delivery warnings.
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}

	server := &Server{Control: store}
	selected := server.coordinator().selectedTaskRuntimeContext(ctx, task, nil, nil, "cli", "cli", "what was the result?", false)
	prompt := selected.Prompt(10000)
	if !strings.Contains(prompt, "Delivery Continuity") || !strings.Contains(prompt, "build id abc-123") ||
		!strings.Contains(prompt, "deployment health is green") {
		t.Fatalf("CLI continuation must see a possibly missed IM final result:\n%s", prompt)
	}

	weixin := server.coordinator().selectedTaskRuntimeContext(ctx, task, nil, nil, "weixin", "wx-chat", "continue", false)
	if strings.Contains(weixin.Prompt(10000), "Delivery Continuity") {
		t.Fatal("same-platform continuation must not inject a cross-endpoint warning")
	}
	preLabel := server.coordinator().selectedTaskRuntimeContext(ctx, task, nil, nil, "cli", "cli", "unrelated message", true)
	if strings.Contains(preLabel.Prompt(10000), "Delivery Continuity") {
		t.Fatal("soft pre-label must not inject delivery history from a guessed task")
	}
}
