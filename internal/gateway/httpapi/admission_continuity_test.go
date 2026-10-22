package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/tools"
)

// admissionProbeTools offers one read-only tool, one write tool and web
// search, so the tool list a model call receives shows the strategy it ran
// under: verification-only drops the writer, web policy decides web_search.
type admissionProbeTools struct{}

func (admissionProbeTools) Dispatch(string, map[string]interface{}) (string, error) { return "ok", nil }
func (admissionProbeTools) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{"name": "read_state", "description": "Read state.", "selfmind": map[string]interface{}{"read_only": true}},
		{"name": "write_state", "description": "Write state."},
		{"name": "web_search", "description": "Search the web."},
	}
}

// admissionProbe is what one model call saw of the Run it served.
type admissionProbe struct {
	tools  map[string]bool
	judged bool
}

// admissionProbeProvider answers the listed calls (1-based) with a 429 and
// every other call with "done". On each call it records the offered tools and
// sends one dangerous command through the Run's approval middleware: smart
// mode consults the judge, full-auto does not. A test whose request text
// carries an explicit prohibition turns the approval probe off: such a turn
// asks a person whatever its mode, and the probe would wait for the answer.
type admissionProbeProvider struct {
	mu            sync.Mutex
	limited       map[int]bool
	personID      string
	judge         *recordingJudge
	probes        []admissionProbe
	skipApprovals bool
	// during, when set, runs inside each model call after its probe is
	// recorded, in the scope of the Run that made the call.
	during func(call int)
}

func (p *admissionProbeProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}

func (p *admissionProbeProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *admissionProbeProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	probe := admissionProbe{tools: map[string]bool{}}
	for _, tool := range req.Tools {
		probe.tools[tool.Name] = true
	}
	if !p.skipApprovals {
		before := p.judge.calls()
		exec := tools.SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ran", nil })
		_, _ = exec(map[string]interface{}{"_tenant_id": p.personID, "_tool_name": "terminal", "command": "chmod 777 script.sh"})
		probe.judged = p.judge.calls() > before
	}
	p.mu.Lock()
	p.probes = append(p.probes, probe)
	call := len(p.probes)
	p.mu.Unlock()
	if p.during != nil {
		p.during(call)
	}
	if p.limited[call] {
		return nil, &llm.ProviderError{Class: llm.ProviderErrorRateLimit, StatusCode: 429, Message: "retry later"}
	}
	out := make(chan llm.StreamEvent, 1)
	out <- llm.StreamEvent{Content: "done"}
	close(out)
	return out, nil
}

func (p *admissionProbeProvider) probe(t *testing.T, call int) admissionProbe {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if call < 1 || call > len(p.probes) {
		t.Fatalf("model call %d was not made; calls=%d", call, len(p.probes))
	}
	return p.probes[call-1]
}

type admissionProbeServer struct {
	daemon   *Server
	store    *control.Store
	dir      string
	sender   *syncedSender
	identity *control.IdentityContext
	provider *admissionProbeProvider
	spine    *memory.SQLiteProvider
}

func newAdmissionProbeServer(t *testing.T, limitedCalls ...int) admissionProbeServer {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	judge := &recordingJudge{reply: `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"The test user authorized this bounded operation."}`}
	provider := &admissionProbeProvider{limited: map[int]bool{}, personID: identity.PersonID, judge: judge}
	for _, call := range limitedCalls {
		provider.limited[call] = true
	}
	spine, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := kernel.NewAgent(memory.NewMemoryManager(spine), admissionProbeTools{},
		llm.NewRequestGate(1).Wrap(provider, "admission-probe"), "test", 1, 3, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default", ApprovalJudge: judge}
	sender := &syncedSender{}
	// The outbox loop runs as the daemon's does: a push recorded for delivery
	// reaches the sender a few milliseconds later.
	daemon.Delivery = delivery.NewService(store, sender, delivery.Options{PollInterval: 10 * time.Millisecond})
	daemon.Delivery.Start(context.Background())
	t.Cleanup(daemon.Delivery.Stop)
	return admissionProbeServer{daemon: daemon, store: store, dir: dir, sender: sender, identity: identity, provider: provider, spine: spine}
}

// execRaw runs one statement against the control database beside the store,
// for the faults a test injects.
func (s admissionProbeServer) execRaw(t *testing.T, statement string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(s.dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Match the store's bounded writer wait when the running gateway writes
	// concurrently with this fault injection.
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

// corruptRunAdmission stores a snapshot this binary cannot read on runID.
func (s admissionProbeServer) corruptRunAdmission(t *testing.T, runID string) {
	t.Helper()
	s.execRaw(t, `UPDATE runs SET admission_json = '{"version":99}' WHERE id = ?`, runID)
}

// interruptedForRecovery starts a Run with admission (none for a Run from
// before snapshots existed), leaves an effect of unknown outcome, interrupts
// it, and schedules its automatic recovery, which is verification-only.
func (s admissionProbeServer) interruptedForRecovery(t *testing.T, admission control.AdmissionSnapshot) (*control.Run, string) {
	t.Helper()
	ctx := context.Background()
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		Title: "deploy", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.store.StartRunWithOptions(ctx, task, "cli", "deploy the docs site", control.StartRunOptions{Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.RecordToolDispatch(ctx, s.identity.TenantID, control.ToolLedgerEntry{RunID: interrupted.ID,
		ToolCallID: "call-unknown", ToolName: "terminal", ArgsHash: "hash", RetryClass: "side_effect",
		EffectID: "effect-unknown", Strategy: "mutate"}); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.store.MarkInterruptedRuns(ctx, 0); err != nil || recovered != 1 {
		t.Fatalf("interrupt: %d %v", recovered, err)
	}
	items, err := s.store.ListPendingRecoveryNotifications(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("recovery items = %+v %v", items, err)
	}
	if scheduled, err := s.daemon.scheduleAutomaticRunRecovery(ctx, items[0], false); err != nil || !scheduled {
		t.Fatalf("schedule = %v %v", scheduled, err)
	}
	return interrupted, "run-recovery:" + interrupted.ID + ":" + control.RunRecoveryModeVerifyOnly
}

func (s admissionProbeServer) setMode(t *testing.T, mode string) {
	t.Helper()
	if err := s.store.SetPersonSetting(context.Background(), s.identity.TenantID, s.identity.PersonID, personSettingApprovalMode, mode); err != nil {
		t.Fatal(err)
	}
}

func (s admissionProbeServer) admission(t *testing.T, runID string) control.AdmissionSnapshot {
	t.Helper()
	got, err := s.store.RunAdmissionSnapshot(context.Background(), s.identity.TenantID, runID)
	if err != nil {
		t.Fatalf("admission of %s: %v", runID, err)
	}
	return got
}

// drainDue drains the person's queue once the row's wake deadline passed and
// returns the Run that finished it.
func (s admissionProbeServer) drainDue(t *testing.T, key string) *control.Run {
	t.Helper()
	ctx := context.Background()
	row, err := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key)
	if err != nil || row == nil {
		t.Fatalf("queue row %s = %+v %v", key, row, err)
	}
	waitUntil(t, 5*time.Second, func() bool { return !time.Now().Before(row.NotBefore) }, "wake deadline did not arrive")
	s.daemon.coordinator().drainQueue(s.identity)
	deadline := time.Now().Add(5 * time.Second)
	for {
		q, _ := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key)
		if q != nil && q.Status == control.QueueStatusDone && q.RunID != "" {
			break
		}
		if time.Now().After(deadline) {
			var events []control.Event
			if q != nil && q.TaskID != "" {
				events, _ = s.store.ListTaskEvents(ctx, q.TaskID, 40)
			}
			t.Fatalf("queued continuation %s did not finish: row=%+v events=%+v", key, q, events)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitUntil(t, 5*time.Second, func() bool { return s.daemon.coordinator().activeCount(s.identity.PersonID) == 0 }, "continuation did not release its slot")
	done, _ := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key)
	run, err := s.store.GetRun(ctx, s.identity.TenantID, done.RunID)
	if err != nil || run == nil {
		t.Fatalf("continuation run = %+v %v", run, err)
	}
	return run
}

// spineUsers returns the user text of every work-spine entry, oldest first.
func (s admissionProbeServer) spineUsers(t *testing.T) []string {
	t.Helper()
	blobs, err := s.spine.GetLatestContext(context.Background(), s.identity.PersonID, kernel.SpineTrajectoryKey)
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, blob := range blobs {
		var entry struct {
			Kind string `json:"kind"`
			User string `json:"user"`
		}
		if json.Unmarshal(blob, &entry) == nil && entry.Kind != "" {
			users = append(users, entry.User)
		}
	}
	return users
}

// A direct turn records the authority actually in force in its Run row: the
// mode it resolved, the strategy its words were classified with, and the
// words themselves. A different endpoint and wording still freezes what was
// in force for that turn.
func TestDirectAdmissionRecordsTheAuthorityInForce(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	s.setMode(t, "full-auto")
	reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-direct", Content: "summarize the design notes in this repository"})
	if code != 200 || reply.Run == nil {
		t.Fatalf("direct turn = %d %+v", code, reply)
	}
	got := s.admission(t, reply.Run.ID)
	want := kernel.BuildTaskStrategy("summarize the design notes in this repository", "session-direct")
	if got.Version != control.AdmissionSnapshotVersion || got.ApprovalMode != "full-auto" || got.Origin != (control.AdmissionOrigin{}) ||
		got.Strategy.Class != string(want.Class) || got.Strategy.WebPolicy != string(want.WebPolicy) ||
		got.Strategy.ChannelMode != want.ChannelMode || got.Input.Summary != "summarize the design notes in this repository" {
		t.Fatalf("direct admission = %+v", got)
	}

	// An explicit per-request mode on a lookup request freezes that mode and
	// the web opt-in its words earned.
	reply, code = s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-lookup", Content: "look up the latest release notes", ApprovalMode: "smart"})
	if code != 200 || reply.Run == nil {
		t.Fatalf("lookup turn = %d %+v", code, reply)
	}
	got = s.admission(t, reply.Run.ID)
	if got.ApprovalMode != "smart" || got.Strategy.WebPolicy != string(kernel.WebPolicyEnabled) || got.Input.Summary != "look up the latest release notes" {
		t.Fatalf("lookup admission = %+v", got)
	}
}

// Queued work runs under the authority it was accepted with, narrowed by a
// tightening of /mode while it waits. The row and the Run keep the admitted
// snapshot; the same change governs the next new turn.
func TestQueuedWorkRunsNarrowedNotRewrittenByATightening(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	s.setMode(t, "full-auto")
	coord := s.daemon.coordinator()
	if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to occupy the person's slot")
	}
	queued, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-queued", Content: "look up the latest release notes"})
	if code != 200 || queued.Turn == nil || queued.Turn.QueueID == "" {
		t.Fatalf("busy turn was not queued: %d %+v", code, queued)
	}
	row, err := s.store.GetQueued(ctx, s.identity.TenantID, queued.Turn.QueueID)
	if err != nil || row == nil || row.Admission.ApprovalMode != "full-auto" || row.Admission.Strategy.WebPolicy != string(kernel.WebPolicyEnabled) {
		t.Fatalf("queued row admission = %+v err=%v", row, err)
	}
	s.setMode(t, "smart")
	coord.endActive(s.identity.PersonID)
	coord.drainQueue(s.identity)
	waitUntil(t, 5*time.Second, func() bool {
		q, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
		return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
	}, "queued work did not run")
	waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(s.identity.PersonID) == 0 }, "queued run did not finish")
	probe := s.provider.probe(t, 1)
	if !probe.judged || !probe.tools["web_search"] {
		t.Fatalf("queued work ignored the tightening, or lost the web access its request earned: %+v", probe)
	}
	done, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if got := s.admission(t, done.RunID); got != row.Admission {
		t.Fatalf("claimed Run admission = %+v, want the row's %+v", got, row.Admission)
	}

	// The changed mode governs new work.
	if _, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-new", Content: "summarize the design notes"}); code != 200 {
		t.Fatalf("new turn = %d", code)
	}
	if probe := s.provider.probe(t, 2); !probe.judged {
		t.Fatalf("a new turn ignored the changed mode: %+v", probe)
	}
}

// A provider wait parks the exact Run and continues it later. Making the mode
// stricter in between narrows the continuation, which keeps the web opt-in its
// original words earned though the scheduling text names no lookup.
func TestProviderWaitContinuationKeepsItsStrategyWhenTheModeTightens(t *testing.T) {
	s := newAdmissionProbeServer(t, 1)
	ctx := context.Background()
	s.setMode(t, "full-auto")
	reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-wait", Content: "look up the latest release notes"})
	if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
		t.Fatalf("parked turn = %d %+v", code, reply)
	}
	if first := s.provider.probe(t, 1); first.judged || !first.tools["web_search"] {
		t.Fatalf("parent ran under %+v", first)
	}
	s.setMode(t, "smart")
	child := s.drainDue(t, "provider-wait:"+reply.Run.ID)
	if resumed := s.provider.probe(t, 2); !resumed.judged || !resumed.tools["web_search"] {
		t.Fatalf("continuation ignored the tightening or lost its web access: %+v", resumed)
	}
	if parent := s.admission(t, reply.Run.ID); s.admission(t, child.ID) != parent || parent.IsZero() {
		t.Fatalf("continuation admission = %+v, parent %+v", s.admission(t, child.ID), parent)
	}
	// The continuation reports the person's request, not its scheduling text.
	if child.InputSummary != "look up the latest release notes" {
		t.Fatalf("continuation summary = %q", child.InputSummary)
	}
	users := s.spineUsers(t)
	if len(users) == 0 || users[len(users)-1] != "look up the latest release notes" {
		t.Fatalf("work spine recorded %q, want the original request", users)
	}
}

// The other direction: a looser mode set while the work waits does not widen
// it, and scheduling text cannot earn a web opt-in the request never had.
func TestProviderWaitContinuationDoesNotWidenWhenModeLoosens(t *testing.T) {
	s := newAdmissionProbeServer(t, 1)
	ctx := context.Background()
	s.setMode(t, "smart")
	reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-wait", Content: "tidy the design notes"})
	if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
		t.Fatalf("parked turn = %d %+v", code, reply)
	}
	if first := s.provider.probe(t, 1); !first.judged || first.tools["web_search"] {
		t.Fatalf("parent ran under %+v", first)
	}
	s.setMode(t, "full-auto")
	child := s.drainDue(t, "provider-wait:"+reply.Run.ID)
	if resumed := s.provider.probe(t, 2); !resumed.judged || resumed.tools["web_search"] {
		t.Fatalf("continuation widened past its admission: %+v", resumed)
	}
	if child.InputSummary != "tidy the design notes" {
		t.Fatalf("continuation summary = %q", child.InputSummary)
	}
}

// A verification-only recovery that parks on the provider keeps the same
// read-only surface after it continues; the continuation used to come back
// with every write tool.
func TestVerificationOnlyRecoveryKeepsItsSurfaceAcrossAProviderWait(t *testing.T) {
	s := newAdmissionProbeServer(t, 1)
	// The recovery instruction says not to repeat effects, which any mode
	// takes to a person; this test reads the tool surface only.
	s.provider.skipApprovals = true
	ctx := context.Background()
	parentAdmission := control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "full-auto",
		Strategy: control.AdmissionStrategy{Class: "general_task", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "disabled", ChannelMode: "interactive"},
		Input:    control.AdmissionInput{Summary: "deploy the docs site"}}
	_, key := s.interruptedForRecovery(t, parentAdmission)
	row, err := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key)
	if err != nil || row == nil || row.Admission.RecoveryMode != control.RunRecoveryModeVerifyOnly ||
		!row.Admission.Strategy.VerificationOnly || row.Admission.Input.Summary != "deploy the docs site" {
		t.Fatalf("recovery row admission = %+v err=%v", row, err)
	}
	recovery := s.drainDue(t, key)
	if recovery.Status != "waiting_external" {
		t.Fatalf("recovery run did not park on the provider: %+v", recovery)
	}
	if first := s.provider.probe(t, 1); first.tools["write_state"] || !first.tools["read_state"] {
		t.Fatalf("verification-only recovery offered %+v", first)
	}
	continued := s.drainDue(t, "provider-wait:"+recovery.ID)
	if resumed := s.provider.probe(t, 2); resumed.tools["write_state"] || !resumed.tools["read_state"] {
		t.Fatalf("continued verification-only recovery offered %+v", resumed)
	}
	if got := s.admission(t, continued.ID); got != row.Admission || got != s.admission(t, recovery.ID) {
		t.Fatalf("continued recovery admission = %+v, want %+v", got, row.Admission)
	}
	if continued.InputSummary != "deploy the docs site" || recovery.InputSummary != "deploy the docs site" {
		t.Fatalf("recovery summaries = %q / %q, want the original request", recovery.InputSummary, continued.InputSummary)
	}
}

// The same lineage from a Run admitted before snapshots existed: its recovery
// row records none and keeps today's derivation from the queue key, the
// recovery Run freezes that derivation, and its provider-wait continuation is
// still verification-only.
func TestLegacyVerificationOnlyRecoveryKeepsItsSurfaceAcrossAProviderWait(t *testing.T) {
	s := newAdmissionProbeServer(t, 1)
	s.provider.skipApprovals = true
	ctx := context.Background()
	_, key := s.interruptedForRecovery(t, control.AdmissionSnapshot{})
	row, err := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key)
	if err != nil || row == nil || row.AdmissionError != nil || !row.Admission.IsZero() {
		t.Fatalf("legacy recovery row = %+v err=%v, want no snapshot", row, err)
	}
	recovery := s.drainDue(t, key)
	if first := s.provider.probe(t, 1); first.tools["write_state"] || !first.tools["read_state"] {
		t.Fatalf("legacy verification-only recovery offered %+v", first)
	}
	derived := s.admission(t, recovery.ID)
	if derived.RecoveryMode != control.RunRecoveryModeVerifyOnly || !derived.Strategy.VerificationOnly {
		t.Fatalf("recovery Run froze %+v, want the key's verification-only derivation", derived)
	}
	// The scheduling text is not the person's request, and its descendants
	// must not inherit it as one.
	if derived.Input.Summary != "" {
		t.Fatalf("recovery Run recorded its scheduling text as the original request: %q", derived.Input.Summary)
	}
	continued := s.drainDue(t, "provider-wait:"+recovery.ID)
	if resumed := s.provider.probe(t, 2); resumed.tools["write_state"] || !resumed.tools["read_state"] {
		t.Fatalf("continued legacy recovery offered %+v", resumed)
	}
	if got := s.admission(t, continued.ID); got != derived {
		t.Fatalf("continued legacy recovery admission = %+v, want %+v", got, derived)
	}
}

// A daemon continuation whose exact parent's snapshot cannot be read never
// starts under a re-derived authority. The drain holds it and its source,
// tells the person once on the work's thread and their preferred IM, and
// independent work still runs.
func TestUnreadableParentAdmissionHoldsTheContinuationAndTellsThePerson(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parked, err := s.store.StartRunWithOptions(ctx, task, "cli", "publish the release", control.StartRunOptions{
		Admission: control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "smart",
			Strategy: control.AdmissionStrategy{Class: "general_task", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "disabled", ChannelMode: "interactive"},
			Input:    control.AdmissionInput{Summary: "publish the release"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, parked.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	s.corruptRunAdmission(t, parked.ID)
	wait := control.ExternalResourceWait{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, RunID: parked.ID, EffectID: "effect-held"}
	if err := s.daemon.enqueueExternalResourceWake(ctx, wait, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	key := "external-resource:" + parked.ID + ":effect-held"
	held := func() *control.QueuedTask {
		row, err := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key)
		if err != nil || row == nil {
			t.Fatalf("held row = %+v %v", row, err)
		}
		return row
	}
	if row := held(); row.Status != control.QueueStatusQueued || row.RunID != "" {
		t.Fatalf("continuation under an unreadable parent started: %+v", row)
	}
	if !hasEventOfType(t, s.store, task.ID, "queue.admission_unreadable") {
		t.Fatal("the held continuation left no durable notice on its work")
	}
	notices := func() int {
		count := 0
		for _, msg := range s.sender.snapshot() {
			if strings.Contains(msg.Content, "was not started") && strings.Contains(msg.Content, "/queue drop") {
				count++
			}
		}
		return count
	}
	waitUntil(t, 5*time.Second, func() bool { return notices() == 1 }, "the person was not told through the outbox")

	// Independent work still runs; the held row neither starts nor repeats its
	// notice on later drains.
	if _, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-other", Content: "summarize the design notes"}); code != 200 {
		t.Fatalf("independent turn = %d", code)
	}
	s.daemon.coordinator().drainQueue(s.identity)
	if row := held(); row.Status != control.QueueStatusQueued || row.RunID != "" || notices() != 1 {
		t.Fatalf("held row after later drains = %+v notices=%d", row, notices())
	}
	s.provider.mu.Lock()
	got := len(s.provider.probes)
	s.provider.mu.Unlock()
	if got != 1 {
		t.Fatalf("model calls = %d, want only the independent turn", got)
	}

	// /queue names the held row as unable to start, and dropping it ends it.
	listing, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Content: "/queue"})
	if code != 200 || !strings.Contains(listing.Content, "[cannot start: /queue drop 1]") {
		t.Fatalf("/queue listing = %d %q", code, listing.Content)
	}
	if reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Content: "/queue drop 1"}); code != 200 || reply.Error != "" {
		t.Fatalf("/queue drop = %d %+v", code, reply)
	}
	if row := held(); row.Status != control.QueueStatusCancelled {
		t.Fatalf("dropped held row = %+v", row)
	}
}

// A scheduled turn that waits behind the person's work keeps the cron origin,
// the web opt-in of its job and the instant its schedule was authorized.
func TestQueuedCronKeepsOriginWebOptInAndCutoff(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	s.setMode(t, "smart")
	coord := s.daemon.coordinator()
	if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to occupy the person's slot")
	}
	authorized := time.Unix(1790000000, 0)
	job := cron.CronJob{ID: 42, TenantID: s.identity.TenantID, Platform: "cli", DeliverTo: "local", Channel: "session-cron",
		Prompt: "tidy the nightly notes", Web: true, AuthorizedAt: authorized}
	if err := NewCronExecutor(s.daemon, nil).RunCronJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	rows, err := s.store.ListQueued(ctx, s.identity.TenantID, s.identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queued cron rows = %+v %v", rows, err)
	}
	row := rows[0]
	want := control.AdmissionOrigin{Kind: runOriginCron, ID: "42"}
	if row.Admission.Origin != want || row.Admission.StandingGrantsAuthorizedAt != authorized.Unix() ||
		row.Admission.Strategy.WebPolicy != string(kernel.WebPolicyEnabled) || row.Admission.Input.Summary != "tidy the nightly notes" {
		t.Fatalf("queued cron admission = %+v", row.Admission)
	}
	coord.endActive(s.identity.PersonID)
	coord.drainQueue(s.identity)
	waitUntil(t, 5*time.Second, func() bool {
		q, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
		return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
	}, "queued cron did not run")
	waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(s.identity.PersonID) == 0 }, "queued cron did not finish")
	done, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if probe := s.provider.probe(t, 1); !probe.tools["web_search"] {
		t.Fatalf("queued cron lost its web opt-in: %+v", probe)
	}
	payload, err := s.store.RunStartedPayload(ctx, s.identity.TenantID, s.identity.PersonID, done.RunID)
	if err != nil || !strings.Contains(string(payload), `"origin":"cron"`) {
		t.Fatalf("queued cron started as %s err=%v, want origin cron", payload, err)
	}
	if got := s.admission(t, done.RunID); got != row.Admission {
		t.Fatalf("queued cron Run admission = %+v, want %+v", got, row.Admission)
	}
}

// The same fire from a job nobody authorized and that has no web opt-in: it
// stays scheduled when it waits and runs, and gains neither web access nor a
// standing class from the wait.
func TestQueuedUnauthorizedCronStaysScheduledAndOffline(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	s.setMode(t, "smart")
	coord := s.daemon.coordinator()
	if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to occupy the person's slot")
	}
	job := cron.CronJob{ID: 9, TenantID: s.identity.TenantID, Platform: "cli", DeliverTo: "local", Channel: "session-cron",
		Prompt: "整理夜间笔记"}
	if err := NewCronExecutor(s.daemon, nil).RunCronJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	rows, err := s.store.ListQueued(ctx, s.identity.TenantID, s.identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queued cron rows = %+v %v", rows, err)
	}
	row := rows[0]
	if row.Admission.Origin != (control.AdmissionOrigin{Kind: runOriginCron, ID: "9"}) || row.Admission.StandingGrantsAuthorizedAt != 0 ||
		row.Admission.Strategy.WebPolicy != string(kernel.WebPolicyDisabled) || row.Admission.Input.Summary != "整理夜间笔记" {
		t.Fatalf("queued cron admission = %+v", row.Admission)
	}
	req := s.daemon.restoreQueuedAuthority(ctx, api.MessageRequest{Content: row.Content}, row)
	if policy := standingGrantPolicy(ctx, req); policy.Allowed {
		t.Fatalf("an unauthorized schedule gained standing classes by waiting: %+v", policy)
	}
	coord.endActive(s.identity.PersonID)
	coord.drainQueue(s.identity)
	waitUntil(t, 5*time.Second, func() bool {
		q, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
		return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
	}, "queued cron did not run")
	waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(s.identity.PersonID) == 0 }, "queued cron did not finish")
	done, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if probe := s.provider.probe(t, 1); probe.tools["web_search"] {
		t.Fatalf("queued cron without a web opt-in was offered web search: %+v", probe)
	}
	payload, err := s.store.RunStartedPayload(ctx, s.identity.TenantID, s.identity.PersonID, done.RunID)
	if err != nil || !strings.Contains(string(payload), `"origin":"cron"`) {
		t.Fatalf("queued cron started as %s err=%v, want origin cron", payload, err)
	}
}

// Standing approval classes follow who authorized the work, read from its
// admission: a continuation of a scheduled lineage keeps the schedule's
// cutoff whatever triggered it, and a person's own lineage keeps theirs.
func TestStandingGrantPolicyFollowsTheAdmittedOrigin(t *testing.T) {
	ctx := context.Background()
	scheduled := control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "smart",
		Strategy:                   control.AdmissionStrategy{Class: "general_task", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "enabled", ChannelMode: "interactive"},
		Origin:                     control.AdmissionOrigin{Kind: runOriginCron, ID: "42"},
		StandingGrantsAuthorizedAt: 1790000000}
	for _, trigger := range []string{runOriginApproval, runOriginProviderWait, runOriginResource, runOriginRecovery, runOriginWatch, ""} {
		policy := standingGrantPolicy(ctx, api.MessageRequest{Origin: trigger, Admission: scheduled})
		if !policy.Allowed || !policy.NotAfter.Equal(time.Unix(1790000000, 0)) {
			t.Fatalf("%q continuation of a schedule: %+v, want its cutoff", trigger, policy)
		}
	}
	unauthorized := scheduled
	unauthorized.StandingGrantsAuthorizedAt = 0
	if policy := standingGrantPolicy(ctx, api.MessageRequest{Origin: runOriginApproval, Admission: unauthorized}); policy.Allowed {
		t.Fatalf("a schedule nobody authorized consumed standing classes through its continuation: %+v", policy)
	}
	personal := scheduled
	personal.Origin, personal.StandingGrantsAuthorizedAt = control.AdmissionOrigin{}, 0
	if policy := standingGrantPolicy(ctx, api.MessageRequest{Origin: runOriginApproval, Admission: personal}); !policy.Allowed || !policy.NotAfter.IsZero() {
		t.Fatalf("a person's lineage lost their decisions: %+v", policy)
	}
}

// Every daemon continuation row records its exact parent's snapshot, adding
// only what its own trigger contributes, so the drain restores the parent's
// authority instead of re-deriving it from the row's scheduling text.
func TestDaemonContinuationRowsCarryTheExactParentsAdmission(t *testing.T) {
	daemon, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to install active-run guard")
	}
	defer daemon.coordinator().endActive(identity.PersonID)
	parentAdmission := control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "auto-edit",
		Strategy:                   control.AdmissionStrategy{Class: "external_lookup", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "enabled", ChannelMode: "im"},
		Origin:                     control.AdmissionOrigin{Kind: runOriginCron, ID: "7"},
		StandingGrantsAuthorizedAt: 1790000000,
		Input:                      control.AdmissionInput{Summary: "watch the nightly release"}}

	// Watcher finalization: the registering Run's authority plus the watch.
	watchRun, err := store.StartRunWithOptions(ctx, task, "cli", "watch the nightly release", control.StartRunOptions{Admission: parentAdmission})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, watchRun.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: watchRun.ID, Channel: "cli", Description: "the latest release notes are published", CWD: t.TempDir(),
		Command: "true", SuccessPattern: "SUCCESS", TimeoutAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, control.ExternalWatchSucceeded, "SUCCESS\n", ""); err != nil || !finished {
		t.Fatalf("finish watch = %v %v", finished, err)
	}
	watch, _ = store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err := daemon.enqueueExternalWatchFinalization(ctx, *watch, identity, "release published"); err != nil {
		t.Fatal(err)
	}
	row, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, externalWatchFinalizationKey(*watch))
	if err != nil || row == nil {
		t.Fatalf("watch finalization row = %+v %v", row, err)
	}
	wantWatch := parentAdmission
	wantWatch.WatchID, wantWatch.ExecutionProfile = watch.ID, externalWatchContinuationProfile(*watch)
	if row.Admission != wantWatch {
		t.Fatalf("watch finalization admission = %+v, want %+v", row.Admission, wantWatch)
	}

	// A newer watch continues with normal tools, but never widens a parent
	// that already ran under the unattended finalization profile.
	narrowParent := parentAdmission
	narrowParent.ExecutionProfile = tools.ExecutionProfileWatchFinalization
	narrowRun, err := store.StartRunWithOptions(ctx, task, "cli", "watch the nightly release", control.StartRunOptions{Admission: narrowParent})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, narrowRun.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	newer, err := store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: narrowRun.ID, Channel: "cli", Description: "release published", CWD: t.TempDir(),
		Command: "true", SuccessPattern: "SUCCESS", TimeoutAt: time.Now().Add(time.Hour),
		PreflightReceipt: control.ExternalWatchPreflightReceipt{Version: control.ExternalWatchContinuationReceiptVersion}})
	if err != nil {
		t.Fatal(err)
	}
	newerRow, err := daemon.continuationAdmission(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, ReplyToRunID: narrowRun.ID, IdempotencyKey: externalWatchFinalizationKey(*newer)})
	if err != nil || newerRow.ExecutionProfile != tools.ExecutionProfileWatchFinalization || newerRow.WatchID != newer.ID {
		t.Fatalf("newer watch continuation admission = %+v err=%v, want the parent's narrow profile kept", newerRow, err)
	}
	// A finalization whose watcher Run already moved on names no Run of its
	// own; the Run that registered the watch still owns the authority.
	unbound, err := daemon.continuationAdmission(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, IdempotencyKey: externalWatchFinalizationKey(*newer)})
	if err != nil || unbound != newerRow {
		t.Fatalf("unbound watch continuation admission = %+v err=%v, want %+v", unbound, err, newerRow)
	}

	// External resource wakeup: the parked Run's authority, unchanged.
	resourceRun, err := store.StartRunWithOptions(ctx, task, "cli", "watch the nightly release", control.StartRunOptions{Admission: parentAdmission})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, resourceRun.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	wait := control.ExternalResourceWait{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: resourceRun.ID, EffectID: "effect-1"}
	if err := daemon.enqueueExternalResourceWake(ctx, wait, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	resourceRow, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, "external-resource:"+resourceRun.ID+":effect-1")
	if err != nil || resourceRow == nil || resourceRow.Admission != parentAdmission {
		t.Fatalf("resource wake row = %+v err=%v, want the parked Run's admission", resourceRow, err)
	}

	// A parent admitted before snapshots existed hands its continuation none,
	// so the drain keeps today's derivation for it.
	legacyRun, err := store.StartRun(ctx, task, "cli", "legacy parked work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, legacyRun.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	legacyWait := control.ExternalResourceWait{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: legacyRun.ID, EffectID: "effect-2"}
	if err := daemon.enqueueExternalResourceWake(ctx, legacyWait, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	legacyRow, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, "external-resource:"+legacyRun.ID+":effect-2")
	if err != nil || legacyRow == nil || legacyRow.AdmissionError != nil || !legacyRow.Admission.IsZero() {
		t.Fatalf("legacy wake row = %+v err=%v, want no snapshot", legacyRow, err)
	}
}

// Every kind of queued hop restores the authority its row recorded; only the
// trigger comes from the row's key. The row's own approval_mode column and
// its scheduling text are never consulted once a snapshot exists.
func TestEveryQueuedHopRestoresItsRecordedAuthority(t *testing.T) {
	daemon, _, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
	frozen := control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "read-only",
		Strategy: control.AdmissionStrategy{Class: "external_lookup", ToolMode: "local_read", PlanPolicy: "optional",
			WebPolicy: "enabled", ChannelMode: "im", VerificationOnly: true},
		RecoveryMode: control.RunRecoveryModeVerifyOnly, ExecutionProfile: tools.ExecutionProfileWatchFinalization,
		WatchID: "watch_frozen", Origin: control.AdmissionOrigin{Kind: runOriginCron, ID: "42"},
		StandingGrantsAuthorizedAt: 1790000000, Input: control.AdmissionInput{Summary: "check the nightly export"}}
	for _, tc := range []struct {
		name, trigger string
		row           control.QueuedTask
	}{
		{"approval resume", runOriginApproval, control.QueuedTask{ApprovalID: "approval_1", IdempotencyKey: "approval-resume:approval_1"}},
		{"clarify resume", "", control.QueuedTask{ClarifyID: "clarify_1", IdempotencyKey: "clarify-resume:clarify_1"}},
		{"provider wait", runOriginProviderWait, control.QueuedTask{ReplyToRunID: "run_1", IdempotencyKey: "provider-wait:run_1"}},
		{"external resource", runOriginResource, control.QueuedTask{ReplyToRunID: "run_1", IdempotencyKey: "external-resource:run_1:effect"}},
		{"watcher continuation", runOriginWatch, control.QueuedTask{ReplyToRunID: "run_1", IdempotencyKey: "external-watch:watch_other:r1:finalization"}},
		{"recovery", runOriginRecovery, control.QueuedTask{ReplyToRunID: "run_1", IdempotencyKey: "run-recovery:run_1:continue"}},
		// A scheduled turn is written with the cron queue class; the class names
		// its trigger and the snapshot carries its authority.
		{"queued scheduled turn", runOriginCron, control.QueuedTask{Class: control.QueueClassCron}},
		// A transfer of scheduled work keeps the schedule's authority but is
		// claimed like the person's own selection.
		{"work_select transfer", "", control.QueuedTask{ReplyToRunID: "run_1", IdempotencyKey: "work-selection:run_0:run_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row
			row.TenantID, row.PersonID, row.Content, row.ApprovalMode = "default", "person", "Continue this exact work.", "full-auto"
			row.Admission = frozen
			req := daemon.restoreQueuedAuthority(context.Background(), api.MessageRequest{Content: row.Content, ApprovalMode: row.ApprovalMode}, row)
			if req.Admission != frozen || req.ApprovalMode != "read-only" || req.ExecutionProfile != frozen.ExecutionProfile ||
				req.WatchID != "watch_frozen" || req.RecoveryMode != control.RunRecoveryModeVerifyOnly ||
				!req.StandingGrantsAuthorizedAt.Equal(time.Unix(1790000000, 0)) {
				t.Fatalf("restored request = %+v", req)
			}
			if got := req.Origin; got != tc.trigger {
				t.Fatalf("trigger = %q, want %q", got, tc.trigger)
			}
			if policy := standingGrantPolicy(context.Background(), req); !policy.NotAfter.Equal(time.Unix(1790000000, 0)) {
				t.Fatalf("standing classes = %+v, want the schedule's cutoff", policy)
			}
			strategy := admittedStrategy(req.Admission)
			if !strategy.VerificationOnly || strategy.WebPolicy != kernel.WebPolicyEnabled || strategy.ChannelMode != "im" ||
				!strategy.AllowsTool("web_search") {
				t.Fatalf("restored strategy = %+v", strategy)
			}
		})
	}
}
