package httpapi

// A turn the daemon starts reaches admission through processDaemonTurn: a
// schedule is the one source that arrives there, and its text is stored text,
// not the person's. These tests run real jobs through CronExecutor on the
// scheduled-text fixture and pin what admission records for them: a snapshot
// only for work that was admitted, the queue class and the snapshot's origin
// agreeing on every way a scheduled turn can be queued, and nothing at all for
// text the gateway refuses.

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/task/cron"
)

// recordedAdmissions counts what the person has in every state: Runs, queue
// rows, and how many of either carry a snapshot.
func (f *scheduledTextFixture) recordedAdmissions(t *testing.T) (runs, queueRows, snapshots int) {
	t.Helper()
	ctx := context.Background()
	digests, err := f.store.ListRecentRunsForPerson(ctx, f.person.TenantID, f.person.PersonID, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range digests {
		runs++
		snapshot, err := f.store.RunAdmissionSnapshot(ctx, f.person.TenantID, digest.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if !snapshot.IsZero() {
			snapshots++
		}
	}
	for _, status := range []string{control.QueueStatusQueued, control.QueueStatusStarted, control.QueueStatusDone,
		control.QueueStatusFailed, control.QueueStatusCancelled} {
		rows, err := f.store.ListQueued(ctx, f.person.TenantID, f.person.PersonID, status)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			queueRows++
			if !row.Admission.IsZero() {
				snapshots++
			}
		}
	}
	return runs, queueRows, snapshots
}

// newestRunAdmission is the snapshot of the Run the fixture's person most
// recently started other than the parked one.
func (f *scheduledTextFixture) newestRunAdmission(t *testing.T) control.AdmissionSnapshot {
	t.Helper()
	digests, err := f.store.ListRecentRunsForPerson(context.Background(), f.person.TenantID, f.person.PersonID, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range digests {
		if digest.RunID == f.parked.ID {
			continue
		}
		snapshot, err := f.store.RunAdmissionSnapshot(context.Background(), f.person.TenantID, digest.RunID)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	t.Fatal("no scheduled Run was started")
	return control.AdmissionSnapshot{}
}

// Text the gateway refuses is never admitted, so nothing is recorded for it:
// no Run, no queue row, and so no snapshot of authority the text never had.
// Scheduled text that is task input is admitted, and records a scheduled
// snapshot.
func TestRefusedDaemonTextRecordsNoAdmission(t *testing.T) {
	f := newScheduledTextFixture(t)
	runs0, rows0, snapshots0 := f.recordedAdmissions(t)
	if snapshots0 != 0 {
		t.Fatalf("the fixture starts with %d snapshots", snapshots0)
	}
	for name, job := range map[string]cron.CronJob{
		"command text":                       imJob("/approve"),
		"command text in another casing":     imJob("/NEW write the release notes"),
		"command text on the CLI job":        cliJob("/tools readonly github/get_issue on"),
		"command text with leading space":    imJob("   /status"),
		"job for an unbound account":         {ID: 9, Name: "exfil", Platform: "telegram", DeliverTo: "900", Prompt: "summarize my files"},
		"job for a chat it does not own":     {ID: 11, Name: "team digest", Platform: "weixin", DeliverTo: "wx-me", Channel: "team@chatroom", Prompt: "post the summary"},
		"job whose account has no endpoint":  {ID: 13, Name: "nowhere", Platform: "weixin", DeliverTo: "wx-nobody", Prompt: "summarize my day"},
		"command text that names a schedule": imJob("/schedule add daily 9am summarize"),
	} {
		f.fire(t, job)
		if runs, rows, snapshots := f.recordedAdmissions(t); runs != runs0 || rows != rows0 || snapshots != snapshots0 {
			t.Fatalf("%s was refused but recorded work: runs %d->%d, queue rows %d->%d, snapshots %d->%d",
				name, runs0, runs, rows0, rows, snapshots0, snapshots)
		}
	}

	// The same person's scheduled task text is admitted and records a snapshot
	// that says it was scheduled.
	f.fire(t, imJob("summarize the overnight build logs"))
	runs, _, snapshots := f.recordedAdmissions(t)
	if runs != runs0+1 || snapshots != snapshots0+1 {
		t.Fatalf("scheduled task text: runs %d->%d, snapshots %d->%d, want one more of each", runs0, runs, snapshots0, snapshots)
	}
	got := f.newestRunAdmission(t)
	if got.Origin.Kind != runOriginCron || got.Input.Summary != "summarize the overnight build logs" ||
		got.StandingGrantsAuthorizedAt != 0 {
		t.Fatalf("scheduled task admission = %+v", got)
	}
}

// A scheduled turn that cannot start now is written with the cron queue class
// and a snapshot whose origin is cron, on every path that queues it, however
// its origin reached the gateway: on the request, or only as the turn tag. The
// person's own turn on the same paths gets neither. The two are written from
// one rule, so a drained row's trigger and its recorded authority cannot
// disagree.
func TestScheduledTurnQueueClassAndAdmissionOriginAgree(t *testing.T) {
	f := newScheduledTextFixture(t)
	authorized := time.Unix(1790000000, 0)
	base := api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-me", Channel: "wx-me", Content: "tidy the nightly notes"}
	for _, path := range []struct {
		name    string
		enqueue func(context.Context, *control.IdentityContext, api.MessageRequest) (api.MessageResponse, *notDispatched)
	}{
		{"behind active work", f.daemon.enqueueBehindActive},
		{"until the model is ready", f.daemon.enqueueUntilModelReady},
		{"during a model change", f.daemon.enqueueDuringModelChange},
	} {
		t.Run(path.name, func(t *testing.T) {
			ctx := context.Background()
			for _, tc := range []struct {
				name       string
				ctx        context.Context
				req        func(api.MessageRequest) api.MessageRequest
				wantOrigin control.AdmissionOrigin
				wantCutoff int64
				wantWeb    bool
			}{
				{"scheduled by its request", ctx, func(r api.MessageRequest) api.MessageRequest {
					r.Origin, r.OriginID, r.StandingGrantsAuthorizedAt, r.AllowWeb = runOriginCron, "7", authorized, true
					return r
				}, control.AdmissionOrigin{Kind: runOriginCron, ID: "7"}, authorized.Unix(), true},
				{"scheduled by the turn tag alone", kernel.WithTurnSource(ctx, runOriginCron), func(r api.MessageRequest) api.MessageRequest {
					return r
				}, control.AdmissionOrigin{Kind: runOriginCron}, 0, false},
				{"the person's own turn", ctx, func(r api.MessageRequest) api.MessageRequest { return r },
					control.AdmissionOrigin{}, 0, false},
			} {
				resp, refusal := path.enqueue(tc.ctx, f.person, tc.req(base))
				if refusal != nil || resp.Turn == nil || resp.Turn.QueueID == "" {
					t.Fatalf("%s: not queued: %+v %v", tc.name, resp, refusal)
				}
				row, err := f.store.GetQueued(ctx, f.person.TenantID, resp.Turn.QueueID)
				if err != nil || row == nil || row.AdmissionError != nil {
					t.Fatalf("%s: queue row = %+v %v", tc.name, row, err)
				}
				scheduled := tc.wantOrigin.Kind == runOriginCron
				if (row.Class == control.QueueClassCron) != scheduled || (row.Admission.Origin.Kind == runOriginCron) != scheduled {
					t.Fatalf("%s: class %q and snapshot origin %+v disagree about whether this turn is scheduled",
						tc.name, row.Class, row.Admission.Origin)
				}
				if (queuedTrigger(*row) == runOriginCron) != scheduled {
					t.Fatalf("%s: the drain's trigger for class %q is %q", tc.name, row.Class, queuedTrigger(*row))
				}
				if row.Admission.Origin != tc.wantOrigin || row.Admission.StandingGrantsAuthorizedAt != tc.wantCutoff ||
					(row.Admission.Strategy.WebPolicy == string(kernel.WebPolicyEnabled)) != tc.wantWeb ||
					row.Admission.Input.Summary != base.Content {
					t.Fatalf("%s: queued admission = %+v", tc.name, row.Admission)
				}
				if !scheduled && row.Class != control.QueueClassForeground {
					t.Fatalf("%s: the person's own turn was queued as %q", tc.name, row.Class)
				}
			}
		})
	}
}

// A scheduled row written without a snapshot, as one queued before admission
// snapshots existed is, still drains as a scheduled turn: its class names the
// trigger. It has no authorisation instant to restore, so it consumes no
// remembered class, and the Run it starts freezes exactly that.
func TestScheduledRowWithoutSnapshotDrainsScheduledWithNoStandingClasses(t *testing.T) {
	f := newScheduledTextFixture(t)
	ctx := context.Background()
	row, err := f.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: f.person.TenantID, PersonID: f.person.PersonID,
		Platform: "weixin", PlatformUserID: "wx-me", Channel: "wx-me", Content: "summarize the overnight build logs",
		Class: control.QueueClassCron})
	if err != nil {
		t.Fatal(err)
	}
	if !row.Admission.IsZero() {
		t.Fatalf("the row carries a snapshot: %+v", row.Admission)
	}
	f.daemon.coordinator().drainQueue(f.person)
	waitUntil(t, 10*time.Second, func() bool {
		done, _ := f.store.GetQueued(ctx, f.person.TenantID, row.ID)
		return done != nil && done.Status == control.QueueStatusDone && done.RunID != ""
	}, "the scheduled row never ran")
	done, _ := f.store.GetQueued(ctx, f.person.TenantID, row.ID)
	payload, err := f.store.RunStartedPayload(ctx, f.person.TenantID, f.person.PersonID, done.RunID)
	if err != nil || !strings.Contains(string(payload), `"origin":"cron"`) {
		t.Fatalf("the drained row started as %s err=%v, want origin cron", payload, err)
	}
	frozen, err := f.store.RunAdmissionSnapshot(ctx, f.person.TenantID, done.RunID)
	if err != nil || frozen.Origin.Kind != runOriginCron || frozen.StandingGrantsAuthorizedAt != 0 {
		t.Fatalf("the Run froze %+v err=%v, want a scheduled snapshot with no authorisation instant", frozen, err)
	}
	if policy := standingGrantPolicy(ctx, api.MessageRequest{Admission: frozen}); policy.Allowed {
		t.Fatalf("a scheduled row with no authorisation instant consumed standing classes: %+v", policy)
	}
}
