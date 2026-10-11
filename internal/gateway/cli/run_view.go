package cli

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/ui/components"
)

type RunViewer func(context.Context, string) (api.RunView, error)

type MsgRunViewLoaded struct {
	RequestID uint64
	View      api.RunView
	Err       error
}

func (c *Controller) SetRunViewer(viewer RunViewer) {
	if c != nil && c.model != nil {
		c.model.runViewer = viewer
	}
}

func (m *uiModel) loadRunView(runID string) tea.Cmd {
	viewer := m.runViewer
	m.runViewRequest++
	m.runViewLoading = true
	requestID := m.runViewRequest
	return func() tea.Msg {
		view, err := viewer(context.Background(), runID)
		return MsgRunViewLoaded{RequestID: requestID, View: view, Err: err}
	}
}

func (m *uiModel) showRunView(msg MsgRunViewLoaded) tea.Cmd {
	if msg.RequestID != m.runViewRequest {
		return nil
	}
	m.runViewLoading = false
	if msg.Err != nil {
		m.setStatusNotice(noticeError, msg.Err.Error())
		return nil
	}
	m.runViewOptions = msg.View.Options
	m.runViewIndex = 0
	m.runViewID = msg.View.RunID
	m.runViewCanContinue = msg.View.CanContinue
	if len(msg.View.Options) > 0 {
		m.pager = components.NewPager(m.common, m.width, m.height, func(width int) string {
			var text strings.Builder
			text.WriteString("Work · view only\n\n")
			text.WriteString(msg.View.Content + "\n\n")
			// Keep the selected row visible even in a short terminal. The
			// chooser is a bounded window, while detail pages scroll normally.
			count := min(8, max(1, (m.height-10)/2))
			start := max(0, m.runViewIndex-count+1)
			end := min(len(m.runViewOptions), start+count)
			for i := start; i < end; i++ {
				option := m.runViewOptions[i]
				marker := "  "
				if i == m.runViewIndex {
					marker = "› "
				}
				id := truncateToWidth(option.RunID, 16)
				label := truncateToWidth(option.Status+" · "+option.Title+" ("+id+")", max(1, width-6))
				fmt.Fprintf(&text, "- %s%s\n", marker, label)
			}
			fmt.Fprintf(&text, "\n%d/%d selected", m.runViewIndex+1, len(m.runViewOptions))
			text.WriteString("\n↑/↓ choose · Enter view · q/Esc close · /queue lists requests not started")
			return renderMarkdown(text.String(), width)
		})
		return nil
	}
	content := strings.ReplaceAll(msg.View.Content, "\n", "  \n") + "\n\nr refresh · q/Esc close"
	if m.runViewResumeActions && msg.View.CanContinue {
		content += " · c select for your next message"
	}
	m.pager = components.NewPager(m.common, m.width, m.height, func(width int) string { return renderMarkdown(content, width) })
	if m.watchingRun && m.watchedRunID != msg.View.RunID && !m.thinking && !m.daemonRunActive {
		m.detachWatchedRunForNewTurn()
	}
	// A parked task is an inspection page. Watching an active task is passive,
	// and must not merge its progress into an executing foreground Run here.
	if msg.View.Status == "running" && m.runWatcher != nil && !m.thinking && !m.daemonRunActive && msg.View.RunID != "" {
		if m.watchingRun && m.watchedRunID == msg.View.RunID {
			return nil
		}
		m.detachWatchedRunForNewTurn()
		m.watchedRunID, m.watchedTaskTitle = msg.View.RunID, msg.View.Title
		if msg.View.PlanJSON != "" {
			m.applyPlanSnapshot(msg.View.PlanJSON, uiEventRef{Source: eventSourceWatch, RunID: msg.View.RunID})
		}
		return m.attachToActiveRun()
	}
	return nil
}

func (m *uiModel) runViewKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	if msg.String() == "c" && m.runViewResumeActions && m.runViewCanContinue && m.runViewID != "" {
		id := m.runViewID
		m.closeRunViewForHumanWait()
		return true, m.handleControlPassthrough("/resume", []string{id})
	}
	if len(m.runViewOptions) > 0 {
		switch msg.String() {
		case "up", "k":
			m.runViewIndex = max(0, m.runViewIndex-1)
			return true, nil
		case "down", "j":
			m.runViewIndex = min(len(m.runViewOptions)-1, m.runViewIndex+1)
			return true, nil
		case "enter":
			return true, m.loadRunView(m.runViewOptions[m.runViewIndex].RunID)
		}
	}
	if msg.String() == "r" && m.runViewID != "" && m.runViewer != nil {
		return true, m.loadRunView(m.runViewID)
	}
	if msg.String() == "q" || msg.String() == "esc" || msg.String() == "ctrl+c" {
		m.runViewOptions, m.runViewID = nil, ""
		m.runViewRequest++
		m.runViewLoading = false
	}
	return false, nil
}

// Human waits from this session take precedence over a transient work view.
func (m *uiModel) closeRunViewForHumanWait() {
	if m.runViewID != "" || len(m.runViewOptions) > 0 || m.runViewLoading {
		m.runViewOptions, m.runViewID = nil, ""
		m.runViewRequest++
		m.runViewLoading = false
		m.pager = nil
	}
}

// Only a committed exact resume edge transfers an explicit observation.
// A new sibling or another terminal's unrelated Run never takes this surface.
func (m *uiModel) followWatchedContinuation(msg MsgDaemonRunStarted) tea.Cmd {
	if !m.watchingRun || msg.ResumesRunID == "" || msg.ResumesRunID != m.watchedRunID {
		return nil
	}
	if m.daemonRunActive && m.daemonRunID != m.watchedRunID && m.daemonRunID != msg.RunID {
		return nil
	}
	if m.watchCancel != nil {
		m.watchCancel()
	}
	m.watchCancel, m.watchingRun = nil, false
	if !m.otherSession(msg.Event.Channel) {
		// The originating session's lifetime stream already supplies this child.
		m.watchedRunID, m.watchedTaskTitle = "", ""
		return nil
	}
	m.finalizeLiveStream("", llm.AssistantPhaseCommentary)
	m.clearActivePlan()
	m.discardOpenToolMessages()
	m.watchedRunID = msg.RunID
	return m.attachToActiveRun()
}
