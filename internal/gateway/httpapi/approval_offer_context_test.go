package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// promptCapturingJudge keeps the prompt it was handed. What matters is not that
// the prompt builder CAN render the offer, but that the gateway actually put it
// in the prompt the judge received: an earlier fix of this shape tested the
// renderer and shipped a send site that passed nothing.
// The verdict is deliberately "approve": the prompt is captured before the
// verdict is acted on, and escalating would park each test on the human-ask
// path until it times out.
type promptCapturingJudge struct {
	mu     sync.Mutex
	reply  string
	prompt string
}

func (j *promptCapturingJudge) Judge(_ context.Context, prompt string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.prompt = prompt
	return j.reply, nil
}

func (j *promptCapturingJudge) seen() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.prompt
}

// On 2026-09-07 a run whose entire user text was "2" — answering a numbered
// list of next steps SelfMind had just printed — reached the approval judge as
// "2" alone. It ruled authorization unknown and denied a Cloud Build approval
// the person had in fact just chosen, so they approved it by hand in the
// console instead. The scope installed at run start must carry what the person
// was answering.
func TestApprovalJudgeReceivesWhatThePersonWasAnswering(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	const offer = "下一步（三选一）\n1. 你在控制台批准三条\n2. 授权我代批\n3. 等待"
	if err := store.RecordChannelMessage(ctx, *identity, "cli", "task-1", "assistant", offer); err != nil {
		t.Fatal(err)
	}

	judge := &promptCapturingJudge{reply: `{"risk_level":"medium","user_authorization":"high","outcome":"approve","rationale":"ok"}`}
	daemon := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}
	coord := daemon.coordinator()
	run := &control.Run{ID: "run-1", Channel: "cli"}
	cleanup := coord.installExecutionScope(ctx, identity, nil, run, nil, api.MessageRequest{Content: "2", Channel: "cli"})
	defer cleanup()

	exec := tools.SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ran", nil })
	_, _ = exec(map[string]interface{}{
		"_tenant_id": identity.PersonID,
		"_tool_name": "terminal",
		"command":    "gcloud builds approve 49d9e7a8",
	})

	prompt := judge.seen()
	if prompt == "" {
		t.Fatal("the judge was never consulted")
	}
	if !strings.Contains(prompt, "\n<assistant_offered>\n") || !strings.Contains(prompt, "授权我代批") {
		t.Fatalf("the judge did not receive what the person was answering:\n%s", prompt)
	}
	// The reply itself must still arrive as the person's own words, separable
	// from the offer: one is the authorization, the other only its referent.
	if !strings.Contains(prompt, "\n<person_asked>\n") {
		t.Fatalf("the person's own reply is missing:\n%s", prompt)
	}
}

// A daemon-originated run carries no current human authorization, so an offer
// recovered from the channel must not be attached to it — otherwise a cron turn
// inherits the last thing a person was shown as if they had just accepted it.
func TestSystemOriginatedRunCarriesNoOfferContext(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordChannelMessage(ctx, *identity, "cli", "task-1", "assistant", "1. delete /tmp  2. stop"); err != nil {
		t.Fatal(err)
	}

	judge := &promptCapturingJudge{reply: `{"risk_level":"high","user_authorization":"unknown","outcome":"approve","rationale":"ok"}`}
	daemon := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}
	coord := daemon.coordinator()
	run := &control.Run{ID: "run-2", Channel: "cli"}
	cleanup := coord.installExecutionScope(ctx, identity, nil, run, nil, api.MessageRequest{
		Content: "run the nightly cleanup", Channel: "cli", Origin: "cron",
	})
	defer cleanup()

	exec := tools.SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ran", nil })
	_, _ = exec(map[string]interface{}{
		"_tenant_id": identity.PersonID,
		"_tool_name": "terminal",
		"command":    "rm -rf /tmp/build",
	})

	prompt := judge.seen()
	if prompt == "" {
		t.Fatal("the judge was never consulted")
	}
	if strings.Contains(prompt, "delete /tmp") {
		t.Fatalf("a system-originated run inherited a person's offer:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<system_request>") {
		t.Fatalf("a system-originated run must be labelled as one:\n%s", prompt)
	}
}

// The offer is withheld from a run with no current human author, and the query
// is not even made: a cron turn must not inherit the last thing a person was
// shown as if they had just accepted it. The judge prompt withholds it a second
// time, so this asserts the gateway's own gate rather than the combined effect.
func TestOfferAttachmentRespectsHumanAuthorship(t *testing.T) {
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordChannelMessage(ctx, *identity, "cli", "task-1", "assistant", "1. approve  2. authorize me"); err != nil {
		t.Fatal(err)
	}
	coord := (&Server{Control: store, DefaultTenantID: "default"}).coordinator()

	human := coord.intentSnapshotWithOffer(ctx, identity, nil, nil, nil,
		api.MessageRequest{Content: "2", Channel: "cli"}, "cli")
	if human.PriorAssistantOffer == "" {
		t.Fatal("a person's reply must carry what they were answering")
	}

	system := coord.intentSnapshotWithOffer(ctx, identity, nil, nil, nil,
		api.MessageRequest{Content: "continue", Channel: "cli", Origin: "cron"}, "cli")
	if system.PriorAssistantOffer != "" {
		t.Fatalf("a system-originated run picked up an offer: %q", system.PriorAssistantOffer)
	}

	// A channel with nothing said in it is not an error; the judge simply gets
	// no context rather than the wrong context.
	quiet := coord.intentSnapshotWithOffer(ctx, identity, nil, nil, nil,
		api.MessageRequest{Content: "2", Channel: "wechat"}, "wechat")
	if quiet.PriorAssistantOffer != "" {
		t.Fatalf("another channel's offer leaked in: %q", quiet.PriorAssistantOffer)
	}
}

// Approval evidence was captured once at run start and never refreshed, while
// the mode three lines above it re-resolved at every ask. A person who added a
// requirement mid-run had changed what the run was for, and every later
// approval was still judged against the opening message.
func TestApprovalEvidenceFollowsMidRunRequirements(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "gcp release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "gcp release")
	if err != nil {
		t.Fatal(err)
	}

	judge := &promptCapturingJudge{reply: `{"risk_level":"medium","user_authorization":"high","outcome":"approve","rationale":"ok"}`}
	daemon := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}
	coord := daemon.coordinator()
	cleanup := coord.installExecutionScope(ctx, identity, task, run, nil,
		api.MessageRequest{Content: "gcp 生产发布 lid-tm-warehouse-job-ci", Channel: "cli"})
	defer cleanup()

	exec := tools.SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ran", nil })
	dangerous := func() map[string]interface{} {
		return map[string]interface{}{
			"_tenant_id": identity.PersonID,
			"_tool_name": "terminal",
			"command":    "gcloud builds approve 49d9e7a8",
		}
	}

	_, _ = exec(dangerous())
	if before := judge.seen(); strings.Contains(before, "\n<person_added>\n") {
		t.Fatalf("nothing was added yet:\n%s", before)
	}

	// The person adds a requirement while the run is already working.
	if _, err := store.AcceptSteering(ctx, control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID,
		Channel: "cli", Content: "做一下自动的审批", ContentHash: "h1",
	}); err != nil {
		t.Fatal(err)
	}

	_, _ = exec(dangerous())
	after := judge.seen()
	if !strings.Contains(after, "\n<person_added>\n") || !strings.Contains(after, "做一下自动的审批") {
		t.Fatalf("the judge did not see the mid-run requirement:\n%s", after)
	}
	// The opening message is a supplement's context, not its replacement.
	if !strings.Contains(after, "gcp 生产发布") {
		t.Fatalf("the original request was dropped:\n%s", after)
	}
}

// A narrowing arrives the same way an addition does, and must constrain the
// rest of the run rather than only the turn that carried it.
func TestMidRunNarrowingReachesApprovalEvidence(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "cleanup", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "tidy the build outputs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcceptSteering(ctx, control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID,
		Channel: "cli", Content: "不要删除文件", ContentHash: "h2",
	}); err != nil {
		t.Fatal(err)
	}

	coord := (&Server{Control: store, DefaultTenantID: "default"}).coordinator()
	live := coord.intentWithAddedRequirements(ctx, identity,
		coord.intentSnapshotWithOffer(ctx, identity, task, run, nil,
			api.MessageRequest{Content: "tidy the build outputs", Channel: "cli"}, "cli"),
		run.ID)

	if !live.HasExplicitDeny() {
		t.Fatalf("a mid-run prohibition did not reach the run's evidence: %+v", live)
	}
}
