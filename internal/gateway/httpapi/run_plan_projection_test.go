package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

// scriptedPlanProvider replays one stream event per model call and then keeps
// claiming the work is complete.
type scriptedPlanProvider struct {
	mu       sync.Mutex
	script   []llm.StreamEvent
	requests []llm.ChatRequest
}

func (p *scriptedPlanProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}

func (p *scriptedPlanProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *scriptedPlanProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	call := len(p.requests)
	p.mu.Unlock()
	event := llm.StreamEvent{Content: "All work is complete."}
	if call <= len(p.script) {
		event = p.script[call-1]
	}
	out := make(chan llm.StreamEvent, 1)
	out <- event
	close(out)
	return out, nil
}

func (p *scriptedPlanProvider) sawMessage(role string, needles ...string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, request := range p.requests {
		for _, message := range request.Messages {
			if message.Role != role {
				continue
			}
			found := true
			for _, needle := range needles {
				found = found && strings.Contains(message.Content, needle)
			}
			if found {
				return true
			}
		}
	}
	return false
}

// The stored plan keeps a requested but unassessed cancellation pending. The
// Run must not finish done over it: the completion gate reads the accepted
// projection, so the plan repair names the open step and the Run stays open.
func TestStoredOpenStepKeepsTheRunFromFinishingDone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct{ step, cancellation string }{
		{"verify in staging", `"status":"cancelled"`},
		{"publish the release notes", `"status":"cancelled","cancellation_disposition":"not_required","cancellation_reason":"not needed"`},
	} {
		t.Run(tc.step, func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			provider := &scriptedPlanProvider{script: []llm.StreamEvent{
				{ToolCalls: []llm.ToolCall{{ID: "plan-initial", Function: "update_plan",
					Args: `{"plan":[{"step":"implement the change","status":"in_progress"},{"step":"` + tc.step + `","status":"pending"}]}`}}},
				{ToolCalls: []llm.ToolCall{{ID: "plan-final", Function: "update_plan",
					Args: `{"plan":[{"step":"implement the change","status":"completed"},{"step":"` + tc.step + `",` + tc.cancellation + `}]}`}}},
			}}
			dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
			dispatcher.RegisterTool(tools.NewUpdatePlanTool())
			agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 10, 1, nil)
			daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-gate", "Local")
			if err != nil {
				t.Fatal(err)
			}
			response, status := daemon.ProcessMessage(ctx, api.MessageRequest{
				Platform: "cli", PlatformUserID: "local-gate", Channel: "cli",
				Content: "implement the change, then " + tc.step,
			})
			if status != 200 || response.Run == nil {
				t.Fatalf("turn failed: status=%d response=%+v", status, response)
			}
			plan, err := store.LatestRunPlan(ctx, identity.TenantID, response.Run.ID)
			if err != nil || plan == nil || len(plan.Steps) != 2 || plan.Steps[1].Status != "pending" {
				t.Fatalf("precondition: the store did not keep the step open: %+v %v", plan, err)
			}
			run, err := store.GetRun(ctx, identity.TenantID, response.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status == "done" || (response.Outcome != nil && response.Outcome.Status == "done") {
				t.Fatalf("the Run finished done over a pending stored step: run=%s outcome=%+v", run.Status, response.Outcome)
			}
			if !provider.sawMessage("user", "reconcile the visible plan", tc.step) {
				t.Fatal("the plan repair did not name the stored open step")
			}
			if !provider.sawMessage("tool", "remains pending:") {
				t.Fatal("the plan update did not tell Main what the cancellation lacked")
			}
		})
	}
}

func reviewFor(review []string, stepID string) string {
	for _, line := range review {
		if strings.HasPrefix(line, "Step "+stepID+" ") {
			return line
		}
	}
	return ""
}

// A kept-open cancellation names the one fact the runtime could not find and
// an exact way to settle it. A step planned without success_criteria must not
// be told to repeat an "original criterion" that never existed.
func TestCancellationReviewNamesTheMissingEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "plan-review", "Plan Review")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "dashboards", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "check the queue dashboards")
	if err != nil {
		t.Fatal(err)
	}
	projection := (&Server{Control: store}).coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	const current, live = "The current dashboard shows queue depth 7", "Current live queue depth is observed"
	first, err := projection.Project(ctx, tools.PlanState{Plan: []tools.PlanStep{
		{Step: "Check the old dashboard", Status: "pending"},
		{Step: "Check the current dashboard", Status: "in_progress", SuccessCriteria: current, WorkUnit: true},
		{Step: "Read live queue depth", Status: "pending", SuccessCriteria: live},
		{Step: "Export the dashboard history", Status: "pending"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	old, dashboard, depth, export := first.Plan.Plan[0].StepID, first.Plan.Plan[1].StepID, first.Plan.Plan[2].StepID, first.Plan.Plan[3].StepID
	assertReview := func(review []string, step string, want, unwanted []string) {
		t.Helper()
		line := reviewFor(review, step)
		if line == "" {
			t.Fatalf("no review for %s: %v", step, review)
		}
		for _, text := range want {
			if !strings.Contains(line, text) {
				t.Errorf("review for %s lacks %q: %s", step, text, line)
			}
		}
		for _, text := range unwanted {
			if strings.Contains(line, text) {
				t.Errorf("review for %s asks for %q: %s", step, text, line)
			}
		}
	}
	open, err := projection.Project(ctx, tools.PlanState{Plan: []tools.PlanStep{
		{StepID: old, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The current dashboard covers it", ReplacementStepID: dashboard},
		{StepID: dashboard, Status: "in_progress"},
		{StepID: depth, Status: "pending"},
		{StepID: export, Status: "cancelled"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	assertReview(open.AcceptanceReview, old, []string{"replacement step " + dashboard + " is in_progress, not completed", "states its own success_criteria"}, []string{"original criterion", "success_criteria is exactly"})
	assertReview(open.AcceptanceReview, export, []string{"no cancellation_disposition", "user_takeover_quote"}, []string{"original criterion"})
	// Following the review settles the criteria-less step; a step with a
	// criterion is held to it and told which bar the replacement missed.
	settled, err := projection.Project(ctx, tools.PlanState{Plan: []tools.PlanStep{
		{StepID: old, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The current dashboard covers it", ReplacementStepID: dashboard},
		{StepID: dashboard, Status: "completed"},
		{StepID: depth, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The dashboard is enough", ReplacementStepID: dashboard},
		{StepID: export, Status: "in_progress"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if settled.Plan.Plan[0].Status != "cancelled" || reviewFor(settled.AcceptanceReview, old) != "" {
		t.Fatalf("the stated replacement did not settle the step: %+v %v", settled.Plan.Plan[0], settled.AcceptanceReview)
	}
	assertReview(settled.AcceptanceReview, depth, []string{
		`has success_criteria "` + current + `" instead of this step's "` + live + `"`,
		`whose success_criteria is exactly "` + live + `"`,
	}, nil)
}
