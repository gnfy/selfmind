package httpapi

import (
	"errors"
	"strings"
	"testing"

	"selfmind/internal/gateway/delivery"
)

// TestStaleResultRecoveryReplyReportsCleanupFailure pins the branch order. The
// recap reaches the person (Confirmed) but the pending rows cannot be closed,
// so Dismissed stays 0 and the rows survive. Checking Confirmed before the
// cleanup failure reported plain success, and because the recap's logical key
// is already delivered and deduplicated, re-running recovery could never
// resend it — the person was told the backlog was cleared while it was not.
func TestStaleResultRecoveryReplyReportsCleanupFailure(t *testing.T) {
	reply := staleResultRecoveryReply(delivery.StaleResultRecovery{
		Candidates: 145, Groups: 12, Accepted: true, Confirmed: true,
		Dismissed: 0, CleanupFailed: true,
	}, errors.New("database is locked"))

	if strings.Contains(reply, "closed 0 old recovery row(s)") {
		t.Fatalf("cleanup failure must not render as a successful recovery: %q", reply)
	}
	for _, want := range []string{"could not be closed", "dismiss stale-results", "will not resend"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q: %q", want, reply)
		}
	}
}

func TestStaleResultRecoveryReplyConfirmedAndTruncated(t *testing.T) {
	clean := staleResultRecoveryReply(delivery.StaleResultRecovery{
		Candidates: 3, Groups: 2, Accepted: true, Confirmed: true, Dismissed: 3,
	}, nil)
	if !strings.Contains(clean, "closed 3 old recovery row(s)") {
		t.Errorf("confirmed reply=%q", clean)
	}
	if strings.Contains(clean, "run the command again") {
		t.Errorf("a complete batch must not claim more remain: %q", clean)
	}

	batched := staleResultRecoveryReply(delivery.StaleResultRecovery{
		Candidates: 1000, Groups: 40, Accepted: true, Confirmed: true, Dismissed: 1000, Truncated: true,
	}, nil)
	if !strings.Contains(batched, "run the command again") {
		t.Errorf("a truncated batch must say more remain: %q", batched)
	}
}

func TestStaleResultRecoveryReplyUnconfirmedKeepsRows(t *testing.T) {
	reply := staleResultRecoveryReply(delivery.StaleResultRecovery{
		Candidates: 5, Groups: 3, Accepted: true,
	}, nil)
	if !strings.Contains(reply, "left untouched") {
		t.Errorf("unconfirmed reply must say rows were kept: %q", reply)
	}
	empty := staleResultRecoveryReply(delivery.StaleResultRecovery{}, nil)
	if !strings.Contains(empty, "No stale final-result messages") {
		t.Errorf("empty reply=%q", empty)
	}
}
