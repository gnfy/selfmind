package control

import (
	"context"
	"errors"
	"testing"
)

func enqueueFromSource(t *testing.T, store *Store, owner *IdentityContext, platform, channel, content string) *QueuedTask {
	t.Helper()
	queued, err := store.EnqueueQueued(context.Background(), QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
		Platform: platform, Channel: channel, Content: content, Admission: verifyOnlyCronAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	return queued
}

func corruptQueuedAdmission(t *testing.T, store *Store, queueID, raw string) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE task_queue SET admission_json = ? WHERE id = ?`, raw, queueID); err != nil {
		t.Fatal(err)
	}
}

func quarantinedRow(t *testing.T, row *QueuedTask, id string) {
	t.Helper()
	var typed *QueuedAdmissionError
	if row == nil || row.ID != id || !row.Admission.IsZero() || !errors.As(row.AdmissionError, &typed) ||
		typed.QueueID != id || !errors.Is(row.AdmissionError, ErrAdmissionSnapshotUnreadable) {
		t.Fatalf("row %s is not quarantined with a typed error: %+v", id, row)
	}
}

// One unreadable snapshot used to fail every queue read for its person: the
// drain stopped, /queue errored, and only /queue clear worked.
func TestUnreadableQueuedAdmissionHoldsOnlyItsRowAndSource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, _ := admissionOwnerAndThread(t, store, "owner")
	head := enqueueFromSource(t, store, owner, "cli", "session-b", "unreadable head")
	later := enqueueFromSource(t, store, owner, "cli", "session-b", "later same-source work")
	other := enqueueFromSource(t, store, owner, "telegram", "chat-c", "independent work")
	corruptQueuedAdmission(t, store, head.ID, `{"version":99}`)

	listed, err := store.ListQueued(ctx, owner.TenantID, owner.PersonID, QueueStatusQueued)
	if err != nil || len(listed) != 3 {
		t.Fatalf("listing failed on one unreadable row: %+v err=%v", listed, err)
	}
	quarantinedRow(t, &listed[0], head.ID)
	if listed[1].AdmissionError != nil || listed[2].AdmissionError != nil || listed[1].Admission != verifyOnlyCronAdmission() {
		t.Fatalf("readable rows were marked: %+v", listed)
	}
	got, err := store.GetQueued(ctx, owner.TenantID, head.ID)
	if err != nil {
		t.Fatal(err)
	}
	quarantinedRow(t, got, head.ID)
	if all, err := store.ListAllQueued(ctx, QueueStatusQueued); err != nil || len(all) != 3 {
		t.Fatalf("daemon-wide listing = %d rows err=%v", len(all), err)
	}

	// The drain never receives the row. Its source waits behind it, other
	// sources go on, and the caller still sees it to report it.
	var seen []string
	next, err := store.NextQueuedWhere(ctx, owner.TenantID, owner.PersonID, func(q QueuedTask) bool {
		seen = append(seen, q.ID)
		return true
	})
	if err != nil || next == nil || next.ID != other.ID {
		t.Fatalf("drain candidate = %+v err=%v, want the independent source", next, err)
	}
	if len(seen) != 2 || seen[0] != head.ID || seen[1] != other.ID {
		t.Fatalf("caller saw %v, want the quarantined head then the independent row", seen)
	}
	if next, err := store.NextQueued(ctx, owner.TenantID, owner.PersonID); err != nil || next == nil || next.ID != other.ID {
		t.Fatalf("unfiltered drain candidate = %+v err=%v", next, err)
	}

	// Dropping the row releases its source.
	if err := store.MarkQueued(ctx, owner.TenantID, head.ID, QueueStatusCancelled); err != nil {
		t.Fatal(err)
	}
	if next, err := store.NextQueued(ctx, owner.TenantID, owner.PersonID); err != nil || next == nil || next.ID != later.ID {
		t.Fatalf("after the drop the source head = %+v err=%v", next, err)
	}
}

// The same holds for a reply row the drain resolves first, for malformed
// bytes rather than a newer version, and for an idempotent replay.
func TestUnreadableQueuedContinuationIsListedNotFatal(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	parent, err := store.StartRun(ctx, task, "cli", "parked")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	reply, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Channel: "cli",
		Content: "continue", ReplyToRunID: parent.ID, IdempotencyKey: "external-resource:" + parent.ID, Admission: verifyOnlyCronAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	corruptQueuedAdmission(t, store, reply.ID, `{"version":1`)
	rows, err := store.ListDueQueuedContinuations(ctx, owner.TenantID, owner.PersonID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("continuation preflight failed on one unreadable row: %+v err=%v", rows, err)
	}
	quarantinedRow(t, &rows[0], reply.ID)
	replay, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Channel: "cli",
		Content: "continue", ReplyToRunID: parent.ID, IdempotencyKey: "external-resource:" + parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	quarantinedRow(t, replay, reply.ID)
	if keyed, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "external-resource:"+parent.ID); err != nil {
		t.Fatal(err)
	} else {
		quarantinedRow(t, keyed, reply.ID)
	}
	if next, err := store.NextQueued(ctx, owner.TenantID, owner.PersonID); err != nil || next != nil {
		t.Fatalf("drain returned %+v err=%v for a quarantined row", next, err)
	}
}
