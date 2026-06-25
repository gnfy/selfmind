package cli

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/platform/config"
)

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
	model.viewport.Width = 80
	model.viewport.Height = 15

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
	if view := stripANSI(model.renderAllMessages()); !strings.Contains(view, "hello") {
		t.Fatalf("live stream was not rendered: %q", view)
	}
}

func TestStreamCommitsCompleteLineImmediately(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 20
	model.viewport.Width = 80
	model.viewport.Height = 15

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
	model.viewport.Width = 80
	model.viewport.Height = 15

	updated, _ := model.Update(MsgAgentActivity{Content: "Reading tool results and deciding the next step"})
	model = updated.(*uiModel)

	if !model.thinking {
		t.Fatalf("agent activity should show a thinking indicator")
	}
	if model.activityText != "Reading tool results and deciding the next step" {
		t.Fatalf("activityText = %q", model.activityText)
	}
	if view := stripANSI(model.renderAllMessages()); !strings.Contains(view, "Reading tool results and deciding the next step") {
		t.Fatalf("activity text was not rendered: %q", view)
	}
}

func TestThinkingIndicatorHasGapAfterTranscript(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 100
	model.height = 24
	model.viewport.Width = 100
	model.viewport.Height = 18
	model.messages = append(model.messages, ChatMessage{
		Role:    "user",
		Content: "analyze this project",
	})
	model.thinking = true
	model.activityText = "Thinking about the request"

	lines := strings.Split(stripANSI(model.renderAllMessages()), "\n")
	activityLine := -1
	for i, line := range lines {
		if strings.Contains(line, "Thinking about the request") {
			activityLine = i
			break
		}
	}
	if activityLine <= 0 {
		t.Fatalf("activity line not found in rendered transcript: %q", strings.Join(lines, "\n"))
	}
	if strings.TrimSpace(lines[activityLine-1]) != "" {
		t.Fatalf("activity line should have a blank gap above it, previous line = %q", lines[activityLine-1])
	}
}

func TestToolStartClearsGenericActivityIndicator(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.thinking = true
	model.activityText = "Thinking about the request"

	updated, _ := model.Update(MsgToolStart{ToolName: "terminal", Args: `{"command":"go test ./..."}`})
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
	if !strings.Contains(rendered, "Updated plan") || !strings.Contains(rendered, "now: 写代码") {
		t.Fatalf("unexpected plan rendering: %q", rendered)
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

func TestDisplayModelNameShowsProviderAndModel(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.providerName = "kimi-coding"
	model.modelName = "kimi-for-coding"

	if got := model.displayModelName(); got != "kimi-coding/kimi-for-coding" {
		t.Fatalf("displayModelName = %q", got)
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

func TestMouseEventsDoNotCreateAppSelection(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 24
	model.viewport.Width = 80
	model.viewport.Height = 20

	events := []tea.MouseMsg{
		{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 10, Y: 5},
		{Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion, X: 70, Y: 12},
		{Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease, X: 70, Y: 12},
		{Button: tea.MouseButtonRight, Action: tea.MouseActionPress, X: 70, Y: 12},
	}
	for _, event := range events {
		updated, _ := model.Update(event)
		model = updated.(*uiModel)
	}
	if model.mouseDragActive {
		t.Fatalf("mouse drag state should be cleared after release")
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

func TestMouseWheelScrollsTranscriptOnlyInChatArea(t *testing.T) {
	model := scrollableTranscriptModel()
	model.viewModel()
	bottom := model.viewport.YOffset

	updated, _ := model.Update(tea.MouseMsg{
		Button: tea.MouseButtonWheelUp,
		Action: tea.MouseActionPress,
		X:      10,
		Y:      4,
	})
	model = updated.(*uiModel)
	if got := model.viewport.YOffset; got >= bottom {
		t.Fatalf("wheel in transcript did not scroll up: got %d, bottom %d", got, bottom)
	}

	scrolled := model.viewport.YOffset
	updated, _ = model.Update(tea.MouseMsg{
		Button: tea.MouseButtonWheelUp,
		Action: tea.MouseActionPress,
		X:      10,
		Y:      model.transcriptVisibleHeight() + 1,
	})
	model = updated.(*uiModel)
	if got := model.viewport.YOffset; got != scrolled {
		t.Fatalf("wheel outside transcript changed offset: got %d, want %d", got, scrolled)
	}
}

func TestMouseDragAtTranscriptEdgeAutoScrolls(t *testing.T) {
	model := scrollableTranscriptModel()
	model.viewModel()
	bottom := model.viewport.YOffset

	updated, _ := model.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		X:      10,
		Y:      5,
	})
	model = updated.(*uiModel)
	if !model.mouseDragActive {
		t.Fatalf("left press in transcript should start drag tracking")
	}

	updated, cmd := model.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionMotion,
		X:      10,
		Y:      0,
	})
	model = updated.(*uiModel)
	if got := model.viewport.YOffset; got >= bottom {
		t.Fatalf("dragging at top edge did not scroll up: got %d, bottom %d", got, bottom)
	}
	if cmd == nil {
		t.Fatalf("edge drag should schedule continuous auto-scroll")
	}

	updated, _ = model.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionRelease,
		X:      10,
		Y:      0,
	})
	model = updated.(*uiModel)
	if model.mouseDragActive || model.mouseAutoScrollDir != 0 {
		t.Fatalf("release should clear drag auto-scroll state")
	}
}

func TestSelectionActionBarIsNotRendered(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 24
	model.viewport.Width = 80
	model.viewport.Height = 20

	view := stripANSI(model.viewModel())

	if strings.Contains(view, "Selection active") || strings.Contains(view, "Right-click: copy") {
		t.Fatalf("selection action bar should not be rendered: %q", view)
	}
}

func TestRenderedTranscriptIgnoresAppSelectionState(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 24
	model.viewport.Width = 80
	model.viewport.Height = 20
	model.messages = []ChatMessage{
		{Role: "user", Content: "question"},
		{Role: "assistant", Content: "answer"},
	}

	rendered := model.renderAllMessages()

	if strings.Contains(rendered, "\x1b[") {
		t.Fatalf("rendered transcript should not contain selection ANSI styling: %q", rendered)
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

func TestTranscriptPageKeysScrollChatHistory(t *testing.T) {
	model := scrollableTranscriptModel()
	model.viewModel()
	bottom := model.viewport.YOffset
	if bottom <= 0 {
		t.Fatalf("test transcript is not scrollable, bottom offset = %d", bottom)
	}

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyPgUp})
	model = updated.(*uiModel)

	if got := model.viewport.YOffset; got >= bottom {
		t.Fatalf("PageUp did not scroll transcript up: got offset %d, bottom %d", got, bottom)
	}

	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyPgDown})
	model = updated.(*uiModel)

	if got := model.viewport.YOffset; got != bottom {
		t.Fatalf("PageDown offset = %d, want bottom %d", got, bottom)
	}
}

func TestTranscriptScrollPositionSurvivesDraftAndWorkingRender(t *testing.T) {
	model := scrollableTranscriptModel()
	model.viewModel()
	bottom := model.viewport.YOffset

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyPgUp})
	model = updated.(*uiModel)
	scrolled := model.viewport.YOffset
	if scrolled >= bottom {
		t.Fatalf("PageUp did not move away from bottom: got %d, bottom %d", scrolled, bottom)
	}

	model.editor.SetValue("draft while reading")
	model.thinking = true
	model.viewModel()

	if got := model.viewport.YOffset; got != scrolled {
		t.Fatalf("viewModel snapped to offset %d, want preserved offset %d", got, scrolled)
	}
}

func TestStreamDoesNotSnapToBottomAfterManualTranscriptScroll(t *testing.T) {
	model := scrollableTranscriptModel()
	model.viewModel()

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyPgUp})
	model = updated.(*uiModel)
	scrolled := model.viewport.YOffset

	updated, _ = model.Update(MsgStream{Content: "new chunk"})
	model = updated.(*uiModel)

	if got := model.viewport.YOffset; got != scrolled {
		t.Fatalf("stream snapped to offset %d, want preserved offset %d", got, scrolled)
	}
}

func TestCtrlArrowKeysScrollTranscriptAndKeepInputHistory(t *testing.T) {
	model := scrollableTranscriptModel()
	model.recordInputHistory("previous task")
	model.viewModel()
	bottom := model.viewport.YOffset

	updated, _ := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlUp})
	model = updated.(*uiModel)
	if got := model.viewport.YOffset; got >= bottom {
		t.Fatalf("Ctrl+Up did not scroll transcript up: got %d, bottom %d", got, bottom)
	}

	updated, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(*uiModel)
	if got := model.editor.Value(); got != "previous task" {
		t.Fatalf("plain Up should still navigate input history, got %q", got)
	}
}

func scrollableTranscriptModel() *uiModel {
	model := NewController(nil, nil, nil, "").model
	model.width = 80
	model.height = 18
	model.viewport.Width = 80
	model.viewport.Height = 12
	for i := 0; i < 40; i++ {
		model.messages = append(model.messages, ChatMessage{
			Role:    "assistant",
			Content: strings.Repeat("line content ", 3),
		})
	}
	return model
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
