package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// TestTransientHelperDoesNotDemandVerification replays the run that stranded a
// finished task in Attention: it wrote a throwaway `.py` helper, used it to
// rewrite six `.md` release records, deleted the helper, committed, and pushed.
// The push succeeded — but `.py` is on the verifiable-suffix list and `.md` is
// not, so the deleted helper alone forced verification_partial and the run
// stayed resumable forever.
func TestTransientHelperDoesNotDemandVerification(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "tmp_resolve_merge.py")
	kept := filepath.Join(dir, "release-notes.md")
	if err := os.WriteFile(kept, []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Created from nothing, then modified, then removed outside the tracker
	// (an `rm` inside a terminal command leaves no file effect).
	createdThenGone := []kernel.FileEffect{{Path: gone, BeforeSHA256: "", AfterSHA256: "aaa"}}
	modifiedAgain := []kernel.FileEffect{{Path: gone, BeforeSHA256: "aaa", AfterSHA256: "bbb"}}
	transient := map[string]bool{gone: true}

	if evidenceChangedFiles(createdThenGone, transient) {
		t.Fatal("a file the run created and removed is not a verifiable change")
	}
	if evidenceChangedFiles(modifiedAgain, transient) {
		t.Fatal("later edits to a transient file are still not a verifiable change")
	}

	// A real file that survives is unaffected.
	if !evidenceChangedFiles([]kernel.FileEffect{{Path: kept, BeforeSHA256: "x", AfterSHA256: "y"}}, transient) {
		t.Fatal("a surviving edited file must still count")
	}

	// Deleting a file the run did NOT create is a real change: its before-hash
	// is real, so it never enters the transient set and still demands
	// verification.
	deletedRealSource := filepath.Join(dir, "service.go")
	if pathExists(deletedRealSource) {
		t.Fatal("fixture error: the path should not exist")
	}
	if !evidenceChangedFiles([]kernel.FileEffect{{Path: deletedRealSource, BeforeSHA256: "real", AfterSHA256: ""}}, map[string]bool{}) {
		t.Fatal("removing pre-existing source is a change worth verifying")
	}
}

// TestPathExistsTreatsUnknownAsPresent: "cannot tell" must never be read as
// "the run cleaned up after itself", or an unreadable path would silently
// excuse a real change from verification.
func TestPathExistsTreatsUnknownAsPresent(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "here.txt")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pathExists(present) {
		t.Fatal("an existing file must read as present")
	}
	if pathExists(filepath.Join(dir, "absent.txt")) {
		t.Fatal("a missing file must read as absent")
	}
	if pathExists("") {
		t.Fatal("an empty path is not a file")
	}
}

// TestEvidenceOutcomeIgnoresATransientHelper drives the whole aggregation the
// way finalization does, not just the file predicate: real evidence events in,
// verification verdict out. This is the exact shape of the run that stayed
// resumable — a `.py` helper created, used, and removed, while the files it
// actually edited were `.md`, which nothing asks to verify.
func TestEvidenceOutcomeIgnoresATransientHelper(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	daemon := &Server{Control: store, DefaultTenantID: "default"}

	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: "default", PersonID: "p1", Title: "backfill records", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "commit and push")
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	helper := filepath.Join(work, "tmp_resolve_merge.py") // created, used, deleted
	record := filepath.Join(work, "2026-09.md")
	if err := os.WriteFile(record, []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	appendEvidence := func(ev kernel.RunEvidence) {
		payload, mErr := json.Marshal(recordedEvidencePayload{Evidence: ev})
		if mErr != nil {
			t.Fatal(mErr)
		}
		if _, aErr := store.AppendEvent(ctx, control.Event{
			TaskID: task.ID, RunID: run.ID, Type: "evidence.recorded",
			Visibility: "task", Channel: "cli", Payload: payload,
		}); aErr != nil {
			t.Fatal(aErr)
		}
	}

	appendEvidence(kernel.RunEvidence{
		ToolName: "patch", Kind: "mutation", Status: "succeeded", StartedAt: 10, FinishedAt: 11,
		Files: []kernel.FileEffect{{Path: helper, Operation: "create", BeforeSHA256: "", AfterSHA256: "aaa"}},
	})
	appendEvidence(kernel.RunEvidence{
		ToolName: "patch", Kind: "mutation", Status: "succeeded", StartedAt: 20, FinishedAt: 21,
		Files: []kernel.FileEffect{{Path: helper, Operation: "update", BeforeSHA256: "aaa", AfterSHA256: "bbb"}},
	})
	appendEvidence(kernel.RunEvidence{
		ToolName: "terminal", Kind: "command", Status: "succeeded", StartedAt: 30, FinishedAt: 31,
		Command: &kernel.CommandEvidence{Command: "python3 tmp_resolve_merge.py && rm -f tmp_resolve_merge.py"},
	})

	verification, files := daemon.coordinator().evidenceOutcome(ctx, task.ID, run.ID)
	if verification == nil {
		t.Fatal("expected a verification verdict")
	}
	if verification.State != "not_applicable" {
		t.Fatalf("state = %q, want not_applicable: a deleted helper is not a change to verify (%s)",
			verification.State, verification.Summary)
	}
	if verification.LatestMutationAt != 0 {
		t.Fatalf("the mutation clock must not be anchored on a file that no longer exists, got %d",
			verification.LatestMutationAt)
	}
	for _, f := range files {
		if f == helper {
			t.Fatalf("a removed helper must not be reported as a changed file: %v", files)
		}
	}
	if verificationRequiresResume(verification, files) {
		t.Fatal("this run must NOT be held resumable; its real work was already done")
	}

	// The same run, but the helper survives: verification is genuinely owed.
	if err := os.WriteFile(helper, []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verification, files = daemon.coordinator().evidenceOutcome(ctx, task.ID, run.ID)
	if verification.State != "not_run" {
		t.Fatalf("a surviving .py helper still owes verification, got %q", verification.State)
	}
	if !verificationRequiresResume(verification, files) {
		t.Fatal("a surviving verifiable file must keep the run resumable")
	}
}
