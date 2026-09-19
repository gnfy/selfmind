package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/kernel"
	"selfmind/internal/verification"
)

func TestCompletionPreflightMatchesFinalEvidence(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "preflight", "Preflight")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "verify then change", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "verify then change")
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []kernel.RunEvidence{
		{ToolName: "verify", Kind: "verification", Status: "succeeded", StartedAt: 10, FinishedAt: 20, Command: &kernel.CommandEvidence{Command: "check", ExitCode: 0}},
		{ToolName: "patch", Kind: "mutation", Status: "succeeded", StartedAt: 30, FinishedAt: 40, Files: []kernel.FileEffect{{Path: "report.md", BeforeSHA256: "old", AfterSHA256: "new"}}},
	} {
		if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": evidence})}); err != nil {
			t.Fatal(err)
		}
	}
	c := (&Server{Control: store}).coordinator()
	verdict, _ := c.evidenceOutcome(ctx, identity.TenantID, run.ID)
	if verdict == nil || verdict.State != "stale" {
		t.Fatalf("final verdict=%+v", verdict)
	}
	err = c.newRunPlanProjection(identity, run, kernel.ToolInvocationScope{}).ValidateCompletion(ctx)
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("completion accepted evidence finalization rejects: %v", err)
	}
}

func TestCompletionPreflightRecoveryAndNewEffects(t *testing.T) {
	ctx := context.Background()
	daemon, store, identity := newTaskViewServer(t)
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inputs and output", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "check inputs and write output")
	if err != nil {
		t.Fatal(err)
	}
	appendEvidence := func(e kernel.RunEvidence) {
		t.Helper()
		if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": e})}); err != nil {
			t.Fatal(err)
		}
	}
	deps := []string{"/project/source.txt"}
	check := kernel.RunEvidence{ToolCallID: "first", ToolName: "verify", Kind: "verification", Status: "succeeded", StartedAt: 10, FinishedAt: 20, Command: &kernel.CommandEvidence{Command: "check", Binding: &verification.Binding{Version: 2, Criterion: "expected value", Target: "source", LocalDependencies: &deps}}}
	appendEvidence(check)
	appendEvidence(kernel.RunEvidence{ToolName: "write_file", Kind: "mutation", Status: "succeeded", StartedAt: 30, FinishedAt: 40, Files: []kernel.FileEffect{{Path: "/project/report.md", BeforeSHA256: "before", AfterSHA256: "after"}}})
	c := daemon.coordinator()
	projection := c.newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	assertVerdict := func(want string) {
		t.Helper()
		v, _ := c.evidenceOutcome(ctx, identity.TenantID, run.ID)
		if v == nil || v.State != want {
			t.Fatalf("verdict=%+v want=%s", v, want)
		}
		err := projection.ValidateCompletion(ctx)
		if (err == nil) != (want == "passed") {
			t.Fatalf("preflight disagrees with %s: %v", want, err)
		}
	}
	assertVerdict("passed")
	// A failed write after a successful preflight still changed a dependency.
	appendEvidence(kernel.RunEvidence{ToolName: "patch", Kind: "mutation", Status: "failed", StartedAt: 50, FinishedAt: 60, Files: []kernel.FileEffect{{Path: "/project/source.txt", BeforeSHA256: "before", AfterSHA256: "partial"}}})
	assertVerdict("stale")
	check.ToolCallID, check.StartedAt, check.FinishedAt = "replacement", 70, 80
	check.Command.Binding.Replaces, check.Command.Binding.Reason = "first", "recheck the same condition after repair"
	if err := store.ValidateVerificationReplacement(ctx, identity.TenantID, run.ID, *check.Command.Binding, ""); err != nil {
		t.Fatalf("Main cannot repair the returned gap: %v", err)
	}
	appendEvidence(check)
	assertVerdict("passed")
	// Malformed durable facts cannot become an empty successful evidence set.
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": "corrupt"})}); err != nil {
		t.Fatal(err)
	}
	assertVerdict("blocked")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.evidenceOutcome(ctx, identity.TenantID, run.ID); v == nil || v.State != "blocked" {
		t.Fatalf("unavailable evidence=%+v", v)
	}
}
