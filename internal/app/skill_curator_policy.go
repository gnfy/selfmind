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
	if ok, reason := c.repairTargetEligible(tenantID, digest); !ok {
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
	// A cohort whose every run reached the same existing Skill is not virgin
	// territory, even when none of them activated it. Minting a second Skill
	// for claimed work is how a library turns into competing near-duplicates
	// that answer the same bare name — and the one candidate this deployment
	// ever produced was exactly that shape: three runs that read a release
	// Skill by hand and would have learned "how to look it up".
	//
	// An activated cohort needs no rule here: activation sets TargetSkillKey,
	// so it takes the repair branch above and a successful run proposes
	// nothing.
	if len(digest.CoveringSkillKeys) > 0 {
		return false
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
			// "dangerous" is deliberately absent. The runtime declares it a
			// call-side FALLBACK for the dangerous-op heuristic "when no more
			// specific class applies", and the deny path already refuses to act
			// on it because "this looked risky" is not something the person
			// said. Publication eligibility must not be the one consumer that
			// treats it as authoritative: the heuristic reports every host exec
			// as dangerous wherever an enforced sandbox cannot be proven, which
			// is every exec on a platform without one. That made publication
			// eligibility depend on the operating system — over three days not
			// one of 378 approvals came from the heuristic itself, while every
			// terminal step in the only candidate ever produced carried the
			// class and blocked it.
			//
			// The classes kept here are specific and platform-independent, and
			// the cohort behind a publication is three verified runs whose
			// commands the person approved.
			case "delete", "network", "exec.delegated":
				return false
			}
		}
	}
	return true
}

// repairTargetEligible decides whether a repair may be PROPOSED for the active
// Skill, not whether it may be applied.
//
// Proposal used to be refused for anything the curator could not also rewrite,
// which withheld it from exactly the Skills a person actually uses: their own,
// in their own repository. Those Skills are activated, they accumulate
// incidents and verified recoveries like any other, and none of it went
// anywhere — the evidence was collected and then never acted on.
//
// A pinned Skill is still refused: a pin is the person saying this content is
// not to be reworked, and a proposal against it is noise.
func (c *llmSkillCurator) repairTargetEligible(tenantID string, digest control.SkillEvidenceDigest) (bool, string) {
	if c == nil || c.skillStorage == nil {
		return false, "Skill storage is unavailable"
	}
	info, err := c.skillInfoForName(tenantID, digest.WorkspaceID, digest.PublicationScope, digest.TargetSkillName)
	if err != nil {
		return false, "active Skill is unavailable"
	}
	if info.Pinned {
		return false, "a pinned Skill is not reworked automatically"
	}
	return true, ""
}

// repairTargetIsPersonOwned reports whether applying a repair would rewrite an
// asset the person owns — their repository's own Skill, or any Skill this
// runtime did not author. Those are proposed but never applied automatically:
// the write lands in the person's working tree, and that authority comes from
// the person, not from evidence.
func (c *llmSkillCurator) repairTargetIsPersonOwned(tenantID, workspaceID, publicationScope, name string) bool {
	info, err := c.skillInfoForName(tenantID, workspaceID, publicationScope, name)
	if err != nil {
		// Fail closed: an unreadable target is never rewritten automatically.
		return true
	}
	return info.Source != tools.SkillSourceAgentCreated || info.Pinned || !info.Writable
}

func (c *llmSkillCurator) skillInfoForName(tenantID, workspaceID, publicationScope, name string) (tools.SkillInfo, error) {
	args := c.skillInvocationArgs(context.Background(), tenantID, workspaceID, publicationScope, kernel.SkillMutationCandidateOnly)
	info, _, _, err := tools.ReadSkillPayloadForTenant(tenantID, name, "", args)
	return info, err
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
		// Evidence decides whether a repair is RIGHT. It does not decide
		// whether this runtime may write it into an asset the person owns:
		// that write lands in their working tree and is theirs to make. The
		// candidate stands with its evidence and is applied with /skills
		// promote.
		if c.repairTargetIsPersonOwned(tenantID, workspaceID, c.versionPublicationScope(tenantID, workspaceID, version), version.SkillName) {
			return "the Skill is yours to change; apply it with /skills promote", nil
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
