package control

import (
	"context"
	"testing"
	"time"
)

// A preference read together with when it was last written lets a reader tell
// something the person set after a moment from something they already had.
func TestPersonSettingIsReadWithTheSecondItWasLastWritten(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, _ := admissionOwnerAndThread(t, store, "owner")

	if value, at, err := store.GetPersonSettingStamped(ctx, owner.TenantID, owner.PersonID, "approval_mode"); err != nil || value != "" || at != 0 {
		t.Fatalf("an unset preference reads as empty with no time: %q %d %v", value, at, err)
	}
	before := time.Now().Unix()
	if err := store.SetPersonSetting(ctx, owner.TenantID, owner.PersonID, "approval_mode", "smart"); err != nil {
		t.Fatal(err)
	}
	value, at, err := store.GetPersonSettingStamped(ctx, owner.TenantID, owner.PersonID, "approval_mode")
	if err != nil || value != "smart" || at < before || at > time.Now().Unix() {
		t.Fatalf("a written preference reads back with the second it was written: %q %d (not before %d) %v", value, at, before, err)
	}
	// Writing again moves the stamp; the person's other preferences keep theirs.
	if _, err := store.db.ExecContext(ctx, `UPDATE person_settings SET updated_at = ? WHERE person_id = ? AND key = ?`, before-100, owner.PersonID, "approval_mode"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPersonSetting(ctx, owner.TenantID, owner.PersonID, "approval_mode", "full-auto"); err != nil {
		t.Fatal(err)
	}
	if value, again, err := store.GetPersonSettingStamped(ctx, owner.TenantID, owner.PersonID, "approval_mode"); err != nil || value != "full-auto" || again < before {
		t.Fatalf("an overwritten preference carries the new stamp: %q %d %v", value, again, err)
	}
	// Resetting to the default deletes the row: it is unset again.
	if err := store.SetPersonSetting(ctx, owner.TenantID, owner.PersonID, "approval_mode", ""); err != nil {
		t.Fatal(err)
	}
	if value, at, err := store.GetPersonSettingStamped(ctx, owner.TenantID, owner.PersonID, "approval_mode"); err != nil || value != "" || at != 0 {
		t.Fatalf("a reset preference is unset: %q %d %v", value, at, err)
	}
	if _, _, err := store.GetPersonSettingStamped(ctx, owner.TenantID, "", "approval_mode"); err == nil {
		t.Fatal("a read without a person is refused")
	}
}
