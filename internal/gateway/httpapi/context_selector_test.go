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
	run, err := store.StartRun(ctx, task, "cli", "input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{
		TaskID:    task.ID,
		Summary:   "Latest handoff summary",
		NextSteps: []string{"finish selector"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveArtifact(ctx, control.Artifact{
		TaskID: task.ID,
		RunID:  run.ID,
		Kind:   "file",
		Name:   "agent.go",
		URI:    "internal/kernel/agent.go",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID:  task.ID,
		RunID:   run.ID,
		Type:    "tool.completed",
		Channel: "cli",
		Payload: mustJSON(map[string]string{"result": "read agent.go"}),
	}); err != nil {
		t.Fatal(err)
	}

	server := &Server{Control: store}
	selected := server.coordinator().selectedTaskRuntimeContext(ctx, task, run, ws, "cli", "cli", "continue the context selector work", false)
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

	server := &Server{Control: store}
	selected := server.coordinator().selectedTaskRuntimeContext(ctx, task, run, nil, "cli", "cli", "what was the result?", false)
	prompt := selected.Prompt(10000)
	if !strings.Contains(prompt, "Delivery Continuity") || !strings.Contains(prompt, "build id abc-123") {
		t.Fatalf("CLI continuation must see a possibly missed IM final result:\n%s", prompt)
	}

	weixin := server.coordinator().selectedTaskRuntimeContext(ctx, task, run, nil, "weixin", "wx-chat", "continue", false)
	if strings.Contains(weixin.Prompt(10000), "Delivery Continuity") {
		t.Fatal("same-platform continuation must not inject a cross-endpoint warning")
	}
	preLabel := server.coordinator().selectedTaskRuntimeContext(ctx, task, run, nil, "cli", "cli", "unrelated message", true)
	if strings.Contains(preLabel.Prompt(10000), "Delivery Continuity") {
		t.Fatal("soft pre-label must not inject delivery history from a guessed task")
	}
}
