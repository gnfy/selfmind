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
	if !planSnapshotFresh(m.activePlanJSON != "", m.activePlanRunID, m.activePlanVersion, m.activePlanCursor, content, ref) {
		return false
	}
	runID := strings.TrimSpace(ref.RunID)
	version := planSnapshotVersion(content)
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

// Foreground and background plan displays use the same durable ordering rule.
// A delayed daemon event cannot move either display back to an older snapshot.
func planSnapshotFresh(hasCurrent bool, currentRunID string, currentVersion int, currentCursor int64, content string, ref uiEventRef) bool {
	if content == "" {
		return false
	}
	if !hasCurrent {
		return true
	}
	runID := strings.TrimSpace(ref.RunID)
	if runID != "" && currentRunID != "" && runID != currentRunID {
		return false
	}
	version := planSnapshotVersion(content)
	switch {
	case currentVersion > 0 && version == 0:
		return false
	case version > 0 && currentVersion > 0 && version < currentVersion:
		return false
	case version == currentVersion && ref.Cursor > 0 && currentCursor > 0 && ref.Cursor <= currentCursor:
		return false
	}
	return true
}

func (m *uiModel) applyBackgroundPlanSnapshot(content string, ref uiEventRef) bool {
	content = strings.TrimSpace(textutil.CleanUTF8(content))
	if ref.RunID != m.backgroundRunID || !planSnapshotFresh(m.backgroundPlanTotal > 0, m.backgroundRunID, m.backgroundPlanVersion, m.backgroundPlanCursor, content, ref) {
		return false
	}
	var snapshot struct {
		Plan []struct {
			Status string `json:"status"`
		} `json:"plan"`
	}
	if json.Unmarshal([]byte(content), &snapshot) != nil || len(snapshot.Plan) == 0 {
		return false
	}
	m.backgroundPlanTotal = len(snapshot.Plan)
	m.backgroundPlanResolved = 0
	for _, step := range snapshot.Plan {
		if step.Status == "completed" || step.Status == "cancelled" {
			m.backgroundPlanResolved++
		}
	}
	m.backgroundPlanVersion = planSnapshotVersion(content)
	if ref.Cursor > m.backgroundPlanCursor {
		m.backgroundPlanCursor = ref.Cursor
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
