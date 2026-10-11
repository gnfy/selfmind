package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// A queued row whose own snapshot cannot be read never starts under a
// re-derived authority. The drain that finds it holds it, tells the person
// once, and lets independent work run; /queue names it and /queue drop ends it.
func TestQueuedRowWithAnUnreadableOwnSnapshotIsHeldAndTheDrainTellsThePerson(t *testing.T) {
	s := newAdmissionProbeServer(t)
	// A stricter mode would ask a person about the probe's dangerous command.
	s.provider.skipApprovals = true
	ctx := context.Background()
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	coord := s.daemon.coordinator()
	if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to occupy the person's slot")
	}
	queued, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-held", Content: "publish the release notes"})
	if code != 200 || queued.Turn == nil || queued.Turn.QueueID == "" {
		t.Fatalf("busy turn was not queued: %d %+v", code, queued)
	}
	// A snapshot this binary cannot interpret, as a newer or damaged one would be.
	s.execRaw(t, `UPDATE task_queue SET admission_json = '{"version":99}' WHERE id = ?`, queued.Turn.QueueID)
	row, err := s.store.GetQueued(ctx, s.identity.TenantID, queued.Turn.QueueID)
	if err != nil || row == nil || row.AdmissionError == nil {
		t.Fatalf("fixture: the row must read as having an unreadable snapshot: %+v %v", row, err)
	}

	coord.endActive(s.identity.PersonID)
	coord.drainQueue(s.identity)
	notices := func() int {
		count := 0
		for _, msg := range s.sender.snapshot() {
			if strings.Contains(msg.Content, "was not started") && strings.Contains(msg.Content, "/queue drop") {
				count++
			}
		}
		return count
	}
	waitUntil(t, 5*time.Second, func() bool { return notices() == 1 }, "the drain that found the unreadable row did not tell the person")
	if held, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID); held == nil || held.Status != control.QueueStatusQueued || held.RunID != "" {
		t.Fatalf("the row started or left the queue: %+v", held)
	}

	// Independent work still runs, and later drains neither start the held row
	// nor tell the person again.
	if _, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-other", Content: "summarize the design notes"}); code != 200 {
		t.Fatalf("independent turn = %d", code)
	}
	coord.drainQueue(s.identity)
	time.Sleep(100 * time.Millisecond)
	if held, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID); held == nil || held.Status != control.QueueStatusQueued || held.RunID != "" || notices() != 1 {
		t.Fatalf("held row after later drains = %+v notices=%d", held, notices())
	}
	s.provider.mu.Lock()
	calls := len(s.provider.probes)
	s.provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("model calls = %d, want only the independent turn", calls)
	}
	listing, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Content: "/queue"})
	if code != 200 || !strings.Contains(listing.Content, "[cannot start: /queue drop 1]") {
		t.Fatalf("/queue listing = %d %q", code, listing.Content)
	}
}
