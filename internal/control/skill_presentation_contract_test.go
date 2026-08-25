package control

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestSkillCandidateRefIsStableScopedAndNeverUnknownAfterIssue(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := IssueSkillCandidateRefInput{
		IdentityTenantID: "default", ControlTenantID: "default", PersonID: "person-1",
		RunID: "run-1", WorkUnitID: "unit-1", SkillKey: "skill-key", SkillName: "flow",
		VersionHash: "version", PackageHash: "package", DescriptionHash: "description",
	}
	first, err := store.IssueSkillCandidateRef(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.IssueSkillCandidateRef(context.Background(), input)
	if err != nil || first.CandidateRef != second.CandidateRef {
		t.Fatalf("refs first=%+v second=%+v err=%v", first, second, err)
	}
	if other, err := store.ResolveSkillCandidateRef(context.Background(), "default", "person-2", "run-1", "unit-1", first.CandidateRef); err != nil || other != nil {
		t.Fatalf("cross-person ref leaked: %+v err=%v", other, err)
	}
	used, err := store.RecordSkillCandidateRefUse(context.Background(), first.CandidateRef, "stale", true)
	if err != nil || used.DriftCount != 1 || used.State != "stale" {
		t.Fatalf("used=%+v err=%v", used, err)
	}
	resolved, err := store.ResolveSkillCandidateRef(context.Background(), "default", "person-1", "run-1", "unit-1", first.CandidateRef)
	if err != nil || resolved == nil || resolved.CandidateRef != first.CandidateRef {
		t.Fatalf("issued ref became unknown: %+v err=%v", resolved, err)
	}
}

func TestSkillCandidateRefLedgerIsBoundedPerWorkUnit(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for index := 0; index < MaxSkillCandidateRefsPerWorkUnit; index++ {
		_, err := store.IssueSkillCandidateRef(ctx, IssueSkillCandidateRefInput{
			IdentityTenantID: "default", ControlTenantID: "default", PersonID: "person-limit",
			RunID: "run-limit", WorkUnitID: "unit-limit", SkillKey: fmt.Sprintf("skill-%03d", index),
			SkillName: fmt.Sprintf("skill-%03d", index), VersionHash: "version",
			PackageHash: fmt.Sprintf("package-%03d", index), DescriptionHash: "description",
		})
		if err != nil {
			t.Fatalf("issue ref %d: %v", index, err)
		}
	}
	_, err = store.IssueSkillCandidateRef(ctx, IssueSkillCandidateRefInput{
		IdentityTenantID: "default", ControlTenantID: "default", PersonID: "person-limit",
		RunID: "run-limit", WorkUnitID: "unit-limit", SkillKey: "overflow", SkillName: "overflow",
		VersionHash: "version", PackageHash: "overflow-package", DescriptionHash: "description",
	})
	if err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("unbounded candidate-ref ledger: %v", err)
	}
}

func TestSkillPackageResourcesAreImmutableAndReadable(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	body := "resource body"
	digest := sha256.Sum256([]byte(body))
	resource := SkillPackageResource{Path: "references/detail.md", ContentHash: fmt.Sprintf("%x", digest[:]), ContentBody: body, Bytes: len(body)}
	if err := store.RecordSkillPackageResources(context.Background(), "default", "skill-key", "package", []SkillPackageResource{resource}); err != nil {
		t.Fatal(err)
	}
	got, err := store.SkillPackageResource(context.Background(), "default", "skill-key", "package", resource.Path)
	if err != nil || got == nil || got.ContentBody != body {
		t.Fatalf("resource=%+v err=%v", got, err)
	}
	conflict := resource
	conflict.ContentBody = "changed"
	changedDigest := sha256.Sum256([]byte(conflict.ContentBody))
	conflict.ContentHash = fmt.Sprintf("%x", changedDigest[:])
	conflict.Bytes = len(conflict.ContentBody)
	if err := store.RecordSkillPackageResources(context.Background(), "default", "skill-key", "package", []SkillPackageResource{conflict}); err == nil {
		t.Fatal("immutable package resource conflict should fail")
	}
}

func TestSkillCandidateRefsExpireOnlyWhenTheirWorkUnitEnds(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "candidate-cleanup", "Candidate Cleanup")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "candidate cleanup", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "inspect then finish")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := store.IssueSkillCandidateRef(ctx, IssueSkillCandidateRefInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		SkillKey: "skill-key", SkillName: "flow", VersionHash: "version",
		PackageHash: "package", DescriptionHash: "description",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := store.ResolveSkillCandidateRef(ctx, identity.TenantID, identity.PersonID, run.ID, run.WorkUnitID, issued.CandidateRef); err != nil || resolved == nil {
		t.Fatalf("live ref=%+v err=%v", resolved, err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: task.ID,
		TaskStatus: "done", Summary: "finished", VerificationState: "passed",
		Channel: "cli", Event: Event{Type: "run.finished", Payload: []byte(`{"status":"done"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if resolved, err := store.ResolveSkillCandidateRef(ctx, identity.TenantID, identity.PersonID, run.ID, run.WorkUnitID, issued.CandidateRef); err != nil || resolved != nil {
		t.Fatalf("terminal work-unit retained candidate ref=%+v err=%v", resolved, err)
	}
}

func TestPruneSkillCandidateRefsIsDryRunFirstAndPreservesLiveOwners(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "candidate-prune", "Candidate Prune")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "candidate prune", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	liveRun, err := store.StartRun(ctx, task, "cli", "keep live")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(runID, workUnitID, name string) *SkillCandidateRef {
		t.Helper()
		ref, issueErr := store.IssueSkillCandidateRef(ctx, IssueSkillCandidateRefInput{
			IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
			PersonID: identity.PersonID, RunID: runID, WorkUnitID: workUnitID,
			SkillKey: "skill-" + name, SkillName: name, VersionHash: "version",
			PackageHash: "package-" + name, DescriptionHash: "description",
		})
		if issueErr != nil {
			t.Fatal(issueErr)
		}
		return ref
	}
	live := issue(liveRun.ID, liveRun.WorkUnitID, "live")
	terminalRun, err := store.StartRun(ctx, task, "cli", "end first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: terminalRun.ID, RunStatus: "done", TaskID: task.ID,
		TaskStatus: "done", Summary: "finished", VerificationState: "passed",
		Channel: "cli", Event: Event{Type: "run.finished", Payload: []byte(`{"status":"done"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	terminal := issue(terminalRun.ID, terminalRun.WorkUnitID, "terminal")
	orphan := issue("run-missing", "unit-missing", "orphan")

	preview, err := store.PruneSkillCandidateRefs(ctx, identity.TenantID, false)
	if err != nil || preview.Terminal != 1 || preview.Orphan != 1 || preview.Deleted != 0 || len(preview.Owners) != 2 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if resolved, err := store.ResolveSkillCandidateRef(ctx, identity.TenantID, identity.PersonID, terminalRun.ID, terminalRun.WorkUnitID, terminal.CandidateRef); err != nil || resolved == nil {
		t.Fatalf("dry-run changed terminal ref=%+v err=%v", resolved, err)
	}
	applied, err := store.PruneSkillCandidateRefs(ctx, identity.TenantID, true)
	if err != nil || applied.Deleted != 2 {
		t.Fatalf("applied=%+v err=%v", applied, err)
	}
	if resolved, err := store.ResolveSkillCandidateRef(ctx, identity.TenantID, identity.PersonID, liveRun.ID, liveRun.WorkUnitID, live.CandidateRef); err != nil || resolved == nil {
		t.Fatalf("live ref was pruned=%+v err=%v", resolved, err)
	}
	if resolved, _ := store.ResolveSkillCandidateRef(ctx, identity.TenantID, identity.PersonID, "run-missing", "unit-missing", orphan.CandidateRef); resolved != nil {
		t.Fatalf("orphan ref survived apply: %+v", resolved)
	}
}

func TestSkillPresentationDiagnosticsRecomputesStoredDeliveryReceipt(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "receipt-doctor", "Receipt Doctor")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "receipt", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "use skill")
	if err != nil {
		t.Fatal(err)
	}
	main := "## Procedure\nUse exact bytes."
	digest := sha256.Sum256([]byte(main))
	resourceBody := "pinned resource"
	resourceDigest := sha256.Sum256([]byte(resourceBody))
	resourceHash := fmt.Sprintf("%x", resourceDigest[:])
	if err := store.RecordSkillPackageResources(ctx, identity.TenantID, "skill-key", "package", []SkillPackageResource{{
		Path: "references/detail.md", ContentHash: resourceHash, ContentBody: resourceBody, Bytes: len(resourceBody),
	}}); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`[{"path":"references/detail.md","content_hash":"%s","bytes":%d}]`, resourceHash, len(resourceBody))
	activation, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		SkillKey: "skill-key", SkillName: "flow", VersionHash: "version", PackageHash: "package",
		ActivationSource: "model", DeliveryContractVersion: 1, DeliveryMode: "full",
		DeliveredMain: main, DeliveredMainHash: fmt.Sprintf("%x", digest[:]), DeliveredMainBytes: len(main),
		ResourceManifestJSON: manifest, ContentBody: "stored source", CreatedBy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.InspectSkillPresentation(ctx, identity.TenantID, identity.PersonID)
	if err != nil || !report.Healthy() || report.FullActivations != 1 || report.PackageResources != 1 {
		t.Fatalf("healthy report=%+v err=%v", report, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE run_skill_activations SET delivered_main_hash='corrupt' WHERE id=?`, activation.ID); err != nil {
		t.Fatal(err)
	}
	report, err = store.InspectSkillPresentation(ctx, identity.TenantID, identity.PersonID)
	if err != nil || report.InvalidDeliveryReceipts != 1 || !report.Fatal() {
		t.Fatalf("corrupt report=%+v err=%v", report, err)
	}
	if len(report.Issues) != 1 {
		t.Fatalf("corrupt receipt did not produce one exact finding: %+v", report.Issues)
	}
	issue := report.Issues[0]
	if issue.Code != "delivered_hash_mismatch" || issue.Severity != "fatal" ||
		!strings.Contains(issue.Location, activation.ID+"/delivered_main_hash") ||
		issue.Expected == "" || issue.Observed != "corrupt" || len(issue.Remediations) == 0 || len(issue.Verify) == 0 {
		t.Fatalf("receipt finding is not actionable: %+v", issue)
	}
}
