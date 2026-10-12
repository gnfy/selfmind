package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

const submittedFinalBody = "The source contains READY.\n\nThe requested inspection is complete."

type submittedFinalProvider struct {
	humanWaitProvider
	calls        atomic.Int32
	beforeFinish func()
	firstAction  *llm.ToolCall
}

type progressBatchProvider struct{ submittedFinalProvider }

func (p *progressBatchProvider) StreamChat(_ context.Context, request llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	n := p.calls.Add(1)
	plan := []map[string]interface{}{{"step": "Inspect sources", "status": "in_progress", "success_criteria": "Source was read"}, {"step": "Review the reported scope", "status": "pending", "success_criteria": "The requested scope is understood"}}
	if n > 1 {
		for i := len(request.Messages) - 1; i >= 0; i-- {
			m := request.Messages[i]
			if m.Name == "update_plan" {
				var result struct {
					Plan []map[string]interface{} `json:"plan"`
				}
				if json.Unmarshal([]byte(m.Content), &result) == nil {
					plan = result.Plan
				}
				break
			}
		}
	}
	if n >= 2 {
		plan[1]["status"] = "completed"
	}
	if n >= 3 {
		plan[0]["status"] = "completed"
	}
	args, _ := json.Marshal(map[string]interface{}{"plan": plan})
	calls := []llm.ToolCall{{ID: fmt.Sprintf("plan-%d", n), Function: "update_plan", Args: string(args)}}
	if n == 2 {
		calls = append(calls, llm.ToolCall{ID: "read", Function: "read_file", Args: `{"path":"input.txt"}`})
	}
	if n == 3 {
		args, _ = json.Marshal(map[string]interface{}{"status": "done", "summary": "Source inspected", "final_response": submittedFinalBody})
		calls = append(calls, llm.ToolCall{ID: "finish", Function: "finish_run", Args: string(args)})
	}
	out := make(chan llm.StreamEvent, 1)
	out <- llm.StreamEvent{ToolCalls: calls}
	close(out)
	return out, nil
}

func TestProgressBatchRechecksRealToolsAfterAcceptedDurablePlan(t *testing.T) {
	store := controltest.NewStore(t)
	provider := &progressBatchProvider{}
	dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
	dispatcher.RegisterTool(tools.NewReadFileTool())
	dispatcher.RegisterTool(tools.NewUpdatePlanTool())
	dispatcher.RegisterTool(tools.NewFinishRunTool())
	dispatcher.InjectMiddleware(tools.WorkspaceScopeMiddleware())
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 6, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), ExecutionRuntime: newTestExecutionRuntime(t), DefaultTenantID: "default"}
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	ws := humanWaitWorkspace(t, store, owner, "progress")
	if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("READY"), 0600); err != nil {
		t.Fatal(err)
	}
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "progress", WorkspaceID: ws.ID, Content: "Inspect input.txt and report the scope", ApprovalMode: "full-auto"})
	if code != 200 || response.Content != submittedFinalBody || provider.calls.Load() != 3 {
		t.Fatalf("batch cost or result: %d %+v calls=%d", code, response, provider.calls.Load())
	}
	events, err := store.ListTaskEvents(ctx, response.Task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, event := range events {
		if event.Type == "tool.started" && strings.Contains(string(event.Payload), `"tool":"read_file"`) {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("Plan progress batch discarded or replayed the read: %d", reads)
	}
	plan, err := store.LatestRunPlan(ctx, owner.TenantID, response.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps {
		if step.Status != "completed" {
			t.Fatalf("unfinished durable Plan: %+v", plan)
		}
	}
}

func (p *submittedFinalProvider) StreamChat(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	out := make(chan llm.StreamEvent, 1)
	count := p.calls.Add(1)
	if count == 1 {
		action := llm.ToolCall{ID: "read", Function: "read_file", Args: `{"path":"input.txt"}`}
		if p.firstAction != nil {
			action = *p.firstAction
		}
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{action}}
	} else {
		if count == 2 && p.beforeFinish != nil {
			p.beforeFinish()
		}
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"Source inspected","done":["Read input.txt"],"final_response":"The source contains READY.\n\nThe requested inspection is complete."}`}}}
	}
	close(out)
	return out, nil
}

func TestSubmittedFinalDoesNotDiscardAConcurrentSupplement(t *testing.T) {
	store := controltest.NewStore(t)
	provider := &submittedFinalProvider{}
	dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
	dispatcher.RegisterTool(tools.NewReadFileTool())
	dispatcher.RegisterTool(tools.NewFinishRunTool())
	dispatcher.InjectMiddleware(tools.WorkspaceScopeMiddleware())
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 4, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), ExecutionRuntime: newTestExecutionRuntime(t), DefaultTenantID: "default"}
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	ws := humanWaitWorkspace(t, store, owner, "supplement")
	if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("READY"), 0600); err != nil {
		t.Fatal(err)
	}
	const supplement = "Also inspect the independent destination after this result"
	provider.beforeFinish = func() {
		response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "supplement", WorkspaceID: ws.ID, Content: supplement})
		if code != 200 || !response.Accepted {
			t.Errorf("supplement not accepted: %d %+v", code, response)
		}
	}
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "supplement", WorkspaceID: ws.ID, Content: "Inspect input.txt", ApprovalMode: "full-auto"})
	if code != 200 || response.Content != submittedFinalBody {
		t.Fatalf("original response: %d %+v", code, response)
	}
	waitUntil(t, 2*time.Second, func() bool {
		tasks, _ := store.ListTasks(ctx, owner.TenantID, owner.PersonID, 10)
		for _, task := range tasks {
			if task.Title == supplement {
				return daemon.coordinator().activeCount(owner.PersonID) == 0
			}
		}
		return false
	}, "accepted supplement was lost at the immediate final-answer boundary")
}

func TestSubmittedFinalResponseCommitsOnceAndReportsStorageFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "before_commit_fault"}[fail], func(t *testing.T) {
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if fail {
				if _, err := raw.Exec(`CREATE TRIGGER fail_final_result BEFORE INSERT ON task_events WHEN NEW.type='run.finished' BEGIN SELECT RAISE(ABORT,'injected final result persistence failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			provider := &submittedFinalProvider{}
			if fail {
				provider.firstAction = &llm.ToolCall{ID: "write", Function: "write_file", Args: `{"path":"receipt.txt","content":"READY"}`}
			}
			dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
			dispatcher.RegisterTool(tools.NewReadFileTool())
			dispatcher.RegisterTool(tools.NewWriteFileTool())
			dispatcher.RegisterTool(tools.NewFinishRunTool())
			dispatcher.InjectMiddleware(tools.WorkspaceScopeMiddleware())
			agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 4, 1, nil)
			daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), ExecutionRuntime: newTestExecutionRuntime(t), DefaultTenantID: "default"}
			owner, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			ws := humanWaitWorkspace(t, store, owner, "final-body")
			if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("READY"), 0600); err != nil {
				t.Fatal(err)
			}
			input := "Inspect input.txt and report the result"
			if fail {
				input = "Write receipt.txt containing READY and report the result"
			}
			response, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "final-body", WorkspaceID: ws.ID, Content: input, ApprovalMode: "full-auto"})
			if response.Run == nil {
				t.Fatalf("no Run: %d %+v", code, response)
			}
			run, err := store.GetRun(context.Background(), owner.TenantID, response.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			content, err := store.RunAssistantContent(context.Background(), owner.TenantID, owner.PersonID, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if provider.calls.Load() != 2 {
				t.Fatalf("model calls=%d, want read + submitted final only", provider.calls.Load())
			}
			if fail {
				data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
				if err != nil || string(data) != "READY" {
					t.Fatalf("completed effect lost: %q %v", data, err)
				}
				events, _ := store.ListTaskEvents(context.Background(), response.Task.ID, 100)
				writes := 0
				for _, event := range events {
					if event.Type == "tool.started" && strings.Contains(string(event.Payload), `"tool":"write_file"`) {
						writes++
					}
				}
				if writes != 1 {
					t.Fatalf("completed effect dispatched %d times", writes)
				}
				if code != 200 || response.Error == "" || response.Outcome == nil || response.Outcome.CompletionReason != "finalization_failed" || run.Status != "blocked" || content == submittedFinalBody {
					t.Fatalf("failed persistence presented as success: %d %+v run=%+v saved=%q", code, response, run, content)
				}
				if !strings.Contains(response.Content, "not saved") {
					t.Fatalf("missing actionable persistence notice: %s", response.Content)
				}
			} else if code != 200 || run.Status != "done" || content != submittedFinalBody || response.Content != submittedFinalBody {
				t.Fatalf("final body lost: %d %+v saved=%q run=%+v", code, response, content, run)
			}
		})
	}
}
