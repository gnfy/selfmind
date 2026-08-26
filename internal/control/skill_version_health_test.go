package control

import (
	"context"
	"testing"
	"time"
)

func TestSkillVersionHealthNominatesStalenessAndQuarantinesRepairRegression(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().Truncate(time.Second)
	verified := WorkflowObservation{
		ID: "verified-a", RunID: "run-a", VerificationState: "passed", OutcomeStatus: WorkUnitCompleted,
		EnvironmentFingerprint: "environment-a", CreatedAt: now.Add(-10 * 24 * time.Hour),
		ToolEvidence: []WorkflowToolEvidence{{Name: "read_file", Origin: "builtin", Category: "filesystem", ReadOnly: true}},
	}
	digest := SkillEvidenceDigest{PublicationScope: "workspace", WorkspaceID: "workspace-a", SuccessObservations: []WorkflowObservation{verified}}
	key := SkillKey("default", "health-skill", "workspace", "agent-created", "/managed/workspace-a", "health-skill/SKILL.md")
	v1, err := store.CreateSkillCandidateVersion(ctx, "default", key, "health-skill", "", "version one", "health-v1", []string{"verified-a"}, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteSkillCandidate(ctx, "default", key, v1, "/managed/workspace-a/health-skill/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	active, _ := store.ActiveSkillVersion(ctx, "default", key)
	if active == nil || active.DependencyFingerprint == "" || active.VerificationEnvironmentFingerprint != "environment-a" || active.LastVerifiedAt == nil {
		t.Fatalf("active health metadata=%+v", active)
	}
	if reason := SkillVersionReviewReason(*active, "different-dependency", now, DefaultSkillVerificationMaxAge); reason != "dependency_fingerprint_changed" {
		t.Fatalf("changed dependency reason=%q", reason)
	}
	if reason := SkillVersionReviewReason(*active, active.DependencyFingerprint, now.Add(31*24*time.Hour), DefaultSkillVerificationMaxAge); reason != "verification_expired" {
		t.Fatalf("expired verification reason=%q", reason)
	}

	repairDigest := SkillEvidenceDigest{PublicationScope: "workspace", WorkspaceID: "workspace-a", NegativeObservations: []WorkflowObservation{verified}}
	v2, err := store.CreateSkillCandidateVersion(ctx, "default", key, "health-skill", v1, "version two", "health-v2", []string{"verified-repair"}, repairDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteSkillCandidate(ctx, "default", key, v2, "/managed/workspace-a/health-skill/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	regression := WorkflowObservation{
		ControlTenantID: "default", SkillKey: key, VersionHash: v2,
		Incident: &SkillIncidentEvidence{
			FailureSignature: "regression", FailedStepID: "Procedure", FailedToolCallID: "call-1",
			ErrorCategory: "schema_changed", ObservedErrorCategory: "tool_schema", FailureObserved: true,
		},
	}
	if err := store.recordSkillVersionObservationHealth(ctx, regression); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.SkillVersionActivationBlocked(ctx, "default", key, v2)
	if err != nil || !blocked {
		t.Fatalf("quarantine blocked=%t err=%v", blocked, err)
	}
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "quarantine", "Quarantine")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "quarantine", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "use quarantined skill")
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: "default", PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: run.WorkUnitID, SkillKey: key, SkillName: "health-skill", VersionHash: v2, ContentBody: "version two",
	}); err == nil {
		t.Fatal("quarantined repaired version was reactivated")
	}
	eligible, err := store.EligiblePreviousSkillVersionForEnvironment(ctx, "default", key, v2, "environment-a")
	if err != nil || eligible == nil || eligible.VersionHash != v1 {
		t.Fatalf("eligible rollback=%+v err=%v", eligible, err)
	}
	if incompatible, err := store.EligiblePreviousSkillVersionForEnvironment(ctx, "default", key, v2, "environment-b"); err != nil || incompatible != nil {
		t.Fatalf("incompatible rollback=%+v err=%v", incompatible, err)
	}
}

func TestSemanticRepairCandidateAccumulatesImmutableEvidenceSnapshotsBeforePromotion(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := SkillKey("default", "semantic-repair", "workspace", "agent-created", "/managed/workspace", "semantic-repair/SKILL.md")
	parent, err := store.CreateSkillCandidateVersion(ctx, "default", key, "semantic-repair", "", "parent", "parent-evidence", []string{"parent"}, SkillEvidenceDigest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteSkillCandidate(ctx, "default", key, parent, "/managed/workspace/semantic-repair/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	incident := func(runID string) WorkflowObservation {
		return WorkflowObservation{
			ID: "observation-" + runID, RunID: runID, EvidenceRole: "failure_guard",
			Incident: &SkillIncidentEvidence{
				FailureSignature: "meaning-v2", FailedStepID: "Verification",
				ErrorCategory: "verification_mismatch", ObservedErrorCategory: "check_definition",
				FailureObserved: true, RecoveryVerified: true,
			},
		}
	}
	one := SkillEvidenceDigest{PublicationScope: "workspace", WorkspaceID: "workspace", NegativeObservations: []WorkflowObservation{incident("run-a")}}
	versionHash, err := store.CreateSkillCandidateVersion(ctx, "default", key, "semantic-repair", parent, "semantic candidate", "semantic-one", []string{"observation-run-a"}, one)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := store.SkillCandidateHasAutomaticRepairEvidence(ctx, "default", key, versionHash); err != nil || ready {
		t.Fatalf("one semantic snapshot ready=%t err=%v", ready, err)
	}
	three := SkillEvidenceDigest{PublicationScope: "workspace", WorkspaceID: "workspace", NegativeObservations: []WorkflowObservation{
		incident("run-a"), incident("run-b"), incident("run-c"),
	}}
	replayedHash, err := store.CreateSkillCandidateVersion(ctx, "default", key, "semantic-repair", parent, "semantic candidate", "semantic-three",
		[]string{"observation-run-a", "observation-run-b", "observation-run-c"}, three)
	if err != nil || replayedHash != versionHash {
		t.Fatalf("immutable candidate hash=%q want=%q err=%v", replayedHash, versionHash, err)
	}
	if ready, err := store.SkillCandidateHasAutomaticRepairEvidence(ctx, "default", key, versionHash); err != nil || !ready {
		t.Fatalf("three semantic snapshots ready=%t err=%v", ready, err)
	}
	if resolved, err := store.SkillCandidateByEvidence(ctx, "default", "semantic-three"); err != nil || resolved == nil || resolved.VersionHash != versionHash {
		t.Fatalf("snapshot evidence lookup=%+v err=%v", resolved, err)
	}
}
