package api

import (
	"testing"
	"time"

	"selfmind/internal/kernel"
)

// The detail names the disposition, the reason, and the authorizing input. An
// input that arrived today reads as a clock time in the reader's location; an
// older one adds its date. A step that is not cancelled, or an input of an
// unknown kind, names no input.
func TestPlanCancellationDetailReadsInThePersonsTerms(t *testing.T) {
	here := time.FixedZone("UTC-4", -4*3600)
	now := time.Date(2026, time.October, 7, 18, 30, 0, 0, here)
	daemon := time.FixedZone("UTC+8", 8*3600)
	from := func(input string, at time.Time, earlier bool) *kernel.PlanQuoteOrigin {
		origin := &kernel.PlanQuoteOrigin{Input: input, EarlierRun: earlier}
		if !at.IsZero() {
			origin.ReceivedAt = at.Format(time.RFC3339)
		}
		return origin
	}
	today := time.Date(2026, time.October, 7, 14, 2, 0, 0, here)
	for _, tc := range []struct {
		status, disposition, reason string
		origin                      *kernel.PlanQuoteOrigin
		want                        string
	}{
		{"cancelled", "user_takeover", "you deploy it", from("guidance", today, false), "Cancelled · user takeover: you deploy it · authorized by your guidance at 14:02"},
		{"cancelled", "user_takeover", "", from("guidance", time.Date(2026, time.October, 6, 23, 59, 0, 0, here), false), "Cancelled · user takeover · authorized by your guidance at Oct 6 23:59"},
		// Recorded in another zone, read in the person's: Oct 8 02:02 +08:00 is Oct 7 14:02 here.
		{"cancelled", "not_required", "out of scope", from("guidance", time.Date(2026, time.October, 8, 2, 2, 0, 0, daemon), false), "Cancelled · not required: out of scope · authorized by your guidance at 14:02"},
		{"cancelled", "not_required", "out of scope", from("request", today, false), "Cancelled · not required: out of scope · authorized by your request that started this run"},
		{"cancelled", "not_required", "out of scope", from("request", today, true), "Cancelled · not required: out of scope · authorized by your earlier request at 14:02"},
		{"cancelled", "user_takeover", "x", from("guidance", time.Time{}, true), "Cancelled · user takeover: x · authorized by your earlier guidance"},
		{"cancelled", "user_takeover", "x", from("tool output", today, false), "Cancelled · user takeover: x"},
		{"cancelled", "not_required", "replaced", nil, "Cancelled · not required: replaced"},
		{"cancelled", "", "", nil, "Cancelled"},
		{"pending", "unfinished", "access is unavailable", from("guidance", today, false), "Unfinished: access is unavailable"},
		{"pending", "", "", nil, ""},
		{"completed", "user_takeover", "stale", from("guidance", today, false), ""},
	} {
		if got := PlanCancellationDetail(tc.status, tc.disposition, tc.reason, tc.origin, now); got != tc.want {
			t.Errorf("%s/%s/%+v = %q, want %q", tc.status, tc.disposition, tc.origin, got, tc.want)
		}
	}
}
