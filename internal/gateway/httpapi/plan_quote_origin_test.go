package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

// authorizationsByStep reads each step's cancellation_authorized_by from a plan
// as the model and the client receive it, keyed by step id.
func authorizationsByStep(t *testing.T, plan interface{}) map[string]map[string]interface{} {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Plan []map[string]interface{} `json:"plan"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]interface{}{}
	for _, step := range decoded.Plan {
		id, _ := step["step_id"].(string)
		if origin, ok := step["cancellation_authorized_by"].(map[string]interface{}); ok {
			out[id] = origin
		}
	}
	return out
}

func assertAuthorization(t *testing.T, got map[string]interface{}, input string, at time.Time, earlier bool) {
	t.Helper()
	if got == nil {
		t.Fatalf("no authorization, want %s", input)
	}
	if got["input"] != input {
		t.Errorf("authorization input = %v, want %s", got["input"], input)
	}
	received, _ := got["received_at"].(string)
	if parsed, err := time.Parse(time.RFC3339, received); err != nil || parsed.Unix() != at.Unix() {
		t.Errorf("authorization received_at = %q, want %s", received, at.Format(time.RFC3339))
	}
	if flag, _ := got["earlier_run"].(bool); flag != earlier {
		t.Errorf("authorization earlier_run = %v, want %v", got["earlier_run"], earlier)
	}
}

// The plan projection names which person input authorized a cancellation: the
// kind of input and when it arrived, and whether an earlier Run of the same
// work received it. It never carries an id or any of the input's text.
func TestPlanProjectionNamesTheInputThatAuthorizedACancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "plan-authority", "Plan Authority")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "Prepare the release. Skip the live dashboard check this time.")
	if err != nil {
		t.Fatal(err)
	}
	guidance, err := store.AcceptSteering(ctx, control.SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Content: "I will rotate the keys myself. The vault code is vault-code-7781-sensitive."})
	if err != nil {
		t.Fatal(err)
	}
	if consumed, err := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, guidance.ID); err != nil || !consumed {
		t.Fatalf("consume guidance: %v %v", consumed, err)
	}
	projection := (&Server{Control: store}).coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	first, err := projection.Project(ctx, tools.PlanState{Plan: []tools.PlanStep{
		{Step: "Rotate the keys", Status: "pending"},
		{Step: "Check the live dashboard", Status: "pending"},
		{Step: "Write the changelog", Status: "pending"},
		{Step: "Prepare the release", Status: "in_progress"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rotate, live, changelog, release := first.Plan.Plan[0].StepID, first.Plan.Plan[1].StepID, first.Plan.Plan[2].StepID, first.Plan.Plan[3].StepID
	cancelled, err := projection.Project(ctx, tools.PlanState{Plan: []tools.PlanStep{
		{StepID: rotate, Status: "cancelled", CancellationDisposition: "user_takeover", CancellationReason: "the person rotates them",
			UserTakeoverQuote: "I will rotate the keys myself"},
		{StepID: live, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "the person removed the live check",
			ScopeChangeQuote: "Skip the live dashboard check this time"},
		{StepID: changelog, Status: "completed"},
		{StepID: release, Status: "in_progress"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	authorized := authorizationsByStep(t, cancelled.Plan)
	assertAuthorization(t, authorized[rotate], "guidance", guidance.CreatedAt, false)
	assertAuthorization(t, authorized[live], "request", run.StartedAt, false)
	if len(authorized) != 2 {
		t.Fatalf("only the two quoted cancellations name an input: %v", authorized)
	}
	data, err := json.Marshal(cancelled)
	if err != nil {
		t.Fatal(err)
	}
	// A complete marker cannot accidentally match an unrelated hex skill ref.
	for _, leaked := range []string{"vault-code-7781-sensitive", "vault code", guidance.ID, run.ID, "steering:", "run_input:", "run_started:"} {
		if strings.Contains(string(data), leaked) {
			t.Fatalf("the projection carries %q beyond the quote: %s", leaked, data)
		}
	}

	// A continuation shows the same inputs as an earlier Run's.
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", control.StartRunOptions{ResumesRunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	continued, err := (&Server{Control: store}).coordinator().newRunPlanProjection(identity, child, kernel.ToolInvocationScope{}).
		Project(ctx, tools.PlanState{Plan: []tools.PlanStep{
			{StepID: rotate, Status: "cancelled"}, {StepID: live, Status: "cancelled"},
			{StepID: changelog, Status: "completed"}, {StepID: release, Status: "in_progress"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	inherited := authorizationsByStep(t, continued.Plan)
	assertAuthorization(t, inherited[rotate], "guidance", guidance.CreatedAt, true)
	assertAuthorization(t, inherited[live], "request", run.StartedAt, true)
}

// A model cannot supply cancellation provenance. update_plan accepts a
// snapshot that echoes the runtime-owned fields of an earlier result, drops
// them, and the runtime records the input the quote actually matched.
func TestModelSuppliedCancellationProvenanceIsIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := controltest.NewStore(t)
	provider := &scriptedPlanProvider{script: []llm.StreamEvent{
		{ToolCalls: []llm.ToolCall{{ID: "plan-initial", Function: "update_plan",
			Args: `{"plan":[{"step":"draft the release notes","status":"in_progress"},{"step":"publish the release notes","status":"pending"}]}`}}},
		{ToolCalls: []llm.ToolCall{{ID: "plan-handoff", Function: "update_plan",
			Args: `{"plan":[{"step":"draft the release notes","status":"completed"},{"step":"publish the release notes","status":"cancelled",` +
				`"cancellation_disposition":"user_takeover","cancellation_reason":"the person publishes them",` +
				`"user_takeover_quote":"I will publish them myself","cancellation_quote_source":"steering:forged",` +
				`"cancellation_authorized_by":{"input":"guidance","received_at":"2020-01-02T03:04:05Z"}}]}`}}},
	}}
	dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
	dispatcher.RegisterTool(tools.NewUpdatePlanTool())
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 10, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-provenance", "Local")
	if err != nil {
		t.Fatal(err)
	}
	response, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local-provenance", Channel: "cli",
		Content: "Draft the release notes. I will publish them myself.",
	})
	if status != 200 || response.Run == nil {
		t.Fatalf("turn failed: status=%d response=%+v", status, response)
	}
	plan, err := store.LatestRunPlan(ctx, identity.TenantID, response.Run.ID)
	if err != nil || plan == nil || len(plan.Steps) != 2 {
		t.Fatalf("plan = %+v err=%v", plan, err)
	}
	if step := plan.Steps[1]; step.Status != "cancelled" || step.CancellationQuoteSource != "run_started:"+response.Run.ID {
		t.Fatalf("handed-over step = %s source %q, want cancelled run_started:%s", step.Status, step.CancellationQuoteSource, response.Run.ID)
	}
	if !provider.sawMessage("tool", `"cancellation_authorized_by":{"input":"request"`) {
		t.Fatal("the update_plan result did not name the request that authorized the handoff")
	}
	if provider.sawMessage("tool", "forged") || provider.sawMessage("tool", "2020-01-02") {
		t.Fatal("the update_plan result repeated provenance the model supplied")
	}
	events, err := store.ListRunEvents(ctx, identity.TenantID, identity.PersonID, response.Run.TaskID, response.Run.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	shown := false
	for _, event := range events {
		if event.Type == "plan.updated" && strings.Contains(string(event.Payload), `"cancellation_authorized_by":{"input":"request"`) {
			shown = true
		}
		// tool.started keeps the arguments as the model sent them; what the
		// runtime accepted and showed never carries them.
		if (event.Type == "plan.updated" || event.Type == "tool.completed") && strings.Contains(string(event.Payload), "forged") {
			t.Fatalf("event %s carries the model's source: %s", event.Type, event.Payload)
		}
	}
	if !shown {
		t.Fatal("no plan.updated event names the request that authorized the handoff")
	}
}

// An explicit resume restores the parent's plan for the client with the input
// that authorized each cancellation, as an earlier Run's input.
func TestResumedPlanNamesTheInputThatAuthorizedACancellation(t *testing.T) {
	provider := newSlowLLMProvider("continuing where we left off")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "release with a handoff")
	waiting, err := store.StartRun(ctx, parked, "cli", "Prepare the release. I will deploy to production myself.")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.SyncRunPlan(ctx, parked.TenantID, waiting.ID, "release", []control.RunPlanStepInput{
		{Step: "build the image", Status: "completed"},
		{Step: "deploy to production", Status: "pending"},
		{Step: "write the release notes", Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, parked.TenantID, waiting.ID, "handed over", []control.RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Status: "completed"},
		{StepID: first.Plan.Steps[1].StepID, Status: "cancelled", CancellationDisposition: "user_takeover",
			CancellationReason: "the person deploys", UserTakeoverQuote: "I will deploy to production myself"},
		{StepID: first.Plan.Steps[2].StepID, Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, parked.TenantID, waiting.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "/resume " + waiting.ID,
	})
	if status != 200 || !strings.Contains(resp.Content, "Selected run") {
		t.Fatalf("resume control: %+v", resp)
	}
	resp, status = daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "Proceed with the selected work."})
	if status != 200 || resp.Run == nil || resp.Run.ResumesRunID != waiting.ID {
		t.Fatalf("continuation turn: status=%d resp=%+v", status, resp)
	}
	events, err := store.ListRunEvents(ctx, parked.TenantID, parked.PersonID, parked.ID, resp.Run.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "plan.updated" || !strings.Contains(string(event.Payload), `"source":"resumed_run"`) {
			continue
		}
		authorized := authorizationsByStep(t, json.RawMessage(event.Payload))
		assertAuthorization(t, authorized[first.Plan.Steps[1].StepID], "request", waiting.StartedAt, true)
		// The client gets the input that authorized the cancellation, never
		// the recorded source, which would name the parent Run.
		var resumed struct {
			Plan json.RawMessage `json:"plan"`
		}
		if err := json.Unmarshal(event.Payload, &resumed); err != nil {
			t.Fatal(err)
		}
		for _, leaked := range []string{"cancellation_quote_source", "run_input:", waiting.ID} {
			if strings.Contains(string(resumed.Plan), leaked) {
				t.Fatalf("the resumed plan carries %q: %s", leaked, resumed.Plan)
			}
		}
		return
	}
	t.Fatalf("no resumed plan event: %+v", events)
}

// /status shows each assessed cancellation as the plan does: disposition,
// reason, and the input that authorized it. A step with none shows nothing
// more.
func TestStatusCardShowsCancellationDetail(t *testing.T) {
	received := time.Date(2025, time.March, 4, 14, 2, 0, 0, time.Local).Format(time.RFC3339)
	var plan []taskPlanStep
	if err := json.Unmarshal([]byte(`[
		{"step":"Build the release","status":"completed"},
		{"step":"Deploy to production","status":"cancelled","cancellation_disposition":"user_takeover",
		 "cancellation_reason":"the person deploys it","cancellation_quote_source":"run_input:run_secret",
		 "cancellation_authorized_by":{"input":"guidance","received_at":"`+received+`"}},
		{"step":"Check the live dashboard","status":"cancelled","cancellation_disposition":"not_required",
		 "cancellation_reason":"the release notes cover it"},
		{"step":"Read live queue depth","status":"pending","cancellation_disposition":"unfinished",
		 "cancellation_reason":"access is unavailable"},
		{"step":"Write the release notes","status":"in_progress"}]`), &plan); err != nil {
		t.Fatal(err)
	}
	card := formatTaskStatus(&control.Task{Title: "release", Status: "running"}, nil, nil, plan)
	for _, want := range []string{
		"- − Deploy to production\n  Cancelled · user takeover: the person deploys it · authorized by your guidance at Mar 4 14:02\n",
		"- − Check the live dashboard\n  Cancelled · not required: the release notes cover it\n",
		"- ○ Read live queue depth\n  Unfinished: access is unavailable\n",
		"- ✓ Build the release\n- − Deploy",
		"- → Write the release notes",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("status card lacks %q:\n%s", want, card)
		}
	}
	for _, leaked := range []string{"run_secret", "run_input:"} {
		if strings.Contains(card, leaked) {
			t.Errorf("status card shows %q:\n%s", leaked, card)
		}
	}
}
