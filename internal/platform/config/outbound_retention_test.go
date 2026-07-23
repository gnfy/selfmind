package config

import (
	"testing"
	"time"
)

func TestOutboundRetentionDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultOutboundRetention},
		{"336h", 14 * 24 * time.Hour},
		{"24h", 24 * time.Hour},
		{"0", 0},
		{"0s", 0},
		{"-1h", 0},
		{"86400", 24 * time.Hour},
		{"garbage", DefaultOutboundRetention},
	}
	for _, tc := range cases {
		g := GatewayConfig{OutboundRetention: tc.raw}
		if got := g.OutboundRetentionDuration(); got != tc.want {
			t.Errorf("OutboundRetentionDuration(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
