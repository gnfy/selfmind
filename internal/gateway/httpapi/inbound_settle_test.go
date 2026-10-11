package httpapi

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

// A synchronous turn may run far longer than one receipt transition is
// allowed to take. The bound applies to each transition, never to the turn,
// so the receipt still settles and the reply still reaches the sender.
func TestReceiptSettlesAfterTurnLongerThanSettleBound(t *testing.T) {
	for _, path := range []string{"durable", "webhook"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			provider := newSlowLLMProvider("release finished")
			daemon := newInboundTestServer(store, provider)
			daemon.inboundSettleTimeout = 20 * time.Millisecond
			type outcome struct {
				code    int
				content string
				err     error
			}
			done := make(chan outcome, 1)
			platform, messageID := "weixin", "wx-long"
			go func() {
				if path == "durable" {
					inbound := weixinInbound(messageID, "run the long release")
					inbound.Request.Async = false
					resp, code, err := daemon.ProcessDurableInbound(ctx, inbound)
					done <- outcome{code: code, content: resp.Content, err: err}
					return
				}
				body := `{"message_id":"in-long","user_id":"im-owner","chat_id":"im-chat","content":"run the long release"}`
				rec := httptest.NewRecorder()
				daemon.handleIMWebhook(rec, httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(body)))
				done <- outcome{code: rec.Code, content: rec.Body.String()}
			}()
			if path == "webhook" {
				platform, messageID = "webhook", "in-long"
			}
			select {
			case <-provider.started:
			case <-time.After(5 * time.Second):
				t.Fatal("the turn never reached its model call")
			}
			time.Sleep(10 * daemon.inboundSettleTimeout)
			provider.releaseNow()
			got := <-done
			if got.err != nil || got.code != http.StatusOK || !strings.Contains(got.content, "release finished") {
				t.Fatalf("long turn did not settle: %+v", got)
			}
			if state, _, err := store.InboundReceipt(ctx, platform, messageID); err != nil || state != control.InboundAccepted {
				t.Fatalf("long turn receipt = %q, %v; want accepted", state, err)
			}
		})
	}
}

// Answering a resume-candidates choice claims it and clears its saved request.
// If the answer then cannot be queued, a redelivered answer would only find
// the choice used, so the receipt is held with a precise reason instead of
// promising a retry, and the sender is told. A bare number on the choice's
// endpoint behaves the same, and so does a steer answer whose Run ended
// before it could be saved as follow-up work.
func TestRefusalAfterChoiceClaimIsHeldWithReason(t *testing.T) {
	for _, answer := range []string{"/choose", "bare", "ended steer"} {
		t.Run(answer, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
			if err != nil {
				t.Fatal(err)
			}
			task, run := seedContinuityHistory(t, store, owner, "waiting_user")
			first := control.TurnChoiceOption{Key: "1", Label: "resume", Action: "resume", TaskID: task.ID, RunID: run.ID}
			if answer == "ended steer" {
				first.Action = "steer"
			}
			choice, err := daemon.createTurnChoice(ctx, owner, api.MessageRequest{Channel: "wx-owner", Content: "ship the hotfix"},
				[]control.TurnChoiceOption{first, {Key: "2", Label: "new", Action: "new"}})
			if err != nil {
				t.Fatal(err)
			}
			// A live Run takes the only slot, so the chosen new work must queue.
			occupyWithBackPressuredRun(t, daemon, store, owner)
			alterStore(t, dir, `CREATE TRIGGER reject_queue BEFORE INSERT ON task_queue BEGIN SELECT RAISE(ABORT, 'injected queue failure'); END`)
			content := "/choose " + choice.ID + " 2"
			switch answer {
			case "bare":
				content = "2"
			case "ended steer":
				content = "/choose " + choice.ID + " 1"
			}
			inbound := weixinInbound("wx-answer", content)
			resp, _, err := daemon.ProcessDurableInbound(ctx, inbound)
			if !errors.Is(err, api.ErrInboundHeld) || !errors.Is(err, api.ErrInboundUncertain) || errors.Is(err, api.ErrInboundNotDispatched) {
				t.Fatalf("a refusal after the claim was not held for the sender: %+v, %v", resp, err)
			}
			if !strings.Contains(resp.Error, "Your answer was recorded") {
				t.Fatalf("held answer text = %q", resp.Error)
			}
			if strings.Contains(resp.Error, "injected") || strings.Contains(resp.Error, "retried automatically") {
				t.Fatalf("held answer text = %q", resp.Error)
			}
			if state, _, err := store.InboundReceipt(ctx, "weixin", inbound.MessageID); err != nil || state != control.InboundDispatching {
				t.Fatalf("held answer receipt = %q, %v; want dispatching", state, err)
			}
			diag, err := daemon.inboundDiagReply(ctx, owner)
			if err != nil || !strings.Contains(diag, "choice") || strings.Contains(diag, "injected") {
				t.Fatalf("diagnostic does not explain the held answer: %q, %v", diag, err)
			}
		})
	}
}

// A steer answer whose Run ended is saved as follow-up work. When that save
// fails, a caller without a durable receipt sees a short cause, never the
// store's error.
func TestEndedSteerAnswerFailureIsSanitized(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	task, run := seedContinuityHistory(t, store, owner, "waiting_user")
	choice, err := daemon.createTurnChoice(ctx, owner, api.MessageRequest{Channel: "wx-owner", Content: "add the missing check"},
		[]control.TurnChoiceOption{
			{Key: "1", Label: "steer", Action: "steer", TaskID: task.ID, RunID: run.ID},
			{Key: "2", Label: "new", Action: "new"},
		})
	if err != nil {
		t.Fatal(err)
	}
	alterStore(t, dir, `CREATE TRIGGER reject_queue BEFORE INSERT ON task_queue BEGIN SELECT RAISE(ABORT, 'injected queue failure'); END`)
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{TenantID: "default", Platform: "weixin",
		PlatformUserID: "wx-owner", Channel: "wx-owner", Content: "/choose " + choice.ID + " 1"})
	if status < http.StatusInternalServerError || resp.Error == "" || strings.Contains(resp.Error, "injected") || strings.Contains(resp.Error, "constraint") {
		t.Fatalf("failed follow-up save: %d %q", status, resp.Error)
	}
}

// The follow-up row for a steer answer whose Run ended must not see the
// caller's deadline. The SQLite driver reports an ended context even after
// the row committed, which would tell the sender the request was not saved
// while it sits in the queue, and a resend would run it twice. The answer
// arrives through a durable receipt or a plain message request.
func TestEndedSteerFollowUpSurvivesCallerDeadline(t *testing.T) {
	for _, path := range []string{"durable", "message"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
			if err != nil {
				t.Fatal(err)
			}
			task, run := seedContinuityHistory(t, store, owner, "waiting_user")
			choice, err := daemon.createTurnChoice(ctx, owner, api.MessageRequest{Channel: "wx-owner", Content: "add the missing check"},
				[]control.TurnChoiceOption{
					{Key: "1", Label: "steer", Action: "steer", TaskID: task.ID, RunID: run.ID},
					{Key: "2", Label: "new", Action: "new"},
				})
			if err != nil {
				t.Fatal(err)
			}
			// A live Run holds the only slot, so the follow-up stays queued.
			daemon.coordinator().activeLimit = 1
			occupyWithBackPressuredRun(t, daemon, store, owner)
			alterStore(t, dir, `CREATE TRIGGER slow_queue AFTER INSERT ON task_queue BEGIN SELECT selfmind_test_pause(400); END`)
			callerCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
			defer cancel()
			content := "/choose " + choice.ID + " 1"
			var resp api.MessageResponse
			if path == "durable" {
				resp, _, err = daemon.ProcessDurableInbound(callerCtx, weixinInbound("wx-follow-up", content))
			} else {
				resp, _ = daemon.ProcessMessage(callerCtx, api.MessageRequest{TenantID: "default", Platform: "weixin",
					PlatformUserID: "wx-owner", Channel: "wx-owner", Content: content})
			}
			queued, listErr := store.ListQueued(ctx, owner.TenantID, owner.PersonID, control.QueueStatusQueued)
			if listErr != nil || len(queued) != 1 {
				t.Fatalf("follow-up rows = %+v, %v; want exactly one", queued, listErr)
			}
			if err != nil || !resp.Accepted || !strings.Contains(resp.Content, "follow-up work") {
				t.Fatalf("a queued follow-up was reported as %q / %q, %v", resp.Content, resp.Error, err)
			}
			if path == "durable" {
				if state, _, err := store.InboundReceipt(ctx, "weixin", "wx-follow-up"); err != nil || state != control.InboundAccepted {
					t.Fatalf("follow-up receipt = %q, %v; want accepted", state, err)
				}
			}
		})
	}
}

// A message that was handled but whose acceptance cannot be saved stays
// dispatching, and /diag inbound says why, whether it came through a durable
// adapter or the webhook.
func TestFailedAcceptanceRecordsItsReason(t *testing.T) {
	for _, path := range []string{"durable", "webhook"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			platform, user, messageID := "weixin", "wx-owner", "wx-unaccepted"
			if path == "webhook" {
				platform, user, messageID = "webhook", "im-owner", "in-unaccepted"
			}
			owner, err := store.ResolveOrCreateAccount(ctx, "default", platform, user, "")
			if err != nil {
				t.Fatal(err)
			}
			alterStore(t, dir, `CREATE TRIGGER refuse_acceptance BEFORE UPDATE OF state ON inbound_dedup
				WHEN NEW.state = 'accepted' BEGIN SELECT RAISE(ABORT, 'injected acceptance failure'); END`)
			if path == "durable" {
				if _, _, err := daemon.ProcessDurableInbound(ctx, weixinInbound(messageID, "/status")); !errors.Is(err, api.ErrInboundUncertain) {
					t.Fatalf("an unsaved acceptance was reported as %v", err)
				}
			} else {
				rec := httptest.NewRecorder()
				daemon.handleIMWebhook(rec, httptest.NewRequest("POST", "/v1/im/webhook",
					strings.NewReader(`{"message_id":"in-unaccepted","user_id":"im-owner","chat_id":"im-chat","content":"/status"}`)))
				if rec.Code < http.StatusInternalServerError {
					t.Fatalf("an unsaved acceptance answered %d", rec.Code)
				}
			}
			if state, _, err := store.InboundReceipt(ctx, platform, messageID); err != nil || state != control.InboundDispatching {
				t.Fatalf("receipt = %q, %v; want dispatching", state, err)
			}
			diag, err := daemon.inboundDiagReply(ctx, owner)
			if err != nil || !strings.Contains(diag, "acceptance") || strings.Contains(diag, "injected") {
				t.Fatalf("diagnostic does not explain the unsaved acceptance: %q, %v", diag, err)
			}
		})
	}
}

// Guidance for a back-pressured Run is saved to the mailbox before the queue
// fallback. If that row cannot be expired it still becomes follow-up work, so
// it is the input's admission: the answer must not also be refused, and no
// second copy may be queued.
func TestSteeringExpiryFailureKeepsTheMailboxAdmission(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	target := occupyWithBackPressuredRun(t, daemon, store, owner)
	alterStore(t, dir, `CREATE TRIGGER keep_steering BEFORE UPDATE OF status ON steering_mailbox
		WHEN NEW.status = 'expired' BEGIN SELECT RAISE(ABORT, 'injected expiry failure'); END`)
	alterStore(t, dir, `CREATE TRIGGER reject_queue BEFORE INSERT ON task_queue BEGIN SELECT RAISE(ABORT, 'injected queue failure'); END`)
	inbound := weixinInbound("wx-guidance", "add the missing check")
	inbound.Request.ReplyToRunID = target.ID
	resp, _, err := daemon.ProcessDurableInbound(ctx, inbound)
	if err != nil || !resp.Accepted || strings.Contains(resp.Error, "injected") {
		t.Fatalf("guidance held in the mailbox was not acknowledged: %+v, %v", resp, err)
	}
	live, err := store.ListUnconsumedSteering(ctx, owner.TenantID, "", 50)
	if err != nil || len(live) != 1 || live[0].Content != "add the missing check" {
		t.Fatalf("mailbox admission = %+v, %v; want the one guidance row", live, err)
	}
	if queued, err := store.ListQueued(ctx, owner.TenantID, owner.PersonID, control.QueueStatusQueued); err != nil || len(queued) != 0 {
		t.Fatalf("a second copy was queued: %+v, %v", queued, err)
	}
	if state, _, err := store.InboundReceipt(ctx, "weixin", "wx-guidance"); err != nil || state != control.InboundAccepted {
		t.Fatalf("guidance receipt = %q, %v; want accepted", state, err)
	}
}

// Between saving guidance and expiring it after back-pressure, the Run's
// finalizer may already have deferred the row to the queue, or a consumption
// proof may have taken it. The expiry then changes nothing, and the fallback
// must not queue a second copy: the row already admitted the input.
func TestSteeringHandledBeforeExpiryIsNotQueuedAgain(t *testing.T) {
	for _, status := range []string{control.SteeringDeferred, control.SteeringConsumed} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
			if err != nil {
				t.Fatal(err)
			}
			target := occupyWithBackPressuredRun(t, daemon, store, owner)
			alterStore(t, dir, `CREATE TRIGGER handled_first AFTER INSERT ON steering_mailbox BEGIN
				UPDATE steering_mailbox SET status = '`+status+`' WHERE id = NEW.id; END`)
			inbound := weixinInbound("wx-guidance", "add the missing check")
			inbound.Request.ReplyToRunID = target.ID
			resp, _, err := daemon.ProcessDurableInbound(ctx, inbound)
			if err != nil || !resp.Accepted {
				t.Fatalf("guidance taken by the finalizer was not acknowledged: %+v, %v", resp, err)
			}
			if queued, err := store.ListQueued(ctx, owner.TenantID, owner.PersonID, control.QueueStatusQueued); err != nil || len(queued) != 0 {
				t.Fatalf("the fallback queued a second copy: %+v, %v", queued, err)
			}
			if state, _, err := store.InboundReceipt(ctx, "weixin", "wx-guidance"); err != nil || state != control.InboundAccepted {
				t.Fatalf("guidance receipt = %q, %v; want accepted", state, err)
			}
		})
	}
}

// The same rule holds wherever the caller's context ends, for every write
// that can admit the input: a mailbox row, a queue row, or a choice row. A
// typed refusal is returned only when none of them was left behind, since
// the redelivery it invites would admit the input a second time.
func TestAdmissionWritesSurviveCallerCancellationAtEveryPoint(t *testing.T) {
	cases := []struct {
		name  string
		runs  int
		setup string
	}{
		// Only the mailbox can hold the guidance: the queue rejects it.
		{name: "guidance", runs: 1, setup: `CREATE TRIGGER reject_queue BEFORE INSERT ON task_queue BEGIN SELECT RAISE(ABORT, 'injected queue failure'); END`},
		// Back-pressure expires the mailbox row and the queue row admits it.
		{name: "queue", runs: 1},
		// Two live Runs park the input as a choice row.
		{name: "choice", runs: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for point := 0; ; point++ {
				if point > 500 {
					t.Fatal("the cancellation probe never outlived the request")
				}
				ctx := context.Background()
				dir := t.TempDir()
				store := controltest.NewStoreInDir(t, dir)
				daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
				owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
				if err != nil {
					t.Fatal(err)
				}
				daemon.coordinator().activeLimit = tc.runs
				targetRunID := ""
				for i := 0; i < tc.runs; i++ {
					targetRunID = occupyWithBackPressuredRun(t, daemon, store, owner).ID
				}
				if tc.setup != "" {
					alterStore(t, dir, tc.setup)
				}
				probe := newCountdownContext(point)
				inbound := weixinInbound("wx-probe", "add the missing check")
				if tc.runs == 1 {
					inbound.Request.ReplyToRunID = targetRunID
				}
				_, _, err = daemon.ProcessDurableInbound(probe, inbound)
				if errors.Is(err, api.ErrInboundNotDispatched) {
					if left := admissionsLeft(t, store, owner); left != "" {
						t.Fatalf("cancellation at point %d refused the input while it stays admitted: %s", point, left)
					}
				}
				if !probe.fired() {
					t.Logf("probed %d cancellation points", point)
					return
				}
			}
		})
	}
}

// admissionsLeft describes whatever still admits the probe input.
func admissionsLeft(t *testing.T, store *control.Store, owner *control.IdentityContext) string {
	t.Helper()
	ctx := context.Background()
	var left []string
	live, err := store.ListUnconsumedSteering(ctx, owner.TenantID, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range live {
		left = append(left, "live guidance "+row.ID)
	}
	queued, err := store.ListQueued(ctx, owner.TenantID, owner.PersonID, control.QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range queued {
		left = append(left, "queued "+row.ID)
	}
	choice, err := store.LatestPendingTurnChoiceForChannel(ctx, owner.TenantID, owner.PersonID, "wx-owner", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if choice != nil {
		left = append(left, "pending choice "+choice.ID)
	}
	return strings.Join(left, ", ")
}

// A redelivered message may differ in transport fields such as a refreshed
// session token or a retry counter. The receipt's state decides first: an
// accepted receipt is a silent duplicate and a dispatching one stays
// uncertain whatever changed. Only a never-started receipt compares the
// message's essential fields, and only a change there is a different message.
func TestRedeliveryStateWinsOverTransportChanges(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	provider := newSlowLLMProvider("release started")
	provider.releaseNow()
	daemon := newInboundTestServer(store, provider)
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(messageID string, accept bool) api.DurableInbound {
		t.Helper()
		inbound := weixinInbound(messageID, "start the release")
		inbound.RawPayload = []byte(`{"msg_id":"` + messageID + `","text":"start the release","context_token":"first"}`)
		if _, err := store.BeginInbound(ctx, "weixin", messageID, inbound.RawPayload,
			control.InboundOwner{TenantID: owner.TenantID, PersonID: owner.PersonID}); err != nil {
			t.Fatal(err)
		}
		claim, err := store.ClaimInbound(ctx, "weixin", messageID)
		if err != nil || claim == 0 {
			t.Fatalf("claim %s: %d %v", messageID, claim, err)
		}
		if accept {
			if err := store.AcceptInbound(ctx, "weixin", messageID, claim); err != nil {
				t.Fatal(err)
			}
		}
		inbound.RawPayload = []byte(`{"msg_id":"` + messageID + `","text":"start the release","context_token":"refreshed"}`)
		return inbound
	}
	if resp, code, err := daemon.ProcessDurableInbound(ctx, seed("wx-accepted", true)); err != nil || code != http.StatusOK || resp.Content != "" || resp.Error != "" {
		t.Fatalf("redelivery of an accepted receipt was not a silent duplicate: %d %+v %v", code, resp, err)
	}
	if resp, _, err := daemon.ProcessDurableInbound(ctx, seed("wx-dispatching", false)); !errors.Is(err, api.ErrInboundUncertain) || resp.Error != "" {
		t.Fatalf("redelivery of a dispatching receipt was not held as uncertain: %+v %v", resp, err)
	}

	// A never-started receipt matches its redelivery on the fields the
	// adapter says identify the message.
	essential := func(raw []byte) ([]byte, error) {
		var fields map[string]interface{}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		delete(fields, "context_token")
		return json.Marshal(fields)
	}
	never := func(messageID, text string) api.DurableInbound {
		t.Helper()
		inbound := weixinInbound(messageID, "start the release")
		inbound.Request.Async = false
		if _, err := store.BeginInbound(ctx, "weixin", messageID, []byte(`{"msg_id":"`+messageID+`","text":"start the release","context_token":"first"}`),
			control.InboundOwner{TenantID: owner.TenantID, PersonID: owner.PersonID}); err != nil {
			t.Fatal(err)
		}
		inbound.RawPayload = []byte(`{"msg_id":"` + messageID + `","text":"` + text + `","context_token":"refreshed"}`)
		inbound.Essential = essential
		return inbound
	}
	if resp, _, err := daemon.ProcessDurableInbound(ctx, never("wx-pending", "start the release")); err != nil || resp.Content != "release started" {
		t.Fatalf("redelivery with a refreshed token was not dispatched: %+v %v", resp, err)
	}
	if resp, _, err := daemon.ProcessDurableInbound(ctx, never("wx-reused", "stop the release")); !errors.Is(err, api.ErrInboundRejected) {
		t.Fatalf("redelivery with different text was not rejected: %+v %v", resp, err)
	}

	// Webhook bodies: a retry counter is a transport field, the text is not.
	deliver := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		daemon.handleIMWebhook(rec, httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(body)))
		return rec
	}
	daemon.beginDraining("signal terminated")
	if refused := deliver(`{"message_id":"in-retry","user_id":"im-owner","chat_id":"im-chat","content":"start the release","attempt":1}`); refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("seed refusal = %d %s", refused.Code, refused.Body.String())
	}
	restarted := newInboundTestServer(store, provider)
	daemon = restarted
	if retried := deliver(`{"message_id":"in-retry","user_id":"im-owner","chat_id":"im-chat","content":"start the release","attempt":2}`); retried.Code != http.StatusOK || !strings.Contains(retried.Body.String(), "release started") {
		t.Fatalf("redelivery with a new retry counter was not dispatched: %d %s", retried.Code, retried.Body.String())
	}
	if changed := deliver(`{"message_id":"in-retry","user_id":"im-owner","chat_id":"im-chat","content":"stop the release","attempt":3}`); changed.Code != http.StatusOK || !strings.Contains(changed.Body.String(), "duplicate") {
		t.Fatalf("a changed copy of an accepted message = %d %s; want duplicate", changed.Code, changed.Body.String())
	}
	daemon.beginDraining("signal terminated")
	if refused := deliver(`{"message_id":"in-text","user_id":"im-owner","chat_id":"im-chat","content":"start the release"}`); refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("seed refusal = %d %s", refused.Code, refused.Body.String())
	}
	daemon = newInboundTestServer(store, provider)
	if changed := deliver(`{"message_id":"in-text","user_id":"im-owner","chat_id":"im-chat","content":"stop the release"}`); changed.Code != http.StatusConflict {
		t.Fatalf("a never-started receipt redelivered with different text = %d %s; want 409", changed.Code, changed.Body.String())
	}
}

// When the person's Run slot is taken between the gateway's check and the
// Run's start, the input falls back to the queue. If that write fails too the
// receipt is held, and neither the chat nor the webhook body carries the
// internal error.
func TestCapacityRaceQueueFailureIsSanitized(t *testing.T) {
	for _, path := range []string{"durable", "webhook"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			provider := newSlowLLMProvider("unused")
			provider.releaseNow()
			daemon := newInboundTestServer(store, provider)
			platform, user := "weixin", "wx-owner"
			if path == "webhook" {
				platform, user = "webhook", "im-owner"
			}
			owner, err := store.ResolveOrCreateAccount(ctx, "default", platform, user, "")
			if err != nil {
				t.Fatal(err)
			}
			// Freeze independent known roots so resource admission can proceed.
			// The running Run the registry does not know still holds the store's
			// slot, so durable Run insertion loses the capacity race after work
			// preparation. An unknown root would now refuse before dispatch.
			workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: owner.TenantID,
				OwnerPersonID: owner.PersonID, Name: "incoming", LocalPath: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetCurrentWorkspace(ctx, owner.TenantID, owner.PersonID, workspace.ID); err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "elsewhere", Channel: "other"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.StartRunWithOptions(ctx, task, "other", "elsewhere", control.StartRunOptions{
				ExecutionRoots: []executionenv.RootBinding{{Path: t.TempDir(), AccessCap: executionenv.RootAccessWrite}}}); err != nil {
				t.Fatal(err)
			}
			alterStore(t, dir, `CREATE TRIGGER reject_queue BEFORE INSERT ON task_queue BEGIN SELECT RAISE(ABORT, 'injected queue failure'); END`)
			var body string
			var code int
			messageID := "wx-race"
			if path == "durable" {
				inbound := weixinInbound(messageID, "start the release")
				inbound.Request.Async = false
				resp, status, _ := daemon.ProcessDurableInbound(ctx, inbound)
				body, code = resp.Error+resp.Content, status
			} else {
				messageID = "in-race"
				rec := httptest.NewRecorder()
				daemon.handleIMWebhook(rec, httptest.NewRequest("POST", "/v1/im/webhook",
					strings.NewReader(`{"message_id":"in-race","user_id":"im-owner","chat_id":"im-chat","content":"start the release"}`)))
				body, code = rec.Body.String(), rec.Code
			}
			if code < http.StatusInternalServerError || strings.Contains(body, "injected") {
				t.Fatalf("capacity race with a failed queue write: %d %q", code, body)
			}
			if state, _, err := store.InboundReceipt(ctx, platform, messageID); err != nil || state != control.InboundDispatching {
				t.Fatalf("capacity race receipt = %q, %v; want dispatching", state, err)
			}
		})
	}
}

// closableStores names stores a trigger may close through selfmind_test_close.
var closableStores sync.Map

// selfmind_test_pause holds a statement open without giving SQLite a point at
// which to honor an interrupt, so a write can outlive its context's deadline
// and still commit — as a slow disk can make it do. selfmind_test_close closes
// a named store from inside a statement, so the write in progress still
// commits while every later read fails.
func init() {
	sqlite.MustRegisterScalarFunction("selfmind_test_pause", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if ms, ok := args[0].(int64); ok {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		return nil, nil
	})
	sqlite.MustRegisterScalarFunction("selfmind_test_close", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if name, ok := args[0].(string); ok {
			if store, ok := closableStores.LoadAndDelete(name); ok {
				_ = store.(*control.Store).Close()
			}
		}
		return nil, nil
	})
}

// When a transition fails and its receipt cannot be read, the gateway does
// not know whether the transition happened, so it promises nothing — neither
// a retry nor an uncertain hold. The source keeps the message, and its
// redelivery reads the receipt again.
func TestUnreadableReceiptAfterFailedTransitionPromisesNothing(t *testing.T) {
	cases := []struct {
		name, when string
		drain      bool
		want       control.InboundState
	}{
		{name: "claim", when: `OLD.state = 'pending' AND NEW.state = 'dispatching'`, want: control.InboundDispatching},
		{name: "release", when: `OLD.state = 'dispatching' AND NEW.state = 'pending'`, drain: true, want: control.InboundPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			if tc.drain {
				daemon.beginDraining("signal terminated")
			}
			key := "unreadable-" + tc.name
			closableStores.Store(key, store)
			t.Cleanup(func() { closableStores.Delete(key) })
			// RAISE(FAIL) keeps the row change, so the transition commits and
			// still reports a failure; the store is gone for the reread.
			alterStore(t, dir, `CREATE TRIGGER lose_store AFTER UPDATE OF state ON inbound_dedup WHEN `+tc.when+`
				BEGIN SELECT selfmind_test_close('`+key+`'); SELECT RAISE(FAIL, 'injected failure after the write'); END`)
			_, status, err := daemon.ProcessDurableInbound(ctx, weixinInbound("wx-lost-"+tc.name, "start the release"))
			if err == nil || status < http.StatusInternalServerError ||
				errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) {
				t.Fatalf("a failed %s with an unreadable receipt was reported as %d %v", tc.name, status, err)
			}
			reopened, err := control.OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if state, _, err := reopened.InboundReceipt(ctx, "weixin", "wx-lost-"+tc.name); err != nil || state != tc.want {
				t.Fatalf("receipt = %q, %v; want %q — the failed %s did not commit", state, err, tc.want, tc.name)
			}
		})
	}
}

// A receipt transition may run far longer than its bound — a slow disk, a
// long trigger. The bound covers only the wait for the store; the transition
// itself runs to completion and reports what it did, because the driver
// would report the deadline for a write that happened. A slow claim therefore
// dispatches its input, never leaving a claimed receipt that nothing will
// run; a slow release is still a refusal the source may redeliver; and a slow
// acceptance still delivers the reply.
func TestSlowReceiptTransitionCompletesPastItsBound(t *testing.T) {
	cases := []struct {
		name, when, content string
		drain               bool
		want                control.InboundState
	}{
		{name: "claim", when: `OLD.state = 'pending' AND NEW.state = 'dispatching'`, content: "start the release", want: control.InboundAccepted},
		{name: "release", when: `OLD.state = 'dispatching' AND NEW.state = 'pending'`, content: "start the release", drain: true, want: control.InboundPending},
		{name: "accept", when: `NEW.state = 'accepted'`, content: "/status", want: control.InboundAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			provider := newSlowLLMProvider("release started")
			provider.releaseNow()
			daemon := newInboundTestServer(store, provider)
			daemon.inboundSettleTimeout = 100 * time.Millisecond
			if tc.drain {
				daemon.beginDraining("signal terminated")
			}
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
			if err != nil {
				t.Fatal(err)
			}
			alterStore(t, dir, `CREATE TRIGGER slow_transition AFTER UPDATE OF state ON inbound_dedup
				WHEN `+tc.when+` BEGIN SELECT selfmind_test_pause(400); END`)
			inbound := weixinInbound("wx-slow-"+tc.name, tc.content)
			inbound.Request.Async = false
			resp, _, err := daemon.ProcessDurableInbound(ctx, inbound)
			state, _, readErr := store.InboundReceipt(ctx, "weixin", inbound.MessageID)
			if readErr != nil || state != tc.want {
				t.Fatalf("receipt = %q, %v; want %q after the slow %s", state, readErr, tc.want, tc.name)
			}
			switch tc.name {
			case "claim":
				if err != nil || strings.TrimSpace(resp.Content) != "release started" {
					t.Fatalf("a slow claim did not dispatch its input: %+v, %v", resp, err)
				}
				if runs, err := store.ListRecentRunsForPerson(ctx, owner.TenantID, owner.PersonID, 10); err != nil || len(runs) != 1 {
					t.Fatalf("runs after a slow claim = %+v, %v; want exactly one", runs, err)
				}
			case "release":
				if !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) {
					t.Fatalf("a slow release was reported as %v: %+v", err, resp)
				}
			case "accept":
				if err != nil || resp.Content == "" {
					t.Fatalf("a slow acceptance lost its reply: %+v, %v", resp, err)
				}
			}
		})
	}
}

// A failed claim whose receipt reads dispatching cannot be attributed: the
// claim may have committed before failing, or another dispatcher holds it.
// Nothing runs, the receipt stays held, and /diag inbound says why.
func TestAmbiguousClaimIsHeldWithRecordedReason(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	// RAISE(FAIL) keeps the row change made before the trigger, so the claim
	// commits and still reports an error.
	alterStore(t, dir, `CREATE TRIGGER ambiguous_claim AFTER UPDATE OF state ON inbound_dedup
		WHEN NEW.state = 'dispatching' BEGIN SELECT RAISE(FAIL, 'injected ambiguous claim'); END`)
	resp, _, err := daemon.ProcessDurableInbound(ctx, weixinInbound("wx-ambiguous", "rotate the keys"))
	if !errors.Is(err, api.ErrInboundUncertain) || errors.Is(err, api.ErrInboundNotDispatched) {
		t.Fatalf("ambiguous claim outcome: %+v, %v", resp, err)
	}
	if state, _, err := store.InboundReceipt(ctx, "weixin", "wx-ambiguous"); err != nil || state != control.InboundDispatching {
		t.Fatalf("ambiguous claim receipt = %q, %v; want dispatching", state, err)
	}
	if runs, err := store.ListRecentRunsForPerson(ctx, owner.TenantID, owner.PersonID, 10); err != nil || len(runs) != 0 {
		t.Fatalf("an unattributed claim dispatched work: %+v, %v", runs, err)
	}
	diag, err := daemon.inboundDiagReply(ctx, owner)
	if err != nil || !strings.Contains(diag, "dispatching") || !strings.Contains(diag, "claim") {
		t.Fatalf("diagnostic gives no reason for the held receipt: %q, %v", diag, err)
	}
}

func TestChoiceClaimPreparationFailureDuringModelDrainStaysHeld(t *testing.T) {
	for _, tc := range []struct {
		name           string
		claimed, drain bool
	}{
		{"fresh model drain", false, true},
		{"claimed normal admission", true, false},
		{"claimed model drain", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			t.Cleanup(func() { _ = store.Close() })
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
			if err != nil {
				t.Fatal(err)
			}
			content := "ship the hotfix"
			var choiceID string
			if tc.claimed {
				task, run := seedContinuityHistory(t, store, owner, "waiting_user")
				choice, err := daemon.createTurnChoice(ctx, owner, api.MessageRequest{Channel: "wx-owner", Content: content}, []control.TurnChoiceOption{
					{Key: "1", Label: "resume", Action: "resume", TaskID: task.ID, RunID: run.ID}, {Key: "2", Label: "new", Action: "new"},
				})
				if err != nil {
					t.Fatal(err)
				}
				choiceID = choice.ID
				content = "/choose " + choice.ID + " 2"
			}
			alterStore(t, dir, "DROP TABLE current_workspace")
			if tc.drain {
				daemon.beginDraining("model_change:independent-probe")
			}
			inbound := weixinInbound("independent-preparation", content)
			resp, code, err := daemon.ProcessDurableInbound(ctx, inbound)
			state, _, readErr := store.InboundReceipt(ctx, "weixin", inbound.MessageID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.claimed {
				if _, peekErr := store.PeekPendingTurnChoice(ctx, owner.TenantID, owner.PersonID, choiceID, time.Now()); !errors.Is(peekErr, control.ErrTurnChoiceNotFound) {
					t.Fatalf("fixture did not claim choice: %v", peekErr)
				}
				if !errors.Is(err, api.ErrInboundHeld) || errors.Is(err, api.ErrInboundNotDispatched) || state != control.InboundDispatching {
					t.Fatalf("claimed choice incorrectly redeliverable: status=%d state=%s err=%v response=%+v", code, state, err, resp)
				}
			} else if !errors.Is(err, api.ErrInboundNotDispatched) || state != control.InboundPending {
				t.Fatalf("fresh refusal must stay redeliverable: status=%d state=%s err=%v", code, state, err)
			}
		})
	}
}
