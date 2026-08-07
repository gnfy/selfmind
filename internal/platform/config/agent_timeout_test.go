package config

import (
	"testing"
	"time"
)

func TestApprovalTriageTimeoutDuration(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultApprovalTriageTimeout},
		{"45s", 45 * time.Second},
		{"2m", 2 * time.Minute},
		{"0", DefaultApprovalTriageTimeout},
		{"invalid", DefaultApprovalTriageTimeout},
	} {
		if got := (AgentConfig{ApprovalTriageTimeout: tc.raw}).ApprovalTriageTimeoutDuration(); got != tc.want {
			t.Fatalf("ApprovalTriageTimeoutDuration(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
