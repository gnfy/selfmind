package cliapp

import (
	"strings"
	"testing"

	"selfmind/internal/buildinfo"
)

func TestGatewayBuildState(t *testing.T) {
	if got := gatewayBuildState(""); got != "unknown" {
		t.Fatalf("empty gateway build = %q", got)
	}
	if got := gatewayBuildState(buildinfo.Fingerprint()); got != buildinfo.Fingerprint() {
		t.Fatalf("matching gateway build = %q", got)
	}
	if got := gatewayBuildState("old-build"); !strings.HasPrefix(got, "mismatch:") {
		t.Fatalf("mismatched gateway build = %q", got)
	}
}
