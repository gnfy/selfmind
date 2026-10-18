package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	"selfmind/internal/verification"
)

// chainScript is one model whose flow does not know whether each approval it
// asks for will be answered inside the Run or after a park. It plans two steps,
// checks and completes the first, then writes one file per ask for the second,
// and closes both steps and the Run. observe runs just before the second call,
// where a real model would have run its check.
func chainScript(observe func(), files []string) func(n int32, receipt []map[string]interface{}) []llm.ToolCall {
	check := map[string]interface{}{"step": "Check the configuration", "success_criteria": "The configuration is valid", "verification_required": true}
	receipt := map[string]interface{}{"step": "Write the receipt", "success_criteria": "The receipt exists"}
	with := func(entry map[string]interface{}, status string) map[string]interface{} {
		out := map[string]interface{}{"status": status}
		for k, v := range entry {
			out[k] = v
		}
		return out
	}
	return func(n int32, _ []map[string]interface{}) []llm.ToolCall {
		switch {
		case n == 1:
			return []llm.ToolCall{planCall(n, with(check, "in_progress"), with(receipt, "pending"))}
		case n == 2:
			observe()
			return []llm.ToolCall{planCall(n, with(check, "completed"), with(receipt, "in_progress"))}
		case int(n) <= 2+len(files):
			args, _ := json.Marshal(map[string]string{"path": files[n-3], "content": "approved effect " + files[n-3]})
			return []llm.ToolCall{{ID: fmt.Sprintf("write-%d", n-2), Function: "write_file", Args: string(args)}}
		case int(n) == 3+len(files):
			return []llm.ToolCall{planCall(n, with(check, "completed"), with(receipt, "completed"))}
		default:
			return []llm.ToolCall{finishCall()}
		}
	}
}

type chainDaemon struct {
	daemon   *Server
	store    *control.Store
	owner    *control.IdentityContext
	ws       *control.Workspace
	provider *receiptScriptedProvider
}

func newChainDaemon(t *testing.T, inPlace bool, files []string) chainDaemon {
	t.Helper()
	provider := &receiptScriptedProvider{}
	var c chainDaemon
	provider.script = chainScript(func() {
		running, err := c.store.ListRunningRuns(context.Background(), c.owner.TenantID, []string{c.owner.PersonID})
		if err != nil || len(running) != 1 {
			t.Errorf("the observation needs exactly one running Run: %+v %v", running, err)
			return
		}
		recordCheck(t, c, running[0].ID)
	}, files)
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
		agents[i] = kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 10, 1, nil)
	}
	daemon := &Server{Control: store, Gateway: router.NewGateway(agents[0], nil), ExecutionRuntime: newTestExecutionRuntime(t), DefaultTenantID: "default", ApprovalWait: 10 * time.Second, ApprovalWaitUnattended: 10 * time.Second, HumanWaitHold: -1}
	if inPlace {
		daemon.HumanWaitHold, daemon.HumanWaitGrace = 0, 20*time.Millisecond
	}
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
	c = chainDaemon{daemon: daemon, store: store, owner: owner, ws: humanWaitWorkspace(t, store, owner, "chain"), provider: provider}
	return c
}

// recordCheck gives the Run the observation it would have made before it asked:
// the configuration check, bound to its step, of something outside the workspace
// so the files written later do not make it stale.
func recordCheck(t *testing.T, c chainDaemon, runID string) {
	t.Helper()
	ctx := context.Background()
	plan, err := c.store.LatestRunPlan(ctx, c.owner.TenantID, runID)
	if err != nil || plan == nil || len(plan.Steps) != 2 {
		t.Errorf("the Run has no plan to check against: %+v %v", plan, err)
		return
	}
	external := []string{}
	binding, err := c.store.ResolveVerificationBinding(ctx, c.owner.TenantID, runID, verification.Binding{StepID: plan.Steps[0].StepID, LocalDependencies: &external}, c.ws.LocalPath)
	if err != nil || binding == nil {
		t.Errorf("binding: %+v %v", binding, err)
		return
	}
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "verification", "tool_name": "verify", "tool_call_id": "check-config", "status": "succeeded",
		"started_at_unix_nano": int64(10), "finished_at_unix_nano": int64(11), "invoked": true,
		"process": map[string]interface{}{"started": true, "exit_code": 0},
		"command": map[string]interface{}{"command": "check config", "cwd": c.ws.LocalPath, "exit_code": 0, "binding": binding},
	}})
	if _, err := c.store.AppendEvent(ctx, control.Event{RunID: runID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Errorf("append evidence: %v", err)
	}
}

// chainProjection is what the work ended as, stripped of what only says which
// Run it happened in: its plan, the evidence it rests on, how it closed, what
// it wrote and how many model calls it took.
type chainProjection struct {
	Runs     int
	Steps    []string
	Evidence []string
	Outcome  map[string]interface{}
	Files    map[string]string
	Writes   int
	Model    int32
}

// runChain plays the whole chain once. With inPlace every ask is answered while
// its Run still holds it; otherwise every ask parks and its answer resumes it in
// another Run.
func runChain(t *testing.T, inPlace bool, files ...string) (chainProjection, *control.Run, chainDaemon) {
	t.Helper()
	ctx := context.Background()
	c := newChainDaemon(t, inPlace, files)
	attachHoldSession(t, c.daemon, c.owner.PersonID, "chain-session")
	response, code := c.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "chain-session", WorkspaceID: c.ws.ID,
		Content: "Write the receipts after approval", ApprovalMode: "smart", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
	var taskID string
	var final *control.Run
	for range files {
		var approval *control.ApprovalRequest
		var asked *control.Run
		if inPlace {
			approval, asked = waitHeldApproval(t, c.store, c.owner)
		} else {
			approval = waitParkedApproval(t, c.daemon, c.store, c.owner)
			asked, _ = c.store.GetRun(ctx, c.owner.TenantID, approval.RunID)
		}
		taskID, final = approval.TaskID, asked
		if _, err := c.daemon.respondApprovalByToken(ctx, c.owner, approval.ID, "approved", "chain-session", control.ApprovalDecisionInput{}); err != nil {
			t.Fatal(err)
		}
		if !inPlace {
			final = resumedChild(t, c.store, c.owner, approval)
		}
	}
	waitUntil(t, 20*time.Second, func() bool {
		run, _ := c.store.GetRun(ctx, c.owner.TenantID, final.ID)
		return run != nil && run.Status != "running" && c.daemon.coordinator().activeCount(c.owner.PersonID) == 0
	}, "the Run did not finish")
	final, _ = c.store.GetRun(ctx, c.owner.TenantID, final.ID)

	runs, _ := c.store.ListTaskRuns(ctx, c.owner.TenantID, taskID, 20)
	plan, err := c.store.LatestRunPlan(ctx, c.owner.TenantID, final.ID)
	if err != nil || plan == nil {
		t.Fatalf("final plan: %+v %v", plan, err)
	}
	out := chainProjection{Runs: len(runs), Files: map[string]string{}}
	for _, step := range plan.Steps {
		out.Steps = append(out.Steps, fmt.Sprintf("%s|%s|verify=%v", step.Step, step.Status, step.VerificationRequired))
	}
	events := taskEvents(t, c.store, c.owner, taskID)
	for _, event := range events {
		switch event.Type {
		case "evidence.recorded":
			var payload struct {
				Evidence struct {
					Kind     string `json:"kind"`
					ToolName string `json:"tool_name"`
					Status   string `json:"status"`
				} `json:"evidence"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Evidence.Kind != "" {
				out.Evidence = append(out.Evidence, fmt.Sprintf("%s|%s|%s", payload.Evidence.Kind, payload.Evidence.ToolName, payload.Evidence.Status))
			}
		case "run.outcome":
			if event.RunID == final.ID {
				if err := json.Unmarshal(event.Payload, &out.Outcome); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	sort.Strings(out.Evidence)
	for _, name := range files {
		data, _ := os.ReadFile(filepath.Join(c.ws.LocalPath, name))
		out.Files[name] = string(data)
	}
	out.Writes = countEvents(events, "tool.completed", `"tool":"write_file"`, `"invoked":true`)
	out.Model = c.provider.calls.Load()
	return out, final, c
}

// The hop is the only thing that differs: the same flow answered in place and
// answered after a park must end with the same plan, the same evidence, the same
// outcome and the same effects, and only the number of Runs it took may differ.
func TestAnswerInPlaceAndAnswerAfterParkEndInTheSameState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  []string
		parked int // Runs the parked flow needs
	}{
		{"one approval", []string{"receipt.txt"}, 2},
		{"two consecutive approvals", []string{"receipt.txt", "next.txt"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parked, _, _ := runChain(t, false, tc.files...)
			inPlace, final, c := runChain(t, true, tc.files...)

			if parked.Runs != tc.parked || inPlace.Runs != 1 {
				t.Fatalf("Runs: parked=%d (want %d) in place=%d (want 1)", parked.Runs, tc.parked, inPlace.Runs)
			}
			if inPlace.Outcome["status"] != "done" || inPlace.Outcome["completion_reason"] != "completed" {
				t.Fatalf("the in-place flow did not complete: %+v", inPlace.Outcome)
			}
			if inPlace.Writes != len(tc.files) {
				t.Fatalf("approved writes dispatched = %d, want %d", inPlace.Writes, len(tc.files))
			}
			a, b := parked, inPlace
			a.Runs, b.Runs = 0, 0
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("the hop changed the end state:\n parked   %+v\n in place %+v", a, b)
			}
			// What a Run boundary had to carry is simply absent in place: the check
			// stands on this Run's own observation, not on an adopted earlier one.
			plan, err := c.store.LatestRunPlan(context.Background(), c.owner.TenantID, final.ID)
			if err != nil || plan == nil || plan.Steps[0].ReusePriorVerification {
				t.Fatalf("an in-place Run adopted a prior check: %+v %v", plan, err)
			}
		})
	}
}
