package tools

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

func TestActivateSkillPackageKeepsModelSlashAndBindingDeliveryIdentical(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "activation-contract", "Activation Contract")
	if err != nil {
		t.Fatal(err)
	}
	pack := SkillPackageSnapshot{
		Info:        SkillInfo{Name: "release-inspection", Scope: SkillScopeUser, Source: SkillSourceAgentCreated},
		MainSource:  strings.Repeat("Inspect the release evidence and verify it.\n", 180),
		VersionHash: "version-shared",
		PackageHash: "package-shared",
	}
	budget := kernel.RuntimeContextBudgetForContextTokens(32 * 1024)
	var baseline ActivatedSkillPackage
	for index, source := range []string{"model", "slash", "task_binding"} {
		task, err := store.CreateTask(ctx, control.TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			Title: "activate via " + source, Channel: "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", "inspect release")
		if err != nil {
			t.Fatal(err)
		}
		activated, err := ActivateSkillPackage(ctx, store, pack, ActivateSkillPackageInput{
			IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
			PersonID: identity.PersonID, WorkspaceID: "workspace-1", RunID: run.ID,
			WorkUnitID: run.WorkUnitID, ExecutionLane: "main", SkillKey: "skill-shared",
			ActivationSource: source, AttachmentMode: "explicit", ContentRef: "/skills/release-inspection/SKILL.md",
			CreatedBy: "external_reconcile", Budget: budget,
		})
		if err != nil {
			t.Fatalf("%s activation: %v", source, err)
		}
		if activated.Context.WorkUnitSequence <= 0 {
			t.Fatalf("%s activation lost daemon-side work-unit sequence", source)
		}
		if index == 0 {
			baseline = activated
			continue
		}
		if activated.Activation.PackageHash != baseline.Activation.PackageHash ||
			activated.Delivery.Content != baseline.Delivery.Content ||
			activated.Delivery.DeliveredHash != baseline.Delivery.DeliveredHash ||
			activated.Delivery.DeliveredBytes != baseline.Delivery.DeliveredBytes ||
			activated.Delivery.Mode != baseline.Delivery.Mode ||
			activated.Delivery.ContractVersion != baseline.Delivery.ContractVersion {
			t.Fatalf("%s delivery diverged from model baseline:\nbase=%+v\ngot=%+v", source, baseline.Delivery, activated.Delivery)
		}
	}
}

func TestActivateSkillPackageIdempotentRetryKeepsPinnedResourceManifest(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "activation-retry", "Activation Retry")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "retry activation", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "retry activation")
	if err != nil {
		t.Fatal(err)
	}
	main := "## Procedure\nRead the pinned reference.\n"
	pack := func(resources map[string]string) SkillPackageSnapshot {
		versionHash, packageHash, manifest := BuildSkillPackageIdentity(main, resources)
		return SkillPackageSnapshot{
			Info:       SkillInfo{Name: "retry-package", Scope: SkillScopeUser, Source: SkillSourceAgentCreated},
			MainSource: main, VersionHash: versionHash, PackageHash: packageHash,
			ResourceManifest: manifest, ResourceBodies: resources,
		}
	}
	input := ActivateSkillPackageInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		ExecutionLane: "main", SkillKey: "skill-retry", ActivationSource: "model",
		ContentRef: "/skills/retry-package/SKILL.md", CreatedBy: "test",
		Budget: kernel.DefaultRuntimeContextBudget(),
	}
	first, err := ActivateSkillPackage(ctx, store, pack(map[string]string{
		"references/original.md": "pinned bytes",
	}), input)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := ActivateSkillPackage(ctx, store, pack(map[string]string{
		"references/original.md": "changed bytes",
		"references/new.md":      "new bytes",
	}), input)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Activation.ID != first.Activation.ID || retried.Context.PackageHash != first.Context.PackageHash {
		t.Fatalf("idempotent retry replaced the activation: first=%+v retry=%+v", first.Activation, retried.Activation)
	}
	if got := retried.Context.LinkedFiles; len(got) != 1 || got[0] != "references/original.md" {
		t.Fatalf("idempotent retry mixed drifted resources into pinned context: %v", got)
	}
	prompt := retried.Context.Prompt(input.Budget.SkillMainBytes)
	if !strings.Contains(prompt, "references/original.md") || strings.Contains(prompt, "references/new.md") {
		t.Fatalf("pinned prompt used the retry manifest:\n%s", prompt)
	}
}
