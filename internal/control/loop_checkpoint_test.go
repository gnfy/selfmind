package control

import (
	"context"
	"testing"
)

func TestLoopCheckpointOverwritesAndFindsIncomplete(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	first := LoopCheckpointRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, ContractVersion: RunRecoveryContractVersion,
		Recovery: []byte(`{"plan_version":2,"current_plan_step_id":"step-a"}`), Iteration: 1,
		Outcome: "execute_tools", Detail: "read_file", Snapshot: []byte(`[1]`),
	}
	if err := store.SaveLoopCheckpoint(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.Iteration = 2
	first.Outcome = "continue_model"
	first.Detail = "tool result recorded"
	first.Snapshot = []byte(`[2]`)
	if err := store.SaveLoopCheckpoint(ctx, first); err != nil {
		t.Fatal(err)
	}

	got, err := store.IncompleteLoopCheckpointForRun(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Iteration != 2 || got.ContractVersion != RunRecoveryContractVersion ||
		string(got.Recovery) != `{"plan_version":2,"current_plan_step_id":"step-a"}` || string(got.Snapshot) != `[2]` {
		t.Fatalf("checkpoint = %+v", got)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM loop_checkpoints WHERE run_id = ?`, run.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("checkpoint rows = %d, want overwrite-only row", rows)
	}

	first.Outcome = "complete_turn"
	if err := store.SaveLoopCheckpoint(ctx, first); err != nil {
		t.Fatal(err)
	}
	if got, err := store.IncompleteLoopCheckpointForRun(ctx, identity.TenantID, run.ID); err != nil || got != nil {
		t.Fatalf("a completed checkpoint must not resume: got=%+v err=%v", got, err)
	}
}
