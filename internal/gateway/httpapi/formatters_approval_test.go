package httpapi

import (
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestFormatApprovalsShowsLiveAndParkedAge(t *testing.T) {
	now := time.Now()
	parkedAt := now.Add(-3 * time.Hour)
	approvals := []control.ApprovalRequest{
		{ID: "apr_live", ActionType: "terminal", Status: "pending", WaiterState: "live", CreatedAt: now.Add(-12 * time.Minute)},
		{ID: "apr_parked", ActionType: "terminal", Status: "pending", WaiterState: "parked", CreatedAt: now.Add(-5 * time.Hour), ParkedAt: &parkedAt},
	}
	got := formatApprovals(approvals, nil)
	for _, want := range []string{"[pending 12m]", "[parked 3h; answering resumes the task]", "apr_live", "apr_parked"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatApprovals missing %q:\n%s", want, got)
		}
	}
}

func TestApprovalAgeDoesNotReportFutureTimestamp(t *testing.T) {
	now := time.Now()
	if got := approvalAge(control.ApprovalRequest{CreatedAt: now.Add(time.Hour)}, now); got != "<1m" {
		t.Fatalf("future approval age=%q", got)
	}
}
