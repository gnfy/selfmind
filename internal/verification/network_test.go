package verification

import (
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

func offlineCheck(id, command string, started int64) Check {
	return Check{
		ToolCallID: id, Kind: "test", Command: command, CWD: "/workspace", Status: "succeeded",
		StartedAt: started, FinishedAt: started + 5,
		NetworkDecision: executionenv.NetworkWithheldNotBound, NetworkPermission: executionenv.NetworkPermissionGrant,
		NetworkMayFetch: true,
	}
}

// A passing verification that ran without network although the workspace's
// network grant permits it says so: the person and the model must not read a
// bare "passed" as coverage of anything that needs a remote service.
func TestPassedVerificationSaysWhenItRanOfflineDespiteAGrant(t *testing.T) {
	for _, command := range []string{"go test ./...", "npm test", "make check-integration"} {
		t.Run(command, func(t *testing.T) {
			state, summary := State(0, []Check{offlineCheck("check", command, 10)})
			if state != "passed" {
				t.Fatalf("state = %s; the result itself stays a pass", state)
			}
			for _, want := range []string{"1 current verification check(s) passed", "without network", "network grant", "network: true"} {
				if !strings.Contains(summary, want) {
					t.Fatalf("summary does not say %q: %s", want, summary)
				}
			}
			if got := OfflineDespiteGrant([]Check{offlineCheck("check", command, 10)}); got != 1 {
				t.Fatalf("OfflineDespiteGrant = %d", got)
			}
		})
	}
}

// Everything else reads as before: a check that had the network, ran under trust
// rather than a grant, or had no permission to withhold says nothing extra.
func TestOnlyAGrantThatWasLeftUnusedIsReported(t *testing.T) {
	base := offlineCheck("check", "go test ./...", 10)
	for name, alter := range map[string]func(*Check){
		"networked": func(c *Check) {
			c.NetworkDecision, c.NetworkPermission = executionenv.NetworkDecisionShared, executionenv.NetworkPermissionGrant
		},
		"trusted workspace": func(c *Check) { c.NetworkPermission = executionenv.NetworkPermissionTrust },
		"no grant":          func(c *Check) { c.NetworkDecision, c.NetworkPermission = executionenv.NetworkWithheldNoGrant, "" },
		"policy disabled": func(c *Check) {
			c.NetworkDecision, c.NetworkPermission = executionenv.NetworkWithheldPolicyDisabled, ""
		},
		"older evidence":       func(c *Check) { c.NetworkDecision, c.NetworkPermission = "", "" },
		"approval for one run": func(c *Check) { c.NetworkPermission = executionenv.NetworkPermissionApproval },
		// A check whose command cannot fetch anything (vet, lint, formatting) has
		// no coverage to lose offline, and saying so is noise.
		"command that cannot fetch": func(c *Check) { c.NetworkMayFetch = false },
	} {
		t.Run(name, func(t *testing.T) {
			check := base
			alter(&check)
			state, summary := State(0, []Check{check})
			if state != "passed" || strings.Contains(summary, "without network") || OfflineDespiteGrant([]Check{check}) != 0 {
				t.Fatalf("state=%s summary=%q", state, summary)
			}
		})
	}
}

// The latest attempt owns the obligation. A networked rerun that replaces the
// offline attempt removes the caveat; a failing check keeps its own verdict.
func TestOfflineCaveatFollowsTheEffectiveAttempt(t *testing.T) {
	deps := []string{"/workspace/go.mod"}
	offline := offlineCheck("offline", "go test ./...", 10)
	offline.Binding = &Binding{Version: 3, StepID: "step-tests", Criterion: "tests pass", Target: "step-tests", LocalDependencies: &deps}
	networked := Check{
		ToolCallID: "networked", Kind: "test", Command: "go test ./...", CWD: "/workspace", Status: "succeeded", StartedAt: 30, FinishedAt: 40,
		NetworkDecision: executionenv.NetworkDecisionShared, NetworkPermission: executionenv.NetworkPermissionGrant,
		Binding: &Binding{Version: 3, StepID: "step-tests", Criterion: "tests pass", Target: "step-tests", LocalDependencies: &deps},
	}
	if state, summary := State(0, []Check{offline}); state != "passed" || !strings.Contains(summary, "without network") {
		t.Fatalf("offline attempt: %s %q", state, summary)
	}
	if state, summary := State(0, []Check{offline, networked}); state != "passed" || strings.Contains(summary, "without network") {
		t.Fatalf("the networked rerun still carried the caveat: %s %q", state, summary)
	}
	failed := offline
	failed.Status = "failed"
	if state, summary := State(0, []Check{failed}); state != "failed" || strings.Contains(summary, "without network") {
		t.Fatalf("a failing check reports its failure, not a coverage caveat: %s %q", state, summary)
	}
	if got := OfflineDespiteGrant([]Check{failed}); got != 0 {
		t.Fatalf("a failing attempt counted as an offline pass: %d", got)
	}
}

// The count is bounded to what the summary can say, and only counts checks that
// are still current: one that ran before the latest relevant change is stale.
func TestOfflineCaveatCountsOnlyCurrentChecks(t *testing.T) {
	first, second := offlineCheck("first", "go test ./pkg/a", 10), offlineCheck("second", "go test ./pkg/b", 20)
	state, summary := State(0, []Check{first, second})
	if state != "passed" || !strings.Contains(summary, "2 of them") {
		t.Fatalf("two offline checks: %s %q", state, summary)
	}
	mixed := second
	mixed.NetworkDecision, mixed.NetworkPermission = executionenv.NetworkDecisionShared, executionenv.NetworkPermissionGrant
	if _, summary := State(0, []Check{first, mixed}); !strings.Contains(summary, "1 of them") {
		t.Fatalf("one offline of two: %q", summary)
	}
	if state, summary := State(100, []Check{first}); state != "stale" || strings.Contains(summary, "without network") {
		t.Fatalf("a stale check is not current evidence: %s %q", state, summary)
	}
}

// A check whose command cannot fetch anything has no coverage to lose offline:
// it passes plainly, however the runtime recorded the network, and only a check
// marked as able to fetch is counted or mentioned.
func TestOfflineCaveatNeedsACommandThatMayFetch(t *testing.T) {
	cannot := offlineCheck("vet", "go vet ./...", 10)
	cannot.NetworkMayFetch = false
	may := offlineCheck("test", "go test ./...", 20)

	state, summary := State(0, []Check{cannot})
	if state != "passed" || strings.Contains(summary, "without network") || OfflineDespiteGrant([]Check{cannot}) != 0 {
		t.Fatalf("a check that cannot fetch carried the caveat: %s %q", state, summary)
	}
	if got := OfflineDespiteGrant([]Check{may}); got != 1 {
		t.Fatalf("a check that may fetch was not counted: %d", got)
	}
	// Mixed: only the one that can fetch is counted, and the sentence says "1 of them".
	state, summary = State(0, []Check{cannot, may})
	if state != "passed" || !strings.Contains(summary, "2 current verification check(s) passed") || !strings.Contains(summary, "1 of them ran without network") {
		t.Fatalf("mixed checks: %s %q", state, summary)
	}
	if got := OfflineDespiteGrant([]Check{cannot, may}); got != 1 {
		t.Fatalf("mixed count = %d, want 1", got)
	}
	// The flag alone is nothing: a networked or trusted check that may fetch is not
	// reported either.
	for name, alter := range map[string]func(*Check){
		"networked": func(c *Check) {
			c.NetworkDecision, c.NetworkPermission = executionenv.NetworkDecisionShared, executionenv.NetworkPermissionGrant
		},
		"trusted": func(c *Check) { c.NetworkPermission = executionenv.NetworkPermissionTrust },
	} {
		check := may
		alter(&check)
		if OfflineDespiteGrant([]Check{check}) != 0 {
			t.Errorf("%s: a check that may fetch but was not offline despite a grant was counted", name)
		}
	}
}
