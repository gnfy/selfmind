package cli

import (
	"encoding/json"
	"strings"

	"selfmind/internal/platform/textutil"
)

// applyPlanSnapshot is the single reducer for digest and live plan state. The
// durable RunPlan version is authoritative; event cursor orders equal-version
// replays. A late event from another run cannot replace a plan that the current
// run already owns.
func (m *uiModel) applyPlanSnapshot(content string, ref uiEventRef) bool {
	content = strings.TrimSpace(textutil.CleanUTF8(content))
	if content == "" {
		return false
	}
	runID := strings.TrimSpace(ref.RunID)
	if m.activePlanJSON != "" && runID != "" && m.activePlanRunID != "" && runID != m.activePlanRunID {
		return false
	}
	version := planSnapshotVersion(content)
	if m.activePlanJSON != "" {
		switch {
		case m.activePlanVersion > 0 && version == 0:
			return false
		case version > 0 && m.activePlanVersion > 0 && version < m.activePlanVersion:
			return false
		case version == m.activePlanVersion && version > 0 && ref.Cursor > 0 && m.activePlanCursor > 0 && ref.Cursor <= m.activePlanCursor:
			return false
		case version == 0 && m.activePlanVersion == 0 && ref.Cursor > 0 && m.activePlanCursor > 0 && ref.Cursor <= m.activePlanCursor:
			return false
		}
	}
	m.activePlanJSON = content
	if runID != "" {
		m.activePlanRunID = runID
	}
	if version > 0 {
		m.activePlanVersion = version
	}
	if ref.Cursor > m.activePlanCursor {
		m.activePlanCursor = ref.Cursor
	}
	return true
}

func (m *uiModel) clearActivePlan() {
	m.activePlanJSON = ""
	m.activePlanRunID = ""
	m.activePlanVersion = 0
	m.activePlanCursor = 0
}

func planSnapshotVersion(content string) int {
	var envelope struct {
		PlanVersion int `json:"plan_version"`
	}
	if json.Unmarshal([]byte(content), &envelope) != nil || envelope.PlanVersion < 1 {
		return 0
	}
	return envelope.PlanVersion
}
