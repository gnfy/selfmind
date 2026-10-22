package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/tools"
)

// Work that waits — queued behind other work, or parked on a provider, an
// approval or a question — runs under the mode it was admitted with, except
// that the person's own tightening since then narrows it. The effective mode is
// the narrower of the admitted mode and the person's persisted /mode, and only
// when that preference was written at or after the work was first admitted:
// a preference the person already had when they asked for, say, an explicit
// full-auto run says nothing about that run. A loosening never widens, and the
// stored snapshot is never rewritten, so each hop decides again from the
// original.

// setModeAt writes the person's /mode and stamps it as written at the given
// unix second.
func (s admissionProbeServer) setModeAt(t *testing.T, mode string, at int64) {
	t.Helper()
	s.setMode(t, mode)
	s.execRaw(t, `UPDATE person_settings SET updated_at = ? WHERE person_id = ? AND key = ?`, at, s.identity.PersonID, personSettingApprovalMode)
}

func admittedUnder(mode string, admittedAt int64) control.AdmissionSnapshot {
	snapshot := webAdmission(mode, "publish the release notes")
	snapshot.AdmittedAt = admittedAt
	return snapshot
}

func TestRestoredApprovalModeIsTheNarrowerOfTheAdmittedAndTheTightenedMode(t *testing.T) {
	const admittedAt = int64(1_790_000_000)
	for _, tc := range []struct {
		name           string
		admitted       string
		admittedAt     int64
		setting        string // "" leaves the preference unset
		settingAt      int64
		want           tools.ApprovalMode
		wantNarrowed   bool
		settingChanged []string // earlier writes of the same preference, oldest first
	}{
		{name: "full-auto-tightened-to-smart", admitted: "full-auto", setting: "smart", settingAt: admittedAt + 60, want: tools.ApprovalSmart, wantNarrowed: true},
		{name: "full-auto-tightened-to-on-request", admitted: "full-auto", setting: "on-request", settingAt: admittedAt + 60, want: tools.ApprovalOnRequest, wantNarrowed: true},
		{name: "full-auto-tightened-to-read-only", admitted: "full-auto", setting: "read-only", settingAt: admittedAt + 60, want: tools.ApprovalReadOnly, wantNarrowed: true},
		{name: "smart-tightened-to-on-request", admitted: "smart", setting: "on-request", settingAt: admittedAt + 60, want: tools.ApprovalOnRequest, wantNarrowed: true},
		{name: "on-request-tightened-to-read-only", admitted: "on-request", setting: "read-only", settingAt: admittedAt + 60, want: tools.ApprovalReadOnly, wantNarrowed: true},
		// A loosening never widens what was admitted.
		{name: "smart-loosened-to-full-auto", admitted: "smart", setting: "full-auto", settingAt: admittedAt + 60, want: tools.ApprovalSmart},
		{name: "read-only-loosened-to-smart", admitted: "read-only", setting: "smart", settingAt: admittedAt + 60, want: tools.ApprovalReadOnly},
		// Tightened and then loosened again: the person's mode now is the
		// admitted one, so nothing narrows.
		{name: "tightened-then-loosened-back", admitted: "full-auto", setting: "full-auto", settingAt: admittedAt + 120, want: tools.ApprovalFullAuto},
		// Tightened and loosened part of the way: the ceiling is where they are now.
		{name: "tightened-then-loosened-part-way", admitted: "full-auto", setting: "smart", settingAt: admittedAt + 120, want: tools.ApprovalSmart, wantNarrowed: true},
		// The modes that ask the same do not narrow each other.
		{name: "auto-edit-and-on-request-ask-alike", admitted: "auto-edit", setting: "on-request", settingAt: admittedAt + 60, want: tools.ApprovalAutoEdit},
		// A preference the person already had when the work was admitted
		// says nothing about it: an explicit full-auto run stays full-auto.
		{name: "preference-older-than-the-work", admitted: "full-auto", setting: "smart", settingAt: admittedAt - 60, want: tools.ApprovalFullAuto},
		// At the same second counts as after: the safe side of a one-second clock.
		{name: "preference-written-the-second-the-work-was-admitted", admitted: "full-auto", setting: "smart", settingAt: admittedAt, want: tools.ApprovalSmart, wantNarrowed: true},
		// A snapshot that cannot say when it was admitted gives a preference no
		// benefit of the doubt.
		{name: "admission-time-unknown", admitted: "full-auto", admittedAt: -1, setting: "smart", settingAt: admittedAt - 3600, want: tools.ApprovalSmart, wantNarrowed: true},
		{name: "no-preference-set", admitted: "full-auto", want: tools.ApprovalFullAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			if tc.setting != "" {
				s.setModeAt(t, tc.setting, tc.settingAt)
			}
			at := tc.admittedAt
			switch at {
			case 0:
				at = admittedAt
			case -1:
				at = 0
			}
			got := s.daemon.coordinator().restoredApprovalMode(ctx, s.identity, admittedUnder(tc.admitted, at))
			if got.Mode != tc.want || got.Narrowed != tc.wantNarrowed {
				t.Fatalf("restored mode = %+v, want %s narrowed=%t", got, tc.want, tc.wantNarrowed)
			}
			if got.Narrowed && got.From != tools.EffectiveApprovalMode(tc.admitted) {
				t.Fatalf("a narrowed mode names what it narrowed from: %+v", got)
			}
		})
	}
}

// When the preference cannot be read the person's current stance is unknown:
// restored work runs under no more than the product default, and never under
// a stricter mode than it was admitted with.
func TestRestoredApprovalModeWithAnUnreadablePreferenceIsNoLooserThanTheDefault(t *testing.T) {
	for _, tc := range []struct {
		admitted     string
		want         tools.ApprovalMode
		wantNarrowed bool
	}{
		{"full-auto", tools.DefaultApprovalMode, true},
		{"smart", tools.ApprovalSmart, false},
		{"on-request", tools.ApprovalOnRequest, false},
		{"read-only", tools.ApprovalReadOnly, false},
	} {
		t.Run(tc.admitted, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			s.setModeAt(t, "full-auto", time.Now().Unix()-3600)
			s.execRaw(t, `ALTER TABLE person_settings RENAME TO person_settings_unreadable`)
			got := s.daemon.coordinator().restoredApprovalMode(context.Background(), s.identity, admittedUnder(tc.admitted, time.Now().Unix()-600))
			if got.Mode != tc.want || got.Narrowed != tc.wantNarrowed || !got.Unreadable && tc.wantNarrowed {
				t.Fatalf("an unreadable preference gave %+v, want %s narrowed=%t (flagged unreadable)", got, tc.want, tc.wantNarrowed)
			}
		})
	}
}

// What the probe provider sees. The probe sends one dangerous command through
// the Run's approval funnel: full-auto lets it through, smart consults the
// judge. (A stricter mode would wait on a person, so these tests tighten to
// smart; the other modes are covered by the table above.)

// Queued work: tightening applies to it, loosening does not, and the Run that
// starts carries the admitted snapshot, never a rewritten one.
func TestTighteningTheModeNarrowsQueuedWorkAndLooseningDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name, admitted, changed string
		wantJudged              bool
	}{
		{"tightened", "full-auto", "smart", true},
		{"loosened", "smart", "full-auto", true},
		{"unchanged", "full-auto", "full-auto", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			s.setModeAt(t, tc.admitted, time.Now().Unix()-3600)
			coord := s.daemon.coordinator()
			if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
				t.Fatal("failed to occupy the person's slot")
			}
			queued, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
				Channel: "session-queued", Content: "look up the latest release notes"})
			if code != 200 || queued.Turn == nil || queued.Turn.QueueID == "" {
				t.Fatalf("busy turn was not queued: %d %+v", code, queued)
			}
			row, err := s.store.GetQueued(ctx, s.identity.TenantID, queued.Turn.QueueID)
			if err != nil || row == nil || row.Admission.ApprovalMode != tc.admitted || row.Admission.AdmittedAt == 0 {
				t.Fatalf("queued row admission = %+v err=%v", row, err)
			}
			s.setModeAt(t, tc.changed, row.Admission.AdmittedAt+30)
			coord.endActive(s.identity.PersonID)
			coord.drainQueue(s.identity)
			waitUntil(t, 5*time.Second, func() bool {
				q, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
				return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
			}, "queued work did not run")
			waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(s.identity.PersonID) == 0 }, "queued run did not finish")
			if probe := s.provider.probe(t, 1); probe.judged != tc.wantJudged || !probe.tools["web_search"] {
				t.Fatalf("queued work ran with %+v after the mode moved from %s to %s; want judged=%t with its web access",
					probe, tc.admitted, tc.changed, tc.wantJudged)
			}
			done, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
			if got := s.admission(t, done.RunID); got != row.Admission {
				t.Fatalf("the Run's admission = %+v, want the row's, unchanged: %+v", got, row.Admission)
			}
			narrowed := s.narrowingEvents(t, done.RunID)
			switch {
			case tc.wantJudged && tc.admitted == "full-auto" && len(narrowed) != 1:
				t.Fatalf("a narrowed Run records one run.authority_narrowed event, got %v", narrowed)
			case tc.admitted != "full-auto" || tc.changed == tc.admitted:
				if len(narrowed) != 0 {
					t.Fatalf("a Run that was not narrowed records no event, got %v", narrowed)
				}
			}
			if len(narrowed) == 1 && (!strings.Contains(narrowed[0], `"from":"full-auto"`) || !strings.Contains(narrowed[0], `"to":"smart"`)) {
				t.Fatalf("the event names both modes: %s", narrowed[0])
			}
		})
	}
}

// narrowingEvents lists the run.authority_narrowed payloads recorded for a Run.
func (s admissionProbeServer) narrowingEvents(t *testing.T, runID string) []string {
	t.Helper()
	run, err := s.store.GetRun(context.Background(), s.identity.TenantID, runID)
	if err != nil || run == nil {
		t.Fatalf("run %s: %v", runID, err)
	}
	events, err := s.store.ListTaskEvents(context.Background(), run.TaskID, 200)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []string
	for _, event := range events {
		if event.Type == "run.authority_narrowed" && event.RunID == runID {
			payloads = append(payloads, string(event.Payload))
		}
	}
	return payloads
}

// Parked on the provider: the continuation narrows with a tightening, keeps
// the web access its words earned, and carries the parent's snapshot verbatim.
func TestProviderWaitContinuationNarrowsWhenTheModeTightens(t *testing.T) {
	s := newAdmissionProbeServer(t, 1)
	ctx := context.Background()
	s.setModeAt(t, "full-auto", time.Now().Unix()-3600)
	reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-wait", Content: "look up the latest release notes"})
	if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
		t.Fatalf("parked turn = %d %+v", code, reply)
	}
	if first := s.provider.probe(t, 1); first.judged || !first.tools["web_search"] {
		t.Fatalf("parent ran under %+v", first)
	}
	parent := s.admission(t, reply.Run.ID)
	s.setModeAt(t, "smart", parent.AdmittedAt+30)
	child := s.drainDue(t, "provider-wait:"+reply.Run.ID)
	if resumed := s.provider.probe(t, 2); !resumed.judged || !resumed.tools["web_search"] {
		t.Fatalf("the continuation ignored the tightening or lost its web access: %+v", resumed)
	}
	if got := s.admission(t, child.ID); got != parent {
		t.Fatalf("the continuation's snapshot = %+v, want the parent's verbatim: %+v", got, parent)
	}
	if events := s.narrowingEvents(t, child.ID); len(events) != 1 {
		t.Fatalf("the narrowed continuation records one event, got %v", events)
	}
}

// Tightened and then loosened back before the drain: the person's mode now is
// the admitted one, so the work keeps it.
func TestProviderWaitContinuationKeepsTheAdmittedModeWhenTheTighteningWasUndone(t *testing.T) {
	s := newAdmissionProbeServer(t, 1)
	ctx := context.Background()
	s.setModeAt(t, "full-auto", time.Now().Unix()-3600)
	reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-wait", Content: "look up the latest release notes"})
	if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
		t.Fatalf("parked turn = %d %+v", code, reply)
	}
	parent := s.admission(t, reply.Run.ID)
	s.setModeAt(t, "smart", parent.AdmittedAt+30)
	s.setModeAt(t, "full-auto", parent.AdmittedAt+60)
	child := s.drainDue(t, "provider-wait:"+reply.Run.ID)
	if resumed := s.provider.probe(t, 2); resumed.judged {
		t.Fatalf("a tightening that was undone narrowed the work anyway: %+v", resumed)
	}
	if events := s.narrowingEvents(t, child.ID); len(events) != 0 {
		t.Fatalf("an unnarrowed continuation records no event, got %v", events)
	}
}

// An explicit per-run full-auto request is admitted after the person's own
// tightening. A provider wait does not demote it: the preference is older than
// the work, so it says nothing about it.
func TestExplicitFullAutoRunKeepsItsModeAcrossAProviderWaitAfterAnEarlierTightening(t *testing.T) {
	s := newAdmissionProbeServer(t, 1)
	ctx := context.Background()
	s.setModeAt(t, "smart", time.Now().Unix()-3600)
	reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-wait", Content: "look up the latest release notes", ApprovalMode: "full-auto"})
	if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
		t.Fatalf("parked turn = %d %+v", code, reply)
	}
	if first := s.provider.probe(t, 1); first.judged {
		t.Fatalf("the explicit full-auto run was not full-auto: %+v", first)
	}
	child := s.drainDue(t, "provider-wait:"+reply.Run.ID)
	if resumed := s.provider.probe(t, 2); resumed.judged {
		t.Fatalf("a provider wait demoted an explicit full-auto run to the person's older preference: %+v", resumed)
	}
	if events := s.narrowingEvents(t, child.ID); len(events) != 0 {
		t.Fatalf("an unnarrowed continuation records no event, got %v", events)
	}
}

// Work admitted after the tightening is a new admission: it takes whatever its
// own request and the mode in force say.
func TestWorkAdmittedAfterATighteningIsUnaffectedByIt(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	s.setModeAt(t, "smart", time.Now().Unix()-3600)
	reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-new", Content: "tidy the design notes", ApprovalMode: "full-auto"})
	if code != 200 || reply.Run == nil {
		t.Fatalf("turn = %d %+v", code, reply)
	}
	if probe := s.provider.probe(t, 1); probe.judged {
		t.Fatalf("a new explicit full-auto turn was narrowed by an older preference: %+v", probe)
	}
	if events := s.narrowingEvents(t, reply.Run.ID); len(events) != 0 {
		t.Fatalf("a new admission is never narrowed, got %v", events)
	}
}

// A scheduled turn that waits behind the person's work is admitted at its fire
// time; tightening before it runs narrows it like any other waiting work, and
// it keeps its schedule's origin and standing-class cutoff.
func TestQueuedCronNarrowsWithATighteningAndKeepsItsSchedule(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	s.setModeAt(t, "full-auto", time.Now().Unix()-3600)
	coord := s.daemon.coordinator()
	if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to occupy the person's slot")
	}
	authorized := time.Unix(1790000000, 0)
	job := cron.CronJob{ID: 42, TenantID: s.identity.TenantID, Platform: "cli", DeliverTo: "local", Channel: "session-cron",
		Prompt: "tidy the nightly notes", Web: true, AuthorizedAt: authorized}
	if err := NewCronExecutor(s.daemon, nil).RunCronJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	rows, err := s.store.ListQueued(ctx, s.identity.TenantID, s.identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queued cron rows = %+v %v", rows, err)
	}
	row := rows[0]
	if row.Admission.ApprovalMode != "full-auto" || row.Admission.AdmittedAt == 0 {
		t.Fatalf("the fire was admitted as %+v", row.Admission)
	}
	s.setModeAt(t, "smart", row.Admission.AdmittedAt+30)
	coord.endActive(s.identity.PersonID)
	coord.drainQueue(s.identity)
	waitUntil(t, 5*time.Second, func() bool {
		q, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
		return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
	}, "queued cron did not run")
	waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(s.identity.PersonID) == 0 }, "queued cron did not finish")
	done, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if probe := s.provider.probe(t, 1); !probe.judged || !probe.tools["web_search"] {
		t.Fatalf("the queued scheduled turn ignored the tightening or lost its web opt-in: %+v", probe)
	}
	if got := s.admission(t, done.RunID); got != row.Admission || got.Origin != (control.AdmissionOrigin{Kind: runOriginCron, ID: "42"}) ||
		got.StandingGrantsAuthorizedAt != authorized.Unix() {
		t.Fatalf("the scheduled Run's admission = %+v, want the fire's %+v", got, row.Admission)
	}
}

// A verification-only recovery keeps its read-only surface whatever the mode
// does; narrowing changes only what it may do without asking.
func TestVerificationOnlyRecoveryIsNarrowedByATighteningAndKeepsItsSurface(t *testing.T) {
	s := newAdmissionProbeServer(t)
	// The recovery instruction says not to repeat effects, which any mode takes
	// to a person; the narrowing is read from its event, the surface from the
	// tools the model was offered.
	s.provider.skipApprovals = true
	ctx := context.Background()
	parentAdmission := admittedUnder("full-auto", time.Now().Unix()-3600)
	parentAdmission.Strategy = control.AdmissionStrategy{Class: "general_task", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "disabled", ChannelMode: "interactive"}
	parentAdmission.Input.Summary = "deploy the docs site"
	s.setModeAt(t, "smart", parentAdmission.AdmittedAt+30)
	_, key := s.interruptedForRecovery(t, parentAdmission)
	row, err := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key)
	if err != nil || row == nil || !row.Admission.Strategy.VerificationOnly || row.Admission.ApprovalMode != "full-auto" {
		t.Fatalf("recovery row admission = %+v err=%v", row, err)
	}
	recovery := s.drainDue(t, key)
	probe := s.provider.probe(t, 1)
	if probe.tools["write_state"] || !probe.tools["read_state"] {
		t.Fatalf("recovery offered %+v; want the read-only surface", probe)
	}
	if got := s.admission(t, recovery.ID); got != row.Admission {
		t.Fatalf("recovery admission = %+v, want the row's %+v", got, row.Admission)
	}
	events := s.narrowingEvents(t, recovery.ID)
	if len(events) != 1 || !strings.Contains(events[0], `"from":"full-auto"`) || !strings.Contains(events[0], `"to":"smart"`) {
		t.Fatalf("the recovery Run runs narrowed and says so: %v", events)
	}
}
