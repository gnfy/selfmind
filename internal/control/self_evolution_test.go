package control

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestWorkflowProfilePromotesAndDegradesReadOnlyCandidate(t *testing.T) {
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
	if candidate == nil || candidate.Status != "enabled" {
		t.Fatalf("candidate = %#v, want enabled", candidate)
	}
	if candidate.ObservationCount != 5 || candidate.ShadowRuns != 3 || candidate.ShadowMatches != 3 {
		t.Fatalf("candidate counters = %#v", candidate)
	}
	advice, err := store.EnabledEvolutionAdvice(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || advice == nil || advice.CandidateID != candidate.ID {
		t.Fatalf("advice = %#v err=%v", advice, err)
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
	if err != nil || advice == nil || advice.CandidateID != candidate.ID {
		t.Fatalf("recovered advice = %#v err=%v", advice, err)
	}
}

func appendReadOnlyProfileRun(t *testing.T, store *Store, identity *IdentityContext, task *Task, suffix, candidateID string) *Run {
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
	appendProfileEvent(t, store, task.ID, run.ID, "run.outcome", map[string]interface{}{"status": "done", "verification": map[string]interface{}{"state": "passed"}})
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
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
