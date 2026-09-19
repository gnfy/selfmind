package control

import (
	"context"
	"testing"
)

// Withdrawal difficulty must match publication difficulty.
//
// A repair version can publish on one verified recovery, so one attributable
// incident withdraws it and its parent returns. A cohort version publishes only
// after three independent verified runs, so a single incident must not overturn
// it — but it cannot be unwithdrawable either: it has no parent to fall back to,
// and nothing else removes it (idle decay is a tool action with no scheduler).
// Recurrence of its known failure is the proportional signal.
func TestCohortVersionWithdrawsOnlyAfterRepeatedFailure(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	const tenant = DefaultTenantID
	const key = "skill:cohort"
	hash, err := store.CreateSkillCandidateVersion(ctx, tenant, key, "cohort-skill", "", "body", "evidence-1", []string{"obs-1"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteSkillCandidate(ctx, tenant, key, hash, "/tmp/cohort-skill/SKILL.md"); err != nil {
		t.Fatal(err)
	}

	activeState := func() string {
		version, err := store.GetSkillVersion(ctx, tenant, key, hash)
		if err != nil {
			t.Fatal(err)
		}
		return version.State
	}
	if activeState() != "active" {
		t.Fatalf("version must start active, got %q", activeState())
	}

	// Below the threshold the version keeps serving every other task shape.
	for occurrences := 1; occurrences < SkillCohortVersionWithdrawalOccurrences; occurrences++ {
		withdrawn, err := store.WithdrawCohortSkillVersionOnRepeatedFailure(ctx, tenant, key, hash, occurrences)
		if err != nil {
			t.Fatal(err)
		}
		if withdrawn {
			t.Fatalf("one cohort version must survive %d occurrence(s): three verified runs published it", occurrences)
		}
		if activeState() != "active" {
			t.Fatalf("state changed below the threshold: %q", activeState())
		}
	}

	withdrawn, err := store.WithdrawCohortSkillVersionOnRepeatedFailure(ctx, tenant, key, hash, SkillCohortVersionWithdrawalOccurrences)
	if err != nil {
		t.Fatal(err)
	}
	if !withdrawn {
		t.Fatal("a repeatedly failing cohort version must be withdrawn; nothing else removes it")
	}
	if activeState() != "quarantined" {
		t.Fatalf("state = %q, want quarantined", activeState())
	}

	// Idempotent: a later match does not report a second withdrawal.
	again, err := store.WithdrawCohortSkillVersionOnRepeatedFailure(ctx, tenant, key, hash, SkillCohortVersionWithdrawalOccurrences+5)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("withdrawal must be reported once")
	}
}

// The other half of the same rule: a REPAIR version is withdrawn by its first
// attributable incident, not by recurrence, because its parent is still there
// to fall back to. Recurrence must not be a second, weaker door onto it.
//
// The same predicate also keeps a person-authored version out of this path
// entirely: the runtime does not disable an asset its owner published.
func TestRepairVersionIsNotWithdrawnByRecurrence(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	const tenant = DefaultTenantID
	const key = "skill:repaired"
	parent, err := store.CreateSkillCandidateVersion(ctx, tenant, key, "repaired-skill", "", "body", "evidence-parent", []string{"obs-1"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteSkillCandidate(ctx, tenant, key, parent, "/tmp/repaired-skill/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	repair, err := store.CreateSkillCandidateVersion(ctx, tenant, key, "repaired-skill", parent, "body v2", "evidence-repair", []string{"obs-2"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteSkillCandidate(ctx, tenant, key, repair, "/tmp/repaired-skill/SKILL.md"); err != nil {
		t.Fatal(err)
	}

	withdrawn, err := store.WithdrawCohortSkillVersionOnRepeatedFailure(ctx, tenant, key, repair, SkillCohortVersionWithdrawalOccurrences+10)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn {
		t.Fatal("a repair version is withdrawn by its first attributable incident, not by recurrence")
	}
	version, err := store.GetSkillVersion(ctx, tenant, key, repair)
	if err != nil {
		t.Fatal(err)
	}
	if version.State != "active" {
		t.Fatalf("repair version state = %q, want active", version.State)
	}
}
