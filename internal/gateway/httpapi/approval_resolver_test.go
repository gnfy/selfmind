package httpapi

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
	"selfmind/internal/gateway/delivery"
)

func pendingFixture(ids ...string) []control.ApprovalRequest {
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	out := make([]control.ApprovalRequest, 0, len(ids))
	for i, id := range ids {
		out = append(out, control.ApprovalRequest{
			ID:        id,
			Status:    "pending",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	return out
}

func TestResolveApprovalReferenceOrdinal(t *testing.T) {
	pending := pendingFixture("apr_aaaa1111", "apr_bbbb2222", "apr_cccc3333")
	got, err := resolveApprovalReference(pending, "2")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apr_bbbb2222" {
		t.Fatalf("resolved = %q, want apr_bbbb2222", got.ID)
	}
	if _, err := resolveApprovalReference(pending, "4"); err == nil || !strings.Contains(err.Error(), "no pending approval number 4") {
		t.Fatalf("out-of-range error = %v", err)
	}
	if _, err := resolveApprovalReference(pending, "0"); err == nil {
		t.Fatal("ordinal 0 should fail")
	}
}

func TestResolveApprovalReferencePrefix(t *testing.T) {
	pending := pendingFixture("apr_aaaa1111", "apr_aaaa2222", "apr_cccc3333")

	got, err := resolveApprovalReference(pending, "apr_cccc")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apr_cccc3333" {
		t.Fatalf("resolved = %q", got.ID)
	}

	// Ambiguous prefix must list the candidates.
	_, err = resolveApprovalReference(pending, "apr_aaaa")
	if err == nil || !strings.Contains(err.Error(), "apr_aaaa1111") || !strings.Contains(err.Error(), "apr_aaaa2222") {
		t.Fatalf("ambiguous error = %v", err)
	}

	// Too-short prefix.
	if _, err := resolveApprovalReference(pending, "apr_a"); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short-prefix error = %v", err)
	}

	// Unknown prefix.
	if _, err := resolveApprovalReference(pending, "apr_zzzz9999"); err == nil || !strings.Contains(err.Error(), "no pending approval matches") {
		t.Fatalf("no-match error = %v", err)
	}
}

func TestResolveApprovalReferenceTaskID(t *testing.T) {
	pending := pendingFixture("apr_aaaa1111")
	_, err := resolveApprovalReference(pending, "task_1234")
	if err == nil || !strings.Contains(err.Error(), "task id") || !strings.Contains(err.Error(), "apr_") {
		t.Fatalf("task-id error = %v", err)
	}
}

func TestResolveApprovalReferenceEmptyToken(t *testing.T) {
	single := pendingFixture("apr_only0001")
	got, err := resolveApprovalReference(single, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apr_only0001" {
		t.Fatalf("resolved = %q", got.ID)
	}

	if _, err := resolveApprovalReference(nil, ""); err == nil || !strings.Contains(err.Error(), "no pending approvals") {
		t.Fatalf("empty error = %v", err)
	}
	if _, err := resolveApprovalReference(pendingFixture("apr_a1111111", "apr_b2222222"), ""); err == nil || !strings.Contains(err.Error(), "2 approvals are pending") {
		t.Fatalf("multi error = %v", err)
	}
}

func TestResolveApprovalReferenceUnrecognized(t *testing.T) {
	if _, err := resolveApprovalReference(pendingFixture("apr_a1111111"), "banana"); err == nil || !strings.Contains(err.Error(), "unrecognized approval reference") {
		t.Fatalf("unrecognized error = %v", err)
	}
}

// newApprovalTestServer seeds a cli/local person with one task and one pending
// tool_call approval, mirroring what toolApprovalHandler persists.
func newApprovalTestServer(t *testing.T) (*Server, *control.Store, *control.IdentityContext, *control.Task, *control.ApprovalRequest) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Deploy service to staging",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"tool":   "terminal",
		"reason": "destructive command",
		"args":   map[string]interface{}{"command": "rm -rf build"},
	})
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		TaskID:     task.ID,
		ActionType: "tool_call",
		Payload:    payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Control: store, DefaultTenantID: "default"}, store, identity, task, approval
}

func TestApproveByOrdinalControlCommand(t *testing.T) {
	daemon, store, identity, _, approval := newApprovalTestServer(t)
	ctx := context.Background()

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/approve 1"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, resp = %+v", status, resp)
	}
	if !strings.Contains(resp.Content, "Approved") || !strings.Contains(resp.Content, approval.ID) {
		t.Fatalf("content = %q", resp.Content)
	}
	current, err := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Status != "approved" {
		t.Fatalf("approval after /approve 1 = %+v", current)
	}
}

func TestApproveFriendlyErrors(t *testing.T) {
	daemon, _, _, task, _ := newApprovalTestServer(t)
	ctx := context.Background()

	// Wrong ordinal is a friendly reply, not a 500.
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/approve 5"})
	if status != http.StatusOK || resp.Error != "" {
		t.Fatalf("status = %d, error = %q", status, resp.Error)
	}
	if !strings.Contains(resp.Content, "no pending approval number 5") {
		t.Fatalf("content = %q", resp.Content)
	}

	// A task id is detected and explained.
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/approve " + task.ID})
	if status != http.StatusOK || !strings.Contains(resp.Content, "task id") {
		t.Fatalf("status = %d, content = %q", status, resp.Content)
	}
}

func TestApproveEmptyTokenWithSinglePending(t *testing.T) {
	daemon, store, identity, _, approval := newApprovalTestServer(t)
	ctx := context.Background()

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/approve"})
	if status != http.StatusOK || !strings.Contains(resp.Content, "Approved") {
		t.Fatalf("status = %d, content = %q", status, resp.Content)
	}
	current, _ := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
	if current == nil || current.Status != "approved" {
		t.Fatalf("approval = %+v", current)
	}
}

func TestApproveAlreadyDecidedReportsStatus(t *testing.T) {
	daemon, store, identity, _, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approved", "cli", ""); err != nil {
		t.Fatal(err)
	}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/approve " + approval.ID})
	if status != http.StatusOK || !strings.Contains(resp.Content, "already approved") {
		t.Fatalf("status = %d, content = %q", status, resp.Content)
	}
}

// TestApproveScopeGrammarRecordsDecisionScope pins the reply-scope mapping:
// "/approve task" records task scope, "/approve always" records person scope,
// and a bare "y" records nothing.
func TestApproveScopeGrammarRecordsDecisionScope(t *testing.T) {
	ctx := context.Background()

	check := func(content, wantScope string) {
		t.Helper()
		daemon, store, identity, _, approval := newApprovalTestServer(t)
		resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: content})
		if status != http.StatusOK {
			t.Fatalf("%q: status = %d, resp = %+v", content, status, resp)
		}
		current, _ := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
		if current == nil || current.Status != "approved" {
			t.Fatalf("%q: approval not approved: %+v", content, current)
		}
		if current.DecisionScope != wantScope {
			t.Fatalf("%q: decision scope = %q, want %q", content, current.DecisionScope, wantScope)
		}
	}

	check("/approve 1 task", "task")
	check("/approve 1 always", "person")
	check("/approve 1 person", "person")
	check("/approve 1", "")
	check("/approve task", "task") // bare scope word targets the lone pending
	check("y", "")                 // bare conversational approve remembers nothing
	check("yt", "task")            // conversational task-scope shortcut
	check("ya", "person")          // conversational person-scope shortcut
}

func TestApprovalsListShowsRichContent(t *testing.T) {
	daemon, _, _, _, approval := newApprovalTestServer(t)
	ctx := context.Background()

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/approvals"})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	content := resp.Content
	for _, want := range []string{
		"1. [terminal]",
		"command=rm -rf build",
		"destructive command",
		"Deploy service to staging",
		approval.ID,
		"Use /approve <number> or /reject <number>.",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("approvals list missing %q:\n%s", want, content)
		}
	}
}

func TestRejectByPrefixControlCommand(t *testing.T) {
	daemon, store, identity, _, approval := newApprovalTestServer(t)
	ctx := context.Background()

	prefix := approval.ID[:12]
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/reject " + prefix})
	if status != http.StatusOK || !strings.Contains(resp.Content, "Rejected") {
		t.Fatalf("status = %d, content = %q", status, resp.Content)
	}
	current, _ := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
	if current == nil || current.Status != "rejected" {
		t.Fatalf("approval = %+v", current)
	}
}

func TestApprovalRespondEndpointAcceptsOrdinal(t *testing.T) {
	daemon, store, identity, _, approval := newApprovalTestServer(t)

	body, _ := json.Marshal(api.ApprovalRespondRequest{
		Platform:       "cli",
		PlatformUserID: "local",
		ApprovalID:     "1",
		Decision:       "approved",
		Channel:        "cli",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/approvals/respond", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	daemon.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	current, _ := store.GetApprovalRequest(context.Background(), identity.TenantID, approval.ID)
	if current == nil || current.Status != "approved" {
		t.Fatalf("approval = %+v", current)
	}
}

// recordingSender captures delivery messages for fan-out assertions.
type recordingSender struct {
	messages []delivery.Message
}

func (r *recordingSender) Send(ctx context.Context, msg delivery.Message) error {
	r.messages = append(r.messages, msg)
	return nil
}

// TestCLIOriginatedApprovalRoutesToPreferredIM: with the CLI detached (no
// presence beat recorded), a CLI-origin approval goes to the person's single
// preferred IM endpoint — here the only bound account.
func TestCLIOriginatedApprovalRoutesToPreferredIM(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()

	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	coord := daemon.coordinator()
	// TUI turns use a session-UUID channel, not the literal "cli" — origin
	// detection must key on the platform (regression: a channel match routed
	// TUI approvals to a nonexistent "cli" sender, stuck in 'sending' forever).
	coord.notifyApprovalRequested(ctx, identity, task.ID, "", "278361aa-ea7b-4f0b-a338-56c0cfab61a6", approval)

	if len(recorder.messages) != 1 {
		t.Fatalf("messages = %+v", recorder.messages)
	}
	msg := recorder.messages[0]
	if msg.Platform != "weixin" || msg.PlatformUserID != "wxid_123" {
		t.Fatalf("target = %s/%s", msg.Platform, msg.PlatformUserID)
	}
	if msg.Kind != delivery.KindApproval || msg.ApprovalID != approval.ID {
		t.Fatalf("kind = %q approval = %q", msg.Kind, msg.ApprovalID)
	}
	// Conversational, task-free push: the command + reason + a y/n prompt, and
	// deliberately NOT the task title or the apr_ id (those stay in the control
	// plane / /approvals, out of the IM UX).
	for _, want := range []string{"[terminal]", "rm -rf build", "reply y or n"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("notification missing %q:\n%s", want, msg.Content)
		}
	}
	for _, absent := range []string{"Deploy service to staging", approval.ID} {
		if strings.Contains(msg.Content, absent) {
			t.Fatalf("notification should not carry %q:\n%s", absent, msg.Content)
		}
	}
}

func TestIMOriginatedApprovalNotifiesOwnChannelOnly(t *testing.T) {
	daemon, store, _, task, _ := newApprovalTestServer(t)
	ctx := context.Background()

	imIdentity, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wxid_456", "WeChat User")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, imIdentity.TenantID, imIdentity.PersonID, "telegram", "9876", "TG"); err != nil {
		t.Fatal(err)
	}
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID:   imIdentity.TenantID,
		PersonID:   imIdentity.PersonID,
		TaskID:     task.ID,
		ActionType: "tool_call",
		Payload:    []byte(`{"tool":"terminal","reason":"x"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	daemon.coordinator().notifyApprovalRequested(ctx, imIdentity, task.ID, "", "wx_chat_1", approval)

	if len(recorder.messages) != 1 {
		t.Fatalf("messages = %+v", recorder.messages)
	}
	if recorder.messages[0].Platform != "weixin" || recorder.messages[0].Channel != "wx_chat_1" {
		t.Fatalf("target = %s/%s", recorder.messages[0].Platform, recorder.messages[0].Channel)
	}
}

func TestMessageRequestFromTelegramCallbackQuery(t *testing.T) {
	payload := map[string]interface{}{
		"callback_query": map[string]interface{}{
			"id":   "cbq-1",
			"from": map[string]interface{}{"id": 4242},
			"message": map[string]interface{}{
				"chat": map[string]interface{}{"id": 4242},
			},
			"data": "approve:apr_1234abcd",
		},
	}
	req := messageRequestFromIM("telegram", payload)
	if req.Content != "/approve apr_1234abcd" {
		t.Fatalf("content = %q", req.Content)
	}
	if req.PlatformUserID != "4242" || req.Channel != "4242" {
		t.Fatalf("user = %q channel = %q", req.PlatformUserID, req.Channel)
	}

	payload["callback_query"].(map[string]interface{})["data"] = "reject:apr_9999"
	req = messageRequestFromIM("telegram", payload)
	if req.Content != "/reject apr_9999" {
		t.Fatalf("content = %q", req.Content)
	}
}

// TestBareYesResolvesLonePendingApproval covers the conversational approval
// path: with exactly one pending approval a bare "y" (or "好") approves it and
// "n" rejects it, no /approve ceremony. This is the common IM case.
func TestBareYesResolvesLonePendingApproval(t *testing.T) {
	daemon, store, identity, _, approval := newApprovalTestServer(t)
	ctx := context.Background()

	handled, reply, err := daemon.tryHandleBareApprovalReply(ctx, identity, "y", "weixin")
	if err != nil || !handled {
		t.Fatalf("bare y should be handled: handled=%v err=%v", handled, err)
	}
	if !strings.Contains(reply, "Approved") {
		t.Fatalf("reply = %q", reply)
	}
	current, _ := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
	if current == nil || current.Status != "approved" {
		t.Fatalf("approval = %+v", current)
	}
}

// TestBareReplyIgnoredWithNoPending ensures a bare "y" with nothing pending is
// NOT claimed, so it reaches the agent (and continuation-cue handling) instead
// of being silently eaten.
func TestBareReplyIgnoredWithNoPending(t *testing.T) {
	daemon, store, identity, _, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approved", "cli", ""); err != nil {
		t.Fatal(err)
	}
	handled, _, _ := daemon.tryHandleBareApprovalReply(ctx, identity, "y", "weixin")
	if handled {
		t.Fatal("bare y with no pending approval must fall through to the agent")
	}
}

// TestBareReplyAmbiguousWithMultiplePending: with >1 pending the word cannot
// resolve, so the handler lists them and asks for /approve <n> instead of
// guessing.
func TestBareReplyAmbiguousWithMultiplePending(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		ActionType: "tool_call", Payload: []byte(`{"tool":"terminal","reason":"second"}`),
	}); err != nil {
		t.Fatal(err)
	}
	handled, reply, err := daemon.tryHandleBareApprovalReply(ctx, identity, "y", "weixin")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(reply, "/approve") || !strings.Contains(reply, "pending") {
		t.Fatalf("ambiguous reply should ask for /approve <n>: %q", reply)
	}
}

// TestOrphanedApprovalsExpireAndStopPoisoningTheList reproduces the live
// incident: a pending approval whose run died (daemon restart) made bare y
// ambiguous and /approve 1 hit the dead request. The recovery sweep must
// expire it, leaving live approvals unambiguous.
func TestOrphanedApprovalsExpireAndStopPoisoningTheList(t *testing.T) {
	daemon, store, identity, task, stale := newApprovalTestServer(t)
	ctx := context.Background()

	// The fixture's approval has no run id (left alone by the sweep); make the
	// stale one explicitly: an approval bound to a dead (non-running) run.
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, stale.ID, "rejected", "cli", ""); err != nil {
		t.Fatal(err)
	}
	deadRun, err := store.StartRun(ctx, task, "cli", "old attempt")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, deadRun.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	stale, err = store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: deadRun.ID,
		ActionType: "tool_call", Payload: []byte(`{"tool":"ls_r","reason":"stale"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A live approval from a running run.
	liveRun, err := store.StartRun(ctx, task, "cli", "new attempt")
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: liveRun.ID,
		ActionType: "tool_call", Payload: []byte(`{"tool":"ls_r","reason":"outside root"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	expired, err := store.ExpireOrphanedApprovals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want exactly the stale one", expired)
	}

	// Bare y now unambiguously answers the live approval.
	handled, reply, err := daemon.tryHandleBareApprovalReply(ctx, identity, "y", "weixin")
	if err != nil || !handled || !strings.Contains(reply, "Approved") {
		t.Fatalf("handled=%v err=%v reply=%q", handled, err, reply)
	}
	current, _ := store.GetApprovalRequest(ctx, identity.TenantID, live.ID)
	if current == nil || current.Status != "approved" {
		t.Fatalf("live approval = %+v", current)
	}
	staleNow, _ := store.GetApprovalRequest(ctx, identity.TenantID, stale.ID)
	if staleNow == nil || staleNow.Status != "expired" {
		t.Fatalf("stale approval = %+v", staleNow)
	}
}

// TestUnknownCommandSuggestion: a near-miss like /approves gets a suggestion
// instead of falling through to the agent (and a busy reply during a run).
func TestUnknownCommandSuggestion(t *testing.T) {
	daemon, _, _, _, _ := newApprovalTestServer(t)
	resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{Content: "/approves"})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(resp.Content, "did you mean /approv") {
		t.Fatalf("content = %q", resp.Content)
	}
	// A far-off slash keeps flowing (handled=false path → here it becomes an
	// agent turn; we only assert it is NOT claimed as a suggestion).
	resp, _ = daemon.ProcessMessage(context.Background(), api.MessageRequest{Content: "/deploy-skill do it"})
	if strings.Contains(resp.Content, "did you mean") {
		t.Fatalf("far-off command must not be claimed: %q", resp.Content)
	}
}

// TestApproveAllClearsEveryPending: parallel tool batches raise several
// approvals at once; /approve all answers them in one reply instead of
// re-numbered one-at-a-time ordinals.
func TestApproveAllClearsEveryPending(t *testing.T) {
	daemon, store, identity, task, first := newApprovalTestServer(t)
	ctx := context.Background()
	second, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		ActionType: "tool_call", Payload: []byte(`{"tool":"search_files","reason":"outside root"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/approve all"})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(resp.Content, "Approved 2:") || !strings.Contains(resp.Content, "[terminal]") || !strings.Contains(resp.Content, "[search_files]") {
		t.Fatalf("content = %q", resp.Content)
	}
	for _, id := range []string{first.ID, second.ID} {
		current, _ := store.GetApprovalRequest(ctx, identity.TenantID, id)
		if current == nil || current.Status != "approved" {
			t.Fatalf("approval %s = %+v", id, current)
		}
	}

	// Nothing left: all is a friendly no-op.
	resp, _ = daemon.ProcessMessage(ctx, api.MessageRequest{Content: "/reject all"})
	if !strings.Contains(resp.Content, "No pending approvals.") {
		t.Fatalf("content = %q", resp.Content)
	}
}
