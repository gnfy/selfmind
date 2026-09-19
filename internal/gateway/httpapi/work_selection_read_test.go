package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

type inspectBeforeResumeProvider struct {
	workSelectionProvider
	firstTool, firstArgs string
	selectionResult      string
}

func (p *inspectBeforeResumeProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls++
	out := make(chan llm.StreamEvent, 1)
	defer close(out)
	switch p.calls {
	case 1:
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "inspect", Function: p.firstTool, Args: p.firstArgs}}}
	case 2:
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "resume", Function: "work_select", Args: `{"action":"resume","run_id":"` + p.targetRunID + `"}`}}}
	case 3:
		for _, msg := range req.Messages {
			if msg.Role == "tool" && msg.ToolCallID == "resume" {
				p.selectionResult = msg.Content
			}
		}
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"Historical work handled.","next_steps":["The gateway will automatically continue the historical work."]}`}}}
	default:
		out <- llm.StreamEvent{Content: "Historical work handled."}
	}
	return out, nil
}

func TestReadBeforeResumeUsesActualDispatcherEffectClassification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate, large bool
	}{{name: "batch_read"}, {name: "large_read", large: true}, {name: "artifact_read"}, {name: "write_file", mutate: true}} {
		t.Run(tc.name, func(t *testing.T) {
			mutate := tc.mutate
			ctx := context.Background()
			store := controltest.NewStore(t)
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "prepared work", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, parent, "cli", "prepare then wait for confirmation")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "context.txt")
			content := "prepared"
			if tc.large {
				content = strings.Repeat(content, 8000)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			provider := &inspectBeforeResumeProvider{workSelectionProvider: workSelectionProvider{targetRunID: run.ID}, firstTool: "batch_read"}
			args := map[string]interface{}{"operations": []map[string]string{{"tool": "read_file", "path": path}}}
			if tc.large {
				provider.firstTool = "read_file"
				args = map[string]interface{}{"path": path}
			}
			if mutate {
				provider.firstTool = "write_file"
				args = map[string]interface{}{"path": path, "content": "changed"}
			}
			artifactDir := t.TempDir()
			if tc.name == "artifact_read" {
				personDir := filepath.Join(artifactDir, identity.PersonID)
				if err := os.MkdirAll(personDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(personDir, "art_readback123.txt"), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				provider.firstTool = "tool_output_view"
				args = map[string]interface{}{"artifact_id": "art_readback123"}
			}
			raw, _ := json.Marshal(args)
			provider.firstArgs = string(raw)
			dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
			dispatcher.RegisterTool(tools.NewReadFileTool())
			dispatcher.RegisterTool(tools.NewToolOutputViewTool(artifactDir))
			dispatcher.RegisterTool(tools.NewWriteFileTool())
			dispatcher.RegisterTool(tools.NewBatchReadTool(dispatcher.Dispatch, dispatcher.ToolExecutionMetadata))
			dispatcher.RegisterTool(tools.NewWorkSelectTool(store))
			dispatcher.RegisterTool(tools.NewFinishRunTool())
			agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 6, 1, nil)
			server := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default", ToolOutputDir: t.TempDir()}
			resp, status := server.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "Continue the prepared work"})
			if status != 200 || resp.Run == nil {
				t.Fatalf("status=%d resp=%+v", status, resp)
			}
			if !mutate {
				if resp.Run.ResumesRunID != run.ID {
					t.Fatalf("read-only inspection blocked continuation: run=%+v selection=%s", resp.Run, provider.selectionResult)
				}
				if tc.large {
					artifacts, err := store.ListTaskArtifacts(ctx, parent.ID, 10)
					if err != nil || len(artifacts) != 1 {
						t.Fatalf("read evidence lost on continuation: %+v err=%v", artifacts, err)
					}
					saved, err := os.ReadFile(artifacts[0].URI)
					if err != nil || string(saved) != content {
						t.Fatal("continued read output is not recoverable")
					}
				}
			} else {
				if resp.Run.ResumesRunID != "" {
					t.Fatal("mutation allowed an implicit scope transfer")
				}
				if provider.calls != 2 || resp.Outcome == nil || resp.Outcome.Status != "waiting_user" {
					t.Fatalf("refused continuation allowed another model turn: calls=%d run=%+v", provider.calls, resp.Run)
				}
				events, err := store.ListRunEvents(ctx, identity.TenantID, identity.PersonID, resp.Task.ID, resp.Run.ID, 100)
				if err != nil {
					t.Fatal(err)
				}
				finished := false
				for _, e := range events {
					if e.Type == "run.finished" {
						finished = true
						var payload struct {
							Outcome api.RunOutcome `json:"outcome"`
						}
						if err := json.Unmarshal(e.Payload, &payload); err != nil {
							t.Fatal(err)
						}
						if payload.Outcome.CompletionReason != "work_selection_rejected" || strings.Contains(strings.Join(payload.Outcome.NextSteps, " "), "automatically continue") {
							t.Fatalf("rejected outcome retained false handoff: %+v", payload.Outcome)
						}
					}
				}
				if !finished {
					t.Fatal("missing durable final outcome")
				}
				queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
				if err != nil || len(queued) != 0 {
					t.Fatalf("blocked continuation queued work: %+v err=%v", queued, err)
				}
			}
		})
	}
}
