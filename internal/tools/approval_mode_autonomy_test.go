package tools

import "testing"

// The order narrowing relies on is read from what the modes actually ask, not
// from their names: for every operation shape, a lower rank asks wherever a
// higher one does. A mode that changes what it asks without changing its rank
// fails here.
func TestApprovalModeAutonomyFollowsWhatEachModeAsks(t *testing.T) {
	modes := []ApprovalMode{ApprovalReadOnly, ApprovalOnRequest, ApprovalAutoEdit, ApprovalSmart, ApprovalFullAuto}
	type operation struct {
		name                         string
		tool                         string
		dangerous, containedByCaller bool
	}
	var operations []operation
	for _, tool := range []string{"read_file", "write_file", "patch", "terminal", "execute_code", "verify", "watch_external", "web_search"} {
		for _, dangerous := range []bool{false, true} {
			for _, contained := range []bool{false, true} {
				operations = append(operations, operation{tool + "-dangerous=" + boolWord(dangerous) + "-contained=" + boolWord(contained), tool, dangerous, contained})
			}
		}
	}
	for _, lower := range modes {
		for _, higher := range modes {
			if ApprovalModeAutonomy(lower) > ApprovalModeAutonomy(higher) {
				continue
			}
			for _, op := range operations {
				asksLower := approvalNeeded(lower, op.tool, op.dangerous, op.containedByCaller)
				asksHigher := approvalNeeded(higher, op.tool, op.dangerous, op.containedByCaller)
				if asksHigher && !asksLower {
					t.Fatalf("%s (rank %d) lets %s run unasked where %s (rank %d) asks", lower, ApprovalModeAutonomy(lower), op.name, higher, ApprovalModeAutonomy(higher))
				}
			}
		}
	}
	// The ranks themselves, so a reorder is a deliberate edit.
	for mode, want := range map[ApprovalMode]int{ApprovalReadOnly: 0, ApprovalOnRequest: 1, ApprovalAutoEdit: 1, ApprovalSmart: 2, ApprovalFullAuto: 3} {
		if got := ApprovalModeAutonomy(mode); got != want {
			t.Fatalf("%s ranks %d, want %d", mode, got, want)
		}
	}
}

func boolWord(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func TestNarrowerApprovalModeKeepsTheModeThatAsksMore(t *testing.T) {
	for _, tc := range []struct{ a, b, want ApprovalMode }{
		{ApprovalFullAuto, ApprovalSmart, ApprovalSmart},
		{ApprovalSmart, ApprovalFullAuto, ApprovalSmart},
		{ApprovalSmart, ApprovalOnRequest, ApprovalOnRequest},
		{ApprovalOnRequest, ApprovalReadOnly, ApprovalReadOnly},
		{ApprovalFullAuto, ApprovalReadOnly, ApprovalReadOnly},
		// Equal rank keeps the first: auto-edit and on-request ask the same.
		{ApprovalAutoEdit, ApprovalOnRequest, ApprovalAutoEdit},
		{ApprovalSmart, ApprovalSmart, ApprovalSmart},
	} {
		if got := NarrowerApprovalMode(tc.a, tc.b); got != tc.want {
			t.Fatalf("NarrowerApprovalMode(%s, %s) = %s, want %s", tc.a, tc.b, got, tc.want)
		}
	}
}
