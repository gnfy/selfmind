package control

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Reads check only the v1 shape: its version, its fields, and the minimum a
// recorded admission must carry. Value rules are enforced on write, so a later
// tightening of them never makes a stored row unreadable.
func TestStoredAdmissionOutsideTodaysWriteRulesStaysReadable(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	run, err := store.StartRunWithOptions(ctx, task, "cli", "work", StartRunOptions{Admission: verifyOnlyCronAdmission()})
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"version":1,"strategy":{"class":"research","tool_mode":"turbo","plan_policy":"optional","web_policy":"disabled","channel_mode":"im"},"approval_mode":"smart","recovery_mode":"replay_everything","origin":{"kind":"webhook"}}`
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET admission_json = ? WHERE id = ?`, raw, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID)
	if err != nil || got.Strategy.ToolMode != "turbo" || got.RecoveryMode != "replay_everything" || got.Origin.Kind != "webhook" {
		t.Fatalf("stored v1 admission = %+v err=%v", got, err)
	}
}

// What the store writes, it can read back exactly. JSON replaces each invalid
// UTF-8 byte with a three-byte replacement character, so a bounded identifier
// could be written and then fail its own bound on read.
func TestAdmissionIdentifiersReadBackAsWritten(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	admission := verifyOnlyCronAdmission()
	admission.Origin.ID = "job-" + strings.Repeat("\xff", 100)
	admission.WatchID = "watch <&> " + strings.Repeat("\xfe", 90) + strings.Repeat("w", 200)
	admission.Input.Summary = strings.Repeat("\xff", 30) + "check the export"
	run, err := store.StartRunWithOptions(ctx, task, "cli", "work", StartRunOptions{Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID)
	if err != nil || got.Origin.ID != "job-" || got.WatchID != "watch <&> "+strings.Repeat("w", 200) ||
		got.Input.Summary != "check the export" || !utf8.ValidString(got.WatchID) {
		t.Fatalf("admission read back = %+v err=%v", got, err)
	}
	queued, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Content: "later", Admission: admission})
	if err != nil || queued.Admission != got {
		t.Fatalf("queue write returned %+v err=%v, want %+v", queued, err, got)
	}
	if row, err := store.GetQueued(ctx, owner.TenantID, queued.ID); err != nil || row == nil || row.AdmissionError != nil || row.Admission != got {
		t.Fatalf("queue row read back = %+v err=%v", row, err)
	}
}

// Reads reject unknown fields, so the v1 wire form is the contract: any field
// change needs a new AdmissionSnapshotVersion, a decoder for v1, and a golden
// form for the new version.
func TestAdmissionSnapshotWireFormIsPinnedToItsVersion(t *testing.T) {
	full := AdmissionSnapshot{
		Version: AdmissionSnapshotVersion,
		Strategy: AdmissionStrategy{Class: "external_lookup", ToolMode: "local_read", PlanPolicy: "optional",
			WebPolicy: "enabled", ChannelMode: "im", VerificationOnly: true},
		RecoveryMode: RunRecoveryModeVerifyOnly, ExecutionProfile: "watch-finalization", WatchID: "watch_1",
		ApprovalMode: "on-request", Origin: AdmissionOrigin{Kind: "cron", ID: "cron_1"},
		StandingGrantsAuthorizedAt: 1790000000, AdmittedAt: 1789999000,
		Input: AdmissionInput{Kind: AdmissionInputChannelMessage, ID: "msg_1", Summary: "check the export"},
	}
	var walk func(path string, value reflect.Value)
	walk = func(path string, value reflect.Value) {
		if value.Kind() == reflect.Struct {
			for i := 0; i < value.NumField(); i++ {
				walk(path+"."+value.Type().Field(i).Name, value.Field(i))
			}
			return
		}
		if value.IsZero() {
			t.Fatalf("%s is zero in the golden snapshot; a new field needs a value here, a golden form, and a version bump", path)
		}
	}
	walk("AdmissionSnapshot", reflect.ValueOf(full))
	golden := map[int]string{
		1: `{"version":1,"strategy":{"class":"external_lookup","tool_mode":"local_read","plan_policy":"optional","web_policy":"enabled","channel_mode":"im","verification_only":true},"recovery_mode":"verify_only","execution_profile":"watch-finalization","watch_id":"watch_1","approval_mode":"on-request","origin":{"kind":"cron","id":"cron_1"},"standing_grants_authorized_at":1790000000,"admitted_at":1789999000,"input":{"kind":"channel_message","id":"msg_1","summary":"check the export"}}`,
	}
	want, ok := golden[AdmissionSnapshotVersion]
	if !ok {
		t.Fatalf("AdmissionSnapshotVersion %d has no golden wire form", AdmissionSnapshotVersion)
	}
	_, encoded, err := encodeAdmissionSnapshot(full)
	if err != nil || encoded != want {
		t.Fatalf("the v%d wire form changed; bump AdmissionSnapshotVersion and keep a decoder for the old form:\n got  %s\n want %s\n err %v",
			AdmissionSnapshotVersion, encoded, want, err)
	}
	decoded, err := decodeAdmissionSnapshot(want)
	if err != nil || decoded != full {
		t.Fatalf("golden form decodes to %+v err=%v", decoded, err)
	}
}
