package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
)

// TestStandingGrantPolicyFollowsWhoAuthorisedTheWork: authorization comes from
// the moment a person decides, not the moment work executes. A person's own
// turn carries their decisions, and so does every daemon-started continuation
// of that same work — an answered approval, a finished watcher, a run recovered
// after a restart. Treating those as unattended would re-ask for work the
// person is in the middle of.
func TestStandingGrantPolicyFollowsWhoAuthorisedTheWork(t *testing.T) {
	ctx := context.Background()
	for _, origin := range []string{"", runOriginApproval, runOriginWatch, runOriginRecovery} {
		policy := standingGrantPolicy(ctx, api.MessageRequest{Origin: origin})
		if !policy.Allowed || !policy.NotAfter.IsZero() {
			t.Fatalf("origin %q must carry the person's decisions unbounded: %+v", origin, policy)
		}
	}
}

// A schedule is authorised when the person creates it, so it is frozen to the
// classes that existed then. A job created in January must not silently gain
// the classes its owner accepts in June for unrelated interactive work.
func TestScheduledWorkIsFrozenToItsAuthorisationInstant(t *testing.T) {
	ctx := context.Background()
	authorized := time.Now().Add(-72 * time.Hour)
	policy := standingGrantPolicy(ctx, api.MessageRequest{Origin: runOriginCron, StandingGrantsAuthorizedAt: authorized})
	if !policy.Allowed {
		t.Fatal("an authorised schedule must be able to run")
	}
	if !policy.NotAfter.Equal(authorized) {
		t.Fatalf("schedule must be frozen to its authorisation instant: %v", policy.NotAfter)
	}

	// A schedule nobody granted standing classes to consumes none and asks,
	// which is exactly what an unattended fire did before they existed.
	bare := standingGrantPolicy(ctx, api.MessageRequest{Origin: runOriginCron})
	if bare.Allowed {
		t.Fatalf("an unauthorised schedule must consume no standing class: %+v", bare)
	}
}
