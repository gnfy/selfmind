package app

import (
	"context"
	"encoding/json"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

// recordSkillCurationEvents writes the durable, structured record of one
// curation outcome. blockedReason names why an eligible candidate was not
// promoted, and is empty otherwise: without it the event says only that nothing
// was promoted, which is not actionable, and the surfaces downstream would have
// to parse the summary prose to say anything useful.
func (c *llmSkillCurator) recordSkillCurationEvents(ctx context.Context, digest control.SkillEvidenceDigest, proposal skillCuratorWire, skillKey, name, versionHash, parent string, promoted bool, blockedReason string) {
	observation := curationEventObservation(digest, proposal.Action)
	if observation == nil {
		return
	}
	eventCtx := context.WithoutCancel(ctx)
	taskID := strings.TrimSpace(observation.RelatedTaskID)
	if taskID == "" {
		if run, err := c.store.GetRun(eventCtx, observation.IdentityTenantID, observation.RunID); err == nil && run != nil {
			taskID = run.TaskID
		}
	}
	if taskID == "" {
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"skill_key": skillKey, "name": name, "version_hash": versionHash,
		"parent_version_hash": parent, "evidence_set_hash": digest.EvidenceSetHash,
		"action": proposal.Action, "reason": strings.TrimSpace(proposal.Reason),
		"changed_sections": proposal.ChangedSections, "promoted": promoted,
		"blocked_reason": strings.TrimSpace(blockedReason),
		"repair_class":   repairClassForCurationObservation(observation),
	})
	if _, err := c.store.AppendEvent(eventCtx, control.Event{
		TaskID: taskID, RunID: observation.RunID, Type: "skill.candidate.created",
		Visibility: "task", Payload: payload,
		IdempotencyKey: "skill-candidate:" + digest.EvidenceSetHash,
	}); err != nil {
		log.Warn("skill candidate event write failed", "skill", name, "error", err)
	}
	if !promoted {
		return
	}
	if _, err := c.store.AppendEvent(eventCtx, control.Event{
		TaskID: taskID, RunID: observation.RunID, Type: "skill.version.promoted",
		Visibility: "task", Payload: payload,
		IdempotencyKey: "skill-promoted:" + digest.EvidenceSetHash,
	}); err != nil {
		log.Warn("skill promotion event write failed", "skill", name, "error", err)
	}
}

func repairClassForCurationObservation(observation *control.WorkflowObservation) string {
	if observation == nil {
		return ""
	}
	return control.ClassifySkillRepairIncident(observation.Incident)
}

func curationEventObservation(digest control.SkillEvidenceDigest, action string) *control.WorkflowObservation {
	if strings.EqualFold(action, "PATCH") {
		for i := range digest.NegativeObservations {
			if verifiedRepairObservation(digest.NegativeObservations[i]) {
				return &digest.NegativeObservations[i]
			}
		}
	}
	if len(digest.SuccessObservations) > 0 {
		return &digest.SuccessObservations[0]
	}
	return nil
}
