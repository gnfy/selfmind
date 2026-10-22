package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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

// The provider only fixes call order. The real gateway, dispatcher, Skill
// activation, tool ledger and continuation transaction decide the result.
type skillBeforeResumeProvider struct {
	workSelectionProvider
	action, path, candidate, activationResult, selectionResult string
	obsoleteBeforeResume                                       bool
}

func (p *skillBeforeResumeProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls++
	out := make(chan llm.StreamEvent, 1)
	defer close(out)
	for _, msg := range req.Messages {
		if msg.Role != "tool" {
			continue
		}
		switch msg.ToolCallID {
		case "activate":
			p.activationResult = msg.Content
		case "resume":
			p.selectionResult = msg.Content
		}
	}
	if p.action == "batch_fallback" && p.calls == 2 {
		p.obsoleteBeforeResume = !strings.Contains(p.activationResult, `"context_expired":true`)
	}
	makeCall := func(id, name string, args interface{}) llm.ToolCall {
		raw, _ := json.Marshal(args)
		return llm.ToolCall{ID: id, Function: name, Args: string(raw)}
	}
	call := func(id, name string, args interface{}) {
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{makeCall(id, name, args)}}
	}
	stage := p.calls
	if p.action == "batch_fallback" && stage > 1 {
		stage++ // The first batch already abandons the selected Skill.
	}
	switch stage {
	case 1:
		for _, msg := range req.Messages {
			if msg.Role == "system" {
				p.candidate = regexp.MustCompile(`skref_[0-9a-f]{16}`).FindString(msg.Content)
			}
		}
		activation := makeCall("activate", "skill_select", map[string]interface{}{"candidate_ref": p.candidate, "reason": "Inspect prepared work"})
		if p.action == "batch_fallback" {
			out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{activation, makeCall("extra", "skill_fallback", map[string]interface{}{"reason": "Use ordinary planning"})}}
		} else {
			out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{activation}}
		}
	case 2:
		switch p.action {
		case "view":
			call("extra", "skill_view", map[string]interface{}{"name": "prepared-inspection"})
		case "fallback":
			call("extra", "skill_fallback", map[string]interface{}{"reason": "Use ordinary planning"})
		case "write":
			call("extra", "write_file", map[string]interface{}{"path": p.path, "content": "changed"})
		case "plan":
			call("extra", "update_plan", map[string]interface{}{"plan": []map[string]interface{}{{"step": "temporary interaction", "status": "in_progress"}}})
		default:
			call("extra", "read_file", map[string]interface{}{"path": p.path})
		}
	case 3:
		call("resume", "work_select", map[string]interface{}{"action": "resume", "run_id": p.targetRunID})
	case 4:
		call("plan", "update_plan", map[string]interface{}{"plan": []map[string]interface{}{{"step": "Inspect prepared work", "status": "completed"}}})
	case 5:
		call("finish", "finish_run", map[string]interface{}{"status": "done", "summary": "Prepared work inspected."})
	default:
		out <- llm.StreamEvent{Content: "Prepared work inspected."}
	}
	return out, nil
}

func TestSkillBookkeepingBeforeResumeUsesActualDispatcher(t *testing.T) {
	for _, action := range []string{"activate", "view", "fallback", "plan", "write", "batch_fallback"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			t.Setenv("SELFMIND_SKILLS_ROOTS", root)
			skillDir := filepath.Join(root, "prepared-inspection")
			if err := os.MkdirAll(skillDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: prepared-inspection\ndescription: Inspect prepared work\n---\n\n## Procedure\n"+strings.Repeat("Read the prepared result. Do not run any script.\n", 250)), 0600); err != nil {
				t.Fatal(err)
			}
			store := controltest.NewStore(t)
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "prepared work", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, parent, "cli", "Inspect prepared work")
			if err != nil {
				t.Fatal(err)
			}
			parentPlan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "prepared", []control.RunPlanStepInput{{Step: "Inspect prepared work", Status: "in_progress"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "prepared.txt")
			if err := os.WriteFile(path, []byte("prepared"), 0600); err != nil {
				t.Fatal(err)
			}
			provider := &skillBeforeResumeProvider{workSelectionProvider: workSelectionProvider{targetRunID: run.ID}, action: action, path: path}
			d := tools.NewDispatcherWithRegistry(tools.NewRegistry())
			for _, tool := range []tools.Tool{tools.NewSkillsListTool(store), tools.NewSkillSelectTool(store), tools.NewSkillViewTool(store), tools.NewSkillFallbackTool(store), tools.NewReadFileTool(), tools.NewWriteFileTool(), tools.NewWorkSelectTool(store), tools.NewUpdatePlanTool(), tools.NewFinishRunTool()} {
				d.RegisterTool(tool)
			}
			agent := kernel.NewAgent(memory.NewMemoryManager(nil), d, provider, "test agent", 10, 1, nil)
			srv := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default", ToolOutputDir: t.TempDir()}
			resp, status := srv.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "Continue the prepared work"})
			if status != 200 || resp.Run == nil {
				t.Fatalf("status=%d resp=%+v", status, resp)
			}
			events, err := store.ListRunEvents(ctx, identity.TenantID, identity.PersonID, resp.Run.TaskID, resp.Run.ID, 100)
			if err != nil {
				t.Fatal(err)
			}
			activated := false
			for _, event := range events {
				activated = activated || event.Type == "skill.activated"
			}
			if !activated {
				t.Fatalf("Skill activation failed before reaching the boundary: candidate=%q result=%q", provider.candidate, provider.activationResult)
			}
			if action == "write" {
				if resp.Run.ResumesRunID != "" || resp.Outcome == nil || resp.Outcome.CompletionReason != "work_selection_rejected" {
					t.Fatalf("material write did not close the window: %+v", resp)
				}
				return
			}
			if resp.Run.ResumesRunID != run.ID || resp.Run.TaskID != parent.ID {
				t.Fatalf("Skill bookkeeping blocked exact continuation: run=%+v", resp.Run)
			}
			plan, err := store.LatestRunPlan(ctx, identity.TenantID, resp.Run.ID)
			if err != nil || plan == nil || len(plan.Steps) != 1 || plan.Steps[0].SourceStepID != parentPlan.Plan.Steps[0].StepID {
				t.Fatalf("inherited Plan provenance lost: %+v err=%v", plan, err)
			}
			if strings.Contains(provider.selectionResult, "not claimed") {
				t.Fatalf("false refusal: %s", provider.selectionResult)
			}
			var selection struct {
				Retained bool `json:"active_skill_retained"`
			}
			if err := json.Unmarshal([]byte(provider.selectionResult), &selection); err != nil {
				t.Fatal(err)
			}
			if selection.Retained != (action != "plan" && action != "fallback" && action != "batch_fallback") {
				t.Fatalf("Skill retained across wrong unit: action=%s result=%s", action, provider.selectionResult)
			}
			if !selection.Retained && !strings.Contains(provider.activationResult, `"context_expired":true`) {
				t.Fatal("replaced Work Unit left obsolete Skill instructions in the model request")
			}
			if provider.obsoleteBeforeResume {
				t.Fatal("same-batch fallback left abandoned Skill instructions in the next model request")
			}
		})
	}
}
