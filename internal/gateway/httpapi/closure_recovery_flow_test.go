package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
)

func TestClosedVerificationCycleRefusesBeforeProcessDispatch(t *testing.T) {
	daemon, store, owner, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "observe")
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []control.RunPlanStepInput{{Step: "Observe", Status: "in_progress", SuccessCriteria: "healthy", WorkUnit: true}})
	if err != nil {
		t.Fatal(err)
	}
	step := p.Plan.Steps[0].StepID
	if _, err = store.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []control.RunPlanStepInput{{StepID: step, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	ctx = tools.WithRunPlanProjection(ctx, &controlRunPlanProjection{coordinator: daemon.coordinator(), identity: owner, run: run})
	root := t.TempDir()
	marker := filepath.Join(root, "dispatched")
	execute := tools.EvidenceMiddleware()(tools.NewVerifyTool().ExecuteResult)
	result, err := execute(map[string]interface{}{"_context": ctx, "command": "touch dispatched", "cwd": root, "step_id": step})
	var facts interface {
		ToolErrorCode() string
		ToolFailurePhase() string
		ToolEffectState() string
		ToolStateChanged() bool
		ToolRetryability() string
		ToolRecoveryHint() string
	}
	if !errors.As(err, &facts) || facts.ToolErrorCode() != "verification_cycle_closed" || facts.ToolFailurePhase() != "preparation" || facts.ToolEffectState() != "not_dispatched" || facts.ToolStateChanged() || facts.ToolRetryability() != "after_state_change" || (!strings.Contains(facts.ToolRecoveryHint(), "pending") || !strings.Contains(facts.ToolRecoveryHint(), "patch")) {
		t.Fatalf("lost preparation facts: result=%+v err=%v", result, err)
	}
	if result.Process != nil {
		t.Fatalf("closed check reached process: %+v", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("closed check dispatched: %v", err)
	}
}

// Scripted-provider contract eval: real gateway, Plan, observation, rollback and retry.
// It proves the recovery message path, not live model compliance.
func TestAtomicFinishRefusalShowsAcceptedPlanAndResubmitsPatchWithoutRepeatingWork(t *testing.T) {
	provider := &receiptScriptedProvider{}
	sawRefusal := false
	provider.observe = func(req llm.ChatRequest) {
		for _, m := range req.Messages {
			if m.Name != "finish_run" {
				continue
			}
			sawRefusal = true
			for _, want := range []string{"not committed", "plan_patch", "accepted_plan_version", "in_progress"} {
				if !strings.Contains(m.Content, want) {
					t.Errorf("refusal missing %q: %s", want, m.Content)
				}
			}
		}
	}
	provider.script = func(n int32, plan []map[string]interface{}) []llm.ToolCall {
		switch n {
		case 1:
			return []llm.ToolCall{planCall(n, map[string]interface{}{"step": "Inspect input", "status": "in_progress", "success_criteria": "Input contents are known"})}
		case 2:
			return []llm.ToolCall{{ID: "read", Function: "read_file", Args: `{"path":"input.txt"}`}}
		default:
			ref := "unavailable"
			if n > 3 {
				ref = "read"
			}
			if n > 4 {
				t.Errorf("unexpected recovery loop: %d", n)
			}
			args, _ := json.Marshal(map[string]interface{}{"status": "done", "final_response": submittedFinalBody, "plan_patch": []map[string]interface{}{{"step_id": plan[0]["step_id"], "status": "completed"}}, "assessments": []map[string]interface{}{{"step_id": plan[0]["step_id"], "result": "met", "evidence_refs": []string{ref}, "reason": "Read input contents"}}})
			return []llm.ToolCall{{ID: fmt.Sprintf("finish-%d", n), Function: "finish_run", Args: string(args)}}
		}
	}
	flow := newPlanFlowDaemon(t, provider)
	response, code := flow.daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "plan-flow", WorkspaceID: flow.ws.ID, Content: "Inspect input.txt and report the scope", ApprovalMode: "full-auto"})
	if code != 200 || response.Outcome == nil || response.Outcome.Status != "done" || !strings.HasPrefix(response.Content, submittedFinalBody) {
		t.Fatalf("correction did not complete: %d %+v", code, response)
	}
	if !sawRefusal || provider.calls.Load() != 4 {
		t.Fatalf("refusal/retry path missing: saw=%t calls=%d", sawRefusal, provider.calls.Load())
	}
	if flow.count(t, response.Task.ID, "tool.started", `"tool":"read_file"`) != 1 || flow.count(t, response.Task.ID, "tool.started", `"tool":"verify"`) != 0 || flow.count(t, response.Task.ID, "run.finished", "") != 1 {
		t.Fatal("correction repeated work or committed more than once")
	}
	plan, err := flow.store.LatestRunPlan(context.Background(), flow.owner.TenantID, response.Run.ID)
	if err != nil || plan.Version != 2 || plan.Steps[0].Status != "completed" {
		t.Fatalf("final patch lost: %+v %v", plan, err)
	}
}

func TestFinishRefusalBoundsAcceptedStateAndPreservesFailureMeaning(t *testing.T) {
	daemon, store, owner, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	var steps []control.RunPlanStepInput
	for i := 0; i < 17; i++ {
		steps = append(steps, control.RunPlanStepInput{Step: fmt.Sprintf("Private criterion %d", i), Status: "pending"})
	}
	plan, err := store.SyncRunPlan(ctx, owner.TenantID, run.ID, "", steps)
	if err != nil {
		t.Fatal(err)
	}
	session := &runCompletionSession{coordinator: daemon.coordinator(), identity: owner, run: run}
	for _, input := range []bool{true, false} {
		failure := session.completionRefusal(ctx, &completionFailure{error: errors.New("synthetic refusal"), input: input}, true).(*completionFailure)
		raw := failure.ModelSafeMessage()
		var receipt struct {
			Version int `json:"accepted_plan_version"`
			Steps   []struct {
				Status string `json:"status"`
			} `json:"accepted_steps"`
			More bool `json:"more_steps"`
		}
		idx := strings.Index(raw, `{"accepted_plan_version":`)
		if idx < 0 || json.Unmarshal([]byte(raw[idx:]), &receipt) != nil || receipt.Version != plan.Plan.Version || len(receipt.Steps) != 16 || !receipt.More {
			t.Fatalf("unbounded or invented accepted state: %s", raw)
		}
		if strings.Contains(raw, "Private criterion") || failure.ToolStateChanged() || failure.ToolEffectState() != "not_dispatched" {
			t.Fatalf("receipt changed state or disclosed prose: %s", raw)
		}
		want := "after_state_change"
		if input {
			want = "corrected_input"
		}
		if failure.ToolRetryability() != want {
			t.Fatal("refusal changed recovery budget class")
		}
	}
	stored, _ := store.LatestRunPlan(ctx, owner.TenantID, run.ID)
	if stored.Version != plan.Plan.Version || stored.Steps[0].Status != "pending" {
		t.Fatal("refusal committed progress")
	}
	failure := session.completionRefusal(ctx, errors.New("injected storage failure"), true)
	var commit interface{ ToolRunError() error }
	if !errors.As(failure, &commit) || !errors.Is(commit.ToolRunError(), errRunResultCommit) {
		t.Fatalf("storage failure became input retry: %v", failure)
	}
	failure = session.completionRefusal(ctx, control.ErrRunCancellationRequested, true)
	if !errors.As(failure, &commit) || !errors.Is(commit.ToolRunError(), context.Canceled) {
		t.Fatalf("cancellation became input retry: %v", failure)
	}
}
