package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
)

func newSinkHarness(t *testing.T) (*toolArtifactSink, *control.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "tenant", "cli", "local", "Tester")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(context.Background(), control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "spool test",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(context.Background(), task, "cli", "spool")
	if err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(dir, "tool-output")
	sink := &toolArtifactSink{
		dir:      filepath.Join(spool, identity.PersonID),
		store:    store,
		tenantID: identity.TenantID,
		runID:    run.ID,
	}
	return sink, store, task.ID
}

// TestToolArtifactSinkSavesFileAndRow: the spool writes the person-scoped
// file (the read path) and the durable control-plane row (resume listings).
func TestToolArtifactSinkSavesFileAndRow(t *testing.T) {
	sink, store, taskID := newSinkHarness(t)
	content := strings.Repeat("x", 30000)

	ref, err := sink.SaveToolOutput(context.Background(), "terminal", content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref.ID, "art_") || ref.Bytes != len(content) {
		t.Fatalf("bad ref: %+v", ref)
	}
	data, err := os.ReadFile(filepath.Join(sink.dir, ref.ID+".txt"))
	if err != nil || len(data) != len(content) {
		t.Fatalf("spooled file must hold the full output: %v len=%d", err, len(data))
	}
	artifacts, err := store.ListTaskArtifacts(context.Background(), taskID, 10)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("expected one artifact row: %v %d", err, len(artifacts))
	}
	if artifacts[0].ID != ref.ID || artifacts[0].Kind != "tool_output" || artifacts[0].Name != "terminal" {
		t.Fatalf("row mismatch: %+v", artifacts[0])
	}
}

// TestToolArtifactSinkPerRunCaps: beyond the per-run budget the sink refuses
// (kernel degrades to the plain truncation note) instead of filling the disk.
func TestToolArtifactSinkPerRunCaps(t *testing.T) {
	sink, _, _ := newSinkHarness(t)
	for i := 0; i < maxToolArtifactsPerRun; i++ {
		if _, err := sink.SaveToolOutput(context.Background(), "terminal", "content"); err != nil {
			t.Fatalf("save %d failed early: %v", i, err)
		}
	}
	if _, err := sink.SaveToolOutput(context.Background(), "terminal", "content"); err == nil {
		t.Fatal("count cap must refuse further spooling")
	}

	fresh, _, _ := newSinkHarness(t)
	big := strings.Repeat("y", maxToolArtifactBytesPerRun/2+1)
	if _, err := fresh.SaveToolOutput(context.Background(), "terminal", big); err != nil {
		t.Fatalf("first big save must pass: %v", err)
	}
	if _, err := fresh.SaveToolOutput(context.Background(), "terminal", big); err == nil {
		t.Fatal("byte cap must refuse further spooling")
	}
}

func TestToolArtifactSinkFollowsClaimedRunForLaterOutputs(t *testing.T) {
	ctx := context.Background()
	sink, store, oldTaskID := newSinkHarness(t)
	run, err := store.GetRun(ctx, sink.tenantID, sink.runID)
	if err != nil {
		t.Fatal(err)
	}
	parentTask, err := store.CreateTask(ctx, control.TaskCreate{TenantID: sink.tenantID, PersonID: run.PersonID, Title: "prepared work", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRun(ctx, parentTask, "cli", "prepared")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, sink.tenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimInteractionContinuation(ctx, sink.tenantID, run.PersonID, run.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	ref, err := sink.SaveToolOutput(ctx, "read_file", "read after continuation")
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := store.ListTaskArtifacts(ctx, parentTask.ID, 10)
	if err != nil || len(artifacts) != 1 || artifacts[0].ID != ref.ID {
		t.Fatalf("later output attached to stale task %s: %+v err=%v", oldTaskID, artifacts, err)
	}
}
