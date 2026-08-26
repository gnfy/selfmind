package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
)

func TestToolSemanticActionColors(t *testing.T) {
	cases := []struct {
		label string
		want  lipgloss.Color
	}{
		{label: "terminal", want: lipgloss.Color("5")},
		{label: "read_file", want: lipgloss.Color("6")},
		{label: "search_files", want: lipgloss.Color("6")},
		{label: "list_files", want: lipgloss.Color("6")},
		{label: "write_file", want: lipgloss.Color("3")},
		{label: "update_plan", want: lipgloss.Color("4")},
	}
	for _, tc := range cases {
		if got := toolSemanticActionStyle(tc.label).GetForeground(); got != tc.want {
			t.Fatalf("%s foreground = %v, want %v", tc.label, got, tc.want)
		}
	}
}

func TestAgentDoneEmptyResponseShowsError(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	updated, _ := model.Update(MsgAgentDone{})
	got := updated.(*uiModel)

	if got.runStatus != "error" {
		t.Fatalf("runStatus = %q, want error", got.runStatus)
	}
	if len(got.messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(got.messages))
	}
	if !got.messages[0].IsError {
		t.Fatalf("empty response message should be marked as error")
	}
	if !strings.Contains(got.messages[0].Content, "empty response") {
		t.Fatalf("message = %q, want empty response diagnostic", got.messages[0].Content)
	}
}

func TestAgentDoneDoesNotDuplicateStreamedResponse(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.messages = append(model.messages, ChatMessage{
		Role:      "assistant",
		Content:   "streamed answer",
		Timestamp: time.Now(),
	})

	updated, _ := model.Update(MsgAgentDone{Response: "streamed answer"})
	got := updated.(*uiModel)

	if got.runStatus != "done" {
		t.Fatalf("runStatus = %q, want done", got.runStatus)
	}
	if len(got.messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(got.messages))
	}
	if got.messages[0].Content != "streamed answer" {
		t.Fatalf("content = %q", got.messages[0].Content)
	}
}

func TestStreamBuffersPartialLineUntilFlush(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 20

	updated, cmd := model.Update(MsgStream{Content: "hello"})
	model = updated.(*uiModel)

	if len(model.messages) != 0 {
		t.Fatalf("stream chunk should not be committed to history yet: %+v", model.messages)
	}
	if model.liveStreamContent != "" {
		t.Fatalf("partial line should wait for flush, got %q", model.liveStreamContent)
	}
	if cmd == nil {
		t.Fatalf("partial line should schedule a stream flush")
	}

	updated, _ = model.Update(MsgStreamFlush(time.Now()))
	model = updated.(*uiModel)
	if model.liveStreamContent != "hello" {
		t.Fatalf("liveStreamContent = %q, want hello", model.liveStreamContent)
	}
	if view := stripANSI(model.renderActiveBlock(80)); !strings.Contains(view, "hello") {
		t.Fatalf("live stream was not rendered in the active region: %q", view)
	}
}

func TestStreamCommitsCompleteLineImmediately(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 20

	updated, _ := model.Update(MsgStream{Content: "hello\nnext"})
	model = updated.(*uiModel)

	if model.liveStreamContent != "hello\n" {
		t.Fatalf("liveStreamContent = %q, want completed line", model.liveStreamContent)
	}
	if len(model.messages) != 0 {
		t.Fatalf("complete stream line should still be live, not history: %+v", model.messages)
	}
}

func TestAgentDoneFinalizesLiveStreamOnce(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	updated, _ := model.Update(MsgStream{Content: "streamed answer\n"})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgAgentDone{Response: "streamed answer"})
	model = updated.(*uiModel)

	if len(model.messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(model.messages))
	}
	if model.messages[0].Content != "streamed answer" {
		t.Fatalf("content = %q", model.messages[0].Content)
	}
	if model.liveStreamContent != "" {
		t.Fatalf("live stream should be cleared, got %q", model.liveStreamContent)
	}
}

func TestToolStartFinalizesLiveStreamBeforeToolCell(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	updated, _ := model.Update(MsgStream{Content: "I will inspect it.\n"})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolStart{ToolName: "read_file", ToolCallID: "call-1", Args: `{"path":"README.md"}`})
	model = updated.(*uiModel)

	if len(model.messages) != 2 {
		t.Fatalf("messages len = %d, want assistant plus tool", len(model.messages))
	}
	if model.messages[0].Role != "assistant" || model.messages[0].Content != "I will inspect it." {
		t.Fatalf("first message not finalized assistant: %+v", model.messages[0])
	}
	if model.messages[1].Role != "tool" || model.messages[1].ToolName != "read_file" {
		t.Fatalf("second message not tool cell: %+v", model.messages[1])
	}
}

func TestAgentDoneErrorKeepsPartialResponse(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	updated, _ := model.Update(MsgAgentDone{Response: "partial answer", Err: io.ErrUnexpectedEOF})
	got := updated.(*uiModel)

	if got.runStatus != "error" {
		t.Fatalf("runStatus = %q, want error", got.runStatus)
	}
	if len(got.messages) != 2 {
		t.Fatalf("messages len = %d, want partial response plus error", len(got.messages))
	}
	if got.messages[0].Content != "partial answer" || got.messages[0].IsError {
		t.Fatalf("partial response was not preserved: %+v", got.messages[0])
	}
	if !got.messages[1].IsError {
		t.Fatalf("second message should be the error: %+v", got.messages[1])
	}
}

func TestWorkingTickKeepsAnimatingWhileThinking(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.thinking = true
	model.thinkingStart = time.Now()

	updated, cmd := model.Update(MsgWorkingTick(time.Now()))
	got := updated.(*uiModel)

	if got.thinkingDots != 1 {
		t.Fatalf("thinkingDots = %d, want 1", got.thinkingDots)
	}
	if cmd == nil {
		t.Fatalf("working tick should reschedule itself while thinking")
	}
}

func TestAgentActivityReplacesGenericWorkingText(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 20

	updated, _ := model.Update(MsgAgentActivity{Content: "Reading tool results and deciding the next step"})
	model = updated.(*uiModel)

	if !model.thinking {
		t.Fatalf("agent activity should show a thinking indicator")
	}
	if model.activityText != "Reading tool results and deciding the next step" {
		t.Fatalf("activityText = %q", model.activityText)
	}
	if view := stripANSI(model.renderActiveBlock(80)); !strings.Contains(view, "Reading tool results and deciding the next step") {
		t.Fatalf("activity text was not rendered in the active region: %q", view)
	}
}

func TestThinkingIndicatorRendersInActiveRegion(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 100
	model.height = 24
	model.messages = append(model.messages, ChatMessage{
		Role:    "user",
		Content: "analyze this project",
	})
	model.thinking = true
	model.activityText = "Thinking about the request"

	if view := stripANSI(model.renderActiveBlock(100)); !strings.Contains(view, "Thinking about the request") {
		t.Fatalf("activity indicator missing from the active region: %q", view)
	}
}

func TestToolStartClearsGenericActivityIndicator(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.thinking = true
	model.activityText = "Thinking about the request"

	updated, _ := model.Update(MsgToolStart{ToolName: "terminal", ToolCallID: "call-terminal", Args: `{"command":"go test ./..."}`})
	model = updated.(*uiModel)

	if model.thinking {
		t.Fatalf("tool start should replace the generic thinking indicator")
	}
	if model.activityText != "" {
		t.Fatalf("activityText = %q, want empty", model.activityText)
	}
	if len(model.messages) == 0 || model.messages[len(model.messages)-1].ToolName != "terminal" {
		t.Fatalf("tool message was not appended: %+v", model.messages)
	}
}

func TestRunningToolMessageShowsConciseProgress(t *testing.T) {
	rendered := stripANSI(renderToolMessage(ChatMessage{
		Role:          "tool",
		ToolName:      "ls_r",
		ToolArgs:      `{"path":"."}`,
		Timestamp:     time.Now().Add(-2 * time.Second),
		IsRunning:     true,
		RunningDetail: "scanned 120 entries",
	}, 120))

	if !strings.Contains(rendered, "Listing .") {
		t.Fatalf("running tool should render action: %q", rendered)
	}
	if !strings.Contains(rendered, "scanned 120 entries") {
		t.Fatalf("running tool should render heartbeat detail: %q", rendered)
	}
	if strings.Contains(rendered, "2.0s") {
		t.Fatalf("running tool should not render per-file elapsed time: %q", rendered)
	}
}

func TestGenericToolHeartbeatDoesNotBecomeNotification(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	updated, _ := model.Update(MsgToolStart{
		ToolName:   "terminal",
		ToolCallID: "call-terminal",
		Args:       `{"command":"go test ./..."}`,
	})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgToolHeartbeat{
		ToolName:   "terminal",
		ToolCallID: "call-terminal",
		Content:    "terminal running",
	})
	model = updated.(*uiModel)

	if model.statusMsg != "" {
		t.Fatalf("generic heartbeat should not set statusMsg: %q", model.statusMsg)
	}
	if len(model.messages) == 0 || model.messages[len(model.messages)-1].RunningDetail != "" {
		t.Fatalf("generic heartbeat should not be rendered as detail: %+v", model.messages)
	}
}

func TestToolDoneMatchesStartedToolByCallID(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	updated, _ := model.Update(MsgToolStart{
		ToolName:   "read_file",
		ToolCallID: "call-a",
		Args:       `{"path":"a.go"}`,
	})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolStart{
		ToolName:   "read_file",
		ToolCallID: "call-b",
		Args:       `{"path":"b.go"}`,
	})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgToolDone{
		ToolName:   "read_file",
		ToolCallID: "call-a",
		Result:     "package a",
		Duration:   0.2,
	})
	model = updated.(*uiModel)

	if len(model.messages) < 2 {
		t.Fatalf("messages len = %d, want at least 2", len(model.messages))
	}
	first := model.messages[len(model.messages)-2]
	second := model.messages[len(model.messages)-1]
	if first.ToolCallID != "call-a" || first.IsRunning {
		t.Fatalf("first tool should be completed by call id: %+v", first)
	}
	if second.ToolCallID != "call-b" || !second.IsRunning {
		t.Fatalf("second tool should still be running: %+v", second)
	}
}

func TestToolStartRejectsAnonymousMutableRow(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.thinking = true
	model.activityText = "Thinking"

	updated, _ := model.Update(MsgToolStart{ToolName: "read_file", Args: `{"path":"a.go"}`})
	model = updated.(*uiModel)

	if len(model.messages) != 0 {
		t.Fatalf("anonymous tool start created a mutable row: %+v", model.messages)
	}
	if !model.thinking || model.activityText != "Thinking" {
		t.Fatalf("rejected event changed active state: thinking=%v activity=%q", model.thinking, model.activityText)
	}
}

func TestOrphanToolCompletionGetsStandaloneHistoryCell(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.runStatus = "working"
	model.daemonRunActive = true
	model.daemonRunID = "run-current"
	ref := uiEventRef{Source: eventSourceDaemon, RunID: "run-current", EventID: "evt-orphan"}

	updated, _ := model.Update(MsgToolDone{
		ToolName:   "read_file",
		ToolCallID: "call-orphan",
		Result:     "package main",
		Duration:   0.2,
		Event:      ref,
	})
	model = updated.(*uiModel)

	if len(model.messages) != 1 {
		t.Fatalf("orphan completion messages = %+v", model.messages)
	}
	got := model.messages[0]
	if got.ToolCallID != "call-orphan" || got.RunID != "run-current" || got.IsRunning || !got.Committed || got.Content != "package main" {
		t.Fatalf("orphan completion cell = %+v", got)
	}

	// A replayed completion for the same call is a committed duplicate, not a
	// second orphan history row.
	updated, _ = model.Update(MsgToolDone{
		ToolName: "read_file", ToolCallID: "call-orphan", Result: "duplicate",
		Event: uiEventRef{Source: eventSourceDaemon, RunID: "run-current", EventID: "evt-orphan-replay"},
	})
	model = updated.(*uiModel)
	if len(model.messages) != 1 {
		t.Fatalf("duplicate completion created another row: %+v", model.messages)
	}
}

func TestLateCompletionFromDifferentRunDoesNotBecomeOrphan(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.runStatus = "working"
	model.daemonRunActive = true
	model.daemonRunID = "run-current"

	updated, _ := model.Update(MsgToolDone{
		ToolName:   "terminal",
		ToolCallID: "call-old",
		Result:     "late",
		Event: uiEventRef{
			Source: eventSourceDaemon, RunID: "run-old", EventID: "evt-old-complete",
		},
	})
	model = updated.(*uiModel)
	if len(model.messages) != 0 {
		t.Fatalf("late different-run completion rendered: %+v", model.messages)
	}
}

func TestToolOutputMatchesActiveToolByCallID(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	updated, _ := model.Update(MsgToolStart{ToolName: "terminal", ToolCallID: "call-a"})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolStart{ToolName: "terminal", ToolCallID: "call-b"})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgToolOutput{ToolName: "terminal", ToolCallID: "call-a", Content: "output-a"})
	model = updated.(*uiModel)

	if model.messages[0].Content != "output-a" {
		t.Fatalf("first tool output = %q, want output-a", model.messages[0].Content)
	}
	if model.messages[1].Content != "" {
		t.Fatalf("second tool was cross-wired: %+v", model.messages[1])
	}
}

func TestUnmatchedToolOutputDoesNotCreateAnonymousRow(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	updated, _ := model.Update(MsgToolOutput{ToolName: "terminal", ToolCallID: "missing", Content: "late output"})
	model = updated.(*uiModel)

	if len(model.messages) != 0 {
		t.Fatalf("unmatched output created a tool row: %+v", model.messages)
	}
}

func TestActiveToolOutputIsBounded(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	updated, _ := model.Update(MsgToolStart{ToolName: "terminal", ToolCallID: "call-a"})
	model = updated.(*uiModel)
	for _, line := range []string{"one", "two", "three", "four"} {
		updated, _ = model.Update(MsgToolOutput{ToolName: "terminal", ToolCallID: "call-a", Content: line})
		model = updated.(*uiModel)
	}

	if got := model.messages[0].Content; got != "two\nthree\nfour" {
		t.Fatalf("bounded output = %q", got)
	}
}

func TestAgentDoneCommitsUnfinishedToolRowsAsInterrupted(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	updated, _ := model.Update(MsgToolStart{ToolName: "terminal", ToolCallID: "call-a"})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolOutput{ToolName: "terminal", ToolCallID: "call-a", Content: "still running"})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgAgentDone{Response: "Finished."})
	model = updated.(*uiModel)

	if len(model.messages) != 2 {
		t.Fatalf("terminal cleanup messages = %+v", model.messages)
	}
	tool := model.messages[0]
	if tool.Role != "tool" || tool.IsRunning || !tool.IsError || !tool.Committed ||
		!strings.Contains(tool.Content, "Completion was not observed") {
		t.Fatalf("unfinished tool was not committed as interrupted: %+v", tool)
	}
	if model.messages[1].Role != "assistant" || model.messages[1].Content != "Finished." {
		t.Fatalf("final answer = %+v", model.messages[1])
	}
	if active := stripANSI(model.renderActiveBlock(100)); strings.TrimSpace(active) != "" {
		t.Fatalf("active region is not clean after completion: %q", active)
	}
}

func TestLateToolEventsAreIgnoredAfterAgentDone(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	updated, _ := model.Update(MsgAgentDone{Response: "Finished."})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgToolStart{ToolName: "terminal", ToolCallID: "late"})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolOutput{ToolName: "terminal", ToolCallID: "late", Content: "late output"})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolDone{ToolName: "terminal", ToolCallID: "late", Result: "late result"})
	model = updated.(*uiModel)

	if len(model.messages) != 1 || model.messages[0].Content != "Finished." {
		t.Fatalf("late events changed terminal history: %+v", model.messages)
	}
}

func TestCompletedToolMessageShowsPerStepDuration(t *testing.T) {
	rendered := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "read_file",
		ToolArgs: `{"path":"a.go"}`,
		Content:  "package a",
		Duration: 0.2,
	}, 120))

	if !strings.Contains(rendered, "Read a.go") {
		t.Fatalf("completed tool should render action: %q", rendered)
	}
	if !strings.Contains(rendered, "0.2s") {
		t.Fatalf("completed tool should render per-step duration: %q", rendered)
	}
}

func TestPatchToolMessageSummarizesStructuredJSON(t *testing.T) {
	rendered := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "patch",
		Content:  `{"Success":true,"Diff":"","FilesModified":["/tmp/app.go"],"FilesCreated":["/tmp/new.go"]}`,
		Duration: 0.1,
	}, 120))

	if strings.Contains(rendered, `"Success"`) || strings.Contains(rendered, `"FilesModified"`) {
		t.Fatalf("patch tool should not render raw JSON: %q", rendered)
	}
	for _, want := range []string{"Edited with patch", "modified /tmp/app.go", "created /tmp/new.go"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered patch message %q missing %q", rendered, want)
		}
	}
}

func TestStatusLineShowsTotalElapsedWhileToolRuns(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.runStatus = "working"
	model.localRequestActive = true
	model.thinkingStart = time.Now().Add(-3 * time.Second)
	model.toolExecuting = "ls_r"

	line := stripANSI(model.statusLine())
	if !strings.Contains(line, "working 3.") {
		t.Fatalf("status line should show total elapsed time: %q", line)
	}
	if strings.Contains(line, " · ls_r · ") {
		t.Fatalf("status line should not use current tool as the main progress indicator: %q", line)
	}
}

func TestStatusLineAlwaysShowsEffectiveApprovalMode(t *testing.T) {
	// Explicit session override wins.
	explicit := NewController(nil, nil, nil, "").model
	explicit.approvalMode = "auto-edit"
	if line := stripANSI(explicit.statusLine()); !strings.Contains(line, "mode:auto-edit") {
		t.Fatalf("status line should show explicit mode: %q", line)
	}

	// No override → the persisted mode learned from the digest is shown.
	c := NewController(nil, nil, nil, "")
	c.SetClientMode(true) // clears approvalMode → defers to persisted
	c.SetPersistedApprovalMode("smart")
	if line := stripANSI(c.model.statusLine()); !strings.Contains(line, "mode:smart") {
		t.Fatalf("status line should show persisted mode when no override: %q", line)
	}

	// Unknown (client mode, digest gave nothing) → on-request fallback.
	unknown := NewController(nil, nil, nil, "")
	unknown.SetClientMode(true)
	if line := stripANSI(unknown.model.statusLine()); !strings.Contains(line, "mode:smart") {
		t.Fatalf("status line should fall back to smart: %q", line)
	}

	// After /mode the bar updates to the new session mode.
	unknown.model.handleMode([]string{"full-auto"})
	if line := stripANSI(unknown.model.statusLine()); !strings.Contains(line, "mode:full-auto") {
		t.Fatalf("status line should update after /mode: %q", line)
	}
}

func TestAssistantMessageDoesNotRenderAsBulletList(t *testing.T) {
	rendered := stripANSI(renderAssistantMessage("hello\nworld", 80))

	if strings.Contains(rendered, "•") {
		t.Fatalf("assistant message should not use a bullet prefix: %q", rendered)
	}
	if !strings.Contains(rendered, "  hello") || !strings.Contains(rendered, "  world") {
		t.Fatalf("assistant message did not render expected body: %q", rendered)
	}
}

func TestComposerGapKeepsProgressAwayFromInput(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 100
	model.height = 24

	if got := model.composerGapHeight(); got != 1 {
		t.Fatalf("composer gap = %d, want 1 for normal-height terminal", got)
	}

	model.height = 10
	if got := model.composerGapHeight(); got != 0 {
		t.Fatalf("composer gap = %d, want 0 for compact terminal", got)
	}
}

func TestRenderMarkdownHidesCodeFenceMarkers(t *testing.T) {
	rendered := stripANSI(renderMarkdown("```go\nfmt.Println(\"hi\")\n```", 80))

	if strings.Contains(rendered, "```") {
		t.Fatalf("rendered markdown should not include fence markers: %q", rendered)
	}
	if strings.Contains(rendered, "code: go") {
		t.Fatalf("rendered markdown should not include synthetic code labels: %q", rendered)
	}
	if !strings.Contains(rendered, "fmt.Println(\"hi\")") {
		t.Fatalf("rendered markdown missing code body: %q", rendered)
	}
}

func TestToolMessageFormatsPlanJSON(t *testing.T) {
	rendered := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "update_plan",
		Content:  `{"plan":[{"step":"确认环境","status":"completed"},{"step":"写代码","status":"in_progress"}]}`,
		Duration: 0.1,
	}, 120))

	if strings.Contains(rendered, `{"plan"`) {
		t.Fatalf("plan JSON should not be rendered directly: %q", rendered)
	}
	// Codex-style checklist (hybrid): header `Updated plan · done/total` + per-step
	// glyphs ✔ completed / □ in-progress (cyan) / □ pending.
	if !strings.Contains(rendered, "Updated plan · 1/2") {
		t.Fatalf("plan should show progress count: %q", rendered)
	}
	if !strings.Contains(rendered, "✔ 确认环境") || !strings.Contains(rendered, "□ 写代码") {
		t.Fatalf("plan should show per-step status markers: %q", rendered)
	}
}

// TestForwardedPlanEventRendersChecklist proves a plan.updated stream event
// (as delivered in daemon-client mode) renders the full multi-step checklist —
// not a one-line "plan updated" note. It drives the same MsgToolStart/MsgToolDone
// pair forwardGatewayEvent emits from the event payload.
func TestForwardedPlanEventRendersChecklist(t *testing.T) {
	event := llm.StreamEvent{
		EventType: "plan.updated",
		Payload: map[string]interface{}{
			"explanation": "getting started",
			"plan": []interface{}{
				map[string]interface{}{"step": "read spec", "status": "completed"},
				map[string]interface{}{"step": "write code", "status": "in_progress"},
				map[string]interface{}{"step": "run tests", "status": "pending"},
			},
		},
	}
	planJSON := planJSONFromEvent(event)
	if planJSON == "" {
		t.Fatalf("planJSONFromEvent returned empty for a populated plan")
	}

	model := NewController(nil, nil, nil, "").model
	updated, _ := model.Update(MsgToolStart{ToolName: "update_plan", ToolCallID: "call-plan"})
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgToolDone{ToolName: "update_plan", ToolCallID: "call-plan", Result: planJSON})
	model = updated.(*uiModel)

	if len(model.messages) == 0 {
		t.Fatalf("expected a plan tool cell")
	}
	rendered := stripANSI(renderToolMessage(model.messages[len(model.messages)-1], 120))
	if !strings.Contains(rendered, "Updated plan · 1/3") {
		t.Fatalf("forwarded plan should render the checklist header: %q", rendered)
	}
	for _, step := range []string{"read spec", "write code", "run tests"} {
		if !strings.Contains(rendered, step) {
			t.Fatalf("forwarded plan missing step %q: %q", step, rendered)
		}
	}
	if strings.Contains(rendered, `{"plan"`) {
		t.Fatalf("forwarded plan should not render raw JSON: %q", rendered)
	}
}

// TestPlanChecklistShowsFullPlanUpToCap confirms a normal-sized plan renders in
// full (no truncation) and truncation fires only beyond the raised maxPlanSteps
// backstop.
func TestPlanChecklistShowsFullPlanUpToCap(t *testing.T) {
	build := func(n int) string {
		steps := make([]map[string]string, 0, n)
		for i := 0; i < n; i++ {
			steps = append(steps, map[string]string{"step": "step " + strconv.Itoa(i), "status": "pending"})
		}
		data, _ := json.Marshal(map[string]interface{}{"plan": steps})
		return string(data)
	}

	atCap := stripANSI(renderPlanCell(build(maxPlanSteps), 0, 120))
	if strings.Contains(atCap, "more steps") {
		t.Fatalf("a plan at the cap should render in full without truncation: %q", atCap)
	}
	if !strings.Contains(atCap, "step "+strconv.Itoa(maxPlanSteps-1)) {
		t.Fatalf("last step at the cap should be shown: %q", atCap)
	}

	over := stripANSI(renderPlanCell(build(maxPlanSteps+1), 0, 120))
	if !strings.Contains(over, "… 1 more steps") {
		t.Fatalf("a plan beyond the cap should truncate with a backstop note: %q", over)
	}
}

func TestToolMessageFormatsFinishRunJSON(t *testing.T) {
	rendered := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "finish_run",
		Content:  `{"status":"done","summary":"二分查找示例已完成"}`,
		Duration: 0.1,
	}, 120))

	if strings.Contains(rendered, `{"status"`) {
		t.Fatalf("finish_run JSON should not be rendered directly: %q", rendered)
	}
	if !strings.Contains(rendered, "Finished run") || !strings.Contains(rendered, "done · 二分查找示例已完成") {
		t.Fatalf("unexpected finish_run rendering: %q", rendered)
	}
}

func TestCommandToolMessageShowsOutputHead(t *testing.T) {
	output := "ok  \tselfmind/internal/kernel\t1.7s\nok  \tselfmind/internal/gateway/cli\t1.6s\nFAIL\tselfmind/internal/tools\nline4\nline5\nline6\nline7\nline8"
	rendered := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "terminal",
		ToolArgs: `{"command":"go test ./..."}`,
		Content:  output,
		Duration: 4.2,
	}, 120))

	if !strings.Contains(rendered, "Ran tests") {
		t.Fatalf("command message missing action: %q", rendered)
	}
	if strings.Contains(rendered, "go test ./...") {
		t.Fatalf("command header should use a semantic summary: %q", rendered)
	}
	if !strings.Contains(rendered, "selfmind/internal/kernel") {
		t.Fatalf("command message should show output head: %q", rendered)
	}
	if !strings.Contains(rendered, "4 more lines") || !strings.Contains(rendered, "line8") {
		t.Fatalf("command output should use a bounded head/tail preview: %q", rendered)
	}
}

func TestRunningCommandToolMessageHasNoEmptyOutputLine(t *testing.T) {
	rendered := stripANSI(renderToolMessage(ChatMessage{
		Role:      "tool",
		ToolName:  "terminal",
		ToolArgs:  `{"command":"go test ./..."}`,
		IsRunning: true,
	}, 120))

	if strings.Contains(rendered, "no output") || strings.Contains(rendered, "0 lines") {
		t.Fatalf("running command must not show an empty-output result line: %q", rendered)
	}
}

func TestReadFileToolMessageSummarizesSize(t *testing.T) {
	rendered := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "read_file",
		ToolArgs: `{"path":"main.go"}`,
		Content:  "package main\n\nfunc main() {}\n",
		Duration: 0.1,
	}, 120))

	if !strings.Contains(rendered, "Read main.go") {
		t.Fatalf("read_file message missing action: %q", rendered)
	}
	if !strings.Contains(rendered, "lines") {
		t.Fatalf("read_file message should report a line count: %q", rendered)
	}
	if strings.Contains(rendered, "package main") {
		t.Fatalf("read_file message should not echo file contents: %q", rendered)
	}
}

func TestPatchCellRendersAddWithGutterAndStat(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: game.html\n+<!doctype html>\n+<title>x</title>\n+done\n*** End Patch"
	out := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "patch",
		ToolArgs: `{"patch":` + strconv.Quote(patch) + `}`,
		Content:  `{"Success":true,"FilesCreated":["game.html"]}`,
		Duration: 0.3,
	}, 100))

	if !strings.Contains(out, "Added game.html (+3 -0)") {
		t.Fatalf("missing codex-style add header: %q", out)
	}
	if !strings.Contains(out, "1 + <!doctype html>") {
		t.Fatalf("new file should show a line-number gutter: %q", out)
	}
	if strings.Contains(out, "Edited with patch") {
		t.Fatalf("should use the new file-change renderer, not the generic verb: %q", out)
	}
}

func TestPatchCellRendersEditHunks(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: main.go\n@@ func main @@\n keep := context.Background()\n-old := 1\n+new := 2\n*** End Patch"
	out := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "patch",
		ToolArgs: `{"patch":` + strconv.Quote(patch) + `}`,
		Content:  `{"Success":true,"FilesModified":["main.go"]}`,
		Duration: 0.1,
	}, 100))

	if !strings.Contains(out, "Edited main.go (+1 -1)") {
		t.Fatalf("missing edit header: %q", out)
	}
	if !strings.Contains(out, "- old := 1") || !strings.Contains(out, "+ new := 2") {
		t.Fatalf("edit hunk should show colored +/- lines: %q", out)
	}
	if !strings.Contains(out, "@@ func main") {
		t.Fatalf("edit should show the hunk context hint: %q", out)
	}
}

func TestPatchCellBoundsLargeAdd(t *testing.T) {
	var b strings.Builder
	b.WriteString("*** Begin Patch\n*** Add File: big.txt\n")
	for i := 0; i < 120; i++ {
		b.WriteString("+line\n")
	}
	b.WriteString("*** End Patch")
	out := stripANSI(renderPatchCell(b.String(), 0, 100, maxPatchPreviewLines))

	if !strings.Contains(out, "Added big.txt (+120 -0)") {
		t.Fatalf("header should report full stat even when body is bounded: %q", out)
	}
	if !strings.Contains(out, "more line(s)") {
		t.Fatalf("large diff should be bounded with a remainder note: %q", out)
	}
}

func TestHistoryContentRendersUnboundedDiff(t *testing.T) {
	var p strings.Builder
	p.WriteString("*** Begin Patch\n*** Add File: big.txt\n")
	for i := 0; i < 120; i++ {
		p.WriteString("+line\n")
	}
	p.WriteString("*** End Patch")
	model := NewController(nil, nil, nil, "").model
	model.width = 100
	model.messages = []ChatMessage{{
		Role: "tool", ToolName: "patch",
		ToolArgs: `{"patch":` + strconv.Quote(p.String()) + `}`,
		Content:  `{"Success":true,"FilesCreated":["big.txt"]}`,
	}}

	out := stripANSI(model.renderHistoryContent(100))
	if strings.Contains(out, "more line(s)") {
		t.Fatalf("history view should show the full diff, not a bounded preview: %q", out)
	}
	if !strings.Contains(out, "120 + line") {
		t.Fatalf("history view should render all diff lines incl. #120: %q", out)
	}
}

func TestTrimHistoryWindowEvictsCommittedPrefix(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.messages = make([]ChatMessage, maxHistoryWindow+50)
	for i := range model.messages {
		model.messages[i] = ChatMessage{Role: "assistant", Content: "x", Committed: true}
	}
	model.trimHistoryWindow()
	if len(model.messages) != maxHistoryWindow {
		t.Fatalf("expected window trimmed to %d, got %d", maxHistoryWindow, len(model.messages))
	}

	// An uncommitted message at the front is never evicted.
	model.messages = make([]ChatMessage, maxHistoryWindow+50)
	model.messages[0] = ChatMessage{Role: "tool", IsRunning: true} // uncommitted
	for i := 1; i < len(model.messages); i++ {
		model.messages[i] = ChatMessage{Role: "assistant", Committed: true}
	}
	model.trimHistoryWindow()
	if len(model.messages) != maxHistoryWindow+50 {
		t.Fatalf("must not evict when an uncommitted cell is at the front: got %d", len(model.messages))
	}
}

func TestHandleCopyLastSelectsAssistant(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.handleCopyLast()
	if model.statusMsg != "No response to copy yet." {
		t.Fatalf("empty history should report nothing to copy: %q", model.statusMsg)
	}

	model.messages = []ChatMessage{
		{Role: "user", Content: "question"},
		{Role: "assistant", Content: "the answer"},
	}
	model.statusMsg = ""
	model.handleCopyLast()
	// Clipboard may be unavailable in CI; we only assert it attempted the copy
	// (i.e. found a response) rather than reporting "nothing to copy".
	if model.statusMsg == "No response to copy yet." || model.statusMsg == "" {
		t.Fatalf("should have attempted to copy the last assistant response: %q", model.statusMsg)
	}
}

// TestHybridSubmitDoesNotDeadlock drives the real bubbletea loop headlessly
// (piped input/output) and submits a prompt. The original bug committed cells
// via Program.Println synchronously inside Update, which deadlocked the loop on
// the first submit. With the fix (queue + flush as tea.Println Cmds) the loop
// keeps running, so p.Quit() is honored and Run returns within the timeout.
func TestHybridSubmitDoesNotDeadlock(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	c.SetMessageProcessor(func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		return api.MessageResponse{Content: "stub answer"}, 200
	})

	in := bytes.NewReader([]byte("hi\r")) // type "hi" then Enter → submit
	var out bytes.Buffer
	p := tea.NewProgram(c.model,
		tea.WithInput(in),
		tea.WithOutput(&out),
		tea.WithoutSignalHandler(),
	)
	c.model.program = p

	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()

	p.Send(tea.WindowSizeMsg{Width: 100, Height: 30})
	go func() {
		time.Sleep(700 * time.Millisecond) // let submit + commit + stub run
		p.Quit()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Skipf("headless bubbletea unavailable in this environment: %v", err)
		}
	case <-time.After(8 * time.Second):
		p.Kill()
		t.Fatal("hybrid program hung on submit — Update-loop deadlock not fixed")
	}
}

func TestHybridCommitsStartupCardToScrollbackOnce(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	model.updateInner(tea.WindowSizeMsg{Width: 100, Height: 30})
	if !model.startupCommitted {
		t.Fatal("startup card should be committed on first resize")
	}
	if len(model.pendingPrintln) != 1 || !strings.Contains(model.pendingPrintln[0], "SelfMind") {
		t.Fatalf("startup card should be queued to scrollback once: %v", model.pendingPrintln)
	}

	model.pendingPrintln = nil
	model.updateInner(tea.WindowSizeMsg{Width: 120, Height: 40})
	if len(model.pendingPrintln) != 0 {
		t.Fatalf("startup card must be committed only once, got: %v", model.pendingPrintln)
	}
}

func TestOnboardingStartupCardShowsModelPairWorkspaceAndFirstTask(t *testing.T) {
	controller := NewControllerWithGateway(nil, nil, nil, "codex-cli", "gpt-primary", nil, "")
	controller.SetOnboardingContext(OnboardingContext{
		BackgroundModel: "openai/gpt-background",
		WorkspaceID:     "ws-1", WorkspaceName: "project", WorkspacePath: "/work/project",
		FirstTaskPending: true,
	})
	rendered := stripANSI(strings.Join(controller.model.renderStartupCard(100), "\n"))
	for _, expected := range []string{"gpt-primary", "openai/gpt-background", "workspace:", "/work/project", "Try: Inspect this workspace"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("startup card missing %q:\n%s", expected, rendered)
		}
	}
}

func TestFirstSuccessfulOnboardingTaskIsRecordedOnce(t *testing.T) {
	controller := NewControllerWithGateway(nil, nil, nil, "codex-cli", "gpt-primary", nil, "")
	calls := 0
	controller.SetOnboardingContext(OnboardingContext{
		FirstTaskPending: true,
		OnFirstSuccess: func() error {
			calls++
			return nil
		},
	})

	updated, _ := controller.model.Update(MsgAgentDone{Input: "Inspect this project", Response: "Finished."})
	model := updated.(*uiModel)
	if calls != 1 || model.firstTaskPending {
		t.Fatalf("first completion calls=%d pending=%v", calls, model.firstTaskPending)
	}
	updated, _ = model.Update(MsgAgentDone{Input: "Inspect it again", Response: "Finished again."})
	model = updated.(*uiModel)
	if calls != 1 {
		t.Fatalf("receipt callback ran %d times, want once", calls)
	}
	if len(model.messages) < 3 || model.messages[1].Role != "notice" || !strings.Contains(model.messages[1].Content, "Setup complete") {
		t.Fatalf("missing one-shot setup completion notice: %+v", model.messages)
	}
}

func TestWriteFileCellRendersDiff(t *testing.T) {
	content := "Edited app.go (+1 -1)\n keep\n-old line\n+new line"
	out := stripANSI(renderToolMessage(ChatMessage{
		Role:     "tool",
		ToolName: "write_file",
		ToolArgs: `{"path":"app.go"}`,
		Content:  content,
		Duration: 0.2,
	}, 100))

	if !strings.Contains(out, "Edited app.go (+1 -1)") {
		t.Fatalf("missing write_file header: %q", out)
	}
	if !strings.Contains(out, "-old line") || !strings.Contains(out, "+new line") {
		t.Fatalf("write_file cell should show the colored diff: %q", out)
	}
}

func TestHybridCommitMarksMessageImmutable(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.messages = []ChatMessage{{Role: "user", Content: "hello"}}

	model.commit(&model.messages[0])
	if !model.messages[0].Committed {
		t.Fatalf("commit should mark the message committed")
	}
	model.commit(&model.messages[0]) // idempotent, no panic with nil program
}

func TestHybridActiveBlockShowsOnlyUncommitted(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.messages = []ChatMessage{
		{Role: "assistant", Content: "committed answer", Committed: true},
		{Role: "tool", ToolName: "terminal", ToolArgs: `{"command":"go test ./..."}`, IsRunning: true},
	}
	model.liveStreamContent = "streaming reply in progress"

	block := stripANSI(model.renderActiveBlock(80))
	if strings.Contains(block, "committed answer") {
		t.Fatalf("active block must not include committed cells: %q", block)
	}
	if !strings.Contains(block, "Running tests") {
		t.Fatalf("active block should show the in-progress tool: %q", block)
	}
	if !strings.Contains(block, "streaming reply in progress") {
		t.Fatalf("active block should show the live stream: %q", block)
	}
}

func TestHybridViewDoesNotReRenderCommittedHistory(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 24
	model.messages = []ChatMessage{{Role: "user", Content: "scrolled-away message", Committed: true}}

	view := stripANSI(model.viewModel())
	if strings.Contains(view, "scrolled-away message") {
		t.Fatalf("hybrid view must render only the active region, not committed history: %q", view)
	}
}

func TestNotificationBarStylesGuidanceDistinctly(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	model.setStatusNotice(noticeGuidance, "Sent to the running task as guidance.")
	guidance := model.notificationBar(80)
	if !strings.Contains(guidance, glyphArrowInto) {
		t.Fatalf("guidance notice should carry the steering glyph: %q", stripANSI(guidance))
	}
	if !strings.Contains(stripANSI(guidance), "Sent to the running task as guidance.") {
		t.Fatalf("guidance notice text missing: %q", stripANSI(guidance))
	}

	cases := []struct {
		message string
		kind    noticeKind
		glyph   string
	}{
		{"Copied to clipboard", noticeSuccess, glyphCheck},
		{"Guidance queue is full; try again", noticeWarning, glyphWarning},
		{"Task cancelled by user.", noticeError, glyphCross},
		{"Some neutral status", noticeInfo, glyphBullet},
	}
	for _, tc := range cases {
		model.setStatusNotice(tc.kind, tc.message)
		if got := model.notificationBar(80); !strings.Contains(got, tc.glyph) {
			t.Fatalf("notice %q should use glyph %q, got %q", tc.message, tc.glyph, stripANSI(got))
		}
		gotGlyph, gotColor := noticeVisual(tc.kind)
		if gotGlyph != tc.glyph || gotColor == "" {
			t.Fatalf("shared notice visual for %q = glyph %q color %q", tc.message, gotGlyph, gotColor)
		}
		if history := renderNoticeMessage(tc.message, tc.kind, 80); !strings.Contains(stripANSI(history), tc.message) {
			t.Fatalf("history notice lost text for %q: %q", tc.message, stripANSI(history))
		}
	}
}

func TestStatusNoticeClearDoesNotEraseNewerNotice(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	oldID := model.setStatusNotice(noticeSuccess, "first")
	newID := model.setStatusNotice(noticeWarning, "second")

	updated, _ := model.Update(MsgClearStatus{NoticeID: oldID})
	model = updated.(*uiModel)

	if model.statusMsg != "second" || model.statusNoticeID != newID {
		t.Fatalf("old timer cleared newer notice: text=%q id=%d", model.statusMsg, model.statusNoticeID)
	}
}

// TestClientModeSteerForwardsToDaemon: in client mode the run executes inside
// the daemon, so mid-run input must go through the steer function (gateway
// API), never the process-local channel the daemon cannot read.
func TestClientModeSteerForwardsToDaemon(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.clientMode = true
	model.thinking = true
	model.steerCh = make(chan string, 1) // local channel must stay untouched
	var got string
	model.steerFn = func(text string) error {
		got = text
		return nil
	}

	cmd := model.injectMidRunGuidance("focus on the edge cases")
	if cmd == nil {
		t.Fatal("expected the transient-notice auto-clear command")
	}
	if got != "focus on the edge cases" {
		t.Fatalf("steer function saw %q", got)
	}
	if len(model.steerCh) != 0 {
		t.Fatal("client mode must not push guidance into the local channel")
	}
	if model.statusMsg != "Sent to the running task as guidance." {
		t.Fatalf("statusMsg = %q", model.statusMsg)
	}
	last := model.messages[len(model.messages)-1]
	if last.Role != "user" || last.Content != "focus on the edge cases" {
		t.Fatalf("guidance missing from transcript: %+v", last)
	}
}

// TestClientModeSteerErrorShowsHonestNotice: a daemon refusal (no active run,
// full buffer, transport error) must surface in the transcript — mid-run input
// must never look accepted when it was dropped.
func TestClientModeSteerErrorShowsHonestNotice(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.clientMode = true
	model.thinking = true
	model.steerFn = func(text string) error {
		return errors.New("no active run to steer")
	}

	_ = model.injectMidRunGuidance("hurry up please")
	last := model.messages[len(model.messages)-1]
	if !last.IsError {
		t.Fatalf("expected an error notice, got %+v", last)
	}
	if !strings.Contains(last.Content, "did not accept the guidance") || !strings.Contains(last.Content, "no active run to steer") {
		t.Fatalf("notice must state the daemon refusal reason: %q", last.Content)
	}
	if model.statusMsg == "Sent to the running task as guidance." {
		t.Fatalf("statusMsg must not claim success: %q", model.statusMsg)
	}
}

// TestInProcessSteerStillUsesLocalChannel: without client mode the existing
// in-process behavior is unchanged — guidance lands on steerCh for the local
// agent loop to drain.
func TestInProcessSteerStillUsesLocalChannel(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.thinking = true
	model.steerCh = make(chan string, 1)

	_ = model.injectMidRunGuidance("prefer table-driven tests")
	select {
	case got := <-model.steerCh:
		if got != "prefer table-driven tests" {
			t.Fatalf("steered text = %q", got)
		}
	default:
		t.Fatal("guidance did not reach the local steering channel")
	}
	if model.statusMsg != "Sent to the running task as guidance." {
		t.Fatalf("statusMsg = %q", model.statusMsg)
	}
}

func TestDisplayModelNameShowsProviderAndModel(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.providerName = "kimi-coding"
	model.modelName = "kimi-for-coding"

	if got := model.displayModelName(); got != "kimi-coding/kimi-for-coding" {
		t.Fatalf("displayModelName = %q", got)
	}
}

func TestModelChangeReceiptKeepsRunningModelUntilObservedApplied(t *testing.T) {
	model := NewControllerWithGateway(nil, nil, nil, "codex-cli", "gpt-old", nil, "").model
	model.modelChangeObserver = func(context.Context, string) (ModelChangeObservation, error) {
		return ModelChangeObservation{}, nil
	}
	change := modelchange.Change{
		ID: "model_test", Status: modelchange.StatusAwaitingSafeBoundary, PhaseStartedAt: time.Now(),
		Previous:  modelchange.Snapshot{Primary: config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-old"}},
		Candidate: modelchange.Snapshot{Primary: config.ModelSelectionConfig{Provider: "codex-cli", Model: "gpt-new", Reasoning: "high"}},
	}
	updated, cmd := model.Update(MsgModelChangeDone{Response: api.ModelChangeResponse{Change: &change, RestartScheduled: true}})
	model = updated.(*uiModel)
	if got := model.displayModelName(); got != "codex-cli/gpt-old" {
		t.Fatalf("running display changed before health: %q", got)
	}
	if model.modelManagerStatus.ConfiguredPrimary != "codex-cli/gpt-new · reasoning=high" {
		t.Fatalf("configured primary = %q", model.modelManagerStatus.ConfiguredPrimary)
	}
	if model.modelChangeID != change.ID || cmd == nil {
		t.Fatalf("change was not observed: id=%q cmd=%v", model.modelChangeID, cmd)
	}
}

func TestModelRestartOfflinePreservesDraftInsteadOfPosting(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.modelGatewayOffline = true
	model.modelChangePhase = modelchange.StatusRestarting
	model.editor.SetValue("inspect the workspace")
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "inspect the workspace" {
		t.Fatalf("draft = %q", got)
	}
	if !strings.Contains(model.statusMsg, "gateway is restarting") {
		t.Fatalf("status = %q", model.statusMsg)
	}
}

func TestModelChangeSlowWarningIsShownOnlyOnce(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.modelChangeObserver = func(context.Context, string) (ModelChangeObservation, error) {
		return ModelChangeObservation{}, nil
	}
	status := modelchange.Status{Pending: &modelchange.Change{
		ID: "model_slow", Status: modelchange.StatusAwaitingSafeBoundary,
		CreatedAt: time.Now().Add(-31 * time.Second), PhaseStartedAt: time.Now().Add(-31 * time.Second),
	}}
	for i := 0; i < 2; i++ {
		updated, _ := model.Update(MsgModelChangeObserved{Observation: ModelChangeObservation{Status: status, GatewayReachable: true}})
		model = updated.(*uiModel)
	}
	warnings := 0
	for _, message := range model.messages {
		if strings.Contains(message.Content, "taking longer than 30 seconds") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("slow warnings = %d, want 1", warnings)
	}
}

func TestFormatUsageUnknownLimit(t *testing.T) {
	if got := formatUsage(42, 0); got != "42 run · ctx ?" {
		t.Fatalf("formatUsage = %q", got)
	}
}

func TestControllerUsesResolvedContextLength(t *testing.T) {
	cfg := testKimiConfig()
	model := NewController(nil, nil, cfg, "").model

	if model.tokenLimit != 262144 {
		t.Fatalf("tokenLimit = %d, want Kimi context length", model.tokenLimit)
	}
	if got := formatUsage(0, model.tokenLimit); got != "0 run · 262.1K ctx" {
		t.Fatalf("usage = %q", got)
	}
}

func TestCursorBlinkTickTogglesComposerCursor(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.cursorVisible = true

	updated, cmd := model.Update(MsgCursorBlinkTick(time.Now()))
	model = updated.(*uiModel)

	if model.cursorVisible {
		t.Fatalf("cursorVisible should toggle off")
	}
	if cmd == nil {
		t.Fatalf("cursor blink should reschedule itself")
	}
}

func TestSelectionActionBarIsNotRendered(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 24

	view := stripANSI(model.viewModel())

	if strings.Contains(view, "Selection active") || strings.Contains(view, "Right-click: copy") {
		t.Fatalf("selection action bar should not be rendered: %q", view)
	}
}

func TestInputHistoryNavigatesWithArrowKeys(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.recordInputHistory("first question")
	model.recordInputHistory("second question")

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "second question" {
		t.Fatalf("up value = %q, want second question", got)
	}

	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "first question" {
		t.Fatalf("second up value = %q, want first question", got)
	}

	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "second question" {
		t.Fatalf("down value = %q, want second question", got)
	}

	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "" {
		t.Fatalf("final down value = %q, want empty draft", got)
	}
}

func TestInputHistoryRestoresDraft(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.recordInputHistory("previous task")
	model.editor.SetValue("draft task")

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "previous task" {
		t.Fatalf("up value = %q, want previous task", got)
	}

	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "draft task" {
		t.Fatalf("restored draft = %q, want draft task", got)
	}
}

func TestInputHistoryDeduplicatesConsecutiveEntries(t *testing.T) {
	model := NewController(nil, nil, nil, "").model

	model.recordInputHistory("repeat")
	model.recordInputHistory("repeat")

	if got := len(model.inputHistory); got != 1 {
		t.Fatalf("history len = %d, want 1", got)
	}
}

func TestInputHistoryDoesNotStealMultilineArrowNavigation(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.recordInputHistory("previous")
	model.editor.SetValue("line one\nline two")

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(*uiModel)

	if got := model.editor.Value(); got != "line one\nline two" {
		t.Fatalf("multiline value = %q, want unchanged multiline draft", got)
	}
	if model.historyIndex != -1 {
		t.Fatalf("historyIndex = %d, want -1", model.historyIndex)
	}
}

func TestInputHistoryDoesNotStealSoftWrappedArrowNavigation(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.recordInputHistory("previous")

	// One logical line that soft-wraps across display rows: Up must move the
	// cursor, not swap in history (matches codex composer behaviour).
	draft := strings.Repeat("分析仓库结构", 8) // 96 display columns
	model.editor.SetLayoutWidth(40)      // text width 36 → several wrapped rows
	model.editor.SetValue(draft)

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(*uiModel)

	if got := model.editor.Value(); got != draft {
		t.Fatalf("soft-wrapped value = %q, want unchanged draft", got)
	}
	if model.historyIndex != -1 {
		t.Fatalf("historyIndex = %d, want -1", model.historyIndex)
	}
}

func TestInputHistoryContinuesFromRecalledMultilineEntry(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.recordInputHistory("older entry")
	model.recordInputHistory("multi\nline entry")

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "multi\nline entry" {
		t.Fatalf("recalled value = %q, want multi-line entry", got)
	}

	// The recall left the cursor at the end of the entry (a text boundary), so
	// a second Up keeps browsing history instead of moving the cursor.
	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "older entry" {
		t.Fatalf("second recall = %q, want older entry", got)
	}
}

// TestTUIGatewayControlCommandsRouteToDaemon proves the cross-endpoint fix: the
// TUI previously OMITTED gateway control commands (/approve, /reject, /stop,
// /id, /new, /resume, /workspace(s), /events, /notify), so typing /approve fell
// through to the skill/unknown path and never reached the approve lifecycle.
// They must now relay to the daemon through the message processor with the exact
// command text.
func TestTUIGatewayControlCommandsRouteToDaemon(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"/approve 1", "/approve 1"},
		{"/reject 2", "/reject 2"},
		{"/stop", "/stop"},
		{"/approvals", "/approvals"},
		{"/events", "/events"},
		{"/notify auto", "/notify auto"},
		{"/resume tsk_1", "/resume tsk_1"},
		{"/workspace ws_1", "/workspace ws_1"},
		{"/workspaces", "/workspaces"},
	} {
		model := NewController(nil, nil, nil, "").model
		var got string
		model.messageProcessor = func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
			got = req.Content
			return api.MessageResponse{Content: "ok"}, 200
		}
		cmd := model.handleCommand(tc.input)
		if cmd == nil {
			t.Fatalf("%q: expected a command (control commands must be routed, not dropped)", tc.input)
		}
		_ = cmd() // execute the relay
		if got != tc.want {
			t.Fatalf("%q routed %q to the gateway, want %q", tc.input, got, tc.want)
		}
	}
}

func testKimiConfig() *config.Config {
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "kimi-coding", Default: "kimi-for-coding"},
		ProviderProfiles: map[string]config.ProviderEndpoint{
			"kimi-coding": {APIKey: "sk-kimi-test"},
		},
	}
	cfg.Normalize()
	return cfg
}

// TestSlashCommandEchoesUserInputBeforeReply: a submitted control command must
// appear in the transcript as a user cell BEFORE its reply. Slash turns used
// to skip the echo normal chat turns get, so a control session (/workspaces →
// /workspace 2 → /resume …) rendered as disembodied replies (observed live:
// "我在cli里面输入的内容,好像也看不到了").
func TestSlashCommandEchoesUserInputBeforeReply(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	model := c.model
	c.SetClientMode(true)
	c.SetMessageProcessor(func(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		return api.MessageResponse{Content: "Open tasks:\n\n1. [paused] demo"}, 200
	})

	cmd := model.handleCommand("/tasks")
	if cmd == nil {
		t.Fatal("expected /tasks to return a command")
	}
	// The echo must be committed synchronously, before the reply arrives.
	if len(model.messages) == 0 {
		t.Fatal("no transcript messages after submit")
	}
	echo := model.messages[len(model.messages)-1]
	if echo.Role != "user" || echo.Content != "/tasks" {
		t.Fatalf("typed command not echoed as a user cell: %+v", echo)
	}

	updated, _ := model.Update(cmd())
	got := updated.(*uiModel)
	last := got.messages[len(got.messages)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "Open tasks") {
		t.Fatalf("control reply missing after echo: %+v", last)
	}
	// Order: user echo strictly before the reply.
	var userIdx, replyIdx = -1, -1
	for i, msg := range got.messages {
		if msg.Role == "user" && msg.Content == "/tasks" {
			userIdx = i
		}
		if msg.Role == "assistant" && strings.Contains(msg.Content, "Open tasks") {
			replyIdx = i
		}
	}
	if userIdx == -1 || replyIdx == -1 || userIdx >= replyIdx {
		t.Fatalf("echo/reply order wrong: user=%d reply=%d", userIdx, replyIdx)
	}
}

// TestSkillSlashDoesNotDoubleEchoInput: the skill-invocation fallback used to
// echo the raw input itself; now handleCommand owns the single echo.
func TestSkillSlashDoesNotDoubleEchoInput(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	_ = model.handleCommand("/definitely-not-a-command-xyz")
	count := 0
	for _, msg := range model.messages {
		if msg.Role == "user" && strings.Contains(msg.Content, "/definitely-not-a-command-xyz") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("typed command echoed %d times, want exactly 1", count)
	}
}
