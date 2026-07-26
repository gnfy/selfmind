package memory

import (
	"context"
	"testing"
	"time"
)

func TestCanonicalMemoryEffectiveAt(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		row  CanonicalMemory
		want bool
	}{
		{name: "unbounded", row: CanonicalMemory{}, want: true},
		{name: "active window", row: CanonicalMemory{ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}, want: true},
		{name: "future", row: CanonicalMemory{ValidFrom: now.Add(time.Hour)}, want: false},
		{name: "expired", row: CanonicalMemory{ValidUntil: now.Add(-time.Nanosecond)}, want: false},
		{name: "expires now", row: CanonicalMemory{ValidUntil: now}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanonicalMemoryEffectiveAt(tt.row, now); got != tt.want {
				t.Fatalf("CanonicalMemoryEffectiveAt()=%v want %v", got, tt.want)
			}
		})
	}
}

func TestReadModelFactsExcludesExpiredCanonicalWithoutRevivingLegacyShadow(t *testing.T) {
	ctx := context.Background()
	provider, err := NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	manager := NewMemoryManager(provider)
	const content = "Temporary rollout is currently queued"
	if err := manager.AddFact(ctx, "person", "memory", content); err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyIntakeWrite(ctx, "person", IntakeWrite{
		Decision:   "ADD",
		Target:     "memory",
		Scope:      "legacy",
		Content:    content,
		ValidUntil: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	facts, served := ReadModelFacts(ctx, manager, "person")
	if len(facts) != 0 {
		t.Fatalf("expired canonical or its legacy shadow reached the read model: %+v", facts)
	}
	if len(served) != 0 {
		t.Fatalf("expired canonical must not be reported as served: %v", served)
	}
}
