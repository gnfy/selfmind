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

func (m *uiModel) completeModelSetup(id string, observation ModelChangeObservation) (tea.Cmd, bool) {
	for _, change := range observation.Status.History {
		if change.ID != id {
			continue
		}
		if change.Status == modelchange.StatusApplied {
			if !observation.GatewayReachable || !observation.Status.ModelReady() {
				m.modelChangeID = id
				return m.observeModelChange(false, 250*time.Millisecond), true
			}
			m.modelSetupComplete = true
			return m.quitNow(), true
		}
		// Rollback/cancellation is not setup success. Reopen the same manager
		// with current evidence so the person can edit or recover.
		m.addErrorMessage(fmt.Sprintf("Model setup ended as %s: %s", change.Status, change.Failure))
		m.modelManager = components.NewModelManagerWithTheme(m.modelManagerStatus, m.modelManagerRoutes, m.width, m.height, m.common.Theme)
		m.modelManager.SetSetupMode()
		return nil, true
	}
	return nil, false
}

func (m *uiModel) modelSetupProgressView() string {
	phase := strings.TrimSpace(m.activityText)
	if m.modelChangePhase != "" {
		phase = string(m.modelChangePhase)
	}
	if phase == "" {
		phase = "Loading model routes"
	}
	return "\nSelfMind setup · Models\n\n" + phase + "\n\nWaiting for the configured daemon to become healthy.\nCtrl+C cancels this setup session.\n"
}
