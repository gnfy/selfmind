package cli

import (
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	uitheme "selfmind/internal/ui/theme"
)

type processEventKind uint8

const (
	processStreamDelta processEventKind = iota + 1
	processToolStarted
	processToolOutput
	processToolHeartbeat
	processToolCompleted
	processToolsInterrupted
	processToolsDiscarded
	processFinished
)

type processEvent struct {
	kind        processEventKind
	content     string
	phase       llm.AssistantPhase
	toolName    string
	toolCallID  string
	toolArgs    string
	runID       string
	result      string
	detail      string
	err         error
	duration    float64
	allowOrphan bool
	reason      string
}

type processEffects struct {
	commits []ChatMessage
}

type processViewport struct {
	width   int
	maxRows int
}

// processSurface owns the mutable projection of one foreground run. Its small
// interface hides stream phase resolution, narration grouping, active tool
// correlation, height budgeting, and the transition to immutable transcript
// cells from the Bubble Tea controller.
type processSurface struct {
	assistantText  string
	livePhase      llm.AssistantPhase
	nextGroupID    uint64
	currentGroupID uint64
	tools          []ChatMessage
	knownTools     map[string]struct{}
}

func newProcessSurface() *processSurface {
	return &processSurface{knownTools: make(map[string]struct{})}
}

func (s *processSurface) Update(event processEvent) processEffects {
	if s == nil {
		return processEffects{}
	}
	switch event.kind {
	case processStreamDelta:
		effects := processEffects{}
		if event.phase != llm.AssistantPhaseUnspecified &&
			s.livePhase != llm.AssistantPhaseUnspecified &&
			event.phase != s.livePhase && s.hasStreamContent() {
			if message, ok := s.resolveAssistant(s.livePhase, ""); ok {
				effects.commits = append(effects.commits, message)
			}
		}
		if event.phase != llm.AssistantPhaseUnspecified {
			s.livePhase = event.phase
		}
		s.assistantText += event.content
		return effects
	case processToolStarted:
		effects := processEffects{}
		if strings.TrimSpace(event.toolCallID) == "" {
			return effects
		}
		key := processToolKey(event.runID, event.toolCallID)
		if _, exists := s.knownTools[key]; exists {
			return effects
		}
		phase := s.livePhase
		if phase == llm.AssistantPhaseUnspecified {
			phase = llm.AssistantPhaseCommentary
		}
		if narration, ok := s.resolveAssistant(phase, ""); ok {
			effects.commits = append(effects.commits, narration)
		}
		s.knownTools[key] = struct{}{}
		s.tools = append(s.tools, ChatMessage{
			Role:           "tool",
			ToolName:       event.toolName,
			ToolCallID:     event.toolCallID,
			RunID:          event.runID,
			ToolArgs:       event.toolArgs,
			IsRunning:      true,
			Timestamp:      time.Now(),
			ProcessGroupID: s.currentGroupID,
		})
		return effects
	case processToolOutput:
		if idx := s.findTool(event.toolCallID, event.toolName, event.runID); idx >= 0 {
			appendProcessToolOutput(&s.tools[idx], event.content)
		}
	case processToolHeartbeat:
		if idx := s.findTool(event.toolCallID, event.toolName, event.runID); idx >= 0 {
			tool := &s.tools[idx]
			if event.toolName != "" {
				tool.ToolName = event.toolName
			}
			if strings.TrimSpace(event.detail) != "" && !isGenericToolHeartbeat(event.toolName, event.detail) {
				tool.RunningDetail = event.detail
			}
		}
	case processToolCompleted:
		return s.completeTool(event)
	case processToolsInterrupted:
		return s.interruptTools(event.reason)
	case processToolsDiscarded:
		s.tools = nil
	case processFinished:
		phase := event.phase
		if phase == llm.AssistantPhaseUnspecified {
			phase = llm.AssistantPhaseFinalAnswer
		}
		effects := processEffects{}
		if phase == llm.AssistantPhaseFinalAnswer &&
			s.livePhase == llm.AssistantPhaseCommentary &&
			strings.TrimSpace(event.content) != "" &&
			strings.TrimSpace(event.content) != strings.TrimSpace(s.streamContent()) {
			if narration, ok := s.resolveAssistant(llm.AssistantPhaseCommentary, ""); ok {
				effects.commits = append(effects.commits, narration)
			}
		}
		if message, ok := s.resolveAssistant(phase, event.content); ok {
			effects.commits = append(effects.commits, message)
		}
		s.currentGroupID = 0
		s.knownTools = make(map[string]struct{})
		return effects
	}
	return processEffects{}
}

func (s *processSurface) interruptTools(reason string) processEffects {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Completion was not observed before the run ended."
	}
	commits := make([]ChatMessage, 0, len(s.tools))
	for _, tool := range s.tools {
		tool.IsRunning = false
		tool.IsError = true
		tool.RunningDetail = ""
		if tool.Duration <= 0 && !tool.Timestamp.IsZero() {
			tool.Duration = time.Since(tool.Timestamp).Seconds()
		}
		if existing := strings.TrimSpace(tool.Content); existing == "" {
			tool.Content = reason
		} else if !strings.Contains(existing, reason) {
			tool.Content = existing + "\n" + reason
		}
		commits = append(commits, tool)
	}
	s.tools = nil
	return processEffects{commits: commits}
}

func (s *processSurface) completeTool(event processEvent) processEffects {
	idx := s.findTool(event.toolCallID, event.toolName, event.runID)
	if idx < 0 {
		key := processToolKey(event.runID, event.toolCallID)
		if strings.TrimSpace(event.toolCallID) == "" || !event.allowOrphan {
			return processEffects{}
		}
		if _, exists := s.knownTools[key]; exists {
			return processEffects{}
		}
		s.knownTools[key] = struct{}{}
		toolName := strings.TrimSpace(event.toolName)
		if toolName == "" {
			toolName = "tool"
		}
		orphan := ChatMessage{
			Role: "tool", ToolName: toolName, ToolCallID: event.toolCallID,
			RunID: event.runID, Content: event.result, Duration: event.duration,
			IsError: event.err != nil, Timestamp: time.Now(),
		}
		if event.err != nil {
			orphan.Content = fmt.Sprintf("%s error: %v", toolName, event.err)
		}
		return processEffects{commits: []ChatMessage{orphan}}
	}

	tool := s.tools[idx]
	s.tools = append(s.tools[:idx], s.tools[idx+1:]...)
	if strings.TrimSpace(event.toolName) != "" {
		tool.ToolName = event.toolName
	}
	tool.Duration = event.duration
	tool.IsRunning = false
	tool.RunningDetail = ""
	if event.err != nil {
		errText := fmt.Sprintf("%s error: %v", tool.ToolName, event.err)
		if existing := strings.TrimSpace(tool.Content); existing != "" {
			tool.Content = existing + "\n" + errText
		} else {
			tool.Content = errText
		}
		tool.IsError = true
	} else {
		if strings.TrimSpace(event.result) != "" {
			tool.Content = event.result
		}
		tool.IsError = false
	}
	return processEffects{commits: []ChatMessage{tool}}
}

func (s *processSurface) findTool(toolCallID, toolName, runID string) int {
	if strings.TrimSpace(toolCallID) != "" {
		for i := len(s.tools) - 1; i >= 0; i-- {
			tool := s.tools[i]
			if tool.ToolCallID == toolCallID && sameToolRun(tool.RunID, runID) {
				return i
			}
		}
		return -1
	}
	if strings.TrimSpace(toolName) == "" {
		return -1
	}
	match := -1
	for i := len(s.tools) - 1; i >= 0; i-- {
		tool := s.tools[i]
		if tool.ToolName != toolName || !sameToolRun(tool.RunID, runID) {
			continue
		}
		if match >= 0 {
			return -1
		}
		match = i
	}
	return match
}

func appendProcessToolOutput(tool *ChatMessage, content string) {
	content = strings.TrimRight(content, "\n")
	if tool == nil || strings.TrimSpace(content) == "" {
		return
	}
	combined := content
	if existing := strings.TrimSpace(tool.Content); existing != "" {
		combined = existing + "\n" + content
	}
	lines := strings.Split(combined, "\n")
	if len(lines) > maxActiveToolOutputLines {
		lines = lines[len(lines)-maxActiveToolOutputLines:]
	}
	tool.Content = strings.Join(lines, "\n")
}

func processToolKey(runID, toolCallID string) string {
	return strings.TrimSpace(runID) + "\x00" + strings.TrimSpace(toolCallID)
}

func (s *processSurface) hasStreamContent() bool {
	return s != nil && strings.TrimSpace(s.assistantText) != ""
}

func (s *processSurface) streamContent() string {
	return strings.TrimSpace(s.assistantText)
}

func (s *processSurface) resolveAssistant(phase llm.AssistantPhase, authoritative string) (ChatMessage, bool) {
	content := strings.TrimSpace(authoritative)
	if content == "" {
		content = s.streamContent()
	}
	if content == "" {
		return ChatMessage{}, false
	}
	s.assistantText = ""
	s.livePhase = llm.AssistantPhaseUnspecified
	message := ChatMessage{
		Role:           "assistant",
		Content:        content,
		AssistantPhase: phase,
		Timestamp:      time.Now(),
	}
	if phase == llm.AssistantPhaseCommentary {
		s.nextGroupID++
		s.currentGroupID = s.nextGroupID
		message.ProcessGroupID = s.currentGroupID
	} else {
		s.currentGroupID = 0
	}
	return message, true
}

func (s *processSurface) Render(view processViewport) string {
	return s.RenderWithTheme(view, uitheme.Default())
}

func (s *processSurface) RenderWithTheme(view processViewport, t uitheme.Theme) string {
	if s == nil {
		return ""
	}
	width := view.width
	if width < 8 {
		width = 8
	}
	var lines []string
	styles := newTranscriptStyles(t)
	if preview := s.previewContent(); strings.TrimSpace(preview) != "" {
		rendered := strings.Trim(renderAssistantStreamPreviewWithStyles(preview, width, s.livePhase, styles), "\n")
		rendered = strings.TrimRight(prepareTerminalCell(rendered, width), "\n")
		if rendered != "" {
			lines = append(lines, strings.Split(rendered, "\n")...)
		}
	}
	for _, tool := range s.tools {
		rendered := strings.TrimRight(prepareTerminalCell(renderCellWithTheme(tool, width, t), width), "\n")
		for _, line := range strings.Split(rendered, "\n") {
			lines = append(lines, line)
		}
	}
	lines = boundProcessLines(lines, view.maxRows)
	return strings.Join(lines, "\n")
}

func boundProcessLines(lines []string, maxRows int) []string {
	if maxRows <= 0 || len(lines) == 0 {
		return nil
	}
	if len(lines) <= maxRows {
		return lines
	}
	if maxRows == 1 {
		return lines[:1]
	}
	if maxRows == 2 {
		return []string{lines[0], lines[len(lines)-1]}
	}
	bounded := make([]string, 0, maxRows)
	bounded = append(bounded, lines[0], "  … earlier process lines hidden")
	bounded = append(bounded, lines[len(lines)-(maxRows-2):]...)
	return bounded
}

func (s *processSurface) previewContent() string {
	if s == nil {
		return ""
	}
	return s.assistantText
}

func (s *processSurface) HasStreamContent() bool {
	return s != nil && s.hasStreamContent()
}

func (s *processSurface) ClearStream() {
	if s == nil {
		return
	}
	s.assistantText = ""
	s.livePhase = llm.AssistantPhaseUnspecified
}

func (m *uiModel) processState() *processSurface {
	if m.process == nil {
		m.process = newProcessSurface()
	}
	return m.process
}

func (s *processSurface) HasRunningTools() bool {
	return s != nil && len(s.tools) > 0
}

func sameToolRun(stored, incoming string) bool {
	stored = strings.TrimSpace(stored)
	incoming = strings.TrimSpace(incoming)
	return stored == "" || incoming == "" || stored == incoming
}
