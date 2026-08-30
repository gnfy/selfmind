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

func TestInactiveProviderRouteSweepDoesNotReleaseRetryLimitPolicy(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const tenant = "tenant"
	if _, err := store.EnqueueMaintenanceJob(ctx, tenant, "retry-limited", 1, `{}`); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMaintenanceJob(ctx, tenant, "retry-limited", 1); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if err := store.FailMaintenanceJob(ctx, tenant, "retry-limited", 1, "temporary provider failure", 0); err != nil {
		t.Fatal(err)
	}
	if blocked, err := store.BlockMaintenanceJobAfterRetries(ctx, tenant, "retry-limited", 1, "retry limit"); err != nil || !blocked {
		t.Fatalf("block: blocked=%v err=%v", blocked, err)
	}

	requeued, err := store.RequeueBlockedJobsForInactiveProviderRoutes(ctx, tenant, []string{"route-current"}, time.Now())
	if err != nil || requeued != 0 {
		t.Fatalf("requeued=%d err=%v", requeued, err)
	}
	job, err := store.GetMaintenanceJob(ctx, tenant, "retry-limited", 1)
	if err != nil || job == nil || job.Status != MaintenanceJobBlockedProvider || job.BlockedRouteID != maintenanceRetryLimitRouteID {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}

func TestInactiveProviderRouteSweepDoesNotReleaseNetworkRoute(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const tenant = "tenant"
	if _, err := store.EnqueueMaintenanceJob(ctx, tenant, "network-blocked", 1, `{}`); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMaintenanceJob(ctx, tenant, "network-blocked", 1); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	routeID := MaintenanceNetworkRouteID("proxy-unreachable")
	if err := store.FailMaintenanceJobForRoute(ctx, tenant, "network-blocked", 1, routeID, "connection refused", 0); err != nil {
		t.Fatal(err)
	}
	if blocked, err := store.blockMaintenanceJobAfterRetriesForRoute(ctx, tenant, "network-blocked", 1, routeID, "connection refused"); err != nil || !blocked {
		t.Fatalf("block: blocked=%v err=%v", blocked, err)
	}

	requeued, err := store.RequeueBlockedJobsForInactiveProviderRoutes(ctx, tenant, []string{"provider-current"}, time.Now())
	if err != nil || requeued != 0 {
		t.Fatalf("requeued=%d err=%v", requeued, err)
	}
	job, err := store.GetMaintenanceJob(ctx, tenant, "network-blocked", 1)
	if err != nil || job == nil || job.Status != MaintenanceJobBlockedProvider || job.BlockedRouteID != routeID {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}

func TestHealthyFallbackRequeuesOnlyMatchingMaintenanceVersion(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const tenant = "tenant"
	const blockedRoute = "route-kimi"
	now := time.Now().Truncate(time.Second)
	if _, _, _, err := store.ClaimProviderRoute(ctx, tenant, blockedRoute, "kimi-coding", "kimi-for-coding", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenProviderRoute(ctx, tenant, blockedRoute, "kimi-coding", "kimi-for-coding", "quota", "quota exhausted", "req", now, time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		key     string
		version int
	}{{"post-run", 1}, {"skill-review", 100}} {
		if _, err := store.EnqueueMaintenanceJob(ctx, tenant, item.key, item.version, `{}`); err != nil {
			t.Fatal(err)
		}
		if ok, err := store.ClaimMaintenanceJob(ctx, tenant, item.key, item.version); err != nil || !ok {
			t.Fatalf("claim %s ok=%v err=%v", item.key, ok, err)
		}
		if ok, err := store.BlockMaintenanceJobForRoute(ctx, tenant, item.key, item.version, blockedRoute, "quota"); err != nil || !ok {
			t.Fatalf("block %s ok=%v err=%v", item.key, ok, err)
		}
	}

	// route-minimax has no failure record, so it is a healthy configured
	// fallback. Only the requested analyzer class may be released.
	requeued, err := store.RequeueBlockedJobsForHealthyProviderRoutes(ctx, tenant, 1, []string{blockedRoute, "route-minimax"}, now.Add(time.Second))
	if err != nil || requeued != 1 {
		t.Fatalf("requeued=%d err=%v", requeued, err)
	}
	postRun, _ := store.GetMaintenanceJob(ctx, tenant, "post-run", 1)
	skillReview, _ := store.GetMaintenanceJob(ctx, tenant, "skill-review", 100)
	if postRun == nil || postRun.Status != MaintenanceJobPending || postRun.BlockedRouteID != "" {
		t.Fatalf("post-run job=%+v", postRun)
	}
	if skillReview == nil || skillReview.Status != MaintenanceJobBlockedProvider || skillReview.BlockedRouteID != blockedRoute {
		t.Fatalf("skill-review job=%+v", skillReview)
	}
}

func TestOpenFallbackRoutesDoNotRequeueBlockedMaintenance(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const tenant = "tenant"
	now := time.Now().Truncate(time.Second)
	for _, route := range []string{"route-kimi", "route-minimax"} {
		if _, _, _, err := store.ClaimProviderRoute(ctx, tenant, route, route, "model", now, time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := store.OpenProviderRoute(ctx, tenant, route, route, "model", "quota", "quota exhausted", "req", now, time.Minute, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.EnqueueMaintenanceJob(ctx, tenant, "job", 1, `{}`); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ClaimMaintenanceJob(ctx, tenant, "job", 1); err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if ok, err := store.BlockMaintenanceJobForRoute(ctx, tenant, "job", 1, "route-kimi", "quota"); err != nil || !ok {
		t.Fatalf("block ok=%v err=%v", ok, err)
	}

	requeued, err := store.RequeueBlockedJobsForHealthyProviderRoutes(ctx, tenant, 1, []string{"route-kimi", "route-minimax"}, now.Add(time.Second))
	if err != nil || requeued != 0 {
		t.Fatalf("requeued=%d err=%v", requeued, err)
	}
	job, _ := store.GetMaintenanceJob(ctx, tenant, "job", 1)
	if job == nil || job.Status != MaintenanceJobBlockedProvider {
		t.Fatalf("job=%+v", job)
	}
}

func TestDaemonMaintenanceHealthRequeuesPersonOwnedJobs(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const healthTenant = "default"
	const personTenant = "person-owner"
	const blockedRoute = "route-kimi"
	now := time.Now().Truncate(time.Second)
	if _, _, _, err := store.ClaimProviderRoute(ctx, healthTenant, blockedRoute, "kimi-coding", "kimi-for-coding", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenProviderRoute(ctx, healthTenant, blockedRoute, "kimi-coding", "kimi-for-coding", "quota", "quota exhausted", "req", now, time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueMaintenanceJob(ctx, personTenant, "skill-review", 100, `{}`); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ClaimMaintenanceJob(ctx, personTenant, "skill-review", 100); err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if ok, err := store.BlockMaintenanceJobForRoute(ctx, personTenant, "skill-review", 100, blockedRoute, "quota"); err != nil || !ok {
		t.Fatalf("block ok=%v err=%v", ok, err)
	}

	requeued, err := store.RequeueBlockedJobsForHealthyProviderRoutesAcrossTenants(ctx, healthTenant, 100, []string{blockedRoute, "route-openrouter"}, now.Add(time.Second))
	if err != nil || requeued != 1 {
		t.Fatalf("requeued=%d err=%v", requeued, err)
	}
	job, err := store.GetMaintenanceJob(ctx, personTenant, "skill-review", 100)
	if err != nil || job == nil || job.Status != MaintenanceJobPending || job.BlockedRouteID != "" {
		t.Fatalf("job=%+v err=%v", job, err)
	}
}
