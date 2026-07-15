package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

func TestVerificationStateRequiresChecksAfterLatestMutation(t *testing.T) {
	checks := []api.VerificationCheck{{Status: "succeeded", StartedAt: 10, FinishedAt: 20}}
	if state, _ := verificationState(30, checks); state != "stale" {
		t.Fatalf("state = %q, want stale", state)
	}
	checks = append(checks, api.VerificationCheck{Status: "succeeded", StartedAt: 40, FinishedAt: 50})
	if state, _ := verificationState(30, checks); state != "passed" {
		t.Fatalf("state = %q, want passed", state)
	}
	checks = append(checks, api.VerificationCheck{Status: "failed", StartedAt: 60, FinishedAt: 70, ExitCode: 1})
	if state, _ := verificationState(30, checks); state != "failed" {
		t.Fatalf("state = %q, want failed", state)
	}
}

func TestVerificationStateUsesLatestAttemptForSameCheck(t *testing.T) {
	checks := []api.VerificationCheck{
		{Kind: "test", Command: "go test ./...", CWD: ".", Status: "failed", StartedAt: 10, FinishedAt: 20, ExitCode: 1},
		{Kind: "test", Command: "go test ./...", CWD: ".", Status: "succeeded", StartedAt: 30, FinishedAt: 40},
	}
	if state, _ := verificationState(0, checks); state != "passed" {
		t.Fatalf("state = %q, want passed after a successful retry", state)
	}

	checks = append(checks, api.VerificationCheck{
		Kind: "lint", Command: "golangci-lint run", CWD: ".", Status: "failed", StartedAt: 50, FinishedAt: 60, ExitCode: 1,
	})
	if state, _ := verificationState(0, checks); state != "failed" {
		t.Fatalf("state = %q, want failed while a distinct check still fails", state)
	}
}

func TestEvidenceOutcomeReadsDurableRunEvidence(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Evidence", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "change code")
	if err != nil {
		t.Fatal(err)
	}
	appendEvidence := func(e kernel.RunEvidence) {
		t.Helper()
		_, err := store.AppendEvent(ctx, control.Event{
			TaskID: task.ID, RunID: run.ID, Type: "evidence.recorded", Visibility: "task",
			Payload: mustJSON(map[string]interface{}{"evidence": e}),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	appendEvidence(kernel.RunEvidence{
		ToolName: "write_file", Kind: "mutation", Status: "succeeded", StartedAt: 10, FinishedAt: 20,
		Files: []kernel.FileEffect{{Path: "main.go", Operation: "write", BeforeSHA256: "old", AfterSHA256: "new"}},
	})
	appendEvidence(kernel.RunEvidence{
		ToolName: "verify", Kind: "verification", Status: "succeeded", StartedAt: 30, FinishedAt: 40,
		Command: &kernel.CommandEvidence{Command: "go test ./...", CWD: ".", Kind: "test", ExitCode: 0},
	})

	server := &Server{Control: store, DefaultTenantID: "default"}
	got, files := server.coordinator().evidenceOutcome(ctx, task.ID, run.ID)
	if got == nil || got.State != "passed" || len(got.Checks) != 1 {
		t.Fatalf("outcome = %+v", got)
	}
	if len(files) != 1 || files[0] != "main.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestVerificationFailureMakesCodeRunResumable(t *testing.T) {
	verification := &api.VerificationOutcome{State: "failed", Summary: "one check failed"}
	if !verificationRequiresResume(verification, []string{"main.go"}) {
		t.Fatal("failed verification must make the run resumable")
	}
	if verificationRequiresResume(&api.VerificationOutcome{State: "not_run"}, []string{"README.md"}) {
		t.Fatal("documentation-only mutation should not require a code verification")
	}
	if !verificationRequiresResume(&api.VerificationOutcome{State: "not_run"}, []string{"main.go"}) {
		t.Fatal("unverified code mutation must make the run resumable")
	}
}

func TestVerificationNoticeIsConciseAndEnglish(t *testing.T) {
	got := withVerificationNotice("Changed the file.", &api.VerificationOutcome{
		State:   "not_run",
		Summary: "Files changed, but no verification command was recorded after the change.",
	}, nil)
	want := "Changed the file.\n\nVerification incomplete: no check ran after file changes."
	if got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}

	passed := withVerificationNotice("Done.", &api.VerificationOutcome{State: "passed", Summary: "1 check passed."}, nil)
	if passed != "Done." {
		t.Fatalf("successful verification should not add UI noise: %q", passed)
	}
}
