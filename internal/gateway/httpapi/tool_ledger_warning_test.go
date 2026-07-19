package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

// TestUncertainToolWarningIsIntentIndependent pins the P0-B closure: a task
// with a crash-orphaned side-effect ledger entry gets the verification warning
// regardless of continuation intent (a boot-requeued run re-drains as a "new"
// message, so gating on IntentContinue would let it silently re-fire).
func TestUncertainToolWarningIsIntentIndependent(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "deploy task", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "deploy")
	if err != nil {
		t.Fatal(err)
	}

	coord := daemon.coordinator()
	// No uncertain entries yet → input is unchanged.
	if got := coord.withUncertainToolWarning(ctx, identity, task, "original input"); got != "original input" {
		t.Fatalf("clean task must pass input through: %q", got)
	}

	// A dispatched-but-unresolved side-effect entry (the crash window).
	if err := store.RecordToolDispatch(ctx, identity.TenantID, control.ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "c1", ToolName: "terminal", ArgsHash: "h", RetryClass: "side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	got := coord.withUncertainToolWarning(ctx, identity, task, "original input")
	if !strings.Contains(got, "uncertain tool calls") || !strings.Contains(got, "terminal") {
		t.Fatalf("warning not injected: %q", got)
	}
	if !strings.HasSuffix(got, "original input") {
		t.Fatalf("original input must be preserved at the tail: %q", got)
	}

	// Recording the outcome closes the window → warning disappears.
	if err := store.RecordToolOutcome(ctx, identity.TenantID, run.ID, "c1", true); err != nil {
		t.Fatal(err)
	}
	if got := coord.withUncertainToolWarning(ctx, identity, task, "original input"); got != "original input" {
		t.Fatalf("resolved entry must not warn: %q", got)
	}
}
