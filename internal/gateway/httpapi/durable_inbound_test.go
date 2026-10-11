package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
)

func TestGatewayOwnsNativeInboundIdentityAndReceipts(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	defer store.Close()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, DefaultTenantID: "default"}
	request := api.MessageRequest{TenantID: "default", Platform: "weixin", PlatformUserID: "wx-owner",
		Channel: "wx-owner", Content: "/status"}
	inbound := api.DurableInbound{Request: request, MessageID: "wx-1", RawPayload: []byte(`{"message_id":"wx-1"}`), OwnerPersonID: owner.PersonID}
	if _, status, err := server.ProcessDurableInbound(ctx, inbound); err != nil || status != http.StatusOK {
		t.Fatalf("first dispatch: status=%d err=%v", status, err)
	}
	bound, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil || bound.PersonID != owner.PersonID {
		t.Fatalf("gateway owner binding: %+v %v", bound, err)
	}
	if reply, status, err := server.ProcessDurableInbound(ctx, inbound); err != nil || status != http.StatusOK || reply.Content != "" {
		t.Fatalf("accepted duplicate was dispatched: %+v %d %v", reply, status, err)
	}

	unknown := api.DurableInbound{Request: api.MessageRequest{TenantID: "default", Platform: "weixin",
		PlatformUserID: "wx-other", Channel: "wx-other", Content: "/status"},
		MessageID: "wx-2", RawPayload: []byte(`{"message_id":"wx-2"}`)}
	if _, status, err := server.ProcessDurableInbound(ctx, unknown); err != nil || status != http.StatusOK {
		t.Fatalf("separate sender: status=%d err=%v", status, err)
	}
	other, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-other", "")
	if err != nil || other.PersonID == owner.PersonID {
		t.Fatalf("unknown sender inherited owner: %+v %v", other, err)
	}

	uncertain := api.DurableInbound{Request: request, MessageID: "wx-uncertain", RawPayload: []byte(`{"message_id":"wx-uncertain"}`)}
	if _, err := store.BeginInbound(ctx, "weixin", uncertain.MessageID, uncertain.RawPayload,
		control.InboundOwner{TenantID: owner.TenantID, PersonID: owner.PersonID}); err != nil {
		t.Fatal(err)
	}
	if claim, err := store.ClaimInbound(ctx, "weixin", uncertain.MessageID); err != nil || claim == 0 {
		t.Fatalf("pre-dispatch claim: %d %v", claim, err)
	}
	if _, _, err := server.ProcessDurableInbound(ctx, uncertain); !errors.Is(err, api.ErrInboundUncertain) {
		t.Fatalf("uncertain receipt was replayed: %v", err)
	}
}

func newInboundTestServer(store *control.Store, provider *slowLLMProvider) *Server {
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test agent", 1, 1, nil)
	return &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}
}

func weixinInbound(messageID, content string) api.DurableInbound {
	return api.DurableInbound{
		Request: api.MessageRequest{TenantID: "default", Platform: "weixin", PlatformUserID: "wx-owner",
			Channel: "wx-owner", Content: content, Async: true},
		MessageID:  messageID,
		RawPayload: []byte(`{"msg_id":"` + messageID + `","text":"` + content + `"}`),
	}
}

// A refusal that happens before anything acts on the input is not an
// uncertain effect: the receipt returns to pending, the sender is told why
// with a retry hint, and the typed result keeps the source redeliverable.
func TestDurableInboundRefusedBeforeAdmissionReturnsToPending(t *testing.T) {
	ctx := context.Background()
	t.Run("shutdown drain", func(t *testing.T) {
		store := controltest.NewStore(t)
		daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
		daemon.beginDraining("signal terminated")
		resp, status, err := daemon.ProcessDurableInbound(ctx, weixinInbound("wx-drain", "start the release"))
		if !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) || status != http.StatusServiceUnavailable {
			t.Fatalf("drain refusal was not reported as not dispatched: status=%d err=%v", status, err)
		}
		if !strings.Contains(resp.Error, "shutting down") || !strings.Contains(resp.Error, "retried automatically") {
			t.Fatalf("sender was not told the cause and the retry: %+v", resp)
		}
		state, _, err := store.InboundReceipt(ctx, "weixin", "wx-drain")
		if err != nil || state != control.InboundPending {
			t.Fatalf("refused receipt = %q, %v; want pending", state, err)
		}
		owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
		if err != nil {
			t.Fatal(err)
		}
		if runs, err := store.ListRecentRunsForPerson(ctx, owner.TenantID, owner.PersonID, 10); err != nil || len(runs) != 0 {
			t.Fatalf("a refused input started work: %+v, %v", runs, err)
		}
	})
	t.Run("identity store unavailable", func(t *testing.T) {
		store := controltest.NewStore(t)
		daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		resp, status, err := daemon.ProcessDurableInbound(ctx, weixinInbound("wx-store", "check the backup"))
		if !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) || status != http.StatusServiceUnavailable {
			t.Fatalf("store refusal was not reported as not dispatched: status=%d err=%v", status, err)
		}
		if !strings.Contains(resp.Error, "not started") || !strings.Contains(resp.Error, "retried automatically") {
			t.Fatalf("sender was not told the cause and the retry: %+v", resp)
		}
		if strings.Contains(resp.Error, "sql") || strings.Contains(resp.Error, "closed") {
			t.Fatalf("the chat notice leaks an internal error: %q", resp.Error)
		}
	})
}

// Once ProcessMessage admitted the input, a 5xx may follow real work. That
// receipt stays dispatching with its diagnostic and is never replayed.
func TestDurableInboundFailureAfterAdmissionStaysUncertain(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	inbound := weixinInbound("wx-admitted", "start the release")
	inbound.Request.Async = false
	resp, status, err := daemon.ProcessDurableInbound(ctx, inbound)
	if !errors.Is(err, api.ErrInboundUncertain) || status < http.StatusInternalServerError || resp.Run == nil {
		t.Fatalf("post-admission failure: run=%v status=%d err=%v", resp.Run, status, err)
	}
	state, _, err := store.InboundReceipt(ctx, "weixin", "wx-admitted")
	if err != nil || state != control.InboundDispatching {
		t.Fatalf("post-admission receipt = %q, %v; want dispatching", state, err)
	}
	if _, _, err := daemon.ProcessDurableInbound(ctx, inbound); !errors.Is(err, api.ErrInboundUncertain) {
		t.Fatalf("uncertain receipt was replayed: %v", err)
	}
}

// Answering a continuity choice while SelfMind shuts down must not consume
// the choice: the answer is refused before the claim, the choice keeps its
// saved request, and the redelivered answer runs that request after restart.
// A bare number on the choice's endpoint follows the same rule.
func TestShutdownDrainLeavesAnsweredChoicePending(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	before := controltest.NewStoreInDir(t, dir)
	draining := newInboundTestServer(before, newSlowLLMProvider("unused"))
	owner, err := before.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	task, run := seedContinuityHistory(t, before, owner, "waiting_user")
	choice, err := draining.createTurnChoice(ctx, owner, api.MessageRequest{Channel: "wx-owner", Content: "ship the hotfix"},
		[]control.TurnChoiceOption{
			{Key: "1", Label: "resume", Action: "resume", TaskID: task.ID, RunID: run.ID},
			{Key: "2", Label: "new", Action: "new"},
		})
	if err != nil {
		t.Fatal(err)
	}
	draining.beginDraining("signal terminated")
	chosen := weixinInbound("wx-choose", "/choose "+choice.ID+" 2")
	bare := weixinInbound("wx-bare", "1")
	for _, answer := range []api.DurableInbound{chosen, bare} {
		answer.Request.Async = false
		resp, _, err := draining.ProcessDurableInbound(ctx, answer)
		if !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) || !strings.Contains(resp.Error, "not started") {
			t.Fatalf("answer %q during shutdown was not refused before the claim: %+v, %v", answer.Request.Content, resp, err)
		}
		if state, _, err := before.InboundReceipt(ctx, "weixin", answer.MessageID); err != nil || state != control.InboundPending {
			t.Fatalf("answer %q receipt = %q, %v; want pending", answer.Request.Content, state, err)
		}
		pending, err := before.PeekPendingTurnChoice(ctx, owner.TenantID, owner.PersonID, choice.ID, time.Now())
		if err != nil || !strings.Contains(pending.RequestJSON, "ship the hotfix") {
			t.Fatalf("answer %q consumed the choice during shutdown: %+v, %v", answer.Request.Content, pending, err)
		}
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = after.Close() })
	provider := newSlowLLMProvider("hotfix shipped")
	provider.releaseNow()
	restarted := newInboundTestServer(after, provider)
	chosen.Request.Async = false
	resp, _, err := restarted.ProcessDurableInbound(ctx, chosen)
	if err != nil || resp.Task == nil || resp.Task.Title != "ship the hotfix" || resp.Run == nil {
		t.Fatalf("redelivered answer did not run the saved request: task=%+v run=%+v err=%v", resp.Task, resp.Run, err)
	}
	if state, _, err := after.InboundReceipt(ctx, "weixin", chosen.MessageID); err != nil || state != control.InboundAccepted {
		t.Fatalf("redelivered answer receipt = %q, %v; want accepted", state, err)
	}
	if _, err := after.PeekPendingTurnChoice(ctx, owner.TenantID, owner.PersonID, choice.ID, time.Now()); !errors.Is(err, control.ErrTurnChoiceNotFound) {
		t.Fatalf("answered choice is still pending: %v", err)
	}
}

// After a restart, a receipt refused before admission is dispatched exactly
// once even when the source redelivers it concurrently, while a receipt that
// was genuinely dispatched stays uncertain with its diagnostic.
func TestNotDispatchedReceiptRunsOnceAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	before := controltest.NewStoreInDir(t, dir)
	draining := newInboundTestServer(before, newSlowLLMProvider("unused"))
	draining.beginDraining("signal terminated")
	refused := weixinInbound("wx-refused", "summarize the incident")
	refused.Request.Async = false
	if _, _, err := draining.ProcessDurableInbound(ctx, refused); !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) {
		t.Fatalf("drain refusal: %v", err)
	}
	owner, err := before.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	dispatched := weixinInbound("wx-dispatched", "rotate the keys")
	if _, err := before.BeginInbound(ctx, "weixin", dispatched.MessageID, dispatched.RawPayload,
		control.InboundOwner{TenantID: owner.TenantID, PersonID: owner.PersonID}); err != nil {
		t.Fatal(err)
	}
	if claim, err := before.ClaimInbound(ctx, "weixin", dispatched.MessageID); err != nil || claim == 0 {
		t.Fatalf("claim before crash: %d %v", claim, err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = after.Close() })
	provider := newSlowLLMProvider("summary ready")
	restarted := newInboundTestServer(after, provider)
	type result struct {
		resp api.MessageResponse
		err  error
	}
	winner := make(chan result, 1)
	go func() {
		resp, _, err := restarted.ProcessDurableInbound(ctx, refused)
		winner <- result{resp: resp, err: err}
	}()
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the redelivered input never reached its model call")
	}
	// Exactly one effect: one Run, no steering into it, no queued copy.
	requireSingleEffect := func(stage string) {
		t.Helper()
		steering, steerErr := after.ListUnconsumedSteering(ctx, owner.TenantID, "", 50)
		queued, queueErr := after.ListQueued(ctx, owner.TenantID, owner.PersonID, control.QueueStatusQueued)
		runs, runErr := after.ListRecentRunsForPerson(ctx, owner.TenantID, owner.PersonID, 20)
		if steerErr != nil || queueErr != nil || runErr != nil || len(steering) != 0 || len(queued) != 0 || len(runs) != 1 {
			t.Fatalf("%s: steering=%d queued=%d runs=%d errors=%v %v %v", stage, len(steering), len(queued), len(runs), steerErr, queueErr, runErr)
		}
	}
	// Every copy delivered while the first one runs is in flight elsewhere:
	// it must neither act nor produce a message for the chat. Half of the
	// copies arrive through the whole boundary; the other half model a
	// dispatcher that read the receipt as pending just before the first one
	// claimed it, so only the claim stands between it and a second effect.
	const redeliveries = 6
	losers := make(chan result, 2*redeliveries)
	var wg sync.WaitGroup
	for i := 0; i < redeliveries; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp, _, err := restarted.ProcessDurableInbound(ctx, refused)
			losers <- result{resp: resp, err: err}
		}()
		go func() {
			defer wg.Done()
			resp, _, err := restarted.dispatchInboundReceipt(ctx, owner, "weixin", refused.MessageID, refused.Request)
			losers <- result{resp: resp, err: err}
		}()
	}
	wg.Wait()
	close(losers)
	for r := range losers {
		if !errors.Is(r.err, api.ErrInboundUncertain) || r.resp.Accepted || r.resp.Content != "" || r.resp.Error != "" {
			t.Fatalf("a concurrent redelivery acted or answered: %+v, %v", r.resp, r.err)
		}
	}
	requireSingleEffect("while the first redelivery runs")
	provider.releaseNow()
	won := <-winner
	if won.err != nil || won.resp.Run == nil || strings.TrimSpace(won.resp.Content) != "summary ready" {
		t.Fatalf("the dispatched redelivery: %+v, %v", won.resp, won.err)
	}
	requireSingleEffect("after the first redelivery finished")
	if state, _, err := after.InboundReceipt(ctx, "weixin", refused.MessageID); err != nil || state != control.InboundAccepted {
		t.Fatalf("redelivered receipt = %q, %v; want accepted", state, err)
	}
	if dup, _, err := restarted.ProcessDurableInbound(ctx, refused); err != nil || dup.Accepted || dup.Content != "" || dup.Error != "" {
		t.Fatalf("a copy after acceptance was not a silent duplicate: %+v, %v", dup, err)
	}
	requireSingleEffect("after a late duplicate")
	if _, _, err := restarted.ProcessDurableInbound(ctx, dispatched); !errors.Is(err, api.ErrInboundUncertain) {
		t.Fatalf("genuinely dispatched receipt was replayed after restart: %v", err)
	}
	if state, _, err := after.InboundReceipt(ctx, "weixin", dispatched.MessageID); err != nil || state != control.InboundDispatching {
		t.Fatalf("genuinely dispatched receipt = %q, %v; want dispatching", state, err)
	}
	diag, err := restarted.inboundDiagReply(ctx, owner)
	if err != nil || !strings.Contains(diag, "Unresolved inbound messages: 1") || !strings.Contains(diag, "dispatching") ||
		!strings.Contains(diag, "may already have caused effects") {
		t.Fatalf("diagnostic lost the uncertain explanation: %q, %v", diag, err)
	}
}

// alterStore runs DDL through a second connection to the store's database so
// a test can make one table fail while the rest of the store keeps working.
func alterStore(t *testing.T, dir, ddl string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("alter store: %v", err)
	}
}

// occupyWithBackPressuredRun registers one live Run whose steering channel
// nobody reads, so new input for the person falls back to the durable queue.
func occupyWithBackPressuredRun(t *testing.T, daemon *Server, store *control.Store, owner *control.IdentityContext) *control.Run {
	t.Helper()
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "long build", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "session-a", "long build")
	if err != nil {
		t.Fatal(err)
	}
	handle := &activeRun{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID,
		Channel: "session-a", Summary: "long build", StartedAt: time.Now(), Steer: make(chan kernel.SteeringInput)}
	coord := daemon.coordinator()
	if !coord.beginActive(owner.PersonID, handle) {
		t.Fatal("cannot register the live run")
	}
	t.Cleanup(func() { coord.endActiveRun(owner.PersonID, handle) })
	return run
}

// Queueing is how input that cannot start now is admitted. When that write
// fails nothing exists for the message, so it must stay redeliverable instead
// of being acknowledged as accepted with an internal error in the chat.
func TestQueueWriteFailureIsNotDispatched(t *testing.T) {
	for _, path := range []string{"durable", "webhook"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			platform, user := "weixin", "wx-owner"
			if path == "webhook" {
				platform, user = "webhook", "im-owner"
			}
			owner, err := store.ResolveOrCreateAccount(ctx, "default", platform, user, "")
			if err != nil {
				t.Fatal(err)
			}
			occupyWithBackPressuredRun(t, daemon, store, owner)
			deliver := func() (api.MessageResponse, bool, bool) {
				if path == "durable" {
					resp, _, err := daemon.ProcessDurableInbound(ctx, weixinInbound("wx-queue", "start the release"))
					return resp, err == nil, errors.Is(err, api.ErrInboundNotDispatched) && !errors.Is(err, api.ErrInboundUncertain)
				}
				body := `{"message_id":"in-queue","user_id":"im-owner","chat_id":"im-chat","content":"start the release"}`
				rec := httptest.NewRecorder()
				daemon.handleIMWebhook(rec, httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(body)))
				var reply api.MessageResponse
				_ = json.Unmarshal(rec.Body.Bytes(), &reply)
				return reply, rec.Code == http.StatusOK, rec.Code == http.StatusServiceUnavailable
			}
			messageID := map[string]string{"durable": "wx-queue", "webhook": "in-queue"}[path]
			alterStore(t, dir, `CREATE TRIGGER reject_queue BEFORE INSERT ON task_queue BEGIN SELECT RAISE(ABORT, 'injected queue failure'); END`)
			resp, _, refused := deliver()
			if !refused || !strings.Contains(resp.Error, "not started") || strings.Contains(resp.Error, "injected") {
				t.Fatalf("failed queue write was not refused cleanly: %+v", resp)
			}
			if state, _, err := store.InboundReceipt(ctx, platform, messageID); err != nil || state != control.InboundPending {
				t.Fatalf("receipt after a failed queue write = %q, %v; want pending", state, err)
			}
			alterStore(t, dir, `DROP TRIGGER reject_queue`)
			resp, accepted, _ := deliver()
			if !accepted || !resp.Accepted || resp.Turn == nil || resp.Turn.QueueID == "" {
				t.Fatalf("redelivery was not queued: %+v", resp)
			}
			queued, err := store.ListQueued(ctx, owner.TenantID, owner.PersonID, control.QueueStatusQueued)
			if err != nil || len(queued) != 1 || queued[0].Content != "start the release" {
				t.Fatalf("queue after redelivery = %+v, %v; want one row", queued, err)
			}
			if state, _, err := store.InboundReceipt(ctx, platform, messageID); err != nil || state != control.InboundAccepted {
				t.Fatalf("redelivered receipt = %q, %v; want accepted", state, err)
			}
		})
	}
}

// A redelivered ID whose content or owner no longer matches its receipt cannot
// be reconciled by retrying. The sender hears it once without a retry promise,
// the source is told to move on, and the owner's own receipt records why.
func TestConflictingRedeliveryIsRejectedWithoutRetry(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	draining := newInboundTestServer(store, newSlowLLMProvider("unused"))
	draining.beginDraining("signal terminated")
	original := weixinInbound("wx-conflict", "start the release")
	if _, _, err := draining.ProcessDurableInbound(ctx, original); !errors.Is(err, api.ErrInboundNotDispatched) {
		t.Fatalf("seed refusal: %v", err)
	}
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
	changed := original
	changed.RawPayload = []byte(`{"msg_id":"wx-conflict","text":"start the release now"}`)
	stranger := weixinInbound("wx-conflict", "start the release")
	stranger.Request.PlatformUserID, stranger.Request.Channel = "wx-stranger", "wx-stranger"
	for name, conflict := range map[string]api.DurableInbound{"changed content": changed, "another sender": stranger} {
		resp, _, err := daemon.ProcessDurableInbound(ctx, conflict)
		if !errors.Is(err, api.ErrInboundRejected) || errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) {
			t.Fatalf("%s: conflicting redelivery was not rejected for good: %v", name, err)
		}
		if !strings.Contains(resp.Error, "not started") || strings.Contains(resp.Error, "retried automatically") ||
			strings.Contains(resp.Error, "payload") || strings.Contains(resp.Error, "identity") {
			t.Fatalf("%s: rejection notice = %q", name, resp.Error)
		}
	}
	rows, err := store.ListUncertainInboundForPerson(ctx, owner.TenantID, owner.PersonID, 20)
	if err != nil || len(rows) != 1 || rows[0].State != control.InboundPending || !strings.Contains(rows[0].LastError, "changed") {
		t.Fatalf("the owner's receipt does not record the rejected redelivery: %+v, %v", rows, err)
	}
	diag, err := daemon.inboundDiagReply(ctx, owner)
	if err != nil || !strings.Contains(diag, "changed") {
		t.Fatalf("diagnostic does not show why the redelivery was rejected: %q, %v", diag, err)
	}
	body := `{"message_id":"wx-conflict","user_id":"wx-owner","chat_id":"wx-owner","content":"different"}`
	rec := httptest.NewRecorder()
	daemon.handleIMWebhook(rec, httptest.NewRequest("POST", "/v1/im/weixin", strings.NewReader(body)))
	if rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), "retried automatically") {
		t.Fatalf("webhook conflict = %d %s; want 409 without a retry promise", rec.Code, rec.Body.String())
	}
}

// A read that fails before any choice is claimed leaves nothing behind, so the
// answer stays redeliverable instead of being held as an uncertain effect.
func TestChoiceReadFailureIsNotDispatched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	task, run := seedContinuityHistory(t, store, owner, "waiting_user")
	choice, err := daemon.createTurnChoice(ctx, owner, api.MessageRequest{Channel: "wx-owner", Content: "ship the hotfix"},
		[]control.TurnChoiceOption{
			{Key: "1", Label: "resume", Action: "resume", TaskID: task.ID, RunID: run.ID},
			{Key: "2", Label: "new", Action: "new"},
		})
	if err != nil {
		t.Fatal(err)
	}
	alterStore(t, dir, `ALTER TABLE pending_turn_choices RENAME TO pending_turn_choices_unavailable`)
	for _, answer := range []api.DurableInbound{
		weixinInbound("wx-read-bare", "2"),
		weixinInbound("wx-read-choose", "/choose "+choice.ID+" 1"),
	} {
		answer.Request.Async = false
		resp, _, err := daemon.ProcessDurableInbound(ctx, answer)
		if !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) || strings.Contains(resp.Error, "no such table") {
			t.Fatalf("answer %q with unreadable choices: %+v, %v", answer.Request.Content, resp, err)
		}
		if state, _, err := store.InboundReceipt(ctx, "weixin", answer.MessageID); err != nil || state != control.InboundPending {
			t.Fatalf("answer %q receipt = %q, %v; want pending", answer.Request.Content, state, err)
		}
	}
	alterStore(t, dir, `ALTER TABLE pending_turn_choices_unavailable RENAME TO pending_turn_choices`)
	if pending, err := store.PeekPendingTurnChoice(ctx, owner.TenantID, owner.PersonID, choice.ID, time.Now()); err != nil || !strings.Contains(pending.RequestJSON, "ship the hotfix") {
		t.Fatalf("the choice changed while it was unreadable: %+v, %v", pending, err)
	}
}

// The writes that admit an answer or a parked request — creating a multi-Run
// choice, claiming a choice, routing a choice answer — each commit in one
// transaction. When one fails nothing holds the input, so it stays
// redeliverable and the next delivery completes the same step.
func TestChoiceWriteFailureIsNotDispatched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	provider := newSlowLLMProvider("hotfix shipped")
	provider.releaseNow()
	daemon := newInboundTestServer(store, provider)
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
	if err != nil {
		t.Fatal(err)
	}
	refusedThenAccepted := func(name string, inbound api.DurableInbound, trigger string) api.MessageResponse {
		t.Helper()
		inbound.Request.Async = false
		alterStore(t, dir, "CREATE TRIGGER inject_failure "+trigger+" BEGIN SELECT RAISE(ABORT, 'injected write failure'); END")
		resp, _, err := daemon.ProcessDurableInbound(ctx, inbound)
		if !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) || strings.Contains(resp.Error, "injected") {
			t.Fatalf("%s: failed write was not refused cleanly: %+v, %v", name, resp, err)
		}
		if state, _, err := store.InboundReceipt(ctx, "weixin", inbound.MessageID); err != nil || state != control.InboundPending {
			t.Fatalf("%s: receipt = %q, %v; want pending", name, state, err)
		}
		alterStore(t, dir, "DROP TRIGGER inject_failure")
		resp, _, err = daemon.ProcessDurableInbound(ctx, inbound)
		if err != nil {
			t.Fatalf("%s: redelivery failed: %+v, %v", name, resp, err)
		}
		return resp
	}

	// A resume-candidates choice whose claim fails stays pending.
	task, run := seedContinuityHistory(t, store, owner, "waiting_user")
	candidates, err := daemon.createTurnChoice(ctx, owner, api.MessageRequest{Channel: "wx-owner", Content: "ship the hotfix"},
		[]control.TurnChoiceOption{
			{Key: "1", Label: "resume", Action: "resume", TaskID: task.ID, RunID: run.ID},
			{Key: "2", Label: "new", Action: "new"},
		})
	if err != nil {
		t.Fatal(err)
	}
	claimed := refusedThenAccepted("claim", weixinInbound("wx-claim", "/choose "+candidates.ID+" 2"),
		`BEFORE UPDATE OF status ON pending_turn_choices WHEN NEW.status = 'claimed'`)
	if claimed.Task == nil || claimed.Task.Title != "ship the hotfix" {
		t.Fatalf("the redelivered answer did not run the saved request: %+v", claimed)
	}

	// With two live Runs, a parked request whose choice row cannot be written
	// is refused, and its redelivery parks it.
	daemon.coordinator().activeLimit = 2
	occupyWithBackPressuredRun(t, daemon, store, owner)
	occupyWithBackPressuredRun(t, daemon, store, owner)
	parked := refusedThenAccepted("park", weixinInbound("wx-park", "please add the missing check"),
		`BEFORE INSERT ON pending_turn_choices`)
	if parked.Choice == nil {
		t.Fatalf("the redelivered request was not parked as a choice: %+v", parked)
	}

	// Routing its answer to a live Run fails once, then lands exactly once.
	routed := refusedThenAccepted("route", weixinInbound("wx-route", "/choose "+parked.Choice.ID+" 1"),
		`BEFORE INSERT ON steering_mailbox`)
	if !routed.Accepted {
		t.Fatalf("the redelivered answer was not routed: %+v", routed)
	}
	if _, err := store.PeekPendingTurnChoice(ctx, owner.TenantID, owner.PersonID, parked.Choice.ID, time.Now()); !errors.Is(err, control.ErrTurnChoiceNotFound) {
		t.Fatalf("the routed choice is still pending: %v", err)
	}
}

// countdownContext reports cancellation once a fixed number of consultations
// have been used, so a test can cancel a request at every point where the
// code observes its context.
type countdownContext struct {
	context.Context
	mu     sync.Mutex
	left   int
	done   chan struct{}
	closed bool
}

func newCountdownContext(consultations int) *countdownContext {
	return &countdownContext{Context: context.Background(), left: consultations, done: make(chan struct{})}
}

func (c *countdownContext) consult() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		if c.left == 0 {
			c.closed = true
			close(c.done)
		} else {
			c.left--
		}
	}
	return c.closed
}

func (c *countdownContext) Done() <-chan struct{} {
	c.consult()
	return c.done
}

func (c *countdownContext) Err() error {
	if c.consult() {
		return context.Canceled
	}
	return nil
}

func (c *countdownContext) fired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// The WeChat poll context is cancelled by Stop() while the gateway drains,
// and a webhook request context ends when its client disconnects. Wherever
// the cancellation lands, a proven refusal must leave its receipt pending (or
// never saved), and the sender is told "not started" only when the receipt
// agrees.
func TestRefusalSurvivesCallerCancellationAtEveryPoint(t *testing.T) {
	for _, path := range []string{"durable", "webhook"} {
		t.Run(path, func(t *testing.T) {
			store := controltest.NewStore(t)
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			daemon.beginDraining("signal terminated")
			for point := 0; ; point++ {
				if point > 500 {
					t.Fatal("the cancellation probe never outlived the request")
				}
				ctx := newCountdownContext(point)
				messageID := fmt.Sprintf("cancel-%d", point)
				receiptPlatform := "weixin"
				var refusal string
				var refused bool
				if path == "durable" {
					resp, _, err := daemon.ProcessDurableInbound(ctx, weixinInbound(messageID, "start the release"))
					refusal, refused = resp.Error, errors.Is(err, api.ErrInboundNotDispatched) && !errors.Is(err, api.ErrInboundUncertain)
				} else {
					receiptPlatform = "webhook"
					body := `{"message_id":"` + messageID + `","user_id":"im-owner","chat_id":"im-chat","content":"start the release"}`
					req := httptest.NewRequest("POST", "/v1/im/webhook", strings.NewReader(body)).WithContext(ctx)
					rec := httptest.NewRecorder()
					daemon.handleIMWebhook(rec, req)
					var reply api.MessageResponse
					_ = json.Unmarshal(rec.Body.Bytes(), &reply)
					refusal, refused = reply.Error, rec.Code == http.StatusServiceUnavailable
				}
				state, _, readErr := store.InboundReceipt(context.Background(), receiptPlatform, messageID)
				if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
					t.Fatal(readErr)
				}
				if !refused || !strings.Contains(refusal, "not started") {
					t.Fatalf("cancellation at point %d was not reported as not started: %q (receipt %q)", point, refusal, state)
				}
				if strings.Contains(refusal, "context") || strings.Contains(refusal, "sql") {
					t.Fatalf("cancellation at point %d leaks an internal error to the chat: %q", point, refusal)
				}
				if state != "" && state != control.InboundPending {
					t.Fatalf("cancellation at point %d left the refused receipt %q", point, state)
				}
				if !ctx.fired() {
					t.Logf("probed %d cancellation points", point)
					return
				}
			}
		})
	}
}

func TestWorkspaceReadFailureBeforeAdmissionReturnsInboundToPending(t *testing.T) {
	for _, activeCount := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("%d active runs", activeCount), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			t.Cleanup(func() { _ = store.Close() })
			daemon := newInboundTestServer(store, newSlowLLMProvider("unused"))
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-owner", "")
			if err != nil {
				t.Fatal(err)
			}
			daemon.coordinator().activeLimit = 2
			for i := 0; i < activeCount; i++ {
				channel := fmt.Sprintf("session-%d", i)
				task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: channel, Channel: channel})
				if err != nil {
					t.Fatal(err)
				}
				run, err := store.StartRun(ctx, task, channel, "build")
				if err != nil {
					t.Fatal(err)
				}
				handle := &activeRun{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Channel: channel, Platform: "cli", PlatformUserID: "local", Summary: "build", Steer: make(chan kernel.SteeringInput, 4), StartedAt: run.StartedAt}
				if !daemon.coordinator().beginActive(owner.PersonID, handle) {
					t.Fatal("cannot register active")
				}
				t.Cleanup(func() { daemon.coordinator().endActiveRun(owner.PersonID, handle) })
			}
			alterStore(t, dir, "DROP TABLE current_workspace")
			inbound := weixinInbound("read-failure", "Inspect the release notes")
			resp, status, err := daemon.ProcessDurableInbound(ctx, inbound)
			state, _, readErr := store.InboundReceipt(ctx, "weixin", inbound.MessageID)
			if !errors.Is(err, api.ErrInboundNotDispatched) || errors.Is(err, api.ErrInboundUncertain) || state != control.InboundPending || readErr != nil || resp.Accepted {
				t.Fatalf("pre-admission read stranded input: status=%d state=%s error=%v readError=%v response=%+v", status, state, err, readErr, resp)
			}
			// Repairing the failed read makes redelivery possible. The refused input
			// never claimed a choice, started a Run or queued itself before that retry.
			alterStore(t, dir, `CREATE TABLE current_workspace (tenant_id TEXT NOT NULL, person_id TEXT NOT NULL, workspace_id TEXT NOT NULL, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY(tenant_id, person_id))`)
			retry, _, retryErr := daemon.ProcessDurableInbound(ctx, inbound)
			if errors.Is(retryErr, api.ErrInboundUncertain) || errors.Is(retryErr, api.ErrInboundNotDispatched) {
				t.Fatalf("repaired input was not redeliverable: %+v %v", retry, retryErr)
			}
		})
	}
}
