package config

import (
	"testing"
	"time"
)

// gateway.presence_idle_timeout remains parseable for old configs, but the
// default is zero because presence now represents process attachment rather
// than keyboard activity.
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
	if DefaultPresenceIdleTimeout != 0 {
		t.Fatalf("default presence idle timeout must stay disabled, got %v", DefaultPresenceIdleTimeout)
	}
}

func TestPendingNotifyAfterDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", 15 * time.Minute},
		{"15m", 15 * time.Minute},
		{"30", 30 * time.Second},
		{"0", 0},
		{"garbage", 15 * time.Minute},
	}
	for _, tc := range cases {
		if got := (GatewayConfig{PendingNotifyAfter: tc.raw}).PendingNotifyAfterDuration(); got != tc.want {
			t.Errorf("PendingNotifyAfterDuration(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
