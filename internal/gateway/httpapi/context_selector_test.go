package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
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
	selected := server.coordinator().selectedTaskRuntimeContext(ctx, task, run, ws, "cli")
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
