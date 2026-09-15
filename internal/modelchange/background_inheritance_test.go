package modelchange

import (
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
)

func TestBackgroundFollowsMainOnlyAfterExplicitReset(t *testing.T) {
	original := Snapshot{
		Primary:   config.ModelSelectionConfig{Provider: "test", Model: "main"},
		Auxiliary: config.ModelSelectionConfig{Provider: "test", Model: "background"},
		Roles:     RoleSelections{MemoryExtract: config.ModelSelectionConfig{Provider: "other", Model: "memory"}},
	}
	shared, err := ResetRoleCandidate(original, RouteAuxiliary)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := BuildCandidate(shared.Snapshot, SelectionPatch{Route: RoutePrimary, Provider: "new", Model: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if got := changed.Snapshot.Auxiliary; !got.FollowPrimary || got.Provider != "new" || got.Model != "next" {
		t.Fatalf("explicit inheritance did not follow Main: %+v", got)
	}
	if changed.Snapshot.Roles.MemoryExtract != original.Roles.MemoryExtract {
		t.Fatal("role override changed")
	}
	cfg := &config.Config{}
	ApplySnapshot(cfg, changed.Snapshot)
	if got := cfg.EffectiveAuxiliary(); got.Provider != "new" || got.Model != "next" {
		t.Fatalf("runtime route: %+v", got)
	}
	if SnapshotFromConfig(cfg) != changed.Snapshot {
		t.Fatal("snapshot round-trip lost inheritance")
	}
	independent, err := BuildCandidate(changed.Snapshot, SelectionPatch{Route: RouteAuxiliary, Provider: "new", Model: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if independent.Snapshot.Auxiliary.FollowPrimary {
		t.Fatal("explicit selection must break inheritance even for the same model")
	}
	for _, snapshot := range []Snapshot{original, independent.Snapshot} {
		result, err := BuildCandidate(snapshot, SelectionPatch{Route: RoutePrimary, Provider: "third", Model: "changed"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Snapshot.Auxiliary != snapshot.Auxiliary {
			t.Fatal("independent Background changed with Main")
		}
	}
}

func TestLegacySnapshotDoesNotGainInheritanceInFingerprint(t *testing.T) {
	snapshot := Snapshot{Primary: config.ModelSelectionConfig{Provider: "p", Model: "m"}, Auxiliary: config.ModelSelectionConfig{Provider: "p", Model: "m"}}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "FollowPrimary") {
		t.Fatal("new zero-value field changes historical snapshot fingerprints")
	}
	if normalizeSnapshot(snapshot).Auxiliary.FollowPrimary {
		t.Fatal("matching legacy routes must remain independent")
	}
}
