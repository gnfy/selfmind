package httpapi

// G0-b presence-based routing: while the user is sitting in the TUI a
// CLI-origin approval prompt stays inline (no IM push), and when the CLI is
// detached the push goes to ONE preferred endpoint — never a fan-out to every
// bound IM account. See docs/identity-continuity.md "Runtime attachment
// model", conversation-layer rules 3/4.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
)

func TestPresenceRegistryTouchAndTTL(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	registry := newPresenceRegistry(presenceTTL)
	registry.now = func() time.Time { return now }

	if registry.IsAttached("person_1", "cli") {
		t.Fatal("untouched endpoint must read detached")
	}
	registry.Touch("person_1", "cli")
	if !registry.IsAttached("person_1", "cli") {
		t.Fatal("touched endpoint must read attached")
	}
	if registry.IsAttached("person_1", "weixin") {
		t.Fatal("presence is per platform")
	}
	if registry.IsAttached("person_2", "cli") {
		t.Fatal("presence is per person")
	}

	now = now.Add(presenceTTL - time.Second)
	if !registry.IsAttached("person_1", "cli") {
		t.Fatal("endpoint must stay attached inside the TTL")
	}
	now = now.Add(2 * time.Second)
	if registry.IsAttached("person_1", "cli") {
		t.Fatal("endpoint must expire to detached after the TTL (implicit detach)")
	}
	// A new beat re-attaches.
	registry.Touch("person_1", "cli")
	if !registry.IsAttached("person_1", "cli") {
		t.Fatal("a fresh beat must re-attach the endpoint")
	}
}

func TestPresenceRegistryThrottlesDurablePersist(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	registry := newPresenceRegistry(presenceTTL)
	registry.now = func() time.Time { return now }

	if !registry.Touch("person_1", "cli") {
		t.Fatal("first beat must request a durable last_seen persist")
	}
	now = now.Add(time.Second)
	if registry.Touch("person_1", "cli") {
		t.Fatal("a beat 1s later must be throttled (write amplification)")
	}
	now = now.Add(presencePersistInterval)
	if !registry.Touch("person_1", "cli") {
		t.Fatal("a beat past the persist interval must request a persist again")
	}
}

// TestApprovalPushSkippedWhileCLIAttached is the double-notification fix: the
// attached TUI shows the inline y/N prompt, so the IM push must not happen.
func TestApprovalPushSkippedWhileCLIAttached(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()

	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	// The mid-turn TUI heartbeats via /v1/message + /v1/tasks/events polls;
	// simulate the beat directly.
	daemon.touchPresence(ctx, identity)

	daemon.coordinator().notifyApprovalRequested(ctx, identity, task.ID, "", "278361aa-ea7b-4f0b-a338-56c0cfab61a6", approval)

	if len(recorder.messages) != 0 {
		t.Fatalf("attached CLI must suppress the IM approval push; got %+v", recorder.messages)
	}

	// Once presence lapses (terminal closed / crashed), the same approval
	// push goes out to the preferred IM endpoint.
	daemon.presenceTracker().now = func() time.Time { return time.Now().Add(presenceTTL + time.Second) }
	daemon.coordinator().notifyApprovalRequested(ctx, identity, task.ID, "", "278361aa-ea7b-4f0b-a338-56c0cfab61a6", approval)
	if len(recorder.messages) != 1 || recorder.messages[0].Platform != "weixin" {
		t.Fatalf("detached CLI must push to the preferred IM endpoint; got %+v", recorder.messages)
	}
}

// TestCLIOriginSingleTargetWithTwoBoundIM: with weixin AND telegram bound,
// only the most recently seen account receives the push (never both), and an
// explicit /notify preference overrides recency.
func TestCLIOriginSingleTargetWithTwoBoundIM(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()

	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "telegram", "tg_456", "Me on Telegram"); err != nil {
		t.Fatal(err)
	}
	// Telegram is the most recently seen endpoint.
	accounts, err := store.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.Platform == "telegram" {
			if err := store.TouchAccountLastSeen(ctx, identity.TenantID, account.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	coord := daemon.coordinator()
	coord.notifyApprovalRequested(ctx, identity, task.ID, "", "278361aa-ea7b-4f0b-a338-56c0cfab61a6", approval)
	if len(recorder.messages) != 1 {
		t.Fatalf("exactly ONE endpoint must receive the push, got %+v", recorder.messages)
	}
	if recorder.messages[0].Platform != "telegram" || recorder.messages[0].PlatformUserID != "tg_456" {
		t.Fatalf("target = %s/%s, want the most recent telegram/tg_456", recorder.messages[0].Platform, recorder.messages[0].PlatformUserID)
	}

	// Explicit preference wins over recency.
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/notify weixin"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "weixin") {
		t.Fatalf("/notify weixin: status=%d content=%q", status, resp.Content)
	}
	// ProcessMessage marked the CLI attached; expire that beat so the
	// detached branch is exercised again.
	daemon.presenceTracker().now = func() time.Time { return time.Now().Add(presenceTTL + time.Second) }

	recorder.messages = nil
	coord.notifyApprovalRequested(ctx, identity, task.ID, "", "278361aa-ea7b-4f0b-a338-56c0cfab61a6", approval)
	if len(recorder.messages) != 1 || recorder.messages[0].Platform != "weixin" || recorder.messages[0].PlatformUserID != "wxid_123" {
		t.Fatalf("after /notify weixin the push must switch to weixin/wxid_123 only, got %+v", recorder.messages)
	}
}

// TestCLIAsyncResultSingleTarget: the async/detached result path obeys the
// same single-preferred-endpoint rule as approvals.
func TestCLIAsyncResultSingleTarget(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()

	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "telegram", "tg_456", "Me on Telegram"); err != nil {
		t.Fatal(err)
	}
	accounts, err := store.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.Platform == "telegram" {
			if err := store.TouchAccountLastSeen(ctx, identity.TenantID, account.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	daemon.coordinator().deliverAsyncResult(ctx, identity,
		api.MessageRequest{Platform: "cli", Channel: "cli"},
		api.MessageResponse{Content: "All done.", Task: task})

	if len(recorder.messages) != 1 {
		t.Fatalf("async result must reach exactly one endpoint, got %+v", recorder.messages)
	}
	if recorder.messages[0].Platform != "telegram" || recorder.messages[0].PlatformUserID != "tg_456" {
		t.Fatalf("target = %s/%s, want telegram/tg_456", recorder.messages[0].Platform, recorder.messages[0].PlatformUserID)
	}
}

// TestNotifyCommandValidatesOwnAccounts: the bound-account check is a security
// boundary — a platform the person has not bound is never a valid target.
func TestNotifyCommandValidatesOwnAccounts(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()

	// No bound IM accounts at all.
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/notify telegram"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "no bound IM accounts") {
		t.Fatalf("status=%d content=%q", status, resp.Content)
	}

	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}

	// An unbound platform is rejected and the bound ones are listed.
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/notify telegram"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "not one of your bound IM accounts") || !strings.Contains(resp.Content, "weixin") {
		t.Fatalf("status=%d content=%q", status, resp.Content)
	}
	if pref, _ := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform); pref != "" {
		t.Fatalf("rejected /notify must not store a preference, got %q", pref)
	}

	// A bound platform is accepted; /notify with no argument reports it.
	if resp, _ = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/notify weixin"}); !strings.Contains(resp.Content, "set to weixin") {
		t.Fatalf("content=%q", resp.Content)
	}
	if resp, _ = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/notify"}); !strings.Contains(resp.Content, "weixin") {
		t.Fatalf("content=%q", resp.Content)
	}

	// auto resets to most-recent selection.
	if resp, _ = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/notify auto"}); !strings.Contains(resp.Content, "auto") {
		t.Fatalf("content=%q", resp.Content)
	}
	if pref, _ := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform); pref != "" {
		t.Fatalf("auto must clear the stored preference, got %q", pref)
	}
}

// TestPresencePingEndpoint: the idle-TUI heartbeat resolves identity, touches
// presence, and refreshes durable account recency.
func TestPresencePingEndpoint(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/presence/ping?platform=cli&platform_user_id=local", nil)
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !daemon.presenceTracker().IsAttached(identity.PersonID, "cli") {
		t.Fatal("ping must mark the cli endpoint attached")
	}

	// The throttled durable beat landed on the account row.
	accounts, err := store.ListAccountsByPerson(context.Background(), identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].LastSeenAt == 0 {
		t.Fatalf("ping must stamp accounts.last_seen_at, got %+v", accounts)
	}

	// Wrong method is rejected.
	rec = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/presence/ping", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}
}

// TestPresencePingRequiresAuth: the ping endpoint uses the same token gate as
// every other daemon endpoint.
func TestPresencePingRequiresAuth(t *testing.T) {
	daemon, _, _, _, _ := newApprovalTestServer(t)
	t.Setenv("SELF_GATEWAY_TOKEN", "secret")

	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/presence/ping", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/presence/ping", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestPresenceBeatWithActiveZeroDoesNotClaim: a heartbeat or event poll
// stamped active=0 (the client's last user input is older than the presence
// idle timeout) must NOT touch presence — an open-but-vacated TUI detaches by
// TTL so pushes route to the preferred IM again. active=1 and absent keep the
// old behavior, and the endpoints still answer normally either way.
func TestPresenceBeatWithActiveZeroDoesNotClaim(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)

	// Inactive ping: 200 OK, but no presence claim and no durable recency stamp.
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/presence/ping?platform=cli&platform_user_id=local&active=0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("inactive ping status = %d", rec.Code)
	}
	if daemon.presenceTracker().IsAttached(identity.PersonID, "cli") {
		t.Fatal("active=0 ping must not mark the endpoint attached")
	}
	accounts, err := store.ListAccountsByPerson(context.Background(), identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].LastSeenAt != 0 {
		t.Fatalf("active=0 ping must not stamp accounts.last_seen_at: %+v", accounts)
	}

	// Inactive event poll: events still served, presence still unclaimed.
	rec = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks/events?platform=cli&platform_user_id=local&active=0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("inactive event poll status = %d", rec.Code)
	}
	if daemon.presenceTracker().IsAttached(identity.PersonID, "cli") {
		t.Fatal("active=0 event poll must not mark the endpoint attached")
	}

	// active=1 claims again (the person typed → the next beat re-attaches).
	rec = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/presence/ping?platform=cli&platform_user_id=local&active=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("active ping status = %d", rec.Code)
	}
	if !daemon.presenceTracker().IsAttached(identity.PersonID, "cli") {
		t.Fatal("active=1 ping must mark the endpoint attached")
	}
}

// TestPresenceIdleBeatsLetAttachmentExpire simulates the walk-away lifecycle:
// an attached endpoint that keeps heartbeating with active=0 stops refreshing
// its presence, so IsAttached flips false once the TTL lapses — exactly as if
// the beats had stopped entirely.
func TestPresenceIdleBeatsLetAttachmentExpire(t *testing.T) {
	daemon, _, identity, _, _ := newApprovalTestServer(t)
	registry := daemon.presenceTracker()
	now := time.Now()
	registry.now = func() time.Time { return now }

	// Attached via an active beat.
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/presence/ping?platform=cli&platform_user_id=local", nil))
	if rec.Code != http.StatusOK || !registry.IsAttached(identity.PersonID, "cli") {
		t.Fatalf("active beat must attach (status=%d)", rec.Code)
	}

	// The person walks away: only inactive beats arrive while time passes the TTL.
	for i := 0; i < 4; i++ {
		now = now.Add(30 * time.Second)
		rec = httptest.NewRecorder()
		daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/presence/ping?platform=cli&platform_user_id=local&active=0", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("inactive beat %d status = %d", i, rec.Code)
		}
	}
	if registry.IsAttached(identity.PersonID, "cli") {
		t.Fatal("inactive beats past the TTL must read as detached")
	}

	// They come back and type: the next active beat re-attaches instantly.
	rec = httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/presence/ping?platform=cli&platform_user_id=local&active=1", nil))
	if rec.Code != http.StatusOK || !registry.IsAttached(identity.PersonID, "cli") {
		t.Fatal("an active beat after idling must re-attach")
	}
}
