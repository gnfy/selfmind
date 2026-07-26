package eval

import (
	"context"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
)

func TestApplyStateSeedsStoresCanonicalMemoryInPersonPartition(t *testing.T) {
	ctx := context.Background()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	manager := memory.NewMemoryManager(provider)
	identity := &control.IdentityContext{
		TenantID: "tenant",
		PersonID: "person",
	}
	setup := &Setup{Memory: []SeedFact{{
		Target:    "user",
		Content:   "Release summaries should use one item per line.",
		Scope:     "global",
		Canonical: true,
	}}}

	if err := applyStateSeeds(ctx, nil, manager, identity, "workspace", setup); err != nil {
		t.Fatal(err)
	}

	personRows, err := provider.ListCanonicalMemories(ctx, identity.PersonID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalActive},
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(personRows) != 1 {
		t.Fatalf("person canonical rows=%d want 1", len(personRows))
	}
	tenantRows, err := provider.ListCanonicalMemories(ctx, identity.TenantID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalActive},
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tenantRows) != 0 {
		t.Fatalf("tenant canonical rows=%d want 0", len(tenantRows))
	}
}
