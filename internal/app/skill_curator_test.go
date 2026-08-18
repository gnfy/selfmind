package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

func TestSkillCuratorFreezesCandidateAndAutoPromotesOnlyReadOnlyCohorts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &judgeCaptureProvider{reply: `{
		"action":"CREATE",
		"name":"curated-release-inspection",
		"reason":"three comparable verified read-only runs",
		"content":"---\nname: curated-release-inspection\ndescription: Inspect declared release metadata.\n---\n\n## Applicability\nDeclared release metadata inspection.\n\n## Inputs\nA manifest path.\n\n## Preconditions\nThe manifest exists.\n\n## Procedure\nRead only the declared manifest and extract the requested fields.\n\n## Failure Guards\nDo not guess missing fields.\n\n## Recovery\nReturn to ordinary planning when the manifest is absent.\n\n## Verification\nCite the manifest fields used.\n"
	}`}
	storage, err := tools.NewSkillStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	curator := &llmSkillCurator{provider: provider, store: store, skillStorage: storage}
	digest := curatorTestDigest("read-only-evidence", []string{"file.read", "file.search"})
	payload, _ := json.Marshal(digest)
	proposal, err := curator.ProposeSkillCuration(ctx, "default", string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proposal, `"action":"CREATE"`) || provider.last.MaxTokens < 4096 {
		t.Fatalf("proposal was not a bounded frozen CREATE: %s request=%+v", proposal, provider.last)
	}
	summary, err := curator.ApplySkillCuration(ctx, "default", string(payload), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "promoted") {
		t.Fatalf("read-only cohort was not auto-promoted: %s", summary)
	}
	version, err := store.SkillCandidateByEvidence(ctx, "default", digest.EvidenceSetHash)
	if err != nil || version == nil || version.State != "active" {
		t.Fatalf("materialized version=%+v err=%v", version, err)
	}
	second, err := curator.ApplySkillCuration(ctx, "default", string(payload), proposal)
	if err != nil || !strings.Contains(second, "already materialized") {
		t.Fatalf("replay did not reuse frozen materialization: %q err=%v", second, err)
	}

	provider.reply = strings.ReplaceAll(provider.reply, "curated-release-inspection", "curated-release-write")
	writeDigest := curatorTestDigest("write-evidence", []string{"terminal"})
	writePayload, _ := json.Marshal(writeDigest)
	writeProposal, err := curator.ProposeSkillCuration(ctx, "default", string(writePayload))
	if err != nil {
		t.Fatal(err)
	}
	writeSummary, err := curator.ApplySkillCuration(ctx, "default", string(writePayload), writeProposal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(writeSummary, "active unchanged") {
		t.Fatalf("write-capable cohort crossed the confirmation gate: %s", writeSummary)
	}
	writeVersion, _ := store.SkillCandidateByEvidence(ctx, "default", writeDigest.EvidenceSetHash)
	if writeVersion == nil || writeVersion.State != "candidate" {
		t.Fatalf("write-capable candidate state=%+v", writeVersion)
	}
}

func TestSkillCuratorSkipsToollessCohortBeforeProviderCall(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &judgeCaptureProvider{reply: `{"action":"CREATE"}`}
	curator := &llmSkillCurator{provider: provider, store: store}
	digest := curatorTestDigest("tool-less", nil)
	payload, _ := json.Marshal(digest)
	proposal, err := curator.ProposeSkillCuration(ctx, "default", string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proposal, `"action":"SKIP"`) {
		t.Fatalf("tool-less cohort proposal=%s", proposal)
	}
	if provider.last.MaxTokens != 0 {
		t.Fatalf("tool-less cohort called provider: %+v", provider.last)
	}
	result, err := curator.ApplySkillCuration(ctx, "default", string(payload), `{"action":"CREATE"}`)
	if err != nil || !strings.Contains(result, "cohort is not ready") {
		t.Fatalf("tool-less apply=%q err=%v", result, err)
	}
}

func TestSkillCuratorCreateFailsClosedWithoutConfiguredStorage(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	curator := &llmSkillCurator{store: store}
	digest := curatorTestDigest("missing-storage", []string{"file.read"})
	payload, _ := json.Marshal(digest)
	proposal := `{"action":"CREATE","name":"missing-storage","content":"---\nname: missing-storage\ndescription: test\n---\n\n## Applicability\nTest.\n\n## Inputs\nNone.\n\n## Preconditions\nNone.\n\n## Procedure\nRead.\n\n## Failure Guards\nStop.\n\n## Recovery\nPlan.\n\n## Verification\nVerify."}`
	if _, err := curator.ApplySkillCuration(context.Background(), "default", string(payload), proposal); err == nil || !strings.Contains(err.Error(), "storage is not configured") {
		t.Fatalf("missing storage error = %v", err)
	}
}

func curatorTestDigest(evidenceHash string, tools []string) control.SkillEvidenceDigest {
	observations := make([]control.WorkflowObservation, 0, 3)
	for i, runID := range []string{"run-a", "run-b", "run-c"} {
		observations = append(observations, control.WorkflowObservation{
			ID: "observation-" + runID, IdentityTenantID: "default", ControlTenantID: "default",
			PersonID: "person", WorkspaceID: "workspace", RunID: runID,
			WorkUnitID: "unit-" + runID, WorkflowSignature: "signature", OutcomeStatus: "completed",
			VerificationState: "passed", ToolSequence: append([]string{}, tools...),
			EvidenceRole: "success_path", DurationMS: int64(100 + i),
		})
	}
	return control.SkillEvidenceDigest{
		EvidenceSetHash: evidenceHash, WorkflowSignature: "signature",
		IdentityTenantID: "default", ControlTenantID: "default", PersonID: "person",
		WorkspaceID: "workspace", SuccessObservations: observations,
	}
}
