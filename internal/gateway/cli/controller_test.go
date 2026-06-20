package cli

import (
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

func TestAssistantMessageDoesNotRenderAsBulletList(t *testing.T) {
	rendered := stripANSI(renderAssistantMessage("hello\nworld", 80))

	if strings.Contains(rendered, "•") {
		t.Fatalf("assistant message should not use a bullet prefix: %q", rendered)
	}
	if !strings.Contains(rendered, "  hello") || !strings.Contains(rendered, "  world") {
		t.Fatalf("assistant message did not render expected body: %q", rendered)
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
	if got := formatUsage(42, 0); got != "42/? tokens" {
		t.Fatalf("formatUsage = %q", got)
	}
}

func TestControllerUsesResolvedContextLength(t *testing.T) {
	cfg := testKimiConfig()
	model := NewController(nil, nil, cfg, "").model

	if model.tokenLimit != 262144 {
		t.Fatalf("tokenLimit = %d, want Kimi context length", model.tokenLimit)
	}
	if got := formatUsage(0, model.tokenLimit); got != "0/262.1K tokens" {
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
