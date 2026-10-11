package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/task/cron"
)

type spineRecord struct{ User, Source string }

// spineRecords returns the user text and source tag of every work-spine entry,
// oldest first.
func (s admissionProbeServer) spineRecords(t *testing.T) []spineRecord {
	t.Helper()
	blobs, err := s.spine.GetLatestContext(context.Background(), s.identity.PersonID, kernel.SpineTrajectoryKey)
	if err != nil {
		t.Fatal(err)
	}
	var records []spineRecord
	for _, blob := range blobs {
		var entry struct {
			Kind   string `json:"kind"`
			User   string `json:"user"`
			Source string `json:"source"`
		}
		if json.Unmarshal(blob, &entry) == nil && entry.Kind != "" {
			records = append(records, spineRecord{entry.User, entry.Source})
		}
	}
	return records
}

// Scheduled work keeps its source tag on the work spine however it continues:
// the tag follows the authority the work was admitted with, not the hop that
// happens to run it. A provider wait or an answered approval of a scheduled
// fire is a continuation of the schedule's work, and the person's own parked
// work is never tagged as scheduled.
func TestScheduledWorkKeepsItsSpineSourceAcrossContinuations(t *testing.T) {
	const request = "tidy the nightly notes"
	t.Run("provider wait", func(t *testing.T) {
		s := newAdmissionProbeServer(t, 1)
		ctx := context.Background()
		s.setMode(t, "smart")
		job := cron.CronJob{ID: 7, TenantID: s.identity.TenantID, Platform: "cli", DeliverTo: "local", Channel: "session-cron", Prompt: request}
		if err := NewCronExecutor(s.daemon, nil).RunCronJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		digests, err := s.store.ListRecentRunsForPerson(ctx, s.identity.TenantID, s.identity.PersonID, 10)
		if err != nil || len(digests) != 1 {
			t.Fatalf("scheduled runs = %+v %v", digests, err)
		}
		s.drainDue(t, "provider-wait:"+digests[0].RunID)
		records := s.spineRecords(t)
		if len(records) == 0 {
			t.Fatal("the continuation recorded nothing on the work spine")
		}
		if last := records[len(records)-1]; last.User != request || last.Source != runOriginCron {
			t.Fatalf("the continuation of a scheduled fire recorded %+v, want user=%q source=%q", last, request, runOriginCron)
		}
	})

	t.Run("answered approval of scheduled work", func(t *testing.T) {
		s := newAdmissionProbeServer(t)
		ctx := context.Background()
		admitted := webAdmission("smart", request)
		admitted.Origin = control.AdmissionOrigin{Kind: runOriginCron, ID: "7"}
		task, run := s.parkedRunForHuman(t, admitted, request)
		approval, err := s.store.CreateApprovalRequest(ctx, control.ApprovalRequest{TenantID: s.identity.TenantID,
			PersonID: s.identity.PersonID, TaskID: task.ID, RunID: run.ID, ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:probe"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.store.ParkApprovalRequest(ctx, s.identity.TenantID, approval.ID, "waiter gone"); err != nil {
			t.Fatal(err)
		}
		if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.daemon.respondApprovalByToken(ctx, s.identity, approval.ID, "approved", "cli", control.ApprovalDecisionInput{DecisionID: "once"}); err != nil {
			t.Fatal(err)
		}
		s.drainDue(t, "approval-resume:"+approval.ID)
		records := s.spineRecords(t)
		if len(records) == 0 || records[len(records)-1] != (spineRecord{request, runOriginCron}) {
			t.Fatalf("the answered approval of scheduled work recorded %+v, want user=%q source=%q", records, request, runOriginCron)
		}
	})

	// The constraint that must change the result: the person's own parked work
	// continues untagged.
	t.Run("the person's own work stays untagged", func(t *testing.T) {
		s := newAdmissionProbeServer(t, 1)
		ctx := context.Background()
		s.setMode(t, "smart")
		reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
			Channel: "session-wait", Content: request})
		if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
			t.Fatalf("parked turn = %d %+v", code, reply)
		}
		s.drainDue(t, "provider-wait:"+reply.Run.ID)
		for _, record := range s.spineRecords(t) {
			if record.Source != "" {
				t.Fatalf("the person's own work was tagged %q on the spine: %+v", record.Source, record)
			}
		}
	})
}
