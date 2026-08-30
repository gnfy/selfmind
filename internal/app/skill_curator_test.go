package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func TestSkillCuratorFreezesCandidateAndAutoPromotesVerifiedBuiltinProcedures(t *testing.T) {
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
	wantRoot := tools.ManagedWorkspaceSkillsDir(storage.BaseDir(), "default", digest.WorkspaceID)
	wantKey := control.SkillKey("default", "curated-release-inspection", tools.SkillScopeWorkspace,
		tools.SkillSourceAgentCreated, wantRoot, "curated-release-inspection/SKILL.md")
	if version.SkillKey != wantKey {
		t.Fatalf("learned Skill key=%q want managed workspace key %q", version.SkillKey, wantKey)
	}
	if _, err := os.Stat(filepath.Join(wantRoot, "curated-release-inspection", "SKILL.md")); err != nil {
		t.Fatalf("managed workspace Skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tools.SkillsDirForTenant(storage.BaseDir(), "default"), "curated-release-inspection")); !os.IsNotExist(err) {
		t.Fatalf("automatic publication wrote the user-global root: %v", err)
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
	if !strings.Contains(writeSummary, "promoted") {
		t.Fatalf("verified builtin write procedure was not promoted: %s", writeSummary)
	}
	writeVersion, _ := store.SkillCandidateByEvidence(ctx, "default", writeDigest.EvidenceSetHash)
	if writeVersion == nil || writeVersion.State != "active" {
		t.Fatalf("write-capable candidate state=%+v", writeVersion)
	}

	provider.reply = strings.ReplaceAll(provider.reply, "curated-release-write", "curated-external-write")
	externalDigest := curatorTestDigest("external-evidence", []string{"mcp_issue_update"})
	for i := range externalDigest.SuccessObservations {
		externalDigest.SuccessObservations[i].ToolEvidence[0] = control.WorkflowToolEvidence{
			Name: "mcp_issue_update", Origin: "external", Category: "mcp", RiskLevel: "medium",
			OperationClasses: []string{"write", "network"},
		}
	}
	externalPayload, _ := json.Marshal(externalDigest)
	externalProposal, err := curator.ProposeSkillCuration(ctx, "default", string(externalPayload))
	if err != nil {
		t.Fatal(err)
	}
	externalSummary, err := curator.ApplySkillCuration(ctx, "default", string(externalPayload), externalProposal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(externalSummary, "active unchanged") {
		t.Fatalf("external-effect procedure crossed the confirmation gate: %s", externalSummary)
	}

	networkDigest := curatorTestDigest("builtin-network-evidence", []string{"web_search"})
	for i := range networkDigest.SuccessObservations {
		networkDigest.SuccessObservations[i].ToolEvidence[0] = control.WorkflowToolEvidence{
			Name: "web_search", Origin: "builtin", Category: "network", RiskLevel: "low", ReadOnly: true,
			OperationClasses: []string{"network"},
		}
	}
	if autoPromoteSkillCandidateEligible(networkDigest) {
		t.Fatal("builtin network cohort crossed the automatic publication gate")
	}
	notApplicableDigest := curatorTestDigest("not-applicable-evidence", []string{"file.read"})
	notApplicableDigest.SuccessObservations[0].VerificationState = "not_applicable"
	if autoPromoteSkillCandidateEligible(notApplicableDigest) {
		t.Fatal("not_applicable verification crossed the automatic publication gate")
	}
}

func TestLegacyNetworkObservationDoesNotAutoPublish(t *testing.T) {
	legacyWeb := control.WorkflowObservation{ToolSequence: []string{"web.search"}}
	if automaticObservationPublicationEligible(legacyWeb) {
		t.Fatal("legacy network observation crossed the automatic publication gate")
	}
	legacyFile := control.WorkflowObservation{ToolSequence: []string{"file.read"}}
	if !automaticObservationPublicationEligible(legacyFile) {
		t.Fatal("legacy file read observation lost its compatibility path")
	}
}

func TestSkillCuratorCreateNameCollisionLeavesCandidateForReview(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	storage, err := tools.NewSkillStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	curator := &llmSkillCurator{store: store, skillStorage: storage}
	name := "collision-skill"
	apply := func(evidenceHash, procedure string) (string, control.SkillEvidenceDigest, error) {
		digest := curatorTestDigest(evidenceHash, []string{"file.read"})
		payload, _ := json.Marshal(digest)
		proposal, _ := json.Marshal(skillCuratorWire{
			Action: "CREATE", Name: name, Reason: "verified cohort",
			Content: repairSkillContent(name, procedure, "Return to ordinary planning."),
		})
		summary, applyErr := curator.ApplySkillCuration(ctx, "default", string(payload), string(proposal))
		return summary, digest, applyErr
	}
	first, _, err := apply("collision-first", "Read the declared record.")
	if err != nil || !strings.Contains(first, "promoted") {
		t.Fatalf("first create=%q err=%v", first, err)
	}
	second, secondDigest, err := apply("collision-second", "Read the declared record and summarize it.")
	if err != nil || !strings.Contains(second, "promotion blocked by name collision") {
		t.Fatalf("colliding create=%q err=%v", second, err)
	}
	version, err := store.SkillCandidateByEvidence(ctx, "default", secondDigest.EvidenceSetHash)
	if err != nil || version == nil || version.State != "candidate" {
		t.Fatalf("colliding version=%+v err=%v", version, err)
	}
	replay, _, err := apply("collision-second", "Read the declared record and summarize it.")
	if err != nil || !strings.Contains(replay, "promotion blocked by name collision") {
		t.Fatalf("colliding replay=%q err=%v", replay, err)
	}
}

func TestSkillCuratorRepairsOnlyDeclaredSectionAfterVerifiedRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "repair-events", "Repair Events")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "repair release record", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "repair release record")
	if err != nil {
		t.Fatal(err)
	}
	storage, err := tools.NewSkillStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := "release-record-repair"
	activeContent := repairSkillContent(name, "Write the legacy record layout.", "Replan when the layout is unavailable.")
	root := tools.SkillsDirForTenant(storage.BaseDir(), "default")
	key := control.SkillKey("default", name, tools.SkillScopeUser, tools.SkillSourceAgentCreated, root, name+"/SKILL.md")
	activeHash, err := store.CreateSkillCandidateVersion(ctx, "default", key, name, "", activeContent, "initial-evidence", []string{"initial"}, map[string]string{"source": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools.NewSkillLifecycleManageTool(store).Execute(tools.WithSkillStorage(map[string]interface{}{
		"action": "candidate_promote", "skill_key": key, "version_hash": activeHash, "_tenant_id": "default", "_context": ctx,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: "default", SkillMutationMode: kernel.SkillMutationDirect},
	}, storage)); err != nil {
		t.Fatal(err)
	}
	repairedContent := repairSkillContent(name, "Write the current record layout learned by the verified recovery.", "Replan when the layout is unavailable.")
	reply, _ := json.Marshal(skillCuratorWire{
		Action: "PATCH", Name: name, Content: repairedContent, ChangedSections: []string{"Procedure"},
		Reason: "the active procedure used a stale layout and the ordinary planner verified the current layout",
	})
	provider := &judgeCaptureProvider{reply: string(reply)}
	curator := &llmSkillCurator{provider: provider, store: store, skillStorage: storage}
	digest := control.SkillEvidenceDigest{
		EvidenceSetHash: "repair-evidence", WorkflowSignature: "repair-signature",
		IdentityTenantID: "default", ControlTenantID: "default", PersonID: identity.PersonID, WorkspaceID: "workspace",
		TargetSkillKey: key, TargetSkillName: name, TargetActiveContent: activeContent, ParentVersionHash: activeHash,
		NegativeObservations: []control.WorkflowObservation{{
			ID: "repair-observation", RunID: run.ID, WorkUnitID: run.WorkUnitID, RelatedTaskID: task.ID, EvidenceRole: "failure_guard",
			OutcomeStatus: "completed", VerificationState: "passed", ToolSequence: []string{"terminal"},
			ToolEvidence: []control.WorkflowToolEvidence{{Name: "terminal", Origin: "builtin", Category: "general", RiskLevel: "high", OperationClasses: []string{"exec.in_turn"}}},
			Incident: &control.SkillIncidentEvidence{
				FailureSignature: "stale-layout", FailedStepID: "Procedure", ErrorCategory: "schema_changed",
				FailedToolCallID: "failed-write", ObservedErrorCategory: "interface_drift", FailureObserved: true,
				Reason: "legacy layout failed", RecoveryToolSequence: []string{"terminal"}, RecoveryVerified: true,
			},
		}},
	}
	payload, _ := json.Marshal(digest)
	proposal, err := curator.ProposeSkillCuration(ctx, "default", string(payload))
	if err != nil {
		t.Fatal(err)
	}
	summary, err := curator.ApplySkillCuration(ctx, "default", string(payload), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "promoted") {
		t.Fatalf("verified repair was not promoted: %s", summary)
	}
	previous, _ := store.GetSkillVersion(ctx, "default", key, activeHash)
	if previous == nil || previous.State != "previous" {
		t.Fatalf("repaired version did not preserve rollback: %+v", previous)
	}
	repaired, _ := store.ActiveSkillVersion(ctx, "default", key)
	var frozen control.SkillEvidenceDigest
	if repaired == nil || json.Unmarshal(repaired.Evidence, &frozen) != nil || len(frozen.NegativeObservations) != 1 ||
		frozen.NegativeObservations[0].Incident == nil || frozen.NegativeObservations[0].Incident.RepairClass != control.SkillRepairClassDeterministicInterface {
		t.Fatalf("repair class was not frozen with candidate evidence: %+v", repaired)
	}
	events, err := store.ListTaskEvents(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	seenPromotion := false
	for _, event := range events {
		if event.Type == "skill.version.promoted" && event.RunID == run.ID {
			seenPromotion = true
		}
	}
	if !seenPromotion {
		t.Fatalf("automatic Skill repair emitted no visible promotion event: %+v", events)
	}

	unrelated := repairSkillContent(name, "Write the current record layout learned by the verified recovery.", "Silently ignore failures.")
	if err := validateNarrowSkillRepair(activeContent, unrelated, []string{"Procedure"}); err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("unrelated repair mutation was accepted: %v", err)
	}
	whitespaceDrift := strings.Replace(activeContent,
		"Replan when the layout is unavailable.\n\n## Verification",
		"Replan when the layout is unavailable.\n\n\n## Verification", 1)
	if err := validateNarrowSkillRepair(activeContent, whitespaceDrift, []string{"Procedure"}); err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("unrelated whitespace drift was accepted: %v", err)
	}
	if err := validateRepairIncidentCoverage(digest, []string{"Recovery"}); err == nil || !strings.Contains(err.Error(), "procedure") {
		t.Fatalf("repair omitted the attributable failed section: %v", err)
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

func TestSkillCuratorExistingSkillRequiresVerifiedIncident(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &judgeCaptureProvider{reply: `{"action":"PATCH"}`}
	curator := &llmSkillCurator{provider: provider, store: store}
	digest := curatorTestDigest("optimization-only", []string{"file.read"})
	digest.TargetSkillKey = "existing-key"
	digest.TargetSkillName = "existing-skill"
	digest.ParentVersionHash = "v1"
	payload, _ := json.Marshal(digest)
	proposal, err := curator.ProposeSkillCuration(ctx, "default", string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proposal, `"action":"SKIP"`) || provider.last.MaxTokens != 0 {
		t.Fatalf("optimization-only cohort reached curator: proposal=%s request=%+v", proposal, provider.last)
	}
}

func TestSkillCuratorRepairPreflightSkipsNoncanonicalSkillBeforeProvider(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &judgeCaptureProvider{reply: `{"action":"PATCH"}`}
	curator := &llmSkillCurator{provider: provider, store: store}
	digest := control.SkillEvidenceDigest{
		EvidenceSetHash: "noncanonical-repair", TargetSkillKey: "manual-skill-key",
		TargetSkillName: "manual-skill", TargetActiveContent: "# Manual Skill\n\nDo the thing.", ParentVersionHash: "v1",
		NegativeObservations: []control.WorkflowObservation{{
			ID: "failure", EvidenceRole: "failure_guard", OutcomeStatus: "completed", VerificationState: "passed",
			Incident: &control.SkillIncidentEvidence{
				FailureSignature: "manual-failure", FailedStepID: "Procedure", ErrorCategory: "invalid_procedure",
				ObservedErrorCategory: "command_failed", FailureObserved: true,
				RecoveryToolSequence: []string{"write_file"}, RecoveryVerified: true,
			},
		}},
	}
	payload, _ := json.Marshal(digest)
	proposal, err := curator.ProposeSkillCuration(context.Background(), "default", string(payload))
	if err != nil || !strings.Contains(proposal, `"action":"SKIP"`) {
		t.Fatalf("noncanonical repair proposal=%q err=%v", proposal, err)
	}
	if provider.last.MaxTokens != 0 {
		t.Fatalf("noncanonical repair burned a provider call: %+v", provider.last)
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

func TestSkillCuratorPublishesShortMainAndLazyLinkedResourcesAsOnePackage(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	storage, err := tools.NewSkillStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	curator := &llmSkillCurator{store: store, skillStorage: storage, budget: kernel.RuntimeContextBudget{SkillMainBytes: 2048, SkillMainTokens: 512}}
	digest := curatorTestDigest("package-evidence", []string{"file.read"})
	payload, _ := json.Marshal(digest)
	name := "lazy-package-skill"
	main := repairSkillContent(name,
		"Read the declared input and use references/detail.md only when the optional lookup table is needed.",
		"Return to ordinary planning when the declared input is absent.")
	resources := map[string]string{"references/detail.md": "# Optional lookup table\n\nOnly load this page when the main procedure requests it."}
	proposal, _ := json.Marshal(skillCuratorWire{
		Action: "CREATE", Name: name, Content: main, Resources: resources, Reason: "verified cohort",
	})
	summary, err := curator.ApplySkillCuration(ctx, "default", string(payload), string(proposal))
	if err != nil || !strings.Contains(summary, "promoted") {
		t.Fatalf("package promotion=%q err=%v", summary, err)
	}
	pack, err := tools.ReadSkillPackageForTenant("default", name,
		tools.WithSkillStorage(map[string]interface{}{
			"_tenant_id": "default", "_context": ctx,
			"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: "default", WorkspaceID: digest.WorkspaceID},
		}, storage))
	if err != nil {
		t.Fatal(err)
	}
	if pack.MainSource != main || pack.ResourceBodies["references/detail.md"] != resources["references/detail.md"] {
		t.Fatalf("published package=%+v", pack)
	}
	version, err := store.SkillCandidateByEvidence(ctx, "default", digest.EvidenceSetHash)
	if err != nil || version == nil || version.PackageHash != pack.PackageHash || string(version.ResourceManifest) == "[]" {
		t.Fatalf("durable package version=%+v pack=%+v err=%v", version, pack, err)
	}
}

func TestSkillCuratorRejectsOversizedMainInsteadOfCreatingAHiddenTruncation(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	storage, err := tools.NewSkillStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	curator := &llmSkillCurator{store: store, skillStorage: storage, budget: kernel.RuntimeContextBudget{SkillMainBytes: 700, SkillMainTokens: 1000}}
	digest := curatorTestDigest("oversized-main", []string{"file.read"})
	payload, _ := json.Marshal(digest)
	name := "oversized-main"
	proposal, _ := json.Marshal(skillCuratorWire{
		Action: "CREATE", Name: name,
		Content: repairSkillContent(name, strings.Repeat("read the declared record; ", 80), "Return to ordinary planning."),
		Reason:  "verified cohort",
	})
	if _, err := curator.ApplySkillCuration(ctx, "default", string(payload), string(proposal)); err == nil || !strings.Contains(err.Error(), "delivered in full") {
		t.Fatalf("oversized main error=%v", err)
	}
	if version, err := store.SkillCandidateByEvidence(ctx, "default", digest.EvidenceSetHash); err != nil || version != nil {
		t.Fatalf("oversized main materialized version=%+v err=%v", version, err)
	}
}

func TestSkillCuratorCreateUsesResourceAndCJKTokenBudgets(t *testing.T) {
	budget := kernel.RuntimeContextBudget{SkillMainBytes: 8192, SkillMainTokens: 120}
	name := "cjk-budget"
	tokenHeavy := repairSkillContent(name, strings.Repeat("检查记录", 80), "Return to ordinary planning.")
	if err := validateCuratedSkillPackageShape(tokenHeavy, nil, name); err != nil {
		t.Fatal(err)
	}
	if err := validateCuratedSkillCreateDelivery(tokenHeavy, nil, budget); err == nil || !strings.Contains(err.Error(), "delivered in full") {
		t.Fatalf("token-heavy CJK main passed exact delivery validation: %v", err)
	}

	resourceBudget := kernel.RuntimeContextBudget{SkillMainBytes: 1000, SkillMainTokens: 1000}
	main := repairSkillContent(name, strings.Repeat("read record; ", 35), "Return to ordinary planning.")
	resources := map[string]string{}
	for i := 0; i < 12; i++ {
		resources[fmt.Sprintf("references/very-long-resource-name-%02d.md", i)] = "detail"
	}
	withoutResources := curatedSkillDelivery(main, nil, resourceBudget)
	withResources := curatedSkillDelivery(main, resources, resourceBudget)
	if withoutResources.Mode != kernel.SkillDeliveryModeFull || withResources.Mode != kernel.SkillDeliveryModePaged {
		t.Fatalf("resource reserve was not part of exact CREATE budget: without=%+v with=%+v", withoutResources, withResources)
	}
}

func TestSkillCuratorPagedLegacyPatchAllowsOnlyNonGrowth(t *testing.T) {
	budget := kernel.RuntimeContextBudget{SkillMainBytes: 700, SkillMainTokens: 1000}
	name := "legacy-paged"
	active := repairSkillContent(name, strings.Repeat("read legacy record; ", 70), "Return to ordinary planning.")
	shorter := repairSkillContent(name, strings.Repeat("read current record; ", 40), "Return to ordinary planning.")
	if curatedSkillDelivery(active, nil, budget).Mode != kernel.SkillDeliveryModePaged {
		t.Fatal("test active main must be paged")
	}
	if err := validateCuratedSkillPatchDelivery(active, shorter, nil, budget); err != nil {
		t.Fatalf("non-growing legacy PATCH rejected: %v", err)
	}
	grown := repairSkillContent(name, strings.Repeat("read expanded record; ", 90), "Return to ordinary planning.")
	if err := validateCuratedSkillPatchDelivery(active, grown, nil, budget); err == nil || !strings.Contains(err.Error(), "cannot grow") {
		t.Fatalf("growing legacy PATCH accepted: %v", err)
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
			EvidenceRole: "success_path", DurationMS: int64(100 + i), ToolEvidence: curatorTestToolEvidence(tools),
		})
	}
	return control.SkillEvidenceDigest{
		EvidenceSetHash: evidenceHash, WorkflowSignature: "signature",
		IdentityTenantID: "default", ControlTenantID: "default", PersonID: "person",
		WorkspaceID: "workspace", SuccessObservations: observations,
	}
}

func curatorTestToolEvidence(toolNames []string) []control.WorkflowToolEvidence {
	out := make([]control.WorkflowToolEvidence, 0, len(toolNames))
	for _, name := range toolNames {
		evidence := control.WorkflowToolEvidence{Name: name, Origin: "builtin", Category: "general", RiskLevel: "low"}
		if name == "terminal" {
			evidence.RiskLevel = "high"
			evidence.OperationClasses = []string{"exec.in_turn"}
		}
		out = append(out, evidence)
	}
	return out
}

func repairSkillContent(name, procedure, recovery string) string {
	return "---\nname: " + name + "\ndescription: Update a release record with the declared layout.\n---\n\n" +
		"## Applicability\nRelease record updates.\n\n## Inputs\nA record path.\n\n" +
		"## Preconditions\nThe record exists.\n\n## Procedure\n" + procedure + "\n\n" +
		"## Failure Guards\nDo not guess the layout.\n\n## Recovery\n" + recovery + "\n\n" +
		"## Verification\nRead the updated record and verify its fields."
}
