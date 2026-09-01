package control

import (
	"context"
	"testing"
)

func TestTaskReferenceEvidencePromotesAndConflictAbstains(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "task-ref-user", "User")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "First work"})
	if err != nil {
		t.Fatal(err)
	}

	write := TaskReferenceWrite{TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: first.ID,
		Class: TaskReferenceLiteral, Value: "客户门户升级", Status: TaskReferenceCandidate, Provenance: "user_text"}
	write.RunID = "run-1"
	ref, err := store.UpsertTaskReference(ctx, write)
	if err != nil || ref.Status != TaskReferenceCandidate {
		t.Fatalf("first evidence = %+v, %v", ref, err)
	}
	write.RunID = "run-2"
	// Run support alone never activates a reference (simplification P2): it
	// stays a candidate search hint with its support recorded.
	ref, err = store.UpsertTaskReference(ctx, write)
	if err != nil || ref.Status != TaskReferenceCandidate || ref.SupportCount != 2 {
		t.Fatalf("second evidence = %+v, %v", ref, err)
	}
	matches, err := store.FindTaskReferenceMatches(ctx, identity.TenantID, identity.PersonID, "继续客户门户升级", 10)
	if err != nil || len(matches) != 0 {
		t.Fatalf("unconfirmed candidate must not match, matches=%+v err=%v", matches, err)
	}
	// An explicit user confirmation is the only activation path.
	if _, err := store.UpsertTaskReference(ctx, TaskReferenceWrite{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: first.ID,
		Class: TaskReferenceLiteral, Value: "客户门户升级", Status: TaskReferenceActive,
		UserConfirmed: true, Provenance: "user_control", SourceRef: "test",
	}); err != nil {
		t.Fatal(err)
	}
	matches, err = store.FindTaskReferenceMatches(ctx, identity.TenantID, identity.PersonID, "继续客户门户升级", 10)
	if err != nil || len(matches) != 1 || matches[0].Task.ID != first.ID {
		t.Fatalf("confirmed reference must match = %+v, %v", matches, err)
	}

	second, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Second work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTaskReference(ctx, TaskReferenceWrite{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: second.ID,
		Class: TaskReferenceLiteral, Value: "客户门户升级", Status: TaskReferenceActive,
		UserConfirmed: true, Provenance: "user_control", SourceRef: "test",
	}); err != nil {
		t.Fatal(err)
	}
	matches, err = store.FindTaskReferenceMatches(ctx, identity.TenantID, identity.PersonID, "继续客户门户升级", 10)
	if err != nil || len(matches) != 0 {
		t.Fatalf("conflicted reference must abstain, matches=%+v err=%v", matches, err)
	}
}

func TestTaskReferenceCorrectionDowngradesAutomaticBinding(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "correction-user", "User")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Wrong task"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Right task"})
	if err != nil {
		t.Fatal(err)
	}
	write := TaskReferenceWrite{TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: first.ID,
		Class: TaskReferenceLiteral, Value: "customer-portal", Status: TaskReferenceCandidate, Provenance: "user_text"}
	for _, runID := range []string{"run-support-1", "run-support-2"} {
		write.RunID = runID
		write.SourceRef = "turn:" + runID
		if _, err := store.UpsertTaskReference(ctx, write); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordTaskResolution(ctx, TaskResolutionRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: "run-corrected", InputHash: "hash",
		MatchedSurfaceForms: []string{"customer-portal"}, SelectedTaskID: first.ID, FinalTaskID: second.ID,
		Outcome: "corrected", AnalyzerEvaluated: true,
	}); err != nil {
		t.Fatal(err)
	}
	refs, err := store.ListTaskReferencesForTask(ctx, identity.TenantID, identity.PersonID, first.ID, 10)
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	if refs[0].Status != TaskReferenceCandidate || refs[0].SupportCount != 1 {
		t.Fatalf("corrected automatic binding must lose authority: %+v", refs[0])
	}
	if matches, err := store.FindTaskReferenceMatches(ctx, identity.TenantID, identity.PersonID, "continue customer-portal", 10); err != nil || len(matches) != 0 {
		t.Fatalf("downgraded binding must abstain: matches=%+v err=%v", matches, err)
	}
}

func TestTaskReferenceCorrectionDoesNotDowngradeUserConfirmedBinding(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "confirmed-correction-user", "User")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Selected task"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Final task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTaskReference(ctx, TaskReferenceWrite{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: first.ID,
		Class: TaskReferenceLiteral, Value: "customer-portal", Status: TaskReferenceActive,
		UserConfirmed: true, Provenance: "user_control", SourceRef: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskResolution(ctx, TaskResolutionRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: "run-corrected-confirmed", InputHash: "hash",
		MatchedSurfaceForms: []string{"customer-portal"}, SelectedTaskID: first.ID, FinalTaskID: second.ID,
		Outcome: "corrected", AnalyzerEvaluated: true,
	}); err != nil {
		t.Fatal(err)
	}
	refs, err := store.ListTaskReferencesForTask(ctx, identity.TenantID, identity.PersonID, first.ID, 10)
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	if refs[0].Status != TaskReferenceActive || !refs[0].UserConfirmed {
		t.Fatalf("user-confirmed binding must remain active after an analyzer correction: %+v", refs[0])
	}
}

func TestTaskReferenceWriteRejectsForeignTask(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	owner, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	other, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "other", "Other")
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "Private task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTaskReference(ctx, TaskReferenceWrite{
		TenantID: other.TenantID, PersonID: other.PersonID, TaskID: task.ID,
		Class: TaskReferenceLiteral, Value: "private-task", UserConfirmed: true,
	}); err == nil {
		t.Fatal("foreign task reference write must be rejected")
	}
}

func TestTaskReferenceSurfaceMatchingIsLanguageAware(t *testing.T) {
	for _, tc := range []struct {
		text, ref string
		want      bool
	}{
		{"continue customer portal", "customer portal", true},
		{"continue customer portals", "customer portal", false},
		{"继续客户门户升级", "客户门户", true},
		{"check https://example.com/app now", "https://example.com/app", true},
	} {
		if got := TaskReferenceAppearsInText(tc.text, tc.ref); got != tc.want {
			t.Errorf("TaskReferenceAppearsInText(%q, %q)=%v want %v", tc.text, tc.ref, got, tc.want)
		}
	}
}

func TestRecordTaskResolutionUpdatesOneRunRow(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	record := TaskResolutionRecord{TenantID: "default", PersonID: "person", RunID: "run-1", InputHash: "hash",
		SelectedTaskID: "task-a", FinalTaskID: "task-a", Reason: "current_prelabel", Outcome: "pending"}
	if err := store.RecordTaskResolution(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.FinalTaskID = "task-b"
	record.Outcome = "corrected"
	record.AnalyzerEvaluated = true
	if err := store.RecordTaskResolution(ctx, record); err != nil {
		t.Fatal(err)
	}
	var count int
	var finalTask, outcome string
	var analyzed int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*), final_task_id, outcome, analyzer_evaluated FROM task_resolution_events WHERE tenant_id = ? AND run_id = ?`, "default", "run-1").Scan(&count, &finalTask, &outcome, &analyzed); err != nil {
		t.Fatal(err)
	}
	if count != 1 || finalTask != "task-b" || outcome != "corrected" || analyzed != 1 {
		t.Fatalf("count=%d final=%q outcome=%q analyzed=%d", count, finalTask, outcome, analyzed)
	}
}

func TestReadTaskReferenceStatsIncludesKnowledgeAndResolution(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "person", "User")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.RegisterWorkspace(ctx, Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: "repo", LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: workspace.ID, Title: "Release alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertTaskReference(ctx, TaskReferenceWrite{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, Class: TaskReferenceLiteral,
		Value: "release-alpha", UserConfirmed: true, Provenance: "user",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTaskResolution(ctx, TaskResolutionRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: "run", InputHash: "hash", Outcome: "corrected",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceWorkspaceKnowledge(ctx, identity.TenantID, identity.PersonID, workspace.ID, []WorkspaceKnowledgeWrite{{
		FilePath: "/repo/AGENTS.md", FileName: "AGENTS.md", ContentHash: "hash", Section: 0, Title: "Build", Excerpt: "go test",
	}}); err != nil {
		t.Fatal(err)
	}
	stats, err := store.ReadTaskReferenceStats(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Active != 1 || stats.ResolutionCorrected != 1 || stats.KnowledgeFiles != 1 || stats.KnowledgeSections != 1 {
		t.Fatalf("stats=%+v", stats)
	}
}
