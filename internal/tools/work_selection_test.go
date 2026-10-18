package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

func TestWorkSelectRecordsTypedPersonScopedProposal(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "target", Channel: "cli"})
	targetRun, _ := store.StartRun(ctx, targetTask, "cli", "target work")
	_ = store.FinishRun(ctx, person.TenantID, targetRun.ID, "interrupted")
	correctTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "correct target", Channel: "cli"})
	correctRun, _ := store.StartRun(ctx, correctTask, "cli", "correct target work")
	_ = store.FinishRun(ctx, person.TenantID, correctRun.ID, "interrupted")
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "progress question", Channel: "weixin"})
	// The interaction runs in another execution domain, so a resume stays a
	// typed proposal for the gateway's transfer commit instead of being
	// claimed in place.
	interactionRun, _ := store.StartRunWithOptions(ctx, interactionTask, "weixin", "how is target going", control.StartRunOptions{
		ExecutionRoots: []executionenv.RootBinding{{Path: "/weixin-scope", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace}},
	})

	tool := NewWorkSelectTool(store)
	result, err := tool.Execute(map[string]interface{}{
		"action": "resume", "run_id": targetRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: person.TenantID, PersonID: person.PersonID,
			TaskID: interactionTask.ID, RunID: interactionRun.ID, ExecutionLane: "main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"status":"proposed"`) || !strings.Contains(result, targetRun.ID) {
		t.Fatalf("proposal result = %s", result)
	}
	events, err := store.ListRunEvents(ctx, person.TenantID, person.PersonID, interactionTask.ID, interactionRun.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "work.selection" || !strings.Contains(string(events[0].Payload), `"action":"resume"`) {
		t.Fatalf("selection events = %+v", events)
	}
	// An identical retry converges. A different selection is an audited
	// correction while this interaction has produced only read-only evidence.
	if _, err := tool.Execute(map[string]interface{}{
		"action": "resume", "run_id": targetRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID},
	}); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	corrected, err := tool.Execute(map[string]interface{}{
		"action": "resume", "run_id": correctRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID},
	})
	if err != nil || !strings.Contains(corrected, `"status":"corrected"`) {
		t.Fatalf("safe correction = %s err=%v", corrected, err)
	}
	events, _ = store.ListRunEvents(ctx, person.TenantID, person.PersonID, interactionTask.ID, interactionRun.ID, 10)
	if len(events) != 2 || !strings.Contains(string(events[0].Payload), correctRun.ID) ||
		!strings.Contains(string(events[0].Payload), targetRun.ID) {
		t.Fatalf("correction audit = %+v", events)
	}
	claim, _ := store.ClaimToolDispatch(ctx, person.TenantID, control.ToolLedgerEntry{
		RunID: interactionRun.ID, ToolCallID: "write", ToolName: "patch", ArgsHash: "x", RetryClass: "side_effect",
	})
	if !claim.Execute {
		t.Fatal("failed to seed material effect")
	}
	_ = store.RecordToolOutcome(ctx, person.TenantID, interactionRun.ID, "write", true)
	if _, err := tool.Execute(map[string]interface{}{
		"action": "resume", "run_id": targetRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID},
	}); err == nil || !strings.Contains(err.Error(), "material effect") {
		t.Fatalf("post-effect correction err=%v", err)
	}
}

func TestWorkSelectRejectsForeignRun(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alice, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	bob, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "bob", "Bob")
	aliceTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: alice.TenantID, PersonID: alice.PersonID, Title: "current", Channel: "cli"})
	aliceRun, _ := store.StartRun(ctx, aliceTask, "cli", "current")
	bobTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: bob.TenantID, PersonID: bob.PersonID, Title: "private", Channel: "cli"})
	bobRun, _ := store.StartRun(ctx, bobTask, "cli", "private")
	tool := NewWorkSelectTool(store)
	if _, err := tool.Execute(map[string]interface{}{
		"action": "observe", "run_id": bobRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: alice.TenantID, PersonID: alice.PersonID, TaskID: aliceTask.ID, RunID: aliceRun.ID},
	}); err == nil {
		t.Fatal("foreign target must fail closed")
	}
}

// Resuming a run that has nothing left to resume is refused with what to do
// instead. qwen twice tried to resume a finished run named in its work
// history, retried, and reported the bare refusal to the person.
func TestWorkSelectSaysWhatToDoInsteadOfResumingSettledWork(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	finishedTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "release notes", Channel: "cli"})
	finished, _ := store.StartRun(ctx, finishedTask, "cli", "write the release notes")
	_ = store.FinishRun(ctx, person.TenantID, finished.ID, "done")
	lineTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "migration", Channel: "cli"})
	older, _ := store.StartRun(ctx, lineTask, "cli", "start the migration")
	_ = store.FinishRun(ctx, person.TenantID, older.ID, "done")
	latest, _ := store.StartRun(ctx, lineTask, "cli", "continue the migration")
	_ = store.FinishRun(ctx, person.TenantID, latest.ID, "interrupted")
	currentTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "continue", Channel: "cli"})
	current, _ := store.StartRun(ctx, currentTask, "cli", "继续")
	scope := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: currentTask.ID, RunID: current.ID}

	tool := NewWorkSelectTool(store)
	for _, tc := range []struct {
		target string
		want   []string
	}{
		{finished.ID, []string{"is done; it has nothing left to resume", "Do not retry resume", "in the current run"}},
		{older.ID, []string{"the resumable run of the same work is " + latest.ID, "Resume " + latest.ID + " instead"}},
	} {
		_, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": tc.target, "_invocation_scope": scope})
		var refusal interface {
			ToolErrorCode() string
			ModelSafeMessage() string
			ToolRecoveryHint() string
		}
		if !errors.As(err, &refusal) || refusal.ToolErrorCode() != "work_run_not_resumable" {
			t.Fatalf("resume %s: err=%v, want a typed not-resumable refusal", tc.target, err)
		}
		text := refusal.ModelSafeMessage() + " " + refusal.ToolRecoveryHint()
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("resume %s: refusal %q, want %q", tc.target, text, want)
			}
		}
	}
}

func TestWorkSelectDirectContinuationReportsPriorVerificationFacts(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "changed dependency"}[changed], func(t *testing.T) {
			ctx := context.Background()
			store, err := control.OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
			parentTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "check artifact", Channel: "cli"})
			parentRun, _ := store.StartRun(ctx, parentTask, "cli", "check artifact")
			first, err := store.SyncRunPlan(ctx, person.TenantID, parentRun.ID, "init", []control.RunPlanStepInput{
				{Step: "Preflight Jira and git state", Status: "in_progress", SuccessCriteria: "Jira issue and tags confirmed", VerificationRequired: true},
				{Step: "Prepare result", Status: "pending", SuccessCriteria: "result ready", VerificationRequired: true, WorkUnit: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			step1ID := first.Plan.Steps[0].StepID
			step2ID := first.Plan.Steps[1].StepID
			evidence, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
				"tool_call_id": "verify-step1", "kind": "verification", "status": "succeeded", "started_at_unix_nano": 10, "finished_at_unix_nano": 11,
				"command": map[string]interface{}{"command": "gh api repos/org/repo/tags", "kind": "command", "cwd": "/workspace", "binding": map[string]interface{}{
					"version": 3, "step_id": step1ID, "criterion": "Jira issue and tags confirmed", "target": step1ID,
				}},
			}})
			if _, err := store.AppendEvent(ctx, control.Event{TaskID: parentTask.ID, RunID: parentRun.ID, Type: "evidence.recorded", Payload: evidence}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SyncRunPlan(ctx, person.TenantID, parentRun.ID, "step 1 done", []control.RunPlanStepInput{
				{StepID: step1ID, Status: "completed", VerificationRequired: true},
				{StepID: step2ID, Status: "in_progress", VerificationRequired: true, WorkUnit: true},
			}); err != nil {
				t.Fatal(err)
			}
			if changed {
				mutation, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
					"tool_call_id": "later-change", "kind": "mutation", "status": "succeeded", "started_at_unix_nano": 20, "finished_at_unix_nano": 21,
					"files": []map[string]interface{}{{"path": "/workspace/state.json", "before_sha256": "before", "after_sha256": "after"}},
				}})
				if _, err := store.AppendEvent(ctx, control.Event{TaskID: parentTask.ID, RunID: parentRun.ID, Type: "evidence.recorded", Payload: mutation}); err != nil {
					t.Fatal(err)
				}
			}
			_ = store.FinishRun(ctx, person.TenantID, parentRun.ID, "interrupted")

			childTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "continue", Channel: "cli"})
			childRun, _ := store.StartRun(ctx, childTask, "cli", "continue checking artifact")
			tool := NewWorkSelectTool(store)
			raw, err := tool.Execute(map[string]interface{}{
				"action": "resume", "run_id": parentRun.ID,
				"_invocation_scope": kernel.ToolInvocationScope{
					ControlTenantID: person.TenantID, PersonID: person.PersonID,
					TaskID: childTask.ID, RunID: childRun.ID, ExecutionLane: "main",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var parsed struct {
				Status        string         `json:"status"`
				CommitMode    string         `json:"commit_mode"`
				ResumeContext string         `json:"resume_context"`
				Plan          []planStepView `json:"plan"`
			}
			if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
				t.Fatalf("unmarshal result: %v (%s)", err, raw)
			}
			if parsed.Status != "committed" || parsed.CommitMode != "direct" || len(parsed.Plan) != 2 {
				t.Fatalf("unexpected direct continuation payload: %+v", parsed)
			}
			if strings.Contains(raw, "reuse_prior_verification_available") || strings.Contains(raw, "do not re-run verify unnecessarily") {
				t.Fatal("resume must not offer a separate, weaker reuse verdict")
			}
			if !strings.Contains(parsed.ResumeContext, "prior_verification=passed") {
				t.Fatalf("missing historical check fact: %s", parsed.ResumeContext)
			}
			prior, err := store.ListInheritedPlanEvidence(ctx, person.TenantID, childRun.ID)
			if err != nil || len(prior) == 0 {
				t.Fatalf("prior evidence=%+v err=%v", prior, err)
			}
			if changed {
				if prior[0].ReuseBlockedReason == "" || !strings.Contains(parsed.ResumeContext, prior[0].ReuseBlockedReason) {
					t.Fatalf("missing blocked reason: prior=%+v context=%s", prior, parsed.ResumeContext)
				}
			} else if strings.Contains(parsed.ResumeContext, "reuse_blocked_reason=") {
				t.Fatalf("invented blocker: %s", parsed.ResumeContext)
			}
			for _, entry := range parsed.Plan {
				encoded, _ := json.Marshal(entry)
				if strings.Contains(string(encoded), "prior_verification") {
					t.Fatalf("observation metadata leaked into plan input: %s", encoded)
				}
			}
			_, adoptErr := store.SyncRunPlan(ctx, person.TenantID, childRun.ID, "assess inherited check", []control.RunPlanStepInput{
				{StepID: parsed.Plan[0].StepID, Status: "completed", VerificationRequired: true, ReusePriorVerification: true, ReuseReason: "same target and criterion remain sufficient"},
				{StepID: parsed.Plan[1].StepID, Status: "in_progress", VerificationRequired: true},
			})
			if changed && adoptErr == nil {
				t.Fatal("changed input must still prevent adoption")
			}
			if !changed && adoptErr != nil {
				t.Fatalf("valid explicit adoption refused: %v", adoptErr)
			}
		})
	}
}
