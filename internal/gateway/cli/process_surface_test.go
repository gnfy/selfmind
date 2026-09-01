package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"selfmind/internal/kernel/llm"
)

func TestProcessSurfaceGroupsToolsUnderActionNarration(t *testing.T) {
	surface := newProcessSurface()
	surface.Update(processEvent{
		kind:    processStreamDelta,
		content: "接下来检查触发器映射。\n",
		phase:   llm.AssistantPhaseCommentary,
	})

	effects := surface.Update(processEvent{
		kind:       processToolStarted,
		toolName:   "read_file",
		toolCallID: "call-1",
		toolArgs:   `{"path":"cloudbuild-ci.yml"}`,
	})

	if len(effects.commits) != 1 {
		t.Fatalf("commits = %+v, want one action narration", effects.commits)
	}
	groupID := effects.commits[0].ProcessGroupID
	if groupID == 0 || effects.commits[0].AssistantPhase != llm.AssistantPhaseCommentary {
		t.Fatalf("action commit = %+v, want grouped commentary", effects.commits[0])
	}

	frame := ansi.Strip(surface.Render(processViewport{width: 80, maxRows: 10}))
	if !strings.Contains(frame, "  ◦ ") {
		t.Fatalf("active tool must be visually nested under its action: %q", frame)
	}
	if len(surface.tools) != 1 || surface.tools[0].ProcessGroupID != groupID {
		t.Fatalf("active tools = %+v, want group %d", surface.tools, groupID)
	}
}

func TestTUIRoutesNarrationAndToolsThroughProcessSurface(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 80
	model.height = 24

	updated, _ := model.Update(MsgStream{
		Content: "接下来检查触发器映射。\n",
		Phase:   llm.AssistantPhaseCommentary,
	})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolStart{
		ToolName:   "read_file",
		ToolCallID: "call-1",
		Args:       `{"path":"cloudbuild-ci.yml"}`,
	})
	model = updated.(*uiModel)

	if len(model.messages) != 1 {
		t.Fatalf("committed messages = %+v, want only the action narration", model.messages)
	}
	groupID := model.messages[0].ProcessGroupID
	if groupID == 0 {
		t.Fatalf("action narration is not grouped: %+v", model.messages[0])
	}
	frame := ansi.Strip(model.renderActiveBlock(80))
	if !strings.Contains(frame, "  ◦ ") {
		t.Fatalf("active tool is not nested in the production frame: %q", frame)
	}
}

func TestTUIToolCompletionPreservesProcessGroup(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 80
	model.height = 24

	updated, _ := model.Update(MsgStream{Content: "Inspect the deployment values.\n", Phase: llm.AssistantPhaseCommentary})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolStart{
		ToolName: "read_file", ToolCallID: "call-1", Args: `{"path":"values.yaml"}`,
	})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolDone{
		ToolName: "read_file", ToolCallID: "call-1", Result: "42 lines", Duration: 0.2,
	})
	model = updated.(*uiModel)

	if len(model.messages) != 2 {
		t.Fatalf("messages = %+v, want narration and completed tool", model.messages)
	}
	groupID := model.messages[0].ProcessGroupID
	tool := model.messages[1]
	if groupID == 0 || tool.ProcessGroupID != groupID || !tool.Committed || tool.IsRunning {
		t.Fatalf("grouped tool = %+v, narration group = %d", tool, groupID)
	}
	if len(model.processState().tools) != 0 {
		t.Fatalf("completed tool remained active: %+v", model.processState().tools)
	}
	rendered := ansi.Strip(renderCell(tool, 80))
	if !strings.HasPrefix(rendered, "  • ") {
		t.Fatalf("committed tool is not nested: %q", rendered)
	}
}

func TestProcessSurfaceUnknownPhasePreviewResolvesAtBoundary(t *testing.T) {
	surface := newProcessSurface()
	surface.Update(processEvent{kind: processStreamDelta, content: "Deployment is ready."})

	preview := surface.Render(processViewport{width: 80, maxRows: 10})
	plain := strings.TrimSpace(ansi.Strip(preview))
	if plain != "Deployment is ready." {
		t.Fatalf("neutral preview = %q", plain)
	}
	if strings.Contains(plain, "›") || strings.HasPrefix(plain, "• ") || strings.Contains(preview, "\x1b[2m") {
		t.Fatalf("unresolved preview claimed a phase or used faint: %q", preview)
	}

	effects := surface.Update(processEvent{kind: processFinished, phase: llm.AssistantPhaseFinalAnswer})
	if len(effects.commits) != 1 {
		t.Fatalf("commits = %+v, want one final answer", effects.commits)
	}
	final := effects.commits[0]
	if final.AssistantPhase != llm.AssistantPhaseFinalAnswer || final.ProcessGroupID != 0 || final.Content != "Deployment is ready." {
		t.Fatalf("resolved final = %+v", final)
	}
	if active := strings.TrimSpace(surface.Render(processViewport{width: 80, maxRows: 10})); active != "" {
		t.Fatalf("resolved preview remained active: %q", active)
	}
}

func TestTUIUnknownPhasePreviewBecomesOneFinalAnswer(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 80
	model.height = 24

	updated, _ := model.Update(MsgStream{Content: "Deployment is ready."})
	model = updated.(*uiModel)
	preview := strings.TrimSpace(ansi.Strip(model.renderActiveBlock(80)))
	if preview != "Deployment is ready." {
		t.Fatalf("preview = %q", preview)
	}

	updated, _ = model.Update(MsgAgentDone{Response: "Deployment is ready."})
	model = updated.(*uiModel)
	if len(model.messages) != 1 {
		t.Fatalf("messages = %+v, want one final answer", model.messages)
	}
	final := model.messages[0]
	if final.AssistantPhase != llm.AssistantPhaseFinalAnswer || final.Content != "Deployment is ready." {
		t.Fatalf("final = %+v", final)
	}
	if active := strings.TrimSpace(model.renderActiveBlock(80)); active != "" {
		t.Fatalf("resolved preview remained active: %q", active)
	}
}

func TestProcessSurfaceKeepsIncompleteMarkdownTailLiteral(t *testing.T) {
	surface := newProcessSurface()
	surface.Update(processEvent{
		kind: processStreamDelta, phase: llm.AssistantPhaseCommentary,
		content: "Context is stable.\n\n- first",
	})
	first := strings.TrimSpace(ansi.Strip(surface.Render(processViewport{width: 80, maxRows: 10})))
	if !strings.Contains(first, "- first") || strings.Contains(first, "• first") {
		t.Fatalf("incomplete list was parsed before its block closed: %q", first)
	}

	surface.Update(processEvent{kind: processStreamDelta, content: "\n- second"})
	second := strings.TrimSpace(ansi.Strip(surface.Render(processViewport{width: 80, maxRows: 10})))
	if !strings.Contains(second, "- first") || !strings.Contains(second, "- second") || strings.Contains(second, "• first") {
		t.Fatalf("growing list changed an incomplete block semantically: %q", second)
	}

	surface.Update(processEvent{kind: processStreamDelta, content: "\n\n"})
	third := strings.TrimSpace(ansi.Strip(surface.Render(processViewport{width: 80, maxRows: 10})))
	if !strings.Contains(third, "• first") || !strings.Contains(third, "• second") {
		t.Fatalf("closed list did not gain semantic rendering: %q", third)
	}
}

func TestProcessSurfaceBoundsFrameWithoutLosingActionAnchor(t *testing.T) {
	surface := newProcessSurface()
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("evidence line %02d", i+1)
	}
	surface.Update(processEvent{
		kind: processStreamDelta, phase: llm.AssistantPhaseCommentary,
		content: strings.Join(lines, "\n"),
	})

	frame := ansi.Strip(surface.Render(processViewport{width: 80, maxRows: 5}))
	if height := lipgloss.Height(frame); height > 5 {
		t.Fatalf("process frame height = %d, want <= 5:\n%s", height, frame)
	}
	if !strings.HasPrefix(strings.TrimSpace(frame), "› evidence line 01") {
		t.Fatalf("bounded frame lost the action anchor: %q", frame)
	}
	if !strings.Contains(frame, "evidence line 20") {
		t.Fatalf("bounded frame lost the live tail: %q", frame)
	}
}

func TestTUIProcessBudgetKeepsComposerAndStatusVisible(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.width = 80
	model.height = 12
	model.activePlanJSON = `{"plan":[{"step":"Inspect configuration","status":"in_progress"},{"step":"Verify deployment","status":"pending"}]}`

	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("process line %02d", i+1)
	}
	updated, _ := model.Update(MsgStream{
		Content: strings.Join(lines, "\n"), Phase: llm.AssistantPhaseCommentary,
	})
	model = updated.(*uiModel)

	view := ansi.Strip(model.viewActiveRegion())
	if height := lipgloss.Height(view); height > model.height {
		t.Fatalf("active region height = %d, terminal height = %d:\n%s", height, model.height, view)
	}
	if !strings.Contains(view, "mode:") || !strings.Contains(view, "Updated plan") {
		t.Fatalf("composer/status or plan was displaced:\n%s", view)
	}
}
