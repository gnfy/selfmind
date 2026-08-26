package control

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestWorkflowProfileObservationsDoNotClaimShadowEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "repeatable inspection", Channel: "cli"})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	policy := EvolutionPolicy{Enabled: true, Mode: "auto-readonly", ShadowAfterObservations: 3, PromoteAfterObservations: 5, MinShadowRuns: 3, MaxShadowFailureRate: 0.05}
	var candidate *EvolutionCandidate
	for i := 0; i < 5; i++ {
		run := appendReadOnlyProfileRun(t, store, identity, task, fmt.Sprintf("run-%d", i), "")
		_, candidate, err = store.MaterializeWorkflowProfile(ctx, identity.TenantID, run.ID, policy)
		if err != nil {
			t.Fatalf("materialize %d: %v", i, err)
		}
	}
	if candidate == nil || candidate.Status != "candidate" {
		t.Fatalf("candidate = %#v, want observation-only candidate", candidate)
	}
	if candidate.ObservationCount != 5 || candidate.ShadowRuns != 0 || candidate.ShadowMatches != 0 {
		t.Fatalf("candidate counters = %#v", candidate)
	}
	advice, err := store.EnabledEvolutionAdvice(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || advice != nil {
		t.Fatalf("ordinary observations must not create advice: %#v err=%v", advice, err)
	}

	// Simulate an enabled row created by the legacy heuristic. It remains
	// inspectable for compatibility, but cannot reach runtime advice because its
	// contract contains no independently verified comparison evidence.
	if _, err := store.db.ExecContext(ctx, `UPDATE evolution_candidates SET status='enabled' WHERE id=?`, candidate.ID); err != nil {
		t.Fatalf("enable legacy candidate: %v", err)
	}
	advice, err = store.EnabledEvolutionAdvice(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || advice != nil {
		t.Fatalf("legacy enabled candidate must not be advised: %#v err=%v", advice, err)
	}

	failing := appendReadOnlyProfileRun(t, store, identity, task, "batch-failure", candidate.ID)
	if _, _, err := store.MaterializeWorkflowProfile(ctx, identity.TenantID, failing.ID, policy); err != nil {
		t.Fatalf("materialize failure: %v", err)
	}
	if advice, err = store.EnabledEvolutionAdvice(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil || advice != nil {
		t.Fatalf("degraded candidate must not be advised: %#v err=%v", advice, err)
	}
	var status string
	var fallbackCount int
	if err := store.db.QueryRowContext(ctx, `SELECT status, fallback_count FROM evolution_candidates WHERE id=?`, candidate.ID).Scan(&status, &fallbackCount); err != nil {
		t.Fatalf("candidate state: %v", err)
	}
	if status != "degraded" || fallbackCount != 1 {
		t.Fatalf("status=%q fallback_count=%d", status, fallbackCount)
	}
	if _, _, err := store.MaterializeWorkflowProfile(ctx, identity.TenantID, failing.ID, policy); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT fallback_count FROM evolution_candidates WHERE id=?`, candidate.ID).Scan(&fallbackCount); err != nil {
		t.Fatalf("candidate replay state: %v", err)
	}
	if fallbackCount != 1 {
		t.Fatalf("replay incremented fallback_count to %d", fallbackCount)
	}

	for i := 0; i < 6; i++ {
		recovery := appendReadOnlyProfileRun(t, store, identity, task, fmt.Sprintf("recovery-%d", i), "")
		if _, _, err := store.MaterializeWorkflowProfile(ctx, identity.TenantID, recovery.ID, policy); err != nil {
			t.Fatalf("recovery materialize %d: %v", i, err)
		}
	}
	advice, err = store.EnabledEvolutionAdvice(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || advice != nil {
		t.Fatalf("ordinary recovery runs must not revive advice: %#v err=%v", advice, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT status, shadow_runs, shadow_matches FROM evolution_candidates WHERE id=?`, candidate.ID).
		Scan(&status, &candidate.ShadowRuns, &candidate.ShadowMatches); err != nil {
		t.Fatalf("recovered candidate state: %v", err)
	}
	if status != "degraded" || candidate.ShadowRuns != 0 || candidate.ShadowMatches != 0 {
		t.Fatalf("ordinary recovery changed degraded evidence: status=%q runs=%d matches=%d", status, candidate.ShadowRuns, candidate.ShadowMatches)
	}
}

func TestEnabledEvolutionAdviceRequiresVerifiedComparisonContract(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "verified-advice", "Verified")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "verified", Channel: "cli"})
	run := appendReadOnlyProfileRun(t, store, identity, task, "observed", "")
	_, candidate, err := store.MaterializeWorkflowProfile(ctx, identity.TenantID, run.ID, EvolutionPolicy{Enabled: true, Mode: "auto-readonly"})
	if err != nil || candidate == nil {
		t.Fatalf("candidate=%+v err=%v", candidate, err)
	}
	verifiedContract := `{"version":2,"kind":"batch_read","evidence_model":"verified_comparison"}`
	if _, err := store.db.ExecContext(ctx, `UPDATE evolution_candidates SET status='enabled', contract_json=? WHERE id=?`, verifiedContract, candidate.ID); err != nil {
		t.Fatal(err)
	}
	advice, err := store.EnabledEvolutionAdvice(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || advice == nil || advice.CandidateID != candidate.ID {
		t.Fatalf("verified advice=%+v err=%v", advice, err)
	}
}

func TestParkedWorkflowProfileDoesNotAdvanceBatchReadCandidate(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "parked-profile", "Parked")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect", Channel: "cli"})
	policy := EvolutionPolicy{Enabled: true, Mode: "auto-readonly", ShadowAfterObservations: 1, PromoteAfterObservations: 10, MinShadowRuns: 3, MaxShadowFailureRate: 0.05}
	done := appendReadOnlyProfileRun(t, store, identity, task, "done", "")
	_, candidate, err := store.MaterializeWorkflowProfile(ctx, identity.TenantID, done.ID, policy)
	if err != nil || candidate == nil {
		t.Fatalf("initial candidate=%+v err=%v", candidate, err)
	}
	beforeObservations, beforeRuns, beforeMatches := candidate.ObservationCount, candidate.ShadowRuns, candidate.ShadowMatches

	parked := appendReadOnlyProfileRunWithStatus(t, store, identity, task, "parked", "", "waiting_user")
	profile, parkedCandidate, err := store.MaterializeWorkflowProfile(ctx, identity.TenantID, parked.ID, policy)
	if err != nil || profile == nil || profile.OutcomeStatus != "waiting_user" || parkedCandidate != nil {
		t.Fatalf("parked profile=%+v candidate=%+v err=%v", profile, parkedCandidate, err)
	}
	var observations, shadowRuns, shadowMatches int
	if err := store.db.QueryRowContext(ctx, `SELECT observation_count, shadow_runs, shadow_matches FROM evolution_candidates WHERE id=?`, candidate.ID).
		Scan(&observations, &shadowRuns, &shadowMatches); err != nil {
		t.Fatal(err)
	}
	if observations != beforeObservations || shadowRuns != beforeRuns || shadowMatches != beforeMatches {
		t.Fatalf("parked run advanced candidate: observations=%d/%d runs=%d/%d matches=%d/%d",
			observations, beforeObservations, shadowRuns, beforeRuns, shadowMatches, beforeMatches)
	}
}

func TestWorkflowEventsRetainOnlyEvolutionInputs(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "event-filter", "Events")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "events", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "events")
	want := []string{
		"run.started", "skill.activated", "tool.started", "tool.completed",
		"evolution.batch_item", "provider.call.usage", "plan.updated", "run.outcome",
		"run.finished", "run.steering_consumed", "skill.fallback",
	}
	for _, typ := range append(append([]string{}, want...), "tool.output", "agent.thinking") {
		appendProfileEvent(t, store, task.ID, run.ID, typ, map[string]interface{}{"message": typ})
	}
	events, err := store.workflowEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(want) {
		t.Fatalf("filtered events=%d want=%d: %+v", len(events), len(want), events)
	}
	for i, event := range events {
		if event.typ != want[i] {
			t.Fatalf("event[%d]=%q want %q", i, event.typ, want[i])
		}
	}
}

func appendReadOnlyProfileRun(t *testing.T, store *Store, identity *IdentityContext, task *Task, suffix, candidateID string) *Run {
	return appendReadOnlyProfileRunWithStatus(t, store, identity, task, suffix, candidateID, "done")
}

func appendReadOnlyProfileRunWithStatus(t *testing.T, store *Store, identity *IdentityContext, task *Task, suffix, candidateID, status string) *Run {
	t.Helper()
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", suffix)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	appendProfileEvent(t, store, task.ID, run.ID, "skill.activated", map[string]interface{}{"name": "inspect", "version_hash": "v1", "source": "test"})
	if candidateID == "" {
		for _, tool := range []string{"read_file", "search_files", "read_file", "ls_r"} {
			appendProfileEvent(t, store, task.ID, run.ID, "tool.started", map[string]interface{}{"tool": tool, "args": `{}`})
			appendProfileEvent(t, store, task.ID, run.ID, "tool.completed", map[string]interface{}{"tool": tool})
		}
	} else {
		args, _ := json.Marshal(map[string]interface{}{"candidate_id": candidateID})
		appendProfileEvent(t, store, task.ID, run.ID, "tool.started", map[string]interface{}{"tool": "batch_read", "args": string(args)})
		appendProfileEvent(t, store, task.ID, run.ID, "evolution.batch_item", map[string]interface{}{"candidate_id": candidateID, "success": false, "error": "missing file"})
		appendProfileEvent(t, store, task.ID, run.ID, "tool.completed", map[string]interface{}{"tool": "batch_read"})
	}
	appendProfileEvent(t, store, task.ID, run.ID, "provider.call.usage", map[string]interface{}{"input_tokens": 100, "output_tokens": 10, "billed_input_tokens": 40})
	appendProfileEvent(t, store, task.ID, run.ID, "run.outcome", map[string]interface{}{"status": status, "verification": map[string]interface{}{"state": "passed"}})
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return run
}

func appendProfileEvent(t *testing.T, store *Store, taskID, runID, typ string, payload map[string]interface{}) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	if _, err := store.AppendEvent(context.Background(), Event{TaskID: taskID, RunID: runID, Type: typ, Visibility: "task", Payload: raw}); err != nil {
		t.Fatalf("AppendEvent %s: %v", typ, err)
	}
}
