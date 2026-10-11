package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/weixin"
)

// laterClaimOnRelease makes a dispatcher's release commit and then hands the
// receipt to a later claim, which leaves it as change describes, before the
// release reports a failure. RAISE(FAIL) keeps both writes.
func laterClaimOnRelease(change string) string {
	return `CREATE TRIGGER later_claim AFTER UPDATE OF state ON inbound_dedup
		WHEN OLD.state = 'dispatching' AND NEW.state = 'pending' BEGIN
		UPDATE inbound_dedup SET ` + change + ` WHERE platform = NEW.platform AND message_id = NEW.message_id;
		SELECT RAISE(FAIL, 'injected failure after a later claim'); END`
}

// A dispatcher refuses its input, and its release commits just before a
// redelivery claims the receipt again — and, in the second case, has already
// handled it. The release still reports a failure, so the dispatcher rereads
// the receipt and finds a later claim's generation: the receipt no longer
// speaks for this dispatch. The outcome is held as uncertain without a retry
// promise, and nothing of this dispatch is recorded on the later claim.
func TestRefusalWhoseReceiptIsClaimedAgainIsHeld(t *testing.T) {
	cases := []struct {
		name, change string
		want         control.InboundState
	}{
		{name: "claimed again", change: `state = 'dispatching', claim_generation = claim_generation + 1`, want: control.InboundDispatching},
		{name: "claimed and accepted again", change: `state = 'accepted', last_error = '', claim_generation = claim_generation + 1`, want: control.InboundAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			daemon.beginDraining("signal terminated")
			alterStore(t, dir, laterClaimOnRelease(tc.change))
			inbound := weixinInbound("wx-claimed-again", "start the release")
			resp, status, err := daemon.ProcessDurableInbound(ctx, inbound)
			if !errors.Is(err, api.ErrInboundUncertain) || errors.Is(err, api.ErrInboundNotDispatched) || status < http.StatusInternalServerError {
				t.Fatalf("a refusal whose receipt was claimed again was reported as %d %v", status, err)
			}
			if strings.Contains(resp.Error, "retried automatically") {
				t.Fatalf("the sender was promised a retry: %q", resp.Error)
			}
			state, generation, err := store.InboundReceiptClaim(ctx, "weixin", inbound.MessageID)
			if err != nil || state != tc.want || generation != 2 {
				t.Fatalf("receipt = %q claim %d, %v; want %q under the later claim 2", state, generation, err, tc.want)
			}
			if tc.want == control.InboundDispatching {
				owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
				if err != nil {
					t.Fatal(err)
				}
				rows, err := store.ListUncertainInboundForPerson(ctx, owner.TenantID, owner.PersonID, 20)
				if err != nil || len(rows) != 1 || rows[0].LastError != "not dispatched: "+refusalShutdown {
					t.Fatalf("the later claim's record = %+v, %v; want only the committed release's reason", rows, err)
				}
			}
		})
	}
}

// A receipt that a later claim left pending is redeliverable whoever released
// it, and its next claim admits one dispatcher. A refusal that finds it so
// keeps its source, whatever the generation: holding the outcome as uncertain
// would let the source move past a message nothing else will dispatch.
func TestRefusalWhoseReceiptALaterClaimLeftPendingKeepsItsSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
	daemon.beginDraining("signal terminated")
	alterStore(t, dir, laterClaimOnRelease(`claim_generation = claim_generation + 1, last_error = 'not dispatched: a later claim was refused'`))
	inbound := weixinInbound("wx-left-pending", "start the release")
	resp, _, err := daemon.ProcessDurableInbound(ctx, inbound)
	if !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) {
		t.Fatalf("a refusal whose receipt is pending again was reported as %v", err)
	}
	if !strings.Contains(resp.Error, "retried automatically") {
		t.Fatalf("the sender was not told the message will be retried: %q", resp.Error)
	}
	if state, generation, err := store.InboundReceiptClaim(ctx, "weixin", inbound.MessageID); err != nil || state != control.InboundPending || generation != 2 {
		t.Fatalf("receipt = %q claim %d, %v; want pending under the later claim 2", state, generation, err)
	}
}

// fakeILink serves one WeChat message until the adapter's sync cursor moves
// past it, and records the cursor of every poll.
type fakeILink struct {
	*httptest.Server
	mu    sync.Mutex
	polls []string
}

func newFakeILink(t *testing.T, message string) *fakeILink {
	t.Helper()
	fake := &fakeILink{}
	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getupdates") {
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		cursor, _ := payload["get_updates_buf"].(string)
		fake.mu.Lock()
		fake.polls = append(fake.polls, cursor)
		fake.mu.Unlock()
		if cursor != "" {
			_, _ = w.Write([]byte(`{"ret":0,"errcode":0,"msgs":[],"get_updates_buf":"` + cursor + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0,"get_updates_buf":"cursor-after-m1","msgs":[` + message + `]}`))
	}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *fakeILink) cursors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.polls...)
}

// A WeChat message is refused during a shutdown drain, and as its release
// commits a later claim takes the receipt and is refused too, leaving it
// pending with no source behind that claim. The adapter must keep its sync
// cursor before the message, so that after a restart the message is fetched
// again and runs exactly once.
func TestWeixinKeepsAMessageThatALaterClaimLeftPending(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	ilink := newFakeILink(t, `{"msg_id":"m1","from_user_id":"wx-user","to_user_id":"wx-account",`+
		`"item_list":[{"type":1,"text_item":{"text":"start the release"}}]}`)
	home := t.TempDir()
	cfg := weixin.RuntimeConfig{Enabled: true, AccountID: "wx-account", Token: "token", BaseURL: ilink.URL, CDNBaseURL: ilink.URL,
		DMPolicy: "open", GroupPolicy: "disabled", HomeDir: home}
	cursorFile := weixin.SyncBufFilePath(home, "wx-account")

	draining := newInboundTestServer(store, newSlowLLMProvider("unused"))
	draining.beginDraining("signal terminated")
	alterStore(t, dir, laterClaimOnRelease(`claim_generation = claim_generation + 1, last_error = 'not dispatched: a later claim was refused'`))
	refusing := weixin.NewAdapter(cfg, draining.ProcessDurableInbound)
	if err := refusing.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// The poll after m1 shows what the adapter did with its cursor.
	waitUntil(t, 10*time.Second, func() bool { return len(ilink.cursors()) >= 2 }, "the adapter never polled again after m1")
	refusing.Stop()
	if polls := ilink.cursors(); polls[1] != "" {
		t.Fatalf("the adapter moved past m1 after its refusal: polls %q", polls)
	}
	if _, err := os.Stat(cursorFile); !os.IsNotExist(err) {
		t.Fatalf("the sync cursor was saved past m1: %v", err)
	}

	alterStore(t, dir, `DROP TRIGGER later_claim`)
	provider := newSlowLLMProvider("release started")
	provider.releaseNow()
	restarted := newInboundTestServer(store, provider)
	var calls atomic.Int32
	adapter := weixin.NewAdapter(cfg, func(ctx context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
		calls.Add(1)
		return restarted.ProcessDurableInbound(ctx, inbound)
	})
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		saved, err := os.ReadFile(cursorFile)
		return err == nil && strings.TrimSpace(string(saved)) == "cursor-after-m1"
	}, "the restarted adapter never moved past m1")
	adapter.Stop()
	if calls.Load() != 1 {
		t.Fatalf("m1 reached the gateway %d times after the restart, want 1", calls.Load())
	}
	if state, _, err := store.InboundReceiptClaim(ctx, "weixin", "m1"); err != nil || state != control.InboundAccepted {
		t.Fatalf("m1 receipt = %q, %v; want accepted", state, err)
	}
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-user", "")
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		runs, err := store.ListRecentRunsForPerson(ctx, owner.TenantID, owner.PersonID, 10)
		return err == nil && len(runs) == 1 && runs[0].FinishedAt != nil
	}, "m1 did not run exactly once")
}

// While a turn runs, its receipt is claimed again by something that does not
// hold the dispatcher's generation — and, in the second case, settled by it.
// The turn's acceptance names its own claim, so it cannot settle the later
// one, and an acceptance found on the reread is not this dispatch's: the
// outcome is held as uncertain and the receipt stays with the later claim.
func TestAcceptanceAfterTheReceiptWasClaimedAgainIsHeld(t *testing.T) {
	cases := []struct {
		name, change string
		want         control.InboundState
	}{
		{name: "claimed again", change: `claim_generation = claim_generation + 1`, want: control.InboundDispatching},
		{name: "claimed and accepted again", change: `claim_generation = claim_generation + 1, state = 'accepted'`, want: control.InboundAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			provider := newSlowLLMProvider("release started")
			daemon := newInboundTestServer(store, provider)
			inbound := weixinInbound("wx-taken-over", "start the release")
			inbound.Request.Async = false
			type outcome struct {
				resp api.MessageResponse
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				resp, _, err := daemon.ProcessDurableInbound(ctx, inbound)
				done <- outcome{resp: resp, err: err}
			}()
			select {
			case <-provider.started:
			case <-time.After(5 * time.Second):
				t.Fatal("the turn never reached its model call")
			}
			// A release and a redelivery handled elsewhere would leave exactly this.
			alterStore(t, dir, `PRAGMA busy_timeout = 5000;
				UPDATE inbound_dedup SET `+tc.change+` WHERE platform = 'weixin' AND message_id = 'wx-taken-over'`)
			provider.releaseNow()
			got := <-done
			if !errors.Is(got.err, api.ErrInboundUncertain) || errors.Is(got.err, api.ErrInboundNotDispatched) {
				t.Fatalf("a handled input whose receipt was claimed again was reported as %v: %+v", got.err, got.resp)
			}
			state, generation, err := store.InboundReceiptClaim(ctx, "weixin", inbound.MessageID)
			if err != nil || state != tc.want || generation != 2 {
				t.Fatalf("receipt = %q claim %d, %v; want %q under the later claim 2", state, generation, err, tc.want)
			}
		})
	}
}

// A delivery's claim fails and the reread finds the receipt held by another
// claim. When that holder has recorded why it holds the receipt — here an
// answer whose choice was used but whose request was not saved — the
// delivery's reason must not replace it; when the holder has recorded none,
// the delivery's reason fills the gap.
func TestFailedClaimKeepsTheHolderReason(t *testing.T) {
	const held = "choice answered, but its request was not saved (" + refusalSave + ")"
	cases := []struct{ name, holder, want string }{
		{name: "holder explained", holder: held, want: held},
		{name: "holder silent", holder: "", want: "claim outcome unknown; this delivery did not dispatch it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
			if err != nil {
				t.Fatal(err)
			}
			// The delivery's claim reports a failure while another claim holds
			// the receipt with the holder's reason, if any.
			alterStore(t, dir, `CREATE TRIGGER held_elsewhere AFTER UPDATE OF state ON inbound_dedup
				WHEN OLD.state = 'pending' AND NEW.state = 'dispatching' BEGIN
				UPDATE inbound_dedup SET claim_generation = claim_generation + 1, last_error = '`+tc.holder+`'
					WHERE platform = NEW.platform AND message_id = NEW.message_id;
				SELECT RAISE(FAIL, 'injected failure: another claim holds the receipt'); END`)
			resp, _, err := daemon.ProcessDurableInbound(ctx, weixinInbound("wx-held", "2"))
			if !errors.Is(err, api.ErrInboundUncertain) || errors.Is(err, api.ErrInboundNotDispatched) {
				t.Fatalf("a claim that found the receipt held elsewhere was reported as %v: %+v", err, resp)
			}
			rows, err := store.ListUncertainInboundForPerson(ctx, owner.TenantID, owner.PersonID, 20)
			if err != nil || len(rows) != 1 || rows[0].State != control.InboundDispatching || rows[0].ClaimGeneration != 2 || rows[0].LastError != tc.want {
				t.Fatalf("held receipt = %+v, %v; want dispatching under claim 2 with reason %q", rows, err, tc.want)
			}
		})
	}
}
