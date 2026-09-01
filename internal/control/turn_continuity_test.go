package control

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPendingTurnChoiceBareClaimRequiresOneRecentChoice(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	create := func(label string) *PendingTurnChoice {
		choice, err := store.CreatePendingTurnChoice(ctx, PendingTurnChoiceCreate{
			TenantID: "default", PersonID: "person", AccountID: "account", Channel: "cli",
			RequestJSON: `{"content":"continue ` + label + `"}`,
			Options: []TurnChoiceOption{
				{Key: "1", Label: label, Action: "resume", TaskID: "task-1", RunID: "run-1"},
				{Key: "2", Label: "new work", Action: "new"},
			},
			ExpiresAt: now.Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		return choice
	}
	first := create("first")
	claimed, option, err := store.ClaimPendingTurnChoice(ctx, "default", "person", "", "1", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("bare claim: %v", err)
	}
	if claimed.ID != first.ID || option.RunID != "run-1" {
		t.Fatalf("claimed=%+v option=%+v", claimed, option)
	}

	second := create("second")
	third := create("third")
	if _, _, err := store.ClaimPendingTurnChoice(ctx, "default", "person", "", "1", time.Now(), 30*time.Minute); !errors.Is(err, ErrTurnChoiceAmbiguous) {
		t.Fatalf("bare ambiguous err=%v", err)
	}
	claimed, _, err = store.ClaimPendingTurnChoice(ctx, "default", "person", second.ID, "2", time.Now(), 30*time.Minute)
	if err != nil || claimed.ID != second.ID {
		t.Fatalf("named claim=%+v err=%v", claimed, err)
	}
	if _, _, err := store.ClaimPendingTurnChoice(ctx, "default", "other-person", third.ID, "1", time.Now(), 30*time.Minute); !errors.Is(err, ErrTurnChoiceNotFound) {
		t.Fatalf("foreign claim err=%v", err)
	}
}

func TestPendingTurnChoiceClaimIsSingleUse(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	choice, err := store.CreatePendingTurnChoice(ctx, PendingTurnChoiceCreate{
		TenantID: "default", PersonID: "person", RequestJSON: `{"content":"continue"}`,
		Options: []TurnChoiceOption{
			{Key: "1", Label: "run", Action: "resume", TaskID: "task-1", RunID: "run-1"},
			{Key: "2", Label: "new", Action: "new"},
		},
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ClaimPendingTurnChoice(ctx, "default", "person", choice.ID, "1", time.Now(), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	var storedRequest string
	if err := store.db.QueryRowContext(ctx, `SELECT request_json FROM pending_turn_choices WHERE id = ?`, choice.ID).Scan(&storedRequest); err != nil {
		t.Fatal(err)
	}
	if storedRequest != "{}" {
		t.Fatalf("claimed choice retained request snapshot: %q", storedRequest)
	}
	if _, _, err := store.ClaimPendingTurnChoice(ctx, "default", "person", choice.ID, "1", time.Now(), 30*time.Minute); !errors.Is(err, ErrTurnChoiceNotFound) {
		t.Fatalf("second claim err=%v", err)
	}
}

func TestPendingTurnChoiceClaimIsSingleUseAcrossStoreConnections(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	choice, err := first.CreatePendingTurnChoice(context.Background(), PendingTurnChoiceCreate{
		TenantID: "default", PersonID: "person", RequestJSON: `{"content":"continue"}`,
		Options: []TurnChoiceOption{
			{Key: "1", Label: "run", Action: "resume", TaskID: "task-1", RunID: "run-1"},
			{Key: "2", Label: "new", Action: "new"},
		}, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	stores := []*Store{first, second}
	var wg sync.WaitGroup
	results := make(chan error, len(stores))
	for _, store := range stores {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			_, _, claimErr := store.ClaimPendingTurnChoice(context.Background(), "default", "person", choice.ID, "1", time.Now(), 30*time.Minute)
			results <- claimErr
		}(store)
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for claimErr := range results {
		if claimErr == nil {
			succeeded++
		} else if !errors.Is(claimErr, ErrTurnChoiceNotFound) {
			t.Fatalf("claim error=%v", claimErr)
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful claims=%d", succeeded)
	}
}

func TestPruneTurnContinuityBoundsChoicesAndResolutionAudit(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	choice, err := store.CreatePendingTurnChoice(ctx, PendingTurnChoiceCreate{
		TenantID: "default", PersonID: "person", RequestJSON: `{"content":"old sensitive request"}`,
		Options:   []TurnChoiceOption{{Key: "1", Label: "run", Action: "resume", TaskID: "task-1", RunID: "run-1"}, {Key: "2", Label: "new", Action: "new"}},
		ExpiresAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pending_turn_choices SET created_at = ?, expires_at = ? WHERE id = ?`, now.Add(-8*24*time.Hour).Unix(), now.Add(-7*24*time.Hour).Unix(), choice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordTurnResolution(ctx, TurnResolutionRecord{
		ID: "old-resolution", TenantID: "default", PersonID: "person", Input: "old",
		Mode: "safe", Decision: "new", Certainty: "no_match", CreatedAt: now.Add(-91 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	choices, resolutions, err := store.PruneTurnContinuity(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if choices != 1 || resolutions != 1 {
		t.Fatalf("pruned choices=%d resolutions=%d", choices, resolutions)
	}
}

func TestRecordTurnResolutionStoresHashNotInput(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	id, err := store.RecordTurnResolution(ctx, TurnResolutionRecord{
		TenantID: "default", PersonID: "person", Input: "sensitive original input",
		Mode: "safe", Decision: "observe", Certainty: "clear", TargetRunID: "run-1",
		CandidateIDs: []string{"run-1"}, Evidence: []string{"active_run"}, Latency: 42 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var inputHash string
	var latency int64
	if err := store.db.QueryRowContext(ctx, `SELECT input_hash, latency_ms FROM turn_resolution_events WHERE id = ?`, id).Scan(&inputHash, &latency); err != nil {
		t.Fatal(err)
	}
	if inputHash == "" || inputHash == "sensitive original input" || latency != 42 {
		t.Fatalf("input_hash=%q latency=%d", inputHash, latency)
	}
}
