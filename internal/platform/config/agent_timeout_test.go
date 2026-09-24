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

func TestCompactionTimeoutDuration(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultCompactionTimeout},
		{"90s", 90 * time.Second},
		{" 2m ", 2 * time.Minute},
		{"0", DefaultCompactionTimeout},
		{"-5s", DefaultCompactionTimeout},
		{"invalid", DefaultCompactionTimeout},
	} {
		if got := (AgentConfig{CompactionTimeout: tc.raw}).CompactionTimeoutDuration(); got != tc.want {
			t.Fatalf("CompactionTimeoutDuration(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
