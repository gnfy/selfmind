package httpapi

// GET /v1/digest (attach digest, G0-c): reopening the CLI must get one
// bounded person-scoped summary — finished/disrupted tasks, pending
// approvals, unconfirmed pushes, and the mid-flight run — anchored on the CLI
// account's last presence, and an empty world must produce an empty digest.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func newDigestTestServer(t *testing.T) (*Server, *control.Store, *control.IdentityContext) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Control: store, DefaultTenantID: "default"}, store, identity
}

func fetchDigest(t *testing.T, daemon *Server) api.DigestResponse {
	t.Helper()
	srv := httptest.NewServer(daemon.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/digest?platform=cli&platform_user_id=local")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("digest status = %d", resp.StatusCode)
	}
	var digest api.DigestResponse
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestDigestReportsAwayStateAndActiveRun(t *testing.T) {
	daemon, store, identity := newDigestTestServer(t)
	ctx := context.Background()

	mkTask := func(title, status string) *control.Task {
		t.Helper()
		task, err := store.CreateTask(ctx, control.TaskCreate{
			TenantID: identity.TenantID,
			PersonID: identity.PersonID,
			Title:    title,
			Channel:  "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, status, "outcome of "+title, nil); err != nil {
			t.Fatal(err)
		}
		return task
	}
	finished := mkTask("Ship the weekly report", "completed")
	interrupted := mkTask("Refactor the parser", "interrupted")

	payload, _ := json.Marshal(map[string]interface{}{
		"tool":   "terminal",
		"reason": "destructive command",
		"args":   map[string]interface{}{"command": "rm -rf build"},
	})
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		TaskID:     finished.ID,
		ActionType: "tool_call",
		Payload:    payload,
	}); err != nil {
		t.Fatal(err)
	}

	push, err := store.EnqueueDelivery(ctx, control.Delivery{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Platform: "weixin",
		Channel:  "weixin",
		Content:  "build finished — line one\nline two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliverySentUnconfirmed(ctx, push.ID); err != nil {
		t.Fatal(err)
	}

	running := mkTask("Long migration", "running")
	daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		TaskID:    running.ID,
		RunID:     "run_1",
		Channel:   "cli",
		Summary:   "migrate the database",
		StartedAt: time.Now().Add(-12 * time.Minute),
	})

	digest := fetchDigest(t, daemon)
	if digest.Empty() {
		t.Fatal("digest must not be empty")
	}
	if len(digest.FinishedTasks) != 1 || digest.FinishedTasks[0].ID != finished.ID ||
		digest.FinishedTasks[0].Title != "Ship the weekly report" ||
		digest.FinishedTasks[0].Summary != "outcome of Ship the weekly report" {
		t.Fatalf("finished tasks = %+v", digest.FinishedTasks)
	}
	if len(digest.DisruptedTasks) != 1 || digest.DisruptedTasks[0].ID != interrupted.ID ||
		digest.DisruptedTasks[0].Status != "interrupted" {
		t.Fatalf("disrupted tasks = %+v", digest.DisruptedTasks)
	}
	if len(digest.PendingApprovals) != 1 || digest.PendingApprovals[0].ID == "" ||
		!strings.Contains(digest.PendingApprovals[0].Line, "[terminal]") {
		t.Fatalf("pending approvals = %+v", digest.PendingApprovals)
	}
	if len(digest.UnconfirmedPushes) != 1 || digest.UnconfirmedPushes[0].Platform != "weixin" ||
		digest.UnconfirmedPushes[0].Status != "sent_unconfirmed" {
		t.Fatalf("unconfirmed pushes = %+v", digest.UnconfirmedPushes)
	}
	if strings.Contains(digest.UnconfirmedPushes[0].Preview, "\n") {
		t.Fatalf("push preview must be one line: %q", digest.UnconfirmedPushes[0].Preview)
	}
	if digest.ActiveRun == nil || digest.ActiveRun.TaskID != running.ID ||
		digest.ActiveRun.Title != "Long migration" || digest.ActiveRun.ElapsedSeconds < 11*60 {
		t.Fatalf("active run = %+v", digest.ActiveRun)
	}
	// Never-seen account: the anchor is the 24h fallback window, not zero.
	if got := time.Unix(digest.SinceUnix, 0); time.Since(got) > 25*time.Hour || time.Since(got) < 23*time.Hour {
		t.Fatalf("fallback anchor not ~24h ago: %v", got)
	}
}

func TestDigestEmptyWorldStaysEmpty(t *testing.T) {
	daemon, _, _ := newDigestTestServer(t)
	digest := fetchDigest(t, daemon)
	if !digest.Empty() {
		t.Fatalf("fresh world must produce an empty digest: %+v", digest)
	}
}

// TestDigestAnchorsOnAccountLastSeen: once a presence beat has stamped
// accounts.last_seen_at, the digest window starts there — tasks that finished
// while the user was watching are not re-reported on the next attach.
func TestDigestAnchorsOnAccountLastSeen(t *testing.T) {
	daemon, store, identity := newDigestTestServer(t)
	ctx := context.Background()

	// Simulate the presence beat (what the TUI's ping loop does while open).
	daemon.touchPresence(ctx, identity)
	accounts, err := store.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || len(accounts) != 1 || accounts[0].LastSeenAt == 0 {
		t.Fatalf("presence beat did not stamp last_seen_at: %+v, %v", accounts, err)
	}

	digest := fetchDigest(t, daemon)
	if digest.SinceUnix != accounts[0].LastSeenAt {
		t.Fatalf("digest anchor = %d, want the account's last_seen_at %d", digest.SinceUnix, accounts[0].LastSeenAt)
	}

	// A task finishing after the anchor shows up.
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "After the beat",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "completed", "", nil); err != nil {
		t.Fatal(err)
	}
	digest = fetchDigest(t, daemon)
	if len(digest.FinishedTasks) != 1 || digest.FinishedTasks[0].ID != task.ID {
		t.Fatalf("task finished after the anchor missing: %+v", digest.FinishedTasks)
	}
}
