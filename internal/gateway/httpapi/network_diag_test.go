package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
)

// The diagnostics say what the network policy means for a command, and count
// the decisions the active Run's calls recorded, so "why was this offline" has
// an answer a person can read.
func TestExecutionDiagnosticsExplainAndCountNetworkDecisions(t *testing.T) {
	for _, mode := range []string{"shared", "isolated"} {
		got := sandboxNetworkExplanation(mode)
		if !strings.HasPrefix(got, mode) || !strings.Contains(got, "remote service or set network: true") {
			t.Fatalf("%s explanation does not say what receives the network: %q", mode, got)
		}
	}
	// Operator policy allowing the network is not the whole answer: an untrusted
	// workspace still needs a grant or an approval, and with the policy off trust
	// alone never shares it.
	shared := sandboxNetworkExplanation("shared")
	for _, want := range []string{"trusted workspace", "untrusted workspace still needs a network:shared grant or approval"} {
		if !strings.Contains(shared, want) {
			t.Fatalf("shared explanation omits %q: %q", want, shared)
		}
	}
	withheld := sandboxNetworkExplanation("isolated")
	for _, want := range []string{"trust alone", "network:shared grant or approval"} {
		if !strings.Contains(withheld, want) {
			t.Fatalf("isolated explanation omits %q: %q", want, withheld)
		}
	}

	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "network-diag", "Network")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Decisions", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "work")
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range []string{
		executionenv.NetworkDecisionShared, executionenv.NetworkWithheldNotBound, executionenv.NetworkWithheldNotBound, "",
	} {
		payload := map[string]interface{}{"mode": "isolated"}
		if decision != "" {
			payload["network_decision"] = decision
		}
		if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "tool.sandbox", Payload: mustJSON(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	summary := (&Server{Control: store}).recentExecutionSummary(ctx, identity.TenantID, task.ID, run.ID)
	want := "Network decisions this run: shared=1, withheld: not network-bound by policy=2"
	if !strings.Contains(summary, want) {
		t.Fatalf("summary does not say %q:\n%s", want, summary)
	}
	// An event that predates the field adds nothing, and a Run with no decisions
	// prints no line.
	other, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Quiet", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := store.StartRunWithOptions(ctx, other, "cli", "quiet", control.StartRunOptions{MaxActiveRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: other.ID, RunID: quiet.ID, Type: "tool.sandbox", Payload: mustJSON(map[string]interface{}{"mode": "isolated"})}); err != nil {
		t.Fatal(err)
	}
	if got := (&Server{Control: store}).recentExecutionSummary(ctx, identity.TenantID, other.ID, quiet.ID); strings.Contains(got, "Network decisions") {
		t.Fatalf("a run with no recorded decision printed one: %q", got)
	}
}
