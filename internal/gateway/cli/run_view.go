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
	if len(msg.View.Options) > 0 {
		m.showRunViewChooser(msg.View, 0)
		return nil
	}
	m.runViewOptions = nil
	m.runViewIndex = 0
	m.runViewID = msg.View.RunID
	m.runViewCanContinue = msg.View.CanContinue
	// The actions live in the fixed footer: keys listed inside the page scroll
	// away on a long Plan, which left a page that read as nothing to do but quit.
	canSelect := m.runViewResumeActions && msg.View.CanContinue
	m.pager = components.NewPager(m.common, m.width, m.height, func(width int) string {
		return renderMarkdown(msg.View.Content, width)
	})
	m.pager.SetHint(runViewDetailHint(canSelect, m.runViewFromChooser && m.runViewChooser != nil))
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

// showRunViewChooser renders the chooser with row `index` selected.
func (m *uiModel) showRunViewChooser(view api.RunView, index int) {
	m.runViewChooser = &view
	m.runViewOptions = view.Options
	m.runViewIndex = index
	m.runViewID = ""
	m.runViewCanContinue = false
	m.pager = components.NewPager(m.common, m.width, m.height, func(width int) string {
		var text strings.Builder
		text.WriteString("Work · view only\n\n")
		text.WriteString(view.Content + "\n\n")
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
		text.WriteString("\n/queue lists requests not started")
		return renderMarkdown(text.String(), width)
	})
	m.pager.SetHint("↑/↓ choose · Enter view · q/Esc close")
}

// runViewDetailHint names the keys of a Run's detail page. Selecting is an
// explicit choice, separate from viewing: `c` makes the next message go to this
// Run, and Esc/b leaves without choosing.
func runViewDetailHint(canSelect, fromChooser bool) string {
	var keys []string
	if canSelect {
		keys = append(keys, "c select for your next message")
	} else {
		keys = append(keys, "view only")
	}
	if fromChooser {
		keys = append(keys, "Esc/b back", "r refresh", "q close")
	} else {
		keys = append(keys, "r refresh", "q/Esc close")
	}
	return strings.Join(keys, " · ")
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
			// Remember the row, so Esc/b on the detail page comes back to the same place.
			m.runViewFromChooser = true
			m.runViewChooserIndex = m.runViewIndex
			return true, m.loadRunView(m.runViewOptions[m.runViewIndex].RunID)
		}
	}
	if msg.String() == "r" && m.runViewID != "" && m.runViewer != nil {
		return true, m.loadRunView(m.runViewID)
	}
	if (msg.String() == "esc" || msg.String() == "b") && m.runViewID != "" && m.runViewFromChooser && m.runViewChooser != nil {
		// Back to the chooser without choosing: drop a late detail load or refresh.
		m.runViewRequest++
		m.runViewLoading = false
		m.showRunViewChooser(*m.runViewChooser, m.runViewChooserIndex)
		return true, nil
	}
	if msg.String() == "q" || msg.String() == "esc" || msg.String() == "ctrl+c" {
		m.closeRunViewState()
	}
	return false, nil
}

// Human waits from this session take precedence over a transient work view.
func (m *uiModel) closeRunViewForHumanWait() {
	if m.runViewID != "" || len(m.runViewOptions) > 0 || m.runViewLoading {
		m.closeRunViewState()
		m.pager = nil
	}
}

// closeRunViewState forgets the open work page, its chooser, and any load still
// on its way.
func (m *uiModel) closeRunViewState() {
	m.runViewOptions, m.runViewID = nil, ""
	m.runViewChooser, m.runViewFromChooser = nil, false
	m.runViewRequest++
	m.runViewLoading = false
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
