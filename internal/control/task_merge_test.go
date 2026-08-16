package control

import (
	"context"
	"encoding/json"
	"testing"
)

func newMergeHarness(t *testing.T) (*Store, *IdentityContext) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "tenant", "cli", "local", "Tester")
	if err != nil {
		t.Fatal(err)
	}
	return store, identity
}

func mergeTask(t *testing.T, store *Store, identity *IdentityContext, title string) *Task {
	t.Helper()
	task, err := store.CreateTask(context.Background(), TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// TestMergeTasksMovesEverythingAndArchivesSource: runs, events, artifacts,
// handoffs, and the current-task pointer all follow the merge; the source is
// archived, never deleted.
func TestMergeTasksMovesEverythingAndArchivesSource(t *testing.T) {
	store, identity := newMergeHarness(t)
	ctx := context.Background()
	src := mergeTask(t, store, identity, "tank game v1")
	dst := mergeTask(t, store, identity, "tank game")

	run, err := store.StartRun(ctx, src, "cli", "build the tank game")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: src.ID, RunID: run.ID, Type: "tool.completed", Visibility: "task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveArtifact(ctx, Artifact{TaskID: src.ID, RunID: run.ID, Kind: "tool_output", URI: "/tmp/x.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTaskReference(ctx, TaskReferenceWrite{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: src.ID,
		Class: TaskReferenceLiteral, Value: "tank-release", UserConfirmed: true,
		Provenance: "user_control", SourceRef: "merge-test",
	}); err != nil {
		t.Fatal(err)
	}
	for taskID, runID := range map[string]string{src.ID: "run-ref-src", dst.ID: "run-ref-dst"} {
		if _, err := store.UpsertTaskReference(ctx, TaskReferenceWrite{
			TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: taskID,
			Class: TaskReferenceLiteral, Value: "customer-portal", Status: TaskReferenceCandidate,
			RunID: runID, Provenance: "user_text", SourceRef: "turn:" + runID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	moved, err := store.MergeTasks(ctx, identity.TenantID, identity.PersonID, src.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("expected 1 run moved, got %d", moved)
	}
	runs, _ := store.ListTaskRuns(ctx, identity.TenantID, dst.ID, 10)
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("run must belong to dst: %+v", runs)
	}
	artifacts, _ := store.ListTaskArtifacts(ctx, dst.ID, 10)
	if len(artifacts) != 1 {
		t.Fatalf("artifact must follow: %d", len(artifacts))
	}
	srcAfter, err := store.GetTask(ctx, identity.TenantID, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if srcAfter.ArchivedAt == nil || srcAfter.Status != "archived" {
		t.Fatalf("source must be archived, got status=%s archived=%v", srcAfter.Status, srcAfter.ArchivedAt)
	}
	current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if current == nil || current.ID != dst.ID {
		t.Fatalf("current-task pointer must follow to dst: %+v", current)
	}
	refs, err := store.ListTaskReferencesForTask(ctx, identity.TenantID, identity.PersonID, dst.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("task references must follow and deduplicate on merge: %+v", refs)
	}
	byValue := map[string]TaskReference{}
	for _, ref := range refs {
		byValue[ref.NormalizedValue] = ref
	}
	if ref := byValue["tank-release"]; !ref.UserConfirmed || ref.Status != TaskReferenceActive {
		t.Fatalf("confirmed source reference did not follow merge: %+v", ref)
	}
	if ref := byValue["customer-portal"]; ref.Status != TaskReferenceActive || ref.SupportCount != 2 {
		t.Fatalf("duplicate evidence was not folded and reconciled: %+v", ref)
	}
	if refs, err := store.ListTaskReferencesForTask(ctx, identity.TenantID, identity.PersonID, src.ID, 10); err != nil || len(refs) != 0 {
		t.Fatalf("source references must be moved: refs=%+v err=%v", refs, err)
	}
}

// TestMergeTasksGuards: self-merge, cross-person, and archived targets are
// refused; nothing is mutated on refusal.
func TestMergeTasksGuards(t *testing.T) {
	store, identity := newMergeHarness(t)
	ctx := context.Background()
	a := mergeTask(t, store, identity, "task a")
	b := mergeTask(t, store, identity, "task b")

	if _, err := store.MergeTasks(ctx, identity.TenantID, identity.PersonID, a.ID, a.ID); err == nil {
		t.Fatal("self-merge must be refused")
	}
	if _, err := store.MergeTasks(ctx, identity.TenantID, "someone_else", a.ID, b.ID); err == nil {
		t.Fatal("cross-person merge must be refused")
	}
	if _, err := store.MergeTasks(ctx, identity.TenantID, identity.PersonID, a.ID, "task_missing"); err == nil {
		t.Fatal("missing target must be refused")
	}
	// Archive b, then refuse to merge into it.
	if _, err := store.MergeTasks(ctx, identity.TenantID, identity.PersonID, b.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MergeTasks(ctx, identity.TenantID, identity.PersonID, a.ID, b.ID); err == nil {
		t.Fatal("merging into an archived task must be refused")
	}
}

func TestMergeTasksPreservesOneSkillBinding(t *testing.T) {
	store, identity := newMergeHarness(t)
	ctx := context.Background()
	src := mergeTask(t, store, identity, "source")
	dst := mergeTask(t, store, identity, "target")
	if _, err := store.BindTaskSkill(ctx, BindTaskSkillInput{
		IdentityTenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: src.ID,
		ControlTenantID: identity.TenantID, SkillKey: "source-skill", SkillName: "source-skill",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MergeTasks(ctx, identity.TenantID, identity.PersonID, src.ID, dst.ID); err != nil {
		t.Fatal(err)
	}
	migrated, err := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, dst.ID)
	if err != nil || migrated == nil || migrated.SkillKey != "source-skill" {
		t.Fatalf("source binding did not migrate to empty target: %+v err=%v", migrated, err)
	}

	src2 := mergeTask(t, store, identity, "source conflict")
	if _, err := store.BindTaskSkill(ctx, BindTaskSkillInput{
		IdentityTenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: src2.ID,
		ControlTenantID: identity.TenantID, SkillKey: "other-skill", SkillName: "other-skill",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MergeTasks(ctx, identity.TenantID, identity.PersonID, src2.ID, dst.ID); err != nil {
		t.Fatal(err)
	}
	preserved, _ := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, dst.ID)
	released, _ := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, src2.ID)
	if preserved == nil || preserved.SkillKey != "source-skill" || released == nil || released.State != TaskSkillBindingReleased {
		t.Fatalf("merge conflict loaded more than one default: target=%+v source=%+v", preserved, released)
	}
}

// TestListDuplicateSuggestions: suggestions read back as pair map from events.
func TestListDuplicateSuggestions(t *testing.T) {
	store, identity := newMergeHarness(t)
	ctx := context.Background()
	newer := mergeTask(t, store, identity, "kof game new")
	older := mergeTask(t, store, identity, "kof game")
	payload, _ := json.Marshal(map[string]string{"duplicate_of": older.ID})
	if _, err := store.AppendEvent(ctx, Event{TaskID: newer.ID, Type: "task.duplicate_suggested", Visibility: "task", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListDuplicateSuggestions(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if got[newer.ID] != older.ID {
		t.Fatalf("suggestion missing: %+v", got)
	}
}
