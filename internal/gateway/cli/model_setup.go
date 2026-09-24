package cli

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"selfmind/internal/modelchange"
	"selfmind/internal/ui/components"
)

// Setup mode reuses Model Manager's draft/transaction boundary but reports
// completion only after the applied transaction and healthy daemon agree.
func (c *Controller) SetModelSetup(enabled bool) {
	c.model.modelSetup = enabled
	if enabled {
		c.SetModelManagerOnly(true)
	}
}

func (c *Controller) ModelSetupResult() (bool, error) {
	return c.model.modelSetupComplete, c.model.modelSetupError
}

func (m *uiModel) finishSetupValidation(msg MsgModelValidationDone) {
	probes := make([]components.ModelSetupProbe, 0, len(msg.Response.Probes))
	for _, probe := range msg.Response.Probes {
		probes = append(probes, components.ModelSetupProbe{Route: string(probe.Route), Provider: probe.Provider, Model: probe.Model, OK: probe.OK, Error: probe.Error})
	}
	message := ""
	if msg.Err != nil {
		message = msg.Err.Error()
	}
	m.modelManager.SetSetupValidation(probes, message, msg.Response.CredentialStage)
}

func (m *uiModel) completeManagedModelChange(id string, observation ModelChangeObservation) (tea.Cmd, bool) {
	for _, change := range observation.Status.History {
		if change.ID != id {
			continue
		}
		if change.Status == modelchange.StatusApplied {
			if !observation.GatewayReachable || !observation.Status.ModelReady() {
				m.modelChangeID = id
				return m.observeModelChange(false, 250*time.Millisecond), true
			}
			m.finishModelOperation()
			if m.modelSetup {
				m.modelSetupComplete = true
			}
			m.addMessage("notice", fmt.Sprintf("Model change %s applied. Running is now %s.", change.ID, m.displayModelName()))
			if m.modelSetup || m.modelManagerOnly {
				return m.quitNow(), true
			}
			m.modelManager = nil
			return nil, true
		}
		// Rollback/cancellation is not setup success. Reopen the same manager
		// with current evidence so the person can edit or recover.
		m.finishModelOperation()
		m.addErrorMessage(fmt.Sprintf("Model change %s ended as %s: %s", change.ID, change.Status, change.Failure))
		m.reopenModelManager()
		if m.modelSetup {
			m.modelManager.SetSetupMode()
		}
		return nil, true
	}
	return nil, false
}

func (m *uiModel) reopenModelManager() {
	m.modelManager = components.NewModelManagerWithTheme(m.modelManagerStatus, m.modelManagerRoutes, m.width, m.height, m.common.Theme)
}

func (m *uiModel) modelChangeProgressView() string {
	if m.modelSetup {
		return m.modelSetupProgressView()
	}
	phase := strings.TrimSpace(m.activityText)
	if m.modelChangePhase != "" {
		phase = string(m.modelChangePhase)
	}
	if phase == "" {
		phase = "Applying model change"
	}
	return "\nSelfMind · Models\n\n" + m.modelOperationProgressLine(phase) + "\n\nWaiting for the selected model transaction and daemon health to agree.\nCtrl+C cancels this client; the durable transaction continues.\n"
}

func (m *uiModel) modelSetupProgressView() string {
	phase := strings.TrimSpace(m.activityText)
	if m.modelChangePhase != "" {
		phase = string(m.modelChangePhase)
	}
	if phase == "" {
		phase = "Loading model routes"
	}
	return "\nSelfMind setup · Models\n\n" + m.modelOperationProgressLine(phase) + "\n\nWaiting for the configured daemon to become healthy.\nCtrl+C cancels this setup session.\n"
}

func (m *uiModel) startModelOperation(label string) tea.Cmd {
	if m == nil {
		return nil
	}
	m.thinking = true
	m.activityText = strings.TrimSpace(label)
	if m.thinkingStart.IsZero() {
		m.thinkingStart = time.Now()
	}
	if m.spinnerRunning {
		return nil
	}
	m.spinnerRunning = true
	return m.spinner.Tick
}

func (m *uiModel) finishModelOperation() {
	if m == nil {
		return
	}
	m.modelApplying = false
	m.thinking = false
	m.stopModelWait()
	m.activityText = ""
	m.thinkingStart = time.Time{}
	m.spinnerRunning = false
}

func (m *uiModel) modelOperationProgressLine(phase string) string {
	phase = strings.TrimSpace(phase)
	if phase == "" {
		phase = "Applying model change"
	}
	started := m.thinkingStart
	if m.modelChangePhase != "" && !m.modelChangePhaseAt.IsZero() {
		started = m.modelChangePhaseAt
	}
	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}
	label := fmt.Sprintf("%s (%s)", phase, formatElapsedCompact(elapsed))
	chat := m.common.Styles.Chat
	return chat.ProgressGlyph.Render(m.spinner.View()) + " " + chat.ProgressLabel.Render(label)
}
