package cli

import "testing"

func TestParseApprovalAnswer(t *testing.T) {
	cases := []struct {
		in       string
		decision string
		approved bool
	}{
		{"y", "approved", true},
		{"yes", "approved", true},
		{"approve", "approved", true},
		{"OK", "approved", true},
		{"n", "rejected", false},
		{"no", "rejected", false},
		{"deny", "rejected", false},
		{"", "rejected", false}, // bare Enter denies (safe default)
		{"maybe", "", false},    // unclear → keep waiting
		{"why?", "", false},
	}
	for _, tc := range cases {
		decision, approved := parseApprovalAnswer(tc.in)
		if decision != tc.decision || approved != tc.approved {
			t.Errorf("parseApprovalAnswer(%q) = (%q,%v), want (%q,%v)", tc.in, decision, approved, tc.decision, tc.approved)
		}
	}
}
