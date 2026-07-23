package control

import (
	"context"
	"testing"
	"time"
)

func TestMaintenanceProviderUsageAggregatesPhysicalCalls(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now()
	for _, call := range []MaintenanceProviderCall{
		{TenantID: "tenant", Role: "memory_extract", Provider: "kimi-coding", Model: "kimi-for-coding",
			Status: MaintenanceProviderCallFailed, ErrorClass: "empty_response", InputTokens: 100, OutputTokens: 20, CreatedAt: now},
		{TenantID: "tenant", Role: "memory_extract", Provider: "kimi-coding", Model: "kimi-for-coding",
			Status: MaintenanceProviderCallCircuitOpen, CreatedAt: now},
		{TenantID: "tenant", Role: "maintenance_backup", Provider: "minimax", Model: "MiniMax-M2.7",
			Status: MaintenanceProviderCallSucceeded, TriggerClass: "empty_response", InputTokens: 100, OutputTokens: 10, CreatedAt: now},
	} {
		if err := store.RecordMaintenanceProviderCall(context.Background(), call); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := store.MaintenanceProviderUsageSince(context.Background(), "tenant", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 {
		t.Fatalf("usage rows = %+v", usage)
	}
	var calls int
	for _, item := range usage {
		calls += item.Calls
	}
	if calls != 3 {
		t.Fatalf("total calls = %d, want 3", calls)
	}
}

func TestPruneMaintenanceProviderCalls(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordMaintenanceProviderCall(context.Background(), MaintenanceProviderCall{
		TenantID: "tenant", Provider: "kimi-coding", Status: MaintenanceProviderCallSucceeded,
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	removed, err := store.PruneMaintenanceProviderCalls(context.Background(), 24*time.Hour)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
}
