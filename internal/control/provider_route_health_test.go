package control

import (
	"context"
	"testing"
	"time"
)

func TestProviderRouteQuotaCircuitAllowsOneProbeAndReplaysBlockedJobs(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const tenant = "tenant"
	const route = "route-kimi"
	now := time.Now().Truncate(time.Second)
	allowed, probe, _, err := store.ClaimProviderRoute(ctx, tenant, route, "kimi-coding", "kimi-for-coding", now, time.Minute)
	if err != nil || !allowed || probe {
		t.Fatalf("initial claim allowed=%v probe=%v err=%v", allowed, probe, err)
	}
	next, err := store.OpenProviderRoute(ctx, tenant, route, "kimi-coding", "kimi-for-coding", "quota", "quota exhausted", "req-1", now, time.Minute, 4*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	allowed, _, gotNext, err := store.ClaimProviderRoute(ctx, tenant, route, "kimi-coding", "kimi-for-coding", now.Add(30*time.Second), time.Minute)
	if err != nil || allowed || !gotNext.Equal(next) {
		t.Fatalf("open claim allowed=%v next=%v want=%v err=%v", allowed, gotNext, next, err)
	}
	allowed, probe, _, err = store.ClaimProviderRoute(ctx, tenant, route, "kimi-coding", "kimi-for-coding", next, time.Minute)
	if err != nil || !allowed || !probe {
		t.Fatalf("half-open claim allowed=%v probe=%v err=%v", allowed, probe, err)
	}
	allowed, _, _, err = store.ClaimProviderRoute(ctx, tenant, route, "kimi-coding", "kimi-for-coding", next, time.Minute)
	if err != nil || allowed {
		t.Fatalf("second half-open claim allowed=%v err=%v", allowed, err)
	}

	for _, key := range []string{"job-1", "job-2"} {
		if _, err := store.EnqueueMaintenanceJob(ctx, tenant, key, 1, `{}`); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.ClaimMaintenanceJob(ctx, tenant, key, 1); err != nil || !ok {
			t.Fatalf("claim %s ok=%v err=%v", key, ok, err)
		}
		if ok, err := store.BlockMaintenanceJobForRoute(ctx, tenant, key, 1, route, "quota"); err != nil || !ok {
			t.Fatalf("block %s ok=%v err=%v", key, ok, err)
		}
	}
	requeued, err := store.CloseProviderRoute(ctx, tenant, route, next.Add(time.Second))
	if err != nil || requeued != 2 {
		t.Fatalf("close requeued=%d err=%v", requeued, err)
	}
	for _, key := range []string{"job-1", "job-2"} {
		job, err := store.GetMaintenanceJob(ctx, tenant, key, 1)
		if err != nil || job == nil || job.Status != MaintenanceJobPending || job.BlockedRouteID != "" {
			t.Fatalf("job %s = %+v err=%v", key, job, err)
		}
	}
}

func TestInactiveProviderRouteRequeuesBlockedJobsAfterConfigChange(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const tenant = "tenant"
	if _, err := store.EnqueueMaintenanceJob(ctx, tenant, "job", 1, `{}`); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ClaimMaintenanceJob(ctx, tenant, "job", 1); err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if ok, err := store.BlockMaintenanceJobForRoute(ctx, tenant, "job", 1, "old-route", "quota"); err != nil || !ok {
		t.Fatalf("block ok=%v err=%v", ok, err)
	}

	requeued, err := store.RequeueBlockedJobsForInactiveProviderRoutes(ctx, tenant, []string{"new-route"}, time.Now())
	if err != nil || requeued != 1 {
		t.Fatalf("requeued=%d err=%v", requeued, err)
	}
	job, err := store.GetMaintenanceJob(ctx, tenant, "job", 1)
	if err != nil || job == nil || job.Status != MaintenanceJobPending || job.BlockedRouteID != "" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}
