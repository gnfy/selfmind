package control

import (
	"context"
	"testing"
)

// Review round 2, M1c in its watcher form: a watcher bound to a claim, or an
// owner-reviewed profile watcher, releases the effect only on a successful
// verdict. A failed or timed-out watcher, or a profile check whose status
// output is not "succeeded", leaves the claim as it was even when it is
// finalized after the call returned.
func TestWatcherReleasesOnlyOnSuccess(t *testing.T) {
	for _, status := range []string{ExternalWatchFailed, ExternalWatchTimedOut, ExternalWatchSucceeded} {
		t.Run("bound/"+status, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, runA, _, _ := externalEffectRuns(t, store)
			claims := uncertainEffect(t, store, owner, runA, "deploy", "service:prod")
			watch := boundWatch(t, store, owner, runA, claims[0].ID)
			if ok, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, status, "abc refs/tags/v1", ""); err != nil || !ok {
				t.Fatalf("finish: %v %v", ok, err)
			}
			if ok, err := store.MarkExternalWatchFinalized(ctx, owner.TenantID, watch.ID); err != nil || !ok {
				t.Fatalf("finalize: %v %v", ok, err)
			}
			_, released, err := store.ReleaseExternalEffectForWatch(ctx, owner.TenantID, watch.ID)
			if err != nil || released != (status == ExternalWatchSucceeded) {
				t.Fatalf("a %s bound watcher released the claim = %v: %v", status, released, err)
			}
		})
	}
	for _, tc := range []struct {
		status, output string
		release        bool
	}{
		{ExternalWatchFailed, `{"status":"succeeded"}`, false},
		{ExternalWatchSucceeded, `{"status":"failed"}`, false},
		{ExternalWatchSucceeded, `{"status":"succeeded"}`, true},
	} {
		t.Run("profile/"+tc.status+tc.output, func(t *testing.T) {
			store := newTestStore(t)
			owner, runA, _, _ := externalEffectRuns(t, store)
			watchers := newProfileWatchers(t, store, owner, "cluster:north")
			uncertainEffect(t, store, owner, runA, "deploy", "cluster:north")
			watch := watchers.register(runA, runA, "deploy")
			if released := watchers.finish(watch, tc.status, tc.output); released != tc.release {
				t.Fatalf("a %s profile watcher reporting %s released the effect = %v", tc.status, tc.output, released)
			}
			if got := claimState(t, store, owner, runA, "deploy"); (got.State == ExternalClaimObserved) != tc.release {
				t.Fatalf("effect after the profile watcher: %+v", got)
			}
		})
	}
}
