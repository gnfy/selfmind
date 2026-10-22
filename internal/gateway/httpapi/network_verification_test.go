package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

func recordVerification(t *testing.T, store *control.Store, task *control.Task, run *control.Run, command, decision, permission string) {
	t.Helper()
	ctx := context.Background()
	zero := 0
	for _, e := range []kernel.RunEvidence{
		{ToolName: "write_file", Kind: "mutation", Status: "succeeded", StartedAt: 10, FinishedAt: 20,
			Files: []kernel.FileEffect{{Path: "main.go", Operation: "write", BeforeSHA256: "old", AfterSHA256: "new"}}},
		{ToolCallID: "check", ToolName: "verify", Kind: "verification", Status: "succeeded", StartedAt: 30, FinishedAt: 40,
			Process: &kernel.ToolProcessResult{Started: true, ExitCode: &zero, NetworkMode: "isolated", NetworkDecision: decision, NetworkPermission: permission},
			Command: &kernel.CommandEvidence{Command: command, CWD: ".", Kind: "test", ExitCode: 0}},
	} {
		if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "evidence.recorded", Visibility: "task",
			Payload: mustJSON(map[string]interface{}{"evidence": e})}); err != nil {
			t.Fatal(err)
		}
	}
}

func startVerificationRun(t *testing.T, name string) (*control.Store, *control.Task, *control.Run) {
	t.Helper()
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	name = strings.NewReplacer(" ", "-", "/", "-", ".", "-").Replace(name)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", name, name)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: name, Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "change code")
	if err != nil {
		t.Fatal(err)
	}
	return store, task, run
}

// A pass that ran without network although the workspace's network grant
// permits it is still a pass, but its summary and the person's closing notice
// say what it did not cover, so the answer cannot quietly claim more.
func TestOfflineVerificationDespiteAGrantIsStatedToThePerson(t *testing.T) {
	for _, command := range []string{"go test ./...", "npm test"} {
		t.Run(command, func(t *testing.T) {
			store, task, run := startVerificationRun(t, "offline-"+command)
			recordVerification(t, store, task, run, command, executionenv.NetworkWithheldNotBound, executionenv.NetworkPermissionGrant)
			server := &Server{Control: store, DefaultTenantID: "default"}
			got, _ := server.coordinator().evidenceOutcome(context.Background(), task.TenantID, run.ID)
			if got == nil || got.State != "passed" || len(got.Checks) != 1 {
				t.Fatalf("outcome = %+v", got)
			}
			if got.Checks[0].NetworkDecision != executionenv.NetworkWithheldNotBound || got.Checks[0].NetworkPermission != executionenv.NetworkPermissionGrant || !got.Checks[0].NetworkMayFetch {
				t.Fatalf("the check lost its network evidence: %+v", got.Checks[0])
			}
			if !strings.Contains(got.Summary, "without network") {
				t.Fatalf("the summary hides the reduced coverage: %q", got.Summary)
			}
			notice := withVerificationNotice("Done.", got, nil)
			for _, want := range []string{"Done.", "without network", "network: true"} {
				if !strings.Contains(notice, want) {
					t.Fatalf("the person's notice does not say %q: %q", want, notice)
				}
			}
		})
	}
}

// The notice is for a pass that could have used the network: a vet, a lint or a
// formatting check that ran offline in a workspace with a grant is a plain pass.
func TestOfflineVerificationOfACommandThatCannotFetchAddsNoNotice(t *testing.T) {
	for _, command := range []string{"go vet ./...", "gofmt -l .", "golangci-lint run", "make lint", "pytest -q"} {
		t.Run(command, func(t *testing.T) {
			store, task, run := startVerificationRun(t, "cannot-fetch-"+command)
			recordVerification(t, store, task, run, command, executionenv.NetworkWithheldNotBound, executionenv.NetworkPermissionGrant)
			server := &Server{Control: store, DefaultTenantID: "default"}
			got, _ := server.coordinator().evidenceOutcome(context.Background(), task.TenantID, run.ID)
			if got == nil || got.State != "passed" || len(got.Checks) != 1 || got.Checks[0].NetworkMayFetch {
				t.Fatalf("outcome = %+v", got)
			}
			// The evidence still records what happened; only the notice is withheld.
			if got.Checks[0].NetworkDecision != executionenv.NetworkWithheldNotBound || strings.Contains(got.Summary, "without network") {
				t.Fatalf("summary=%q check=%+v", got.Summary, got.Checks[0])
			}
			if notice := withVerificationNotice("Done.", got, nil); notice != "Done." {
				t.Fatalf("a vet or lint pass added UI noise: %q", notice)
			}
		})
	}
}

func TestNetworkedOrUnpermittedVerificationAddsNoNotice(t *testing.T) {
	for name, tc := range map[string]struct{ decision, permission string }{
		"networked":     {executionenv.NetworkDecisionShared, executionenv.NetworkPermissionGrant},
		"trusted":       {executionenv.NetworkWithheldNotBound, executionenv.NetworkPermissionTrust},
		"no permission": {executionenv.NetworkWithheldNoGrant, ""},
		"old evidence":  {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			store, task, run := startVerificationRun(t, "plain-"+name)
			recordVerification(t, store, task, run, "go test ./...", tc.decision, tc.permission)
			server := &Server{Control: store, DefaultTenantID: "default"}
			got, _ := server.coordinator().evidenceOutcome(context.Background(), task.TenantID, run.ID)
			if got == nil || got.State != "passed" || strings.Contains(got.Summary, "without network") {
				t.Fatalf("outcome = %+v", got)
			}
			if notice := withVerificationNotice("Done.", got, nil); notice != "Done." {
				t.Fatalf("successful verification added UI noise: %q", notice)
			}
		})
	}
}

func TestOfflineNoticeNeedsStructuredChecks(t *testing.T) {
	// A bare summary or an outcome with no checks never invents the notice.
	if got := conciseVerificationNotice(&api.VerificationOutcome{State: "passed", Summary: "1 check passed."}); got != "" {
		t.Fatalf("notice = %q", got)
	}
}
