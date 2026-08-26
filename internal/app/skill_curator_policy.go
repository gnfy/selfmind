package app

import (
	"context"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func (c *llmSkillCurator) repairPreflightReason(tenantID string, digest control.SkillEvidenceDigest) string {
	if strings.TrimSpace(digest.TargetSkillName) == "" || strings.TrimSpace(digest.TargetActiveContent) == "" {
		return "active Skill content is unavailable"
	}
	if err := validateCuratedSkillContent(digest.TargetActiveContent, digest.TargetSkillName); err != nil {
		return "active Skill is not eligible for deterministic narrow repair: " + err.Error()
	}
	if ok, reason := c.automaticRepairTargetEligible(tenantID, digest); !ok {
		return reason
	}
	return ""
}

func autoPromoteSkillCandidateEligible(digest control.SkillEvidenceDigest) bool {
	if digest.TargetSkillKey != "" {
		if !control.SkillRepairAutomaticPromotionReady(digest) {
			return false
		}
		for _, observation := range digest.NegativeObservations {
			if verifiedRepairObservation(observation) && !automaticObservationPublicationEligible(observation) {
				return false
			}
		}
		return true
	}
	if len(digest.SuccessObservations) < 3 {
		return false
	}
	runs := map[string]bool{}
	for _, observation := range digest.SuccessObservations {
		runs[observation.RunID] = true
		if observation.EvidenceRole != "success_path" {
			return false
		}
		if len(observation.ToolSequence) == 0 {
			return false
		}
		if observation.VerificationState != "passed" {
			return false
		}
		if !automaticObservationPublicationEligible(observation) {
			return false
		}
	}
	return len(runs) >= 3
}

func skillCurationProposalEligible(digest control.SkillEvidenceDigest) bool {
	if strings.TrimSpace(digest.EvidenceSetHash) == "" {
		return false
	}
	if digest.TargetSkillKey != "" {
		return digestHasVerifiedRepairIncident(digest) && control.SkillRepairCandidateEvidenceReady(digest)
	}
	if len(digest.SuccessObservations) < 3 {
		return false
	}
	for _, observation := range digest.SuccessObservations {
		if observation.EvidenceRole != "success_path" || len(observation.ToolSequence) == 0 {
			return false
		}
	}
	return true
}

func digestHasVerifiedRepairIncident(digest control.SkillEvidenceDigest) bool {
	for _, observation := range digest.NegativeObservations {
		if verifiedRepairObservation(observation) {
			return true
		}
	}
	return false
}

// verifiedRepairObservation adds the curator's own requirement - the observation
// must be a failure guard - on top of the control-owned incident gate. The
// incident conditions themselves stay in control so the readiness query and this
// publication check cannot diverge.
func verifiedRepairObservation(observation control.WorkflowObservation) bool {
	return observation.EvidenceRole == "failure_guard" && control.VerifiedSkillRepairIncident(observation)
}

func automaticObservationPublicationEligible(observation control.WorkflowObservation) bool {
	if len(observation.ToolEvidence) == 0 {
		// Historical observations did not capture trusted registry metadata. Keep
		// their previous read-only behavior instead of guessing from tool names.
		for _, tool := range observation.ToolSequence {
			switch tool {
			case "file.read", "file.search", "file.list", "session.search", "skill.read", "batch.read":
			default:
				return false
			}
		}
		return len(observation.ToolSequence) > 0
	}
	for _, tool := range observation.ToolEvidence {
		if tool.Origin != "builtin" || tool.Category == "mcp" {
			return false
		}
		switch tool.Name {
		case "watch_external", "delegate", "skill_manage", "skill_lifecycle_manage":
			return false
		}
		for _, class := range tool.OperationClasses {
			switch class {
			case "delete", "network", "exec.delegated", "dangerous":
				return false
			}
		}
	}
	return true
}

func (c *llmSkillCurator) automaticRepairTargetEligible(tenantID string, digest control.SkillEvidenceDigest) (bool, string) {
	if c == nil || c.skillStorage == nil {
		return false, "Skill storage is unavailable"
	}
	args := c.skillInvocationArgs(context.Background(), tenantID, digest.WorkspaceID, digest.PublicationScope, kernel.SkillMutationCandidateOnly)
	info, _, _, err := tools.ReadSkillPayloadForTenant(tenantID, digest.TargetSkillName, "", args)
	if err != nil {
		return false, "active Skill is unavailable"
	}
	if info.Source != tools.SkillSourceAgentCreated || info.Pinned || !info.Writable {
		return false, "only writable, unpinned, agent-created Skills can be repaired automatically"
	}
	return true, ""
}

func (c *llmSkillCurator) automaticCandidatePromotionBlockedReason(ctx context.Context, tenantID, workspaceID string, version *control.SkillVersion) (string, error) {
	if version == nil {
		return "", nil
	}
	if strings.TrimSpace(version.ParentVersionHash) != "" {
		ready, err := c.store.SkillCandidateHasAutomaticRepairEvidence(ctx, tenantID, version.SkillKey, version.VersionHash)
		if err != nil {
			return "", err
		}
		if !ready {
			return "class-specific repair evidence threshold is not met", nil
		}
		return "", nil
	}
	active, err := c.store.ActiveSkillVersion(ctx, tenantID, version.SkillKey)
	if err != nil {
		return "", err
	}
	if active != nil {
		return "name collision with an active Skill", nil
	}
	publicationScope := c.versionPublicationScope(tenantID, workspaceID, version)
	skills, err := tools.ListSkillsForTenant(tenantID, false,
		c.skillInvocationArgs(ctx, tenantID, workspaceID, publicationScope, kernel.SkillMutationCandidateOnly))
	if err != nil {
		return "", err
	}
	for _, skill := range skills {
		if strings.EqualFold(kernel.SanitizeSkillName(skill.Name), kernel.SanitizeSkillName(version.SkillName)) {
			return "name collision with an existing Skill", nil
		}
	}
	return "", nil
}
