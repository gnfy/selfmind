package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHumanCancellationEmitsExactClarifyExpiry(t *testing.T) {
	store, owner, _, run, ref := parkedHumanCancelFixture(t, LoopCheckpointWaitClarification)
	if stopped, err := store.CancelParkedHumanRun(context.Background(), owner.TenantID, owner.PersonID, run.ID); err != nil || !stopped {
		t.Fatalf("cancel=%v err=%v", stopped, err)
	}
	var raw, channel string
	if err := store.db.QueryRow(`SELECT payload_json,channel FROM task_events WHERE run_id=? AND type='clarify.expired'`, run.ID).Scan(&raw, &channel); err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || payload["clarify_id"] != ref || channel != run.Channel {
		t.Fatalf("wrong resolution: payload=%s channel=%q err=%v", raw, channel, err)
	}
}
