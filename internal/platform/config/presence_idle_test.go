package config

import (
	"testing"
	"time"
)

// gateway.presence_idle_timeout: default 5m, explicit zero disables (never
// idle — the old always-attached behavior), invalid values fall back to the
// default, bare numbers read as seconds.
func TestPresenceIdleTimeoutDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultPresenceIdleTimeout},
		{"5m", 5 * time.Minute},
		{"90s", 90 * time.Second},
		{"0", 0},
		{"0s", 0},
		{"-1m", 0},
		{"300", 300 * time.Second},
		{"garbage", DefaultPresenceIdleTimeout},
	}
	for _, tc := range cases {
		g := GatewayConfig{PresenceIdleTimeout: tc.raw}
		if got := g.PresenceIdleTimeoutDuration(); got != tc.want {
			t.Errorf("PresenceIdleTimeoutDuration(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
	if DefaultPresenceIdleTimeout != 5*time.Minute {
		t.Fatalf("default presence idle timeout must stay 5m, got %v", DefaultPresenceIdleTimeout)
	}
}
