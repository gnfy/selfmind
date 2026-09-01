package control

import (
	"context"
	"testing"
)

func TestMigrateLegacyTaskReferencesRequiresExactUserEvidenceAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, identity, task, initial := newRecoveryFixture(t)
	if err := store.FinishRun(ctx, identity.TenantID, initial.ID, "done"); err != nil {
		t.Fatal(err)
	}

	exactInputs := []string{"continue RUQX-500 deployment", "check whether RUQX-500 completed"}
	for _, input := range exactInputs {
		run, err := store.StartRunWithWorkKey(ctx, task, "cli", input, "RUQX-500")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
			t.Fatal(err)
		}
	}
	inferred, err := store.StartRunWithWorkKey(ctx, task, "cli", "continue the deployment", "RUQX-999")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, inferred.ID, "done"); err != nil {
		t.Fatal(err)
	}

	dry, err := store.MigrateLegacyTaskReferences(ctx, identity.TenantID, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Scanned != 3 || dry.Eligible != 2 || dry.SkippedInferred != 1 || dry.Applied != 0 {
		t.Fatalf("dry-run result = %+v", dry)
	}
	if refs, err := store.ListTaskReferencesForTask(ctx, identity.TenantID, identity.PersonID, task.ID, 10); err != nil || len(refs) != 0 {
		t.Fatalf("dry-run mutated references: refs=%+v err=%v", refs, err)
	}

	applied, err := store.MigrateLegacyTaskReferences(ctx, identity.TenantID, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Applied != 2 || applied.AlreadyImported != 0 || applied.SkippedInferred != 1 {
		t.Fatalf("apply result = %+v", applied)
	}
	refs, err := store.ListTaskReferencesForTask(ctx, identity.TenantID, identity.PersonID, task.ID, 10)
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	// Imported run support keeps the reference at candidate (search hint):
	// auto-promotion is frozen by simplification P2.
	if refs[0].RawValue != "RUQX-500" || refs[0].SupportCount != 2 || refs[0].Status != TaskReferenceCandidate {
		t.Fatalf("migrated reference = %+v", refs[0])
	}

	again, err := store.MigrateLegacyTaskReferences(ctx, identity.TenantID, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Applied != 0 || again.AlreadyImported != 2 {
		t.Fatalf("idempotent apply = %+v", again)
	}
}
