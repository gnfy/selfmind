package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// The RUQX-1272 chain, replayed through the real daemon, dispatcher, approval
// funnel, durable plan and kernel loop with a scripted model: a Run parks on an
// approval while its first step is verified; the answer resumes it; the model
// cites the previous Run's check id in its closing judgment and is refused; the
// corrected finish is accepted. The progress must not move backwards, the
// approved write must happen exactly once, and the Run must report its result.
func TestApprovalChainKeepsProgressAndAcceptsOneCorrectedFinish(t *testing.T) {
	provider := &receiptScriptedProvider{}
	provider.script = func(n int32, _ []map[string]interface{}) []llm.ToolCall {
		switch n {
		case 1:
			return []llm.ToolCall{{ID: "pending-write", Function: "write_file", Args: `{"path":"receipt.txt","content":"approved effect"}`}}
		case 2:
			// The resumed Run completes its remaining step; the verified one is sent
			// back unchanged, as a model that read the plan would.
			return []llm.ToolCall{planCall(n,
				map[string]interface{}{"step": "Check the configuration", "status": "completed", "success_criteria": "The configuration is valid", "verification_required": true},
				map[string]interface{}{"step": "Write the receipt", "status": "completed", "success_criteria": "The receipt exists"})}
		case 3:
			args, _ := json.Marshal(map[string]interface{}{
				"status": "done", "final_response": submittedFinalBody,
				"assessments": []interface{}{map[string]interface{}{"result": "met", "evidence_refs": []interface{}{"toolcall-3-0"}, "reason": "The configuration was checked before the approval."}},
			})
			return []llm.ToolCall{{ID: "finish-cites-previous-run", Function: "finish_run", Args: string(args)}}
		default:
			return []llm.ToolCall{finishCall()}
		}
	}
	store := controltest.NewStore(t)
	agents := make([]*kernel.Agent, 2)
	for i := range agents {
		dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
		dispatcher.RegisterTool(tools.NewWriteFileTool())
		dispatcher.RegisterTool(tools.NewReadFileTool())
		dispatcher.RegisterTool(tools.NewUpdatePlanTool())
		dispatcher.RegisterTool(tools.NewFinishRunTool())
		dispatcher.InjectMiddleware(tools.SmartApprovalMiddleware(t.TempDir()))
		dispatcher.InjectMiddleware(tools.WorkspaceScopeMiddleware())
		agents[i] = kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 8, 1, nil)
	}
	// This chain pins the park-and-resume hop; the same chain answered in place is
	// compared against it in human_wait_hold_test.go.
	daemon := &Server{Control: store, Gateway: router.NewGateway(agents[0], nil), ExecutionRuntime: newTestExecutionRuntime(t), DefaultTenantID: "default", ApprovalWait: 10 * time.Second, ApprovalWaitUnattended: 10 * time.Second, HumanWaitHold: -1}
	daemon.Gateway.EnableWorkerPool(agents[1:])
	daemon.coordinator().activeLimit = 2
	owner, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.coordinator().stopAllActive("test cleanup")
		waitUntil(t, 2*time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "run ignored cancellation")
	})
	ctx := context.Background()
	ws := humanWaitWorkspace(t, store, owner, "ruqx-chain")

	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "ruqx-chain", WorkspaceID: ws.ID,
		Content: "Write a receipt after approval", ApprovalMode: "smart", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
	approval := waitParkedApproval(t, daemon, store, owner)
	parked := recordParkedProgress(t, store, owner, approval.RunID, ws.LocalPath)
	if got := planShapeOf(parked); got != "C>" {
		t.Fatalf("fixture: parked plan [%s]", got)
	}
	if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, "approved", "ruqx-chain", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	child := resumedChild(t, store, owner, approval)
	waitUntil(t, 10*time.Second, func() bool {
		run, _ := store.GetRun(ctx, owner.TenantID, child.ID)
		return run != nil && run.Status == "done" && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "the resumed Run did not finish")

	count := func(eventType string, contains ...string) int {
		events, err := store.ListTaskEvents(ctx, approval.TaskID, 400)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, event := range events {
			matched := event.Type == eventType
			for _, part := range contains {
				matched = matched && strings.Contains(string(event.Payload), part)
			}
			if matched {
				n++
			}
		}
		return n
	}
	if got := provider.calls.Load(); got != 4 {
		t.Fatalf("model calls = %d, want the write, the plan, the refused finish and the corrected finish", got)
	}
	if count("tool.completed", "condition_assessment_invalid") != 1 || count("tool.completed", "already stands on") != 1 {
		t.Fatal("the refusal for the previous Run's check did not say what that observation is")
	}
	if count("tool.completed", "tool_not_available") != 0 {
		t.Fatal("finish_run was hidden after the refusal")
	}
	if count("run.outcome", `"status":"done"`) != 1 {
		t.Fatal("the corrected finish did not close the Run")
	}
	data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
	if err != nil || string(data) != "approved effect" {
		t.Fatalf("the approved write: %q %v", data, err)
	}
	// The parked Run records the write as skipped; only the resumed Run dispatches it.
	if count("tool.completed", `"tool":"write_file"`, `"invoked":true`) != 1 {
		t.Fatalf("the approved write was dispatched %d times", count("tool.completed", `"tool":"write_file"`, `"invoked":true`))
	}
	plan, err := store.LatestRunPlan(ctx, owner.TenantID, child.ID)
	if err != nil || plan == nil || planShapeOf(plan.Steps) != "CC" || !plan.Steps[0].ReusePriorVerification {
		t.Fatalf("final plan: %+v %v", plan, err)
	}
}
