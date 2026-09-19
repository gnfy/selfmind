package httpapi

import (
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

// A workspace class is the standing answer, so it has to read as one in the
// review surface: the person must be able to see what they widened and take it
// back from any endpoint. It carries no expiry — what bounds it is that the
// class is narrow, listed and revocable, not a deadline shorter than the work
// it covers.
func TestWorkspaceGrantReadsAsAStandingAnswer(t *testing.T) {
	grant := control.ApprovalGrant{
		ScopeKind:  "workspace",
		ScopeID:    "ws-1",
		PatternKey: "exec:requests execution on the host outside the isolated sandbox|resource=workspace:abc123:command:aws codebuild batch-get-builds",
	}
	line := describeApprovalGrant(grant, time.Now())
	for _, want := range []string{"aws codebuild batch-get-builds", "this workspace", "no expiry"} {
		if !strings.Contains(line, want) {
			t.Fatalf("grant line missing %q: %s", want, line)
		}
	}
	// The stored key is never shown: it carries a hashed workspace fingerprint
	// and internal reason text.
	if strings.Contains(line, "abc123") || strings.Contains(line, "|resource=") {
		t.Fatalf("storage detail leaked into the review surface: %s", line)
	}
}

// The prefix is what makes the class reviewable AND what keeps it narrow. A key
// whose leading program the floor has since banned must not survive review.
func TestWorkspaceGrantKeyStillFacesTheFloor(t *testing.T) {
	banned := control.ApprovalGrant{
		ScopeKind:  "workspace",
		PatternKey: "exec:requests execution on the host outside the isolated sandbox|resource=workspace:abc123:command:curl",
	}
	if line := describeApprovalGrant(banned, time.Now()); !strings.Contains(line, "host") {
		t.Fatalf("unexpected rendering: %s", line)
	}
}
