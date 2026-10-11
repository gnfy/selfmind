package httpapi

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	platformlog "selfmind/internal/platform/log"
)

// logSink collects the daemon log; other goroutines may write while a test
// reads it.
type logSink struct {
	mu   sync.Mutex
	text strings.Builder
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

// lines counts log lines that contain every part.
func (s *logSink) lines(parts ...string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, line := range strings.Split(s.text.String(), "\n") {
		matched := line != ""
		for _, part := range parts {
			matched = matched && strings.Contains(line, part)
		}
		if matched {
			count++
		}
	}
	return count
}

func captureWarnings(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	platformlog.Init(platformlog.Options{Level: "warn", Output: sink})
	t.Cleanup(func() { platformlog.Init(platformlog.Options{}) })
	return sink
}

// One queued row whose admission snapshot cannot be read used to stop every
// drain for its person with no log. The drain now reports it once, holds only
// its source, and /queue marks it so the person can drop it.
func TestDrainSkipsUnreadableAdmissionAndHoldsOnlyItsSource(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	warnings := captureWarnings(t)
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	provider := newSlowLLMProvider("done")
	t.Cleanup(provider.releaseNow)
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test agent", 1, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}
	daemon.Delivery = delivery.NewService(store, &syncedSender{}, delivery.Options{})
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, entry := range []struct{ channel, content string }{
		{"session-b", "unreadable head"},
		{"session-b", "later same-source work"},
		{"session-c", "independent work"},
	} {
		queued, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
			Channel: entry.channel, Platform: "cli", PlatformUserID: "local", Content: entry.content})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, queued.ID)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE task_queue SET admission_json = '{"version":99}' WHERE id = ?`, ids[0]); err != nil {
		t.Fatal(err)
	}

	coord := daemon.coordinator()
	coord.drainQueue(identity)
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the independent source did not start behind an unreadable row")
	}
	status := func(id string) string {
		row, err := store.GetQueued(ctx, identity.TenantID, id)
		if err != nil || row == nil {
			t.Fatalf("queue row %s: %+v err=%v", id, row, err)
		}
		return row.Status
	}
	if status(ids[0]) != control.QueueStatusQueued || status(ids[1]) != control.QueueStatusQueued || status(ids[2]) == control.QueueStatusQueued {
		t.Fatalf("drain order: head=%s later=%s independent=%s", status(ids[0]), status(ids[1]), status(ids[2]))
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(identity.PersonID) == 0 }, "independent run did not finish")

	// The daemon says once why that source waits, however often it drains.
	coord.drainQueue(identity)
	if got := warnings.lines("unreadable admission snapshot", "queue_id="+ids[0], "person_id="+identity.PersonID); got != 1 {
		t.Fatalf("unreadable-row warnings for %s = %d, want 1; log:\n%s", ids[0], got, warnings)
	}
	if status(ids[1]) != control.QueueStatusQueued {
		t.Fatalf("a later drain started work held behind the unreadable row: %s", status(ids[1]))
	}

	// The person can still list and drop the row, which releases its source.
	// The listing marks the row that cannot start, and only that row.
	listing, code := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/queue"})
	if code != http.StatusOK || listing.Error != "" {
		t.Fatalf("/queue listing = %d %+v", code, listing)
	}
	var rows []string
	for _, line := range strings.Split(listing.Content, "\n") {
		if strings.HasPrefix(line, "1. ") || strings.HasPrefix(line, "2. ") {
			rows = append(rows, line)
		}
	}
	if len(rows) != 2 || !strings.Contains(rows[0], "1. unreadable head [cannot start: /queue drop 1]") ||
		strings.Contains(rows[1], "cannot start") || !strings.Contains(rows[1], "2. later same-source work") {
		t.Fatalf("/queue listing does not mark the unreadable row:\n%s", listing.Content)
	}
	reply, code := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/queue drop 1"})
	if code != http.StatusOK || reply.Error != "" {
		t.Fatalf("/queue drop failed: %d %+v", code, reply)
	}
	if status(ids[0]) != control.QueueStatusCancelled {
		t.Fatalf("/queue drop 1 did not drop the unreadable head: %s", status(ids[0]))
	}
	coord.drainQueue(identity)
	waitUntil(t, 5*time.Second, func() bool { return status(ids[1]) != control.QueueStatusQueued }, "the released source did not drain")
	waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(identity.PersonID) == 0 }, "released run did not finish")
}
