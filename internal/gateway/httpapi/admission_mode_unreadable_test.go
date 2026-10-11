package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// A new admission freezes the approval mode for the whole lineage, so a mode
// that could not be read must never be replaced by a guess: someone who set
// on-request would be admitted under the looser smart default for as long as
// their work lives. A preference that cannot be read is retried once and the
// admission is then refused, retryably and before anything is accepted.

// failModeReads makes the read of the person's /mode fail, as a store error on
// it would, until restoreModeReads. Every other read keeps working.
func (s admissionProbeServer) failModeReads() {
	s.daemon.coordinator().approvalModeRead = func(context.Context, string, string) (string, int64, error) {
		return "", 0, errors.New("injected settings read failure")
	}
}

func (s admissionProbeServer) restoreModeReads() { s.daemon.coordinator().approvalModeRead = nil }

func (s admissionProbeServer) recordedWork(t *testing.T) (runs, queued int) {
	t.Helper()
	ctx := context.Background()
	digests, err := s.store.ListRecentRunsForPerson(ctx, s.identity.TenantID, s.identity.PersonID, 50)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, status := range []string{control.QueueStatusQueued, control.QueueStatusStarted, control.QueueStatusDone, control.QueueStatusFailed} {
		listed, err := s.store.ListQueued(ctx, s.identity.TenantID, s.identity.PersonID, status)
		if err != nil {
			t.Fatal(err)
		}
		rows += len(listed)
	}
	return len(digests), rows
}

func TestDirectTurnIsRefusedNotAdmittedUnderAGuessedModeWhenThePreferenceIsUnreadable(t *testing.T) {
	s := newAdmissionProbeServer(t)
	// A stricter mode asks a person about the probe's dangerous command and would
	// wait for the answer; these tests read the frozen mode from the snapshot.
	s.provider.skipApprovals = true
	ctx := context.Background()
	s.setModeAt(t, "on-request", time.Now().Unix()-3600)
	s.failModeReads()

	req := api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-direct", Content: "summarize the design notes"}
	resp, status, refusal := s.daemon.processMessage(ctx, req)
	if refusal == nil || status != http.StatusServiceUnavailable {
		t.Fatalf("a turn whose mode cannot be read must be refused as not dispatched: status=%d refusal=%+v resp=%+v", status, refusal, resp)
	}
	if !strings.Contains(resp.Error, "approval mode") || resp.Run != nil || resp.Accepted {
		t.Fatalf("the refusal names the mode and accepts nothing: %+v", resp)
	}
	if runs, queued := s.recordedWork(t); runs != 0 || queued != 0 {
		t.Fatalf("a refused turn left %d Runs and %d queue rows behind", runs, queued)
	}
	s.provider.mu.Lock()
	calls := len(s.provider.probes)
	s.provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("a refused turn reached the model %d times", calls)
	}

	// The constraint that must change the result: once the preference can be
	// read again, the same message is admitted under the mode the person set.
	s.restoreModeReads()
	reply, code := s.daemon.ProcessMessage(ctx, req)
	if code != http.StatusOK || reply.Run == nil {
		t.Fatalf("the retried turn = %d %+v", code, reply)
	}
	if got := s.admission(t, reply.Run.ID); got.ApprovalMode != "on-request" {
		t.Fatalf("the retried turn was admitted under %q, want the person's on-request", got.ApprovalMode)
	}
}

// An explicit per-request mode needs no preference, so an unreadable one does
// not stop it.
func TestExplicitModeTurnDoesNotNeedThePreference(t *testing.T) {
	s := newAdmissionProbeServer(t)
	// A stricter mode asks a person about the probe's dangerous command and would
	// wait for the answer; these tests read the frozen mode from the snapshot.
	s.provider.skipApprovals = true
	s.failModeReads()
	reply, code := s.daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-explicit", Content: "summarize the design notes", ApprovalMode: "read-only"})
	if code != http.StatusOK || reply.Run == nil {
		t.Fatalf("explicit-mode turn = %d %+v", code, reply)
	}
	if got := s.admission(t, reply.Run.ID); got.ApprovalMode != "read-only" {
		t.Fatalf("the explicit mode was not honoured: %q", got.ApprovalMode)
	}
}

// A queued row written before snapshots existed has no frozen mode, so the
// drain reads the person's preference when it claims the row. Unreadable, the
// row stays queued — unclaimed, so nothing spins — and starts under the
// configured mode once it can be read.
func TestQueuedRowWithoutASnapshotWaitsForAReadablePreferenceInsteadOfGuessing(t *testing.T) {
	s := newAdmissionProbeServer(t)
	// A stricter mode asks a person about the probe's dangerous command and would
	// wait for the answer; these tests read the frozen mode from the snapshot.
	s.provider.skipApprovals = true
	ctx := context.Background()
	s.setModeAt(t, "on-request", time.Now().Unix()-3600)
	coord := s.daemon.coordinator()
	if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to occupy the person's slot")
	}
	queued, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-queued", Content: "summarize the design notes"})
	if code != 200 || queued.Turn == nil || queued.Turn.QueueID == "" {
		t.Fatalf("busy turn was not queued: %d %+v", code, queued)
	}
	// The row as an older version wrote it: no snapshot, no mode of its own.
	s.execRaw(t, `UPDATE task_queue SET admission_json = '', approval_mode = '' WHERE id = ?`, queued.Turn.QueueID)
	coord.endActive(s.identity.PersonID)
	s.failModeReads()
	coord.drainQueue(s.identity)
	row, err := s.store.GetQueued(ctx, s.identity.TenantID, queued.Turn.QueueID)
	if err != nil || row == nil || row.Status != control.QueueStatusQueued || row.RunID != "" || row.AttemptGeneration != 0 {
		t.Fatalf("a row whose mode cannot be read must stay queued, unstarted and unclaimed: %+v %v", row, err)
	}
	// Never claimed: a row that is claimed, refused and released again would
	// show as queued here while the drain spun on it, so its claim count is
	// the proof that nothing spins. Let any such loop run before looking.
	coord.drainQueue(s.identity)
	time.Sleep(300 * time.Millisecond)
	again, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if err != nil || again == nil {
		t.Fatalf("re-read the row: %+v %v", again, err)
	}
	if again.AttemptGeneration != 0 {
		t.Fatalf("a row whose mode cannot be read must never be claimed; it was claimed %d times: %+v", again.AttemptGeneration, again)
	}
	if runs, _ := s.recordedWork(t); runs != 0 {
		t.Fatalf("the drain started %d Runs under a guessed mode", runs)
	}

	s.restoreModeReads()
	coord.drainQueue(s.identity)
	waitUntil(t, 5*time.Second, func() bool {
		q, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
		return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
	}, "the row did not start once the preference was readable")
	done, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if got := s.admission(t, done.RunID); got.ApprovalMode != "on-request" {
		t.Fatalf("the row started under %q, want the person's on-request", got.ApprovalMode)
	}
}
