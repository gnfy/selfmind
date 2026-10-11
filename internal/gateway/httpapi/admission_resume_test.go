package httpapi

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

// webAdmission is the snapshot of a Run admitted with web access, under the
// given approval mode and original request, ten minutes ago.
func webAdmission(mode, request string) control.AdmissionSnapshot {
	return control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: mode,
		AdmittedAt: time.Now().Unix() - 600,
		Strategy: control.AdmissionStrategy{Class: "external_lookup", ToolMode: "full", PlanPolicy: "optional",
			WebPolicy: "enabled", ChannelMode: "interactive"},
		Input: control.AdmissionInput{Summary: request}}
}

// parkedRunForHuman starts a Run admitted with admission and leaves it waiting
// on the person, the state a human-wait resume row continues.
func (s admissionProbeServer) parkedRunForHuman(t *testing.T, admission control.AdmissionSnapshot, request string) (*control.Task, *control.Run) {
	t.Helper()
	ctx := context.Background()
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.store.StartRunWithOptions(ctx, task, "cli", request, control.StartRunOptions{Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	// A Run started through the gateway records the person's request as its
	// approval evidence; a continuation that finds none asks a person in every
	// mode, which would hide the authority this test reads.
	if _, err := s.store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: run.ID, Type: "run.started", Visibility: "task", Channel: "cli",
		Payload: mustJSON(map[string]interface{}{"input": request, "approval_intent": map[string]interface{}{"version": 3,
			"snapshot": map[string]interface{}{"model_authorization": true, "raw_user_text": request, "goal_summary": request, "source": "direct"}}})}); err != nil {
		t.Fatal(err)
	}
	return task, run
}

// An approval answered after its Run parked continues under the authority that
// Run was admitted with, narrowed by a tightening of the person's mode since
// and never widened by a loosening: a stricter mode takes the full-auto mode
// away but not the web access the request earned, and a looser one does not
// give a smart-mode Run more.
func TestParkedApprovalResumeKeepsAdmittedAuthorityInBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name, admitted, changed string
		wantJudged              bool
	}{
		{"stricter mode", "full-auto", "smart", true},
		{"looser mode", "smart", "full-auto", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			admitted := webAdmission(tc.admitted, "publish the release notes")
			task, run := s.parkedRunForHuman(t, admitted, "publish the release notes")
			approval, err := s.store.CreateApprovalRequest(ctx, control.ApprovalRequest{TenantID: s.identity.TenantID,
				PersonID: s.identity.PersonID, TaskID: task.ID, RunID: run.ID, ActionType: "tool_call",
				AuthorizationFingerprint: "resume:v1:probe"})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.store.ParkApprovalRequest(ctx, s.identity.TenantID, approval.ID, "waiter gone"); err != nil {
				t.Fatal(err)
			}
			if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			s.setMode(t, tc.changed)
			if _, err := s.daemon.respondApprovalByToken(ctx, s.identity, approval.ID, "approved", "cli",
				control.ApprovalDecisionInput{DecisionID: "once"}); err != nil {
				t.Fatal(err)
			}
			child := s.drainDue(t, "approval-resume:"+approval.ID)
			probe := s.provider.probe(t, 1)
			if probe.judged != tc.wantJudged || !probe.tools["web_search"] {
				t.Fatalf("approval continuation ran under %+v after the mode moved to %s; want judged=%t with web access",
					probe, tc.changed, tc.wantJudged)
			}
			if got := s.admission(t, child.ID); got != admitted {
				t.Fatalf("continuation admission = %+v, want the parked Run's %+v", got, admitted)
			}
			if child.InputSummary != "publish the release notes" {
				t.Fatalf("continuation summary = %q, want the original request", child.InputSummary)
			}
		})
	}
}

// A clarification answered after its Run parked: the answer is the person's own
// text, and everything that carries authority comes from the parked Run, the
// mode narrowed by a tightening since and never widened.
func TestParkedClarificationResumeKeepsAdmittedAuthorityInBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name, admitted, changed string
		wantJudged              bool
	}{
		{"stricter mode", "full-auto", "smart", true},
		{"looser mode", "smart", "full-auto", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			admitted := webAdmission(tc.admitted, "publish the release notes")
			task, run := s.parkedRunForHuman(t, admitted, "publish the release notes")
			clarify, err := s.store.CreateClarifyRequest(ctx, control.ClarifyRequest{TenantID: s.identity.TenantID,
				PersonID: s.identity.PersonID, TaskID: task.ID, RunID: run.ID, Question: "Which environment?", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			s.setMode(t, tc.changed)
			if _, err := s.daemon.answerClarifyTarget(ctx, s.identity, clarify, "staging", "cli"); err != nil {
				t.Fatal(err)
			}
			child := s.drainDue(t, "clarify-resume:"+clarify.ID)
			probe := s.provider.probe(t, 1)
			if probe.judged != tc.wantJudged || !probe.tools["web_search"] {
				t.Fatalf("clarification continuation ran under %+v after the mode moved to %s; want judged=%t with web access",
					probe, tc.changed, tc.wantJudged)
			}
			if got := s.admission(t, child.ID); got != admitted {
				t.Fatalf("continuation admission = %+v, want the parked Run's %+v", got, admitted)
			}
		})
	}
}

// A continuation row stored without a snapshot, because its parent's could not
// be read when the row was written, still runs under its exact parent's
// authority: the drain reads the parent again, and takes the person's mode of
// the moment only to narrow it.
func TestContinuationRowWithoutSnapshotInheritsItsParentsAtDrain(t *testing.T) {
	for _, tc := range []struct {
		name, admitted, changed string
		wantJudged              bool
	}{
		{"stricter mode", "full-auto", "smart", true},
		{"looser mode", "smart", "full-auto", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t, 1)
			ctx := context.Background()
			s.setMode(t, tc.admitted)
			reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
				Channel: "session-wait", Content: "look up the latest release notes"})
			if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
				t.Fatalf("parked turn = %d %+v", code, reply)
			}
			key := "provider-wait:" + reply.Run.ID
			s.execRaw(t, `UPDATE task_queue SET admission_json = '' WHERE idempotency_key = ?`, key)
			if row, err := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key); err != nil || row == nil || !row.Admission.IsZero() {
				t.Fatalf("the row kept its snapshot: %+v %v", row, err)
			}
			s.setMode(t, tc.changed)
			child := s.drainDue(t, key)
			if probe := s.provider.probe(t, 2); probe.judged != tc.wantJudged || !probe.tools["web_search"] {
				t.Fatalf("a row with no snapshot ran under %+v after the mode moved to %s; want judged=%t with web access",
					probe, tc.changed, tc.wantJudged)
			}
			if got, parent := s.admission(t, child.ID), s.admission(t, reply.Run.ID); got != parent || parent.IsZero() {
				t.Fatalf("continuation admission = %+v, want the parent's %+v", got, parent)
			}
		})
	}
}

// A continuation's summary and the work spine name what the person asked for,
// not the text the daemon scheduled the continuation with, for each kind of
// daemon continuation and whatever language the request was in.
func TestContinuationSummaryAndSpineRecordTheOriginalRequest(t *testing.T) {
	for _, request := range []string{"look up the latest release notes", "把设计笔记里的标题整理一遍"} {
		t.Run("provider wait: "+request, func(t *testing.T) {
			s := newAdmissionProbeServer(t, 1)
			reply, code := s.daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local",
				Channel: "session-wait", Content: request})
			if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
				t.Fatalf("parked turn = %d %+v", code, reply)
			}
			child := s.drainDue(t, "provider-wait:"+reply.Run.ID)
			if child.InputSummary != request {
				t.Errorf("continuation summary = %q, want %q", child.InputSummary, request)
			}
			if users := s.spineUsers(t); len(users) != 1 || users[0] != request {
				t.Errorf("work spine = %q, want only %q", users, request)
			}
		})
	}
	t.Run("recovery", func(t *testing.T) {
		s := newAdmissionProbeServer(t, 1)
		s.provider.skipApprovals = true
		_, key := s.interruptedForRecovery(t, webAdmission("full-auto", "deploy the docs site"))
		recovery := s.drainDue(t, key)
		child := s.drainDue(t, "provider-wait:"+recovery.ID)
		if recovery.InputSummary != "deploy the docs site" || child.InputSummary != "deploy the docs site" {
			t.Errorf("recovery summaries = %q / %q", recovery.InputSummary, child.InputSummary)
		}
		if users := s.spineUsers(t); len(users) != 1 || users[0] != "deploy the docs site" {
			t.Errorf("work spine = %q, want the original request", users)
		}
	})
}

// A scheduled fire that finds a free slot records its schedule in its Run: the
// job it came from, the instant its schedule was authorized, and the web
// opt-in of the job. A job nobody authorized records no instant.
func TestScheduledFireRecordsItsScheduleAuthority(t *testing.T) {
	authorized := time.Unix(1790000000, 0)
	for _, tc := range []struct {
		name       string
		job        cron.CronJob
		wantID     string
		wantCutoff int64
		wantWeb    bool
	}{
		{"authorized job with web", cron.CronJob{ID: 7, Prompt: "tidy the nightly notes", Web: true, AuthorizedAt: authorized}, "7", authorized.Unix(), true},
		{"unauthorized job without web", cron.CronJob{ID: 9, Prompt: "整理夜间笔记"}, "9", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			s.setMode(t, "smart")
			job := tc.job
			job.TenantID, job.Platform, job.DeliverTo, job.Channel = s.identity.TenantID, "cli", "local", "session-cron"
			if err := NewCronExecutor(s.daemon, nil).RunCronJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			digests, err := s.store.ListRecentRunsForPerson(ctx, s.identity.TenantID, s.identity.PersonID, 10)
			if err != nil || len(digests) != 1 {
				t.Fatalf("scheduled runs = %+v %v", digests, err)
			}
			got := s.admission(t, digests[0].RunID)
			if got.Origin != (control.AdmissionOrigin{Kind: runOriginCron, ID: tc.wantID}) ||
				got.StandingGrantsAuthorizedAt != tc.wantCutoff || got.ApprovalMode != "smart" ||
				(got.Strategy.WebPolicy == string(kernel.WebPolicyEnabled)) != tc.wantWeb ||
				got.Input.Summary != job.Prompt {
				t.Fatalf("scheduled admission = %+v", got)
			}
			if probe := s.provider.probe(t, 1); probe.tools["web_search"] != tc.wantWeb {
				t.Fatalf("scheduled fire offered web_search=%t, want %t", probe.tools["web_search"], tc.wantWeb)
			}
		})
	}
}

// Origin is read from the request or the turn tag alike. A scheduled turn that
// carries only the tag is still scheduled, and its standing classes stay frozen
// rather than opening to the person's own.
func TestTaggedScheduledTurnIsAdmittedAsScheduled(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := kernel.WithTurnSource(context.Background(), runOriginCron)
	reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-tagged", Content: "tidy the nightly notes"})
	if code != 200 || reply.Run == nil {
		t.Fatalf("tagged turn = %d %+v", code, reply)
	}
	got := s.admission(t, reply.Run.ID)
	if got.Origin.Kind != runOriginCron || got.StandingGrantsAuthorizedAt != 0 {
		t.Fatalf("tagged scheduled turn admission = %+v", got)
	}
	policy := standingGrantPolicy(ctx, api.MessageRequest{Admission: got})
	if policy.Allowed {
		t.Fatalf("a scheduled turn nobody authorized consumed standing classes: %+v", policy)
	}
}

// Recording the authority must never refuse a turn. A channel name longer than
// a snapshot identifier is still a valid request; the channel mode it records
// is bounded, which only changes a label in the prompt.
func TestOverlongChannelStillAdmitsWithABoundedChannelMode(t *testing.T) {
	for _, channel := range []string{strings.Repeat("c", 400), strings.Repeat("频", 200)} {
		s := newAdmissionProbeServer(t)
		reply, code := s.daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local",
			Channel: channel, Content: "tidy the design notes"})
		if code != 200 || reply.Run == nil {
			t.Fatalf("a %d-byte channel was refused: %d %+v", len(channel), code, reply)
		}
		if mode := s.admission(t, reply.Run.ID).Strategy.ChannelMode; mode == "" || len(mode) > 256 {
			t.Fatalf("channel mode = %d bytes", len(mode))
		}
	}
}

// The strategy a continuation runs under is the admitted one whatever its
// scheduling text says. Text that reads like a lookup request never earns web
// access for work admitted without it, and text that does not never takes it
// from work admitted with it.
func TestRestoredStrategyIgnoresSchedulingText(t *testing.T) {
	daemon, _, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
	withWeb, withoutWeb := webAdmission("smart", "publish the notes"), webAdmission("smart", "tidy the notes")
	withoutWeb.Strategy.WebPolicy, withoutWeb.Strategy.Class = string(kernel.WebPolicyDisabled), "general_task"
	for _, tc := range []struct {
		name      string
		admission control.AdmissionSnapshot
		text      string
		wantWeb   bool
	}{
		{"lookup wording cannot earn web", withoutWeb, "Look up the latest release notes today and search the web for pricing.", false},
		{"plain wording cannot lose web", withWeb, "Continue this exact work after the provider became available.", true},
		{"lookup wording in Chinese cannot earn web", withoutWeb, "查询最新的发布说明，上网搜索价格", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := control.QueuedTask{TenantID: "default", PersonID: "person", Content: tc.text, ReplyToRunID: "run_1",
				IdempotencyKey: "provider-wait:run_1", Admission: tc.admission}
			req := daemon.restoreQueuedAuthority(context.Background(), api.MessageRequest{Content: tc.text}, row)
			if strategy := admittedStrategy(req.Admission); strategy.AllowsTool("web_search") != tc.wantWeb {
				t.Fatalf("restored strategy web_search=%t for %q, want %t", strategy.AllowsTool("web_search"), tc.text, tc.wantWeb)
			}
		})
	}
}

// A person who has a terminal attached is told by the live notice, and the
// durable record backs it: when that record cannot be written the push still
// goes out, and the record is written once the store recovers, without a
// second push.
func TestUnreadableAdmissionNoticeSurvivesAFailedEventWrite(t *testing.T) {
	s := newAdmissionProbeServer(t)
	s.daemon.coordinator().unreadableNoticeRetry = time.Nanosecond
	ctx := context.Background()
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	s.daemon.presenceTracker().Touch(s.identity.PersonID, "cli")
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parked, err := s.store.StartRunWithOptions(ctx, task, "cli", "publish the release",
		control.StartRunOptions{Admission: webAdmission("smart", "publish the release")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, parked.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	s.corruptRunAdmission(t, parked.ID)
	s.failEventWrites(t, "queue.admission_unreadable")

	wait := control.ExternalResourceWait{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, RunID: parked.ID, EffectID: "effect-notice"}
	if err := s.daemon.enqueueExternalResourceWake(ctx, wait, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	pushes := func() int {
		count := 0
		for _, msg := range s.sender.snapshot() {
			if msg.Platform == "weixin" && strings.Contains(msg.Content, "/queue drop") {
				count++
			}
		}
		return count
	}
	waitUntil(t, 5*time.Second, func() bool { return pushes() == 1 },
		"the person with an attached terminal was left uninformed when the durable notice could not be written")
	if hasEventOfType(t, s.store, task.ID, "queue.admission_unreadable") {
		t.Fatal("the injected event failure did not apply")
	}

	s.allowEventWrites(t)
	s.daemon.coordinator().drainQueue(s.identity)
	if !hasEventOfType(t, s.store, task.ID, "queue.admission_unreadable") {
		t.Fatal("the durable notice was not retried after the store recovered")
	}
	if got := pushes(); got != 1 {
		t.Fatalf("the person was pushed %d times, want once", got)
	}
}

// failEventWrites makes appending an event of the given type fail until
// allowEventWrites.
func (s admissionProbeServer) failEventWrites(t *testing.T, eventType string) {
	t.Helper()
	s.execRaw(t, `CREATE TRIGGER fail_notice_event BEFORE INSERT ON task_events WHEN NEW.type='`+eventType+`' BEGIN SELECT RAISE(ABORT,'injected event failure'); END`)
}

func (s admissionProbeServer) allowEventWrites(t *testing.T) {
	t.Helper()
	s.execRaw(t, `DROP TRIGGER fail_notice_event`)
}
