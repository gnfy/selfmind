package executionenv

import (
	"slices"
	"testing"
)

func TestGitProbeEnvUsesControlPlaneBoundary(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "GH_TOKEN=operator-login", "SELF_GATEWAY_TOKEN=daemon-secret",
		"SELFMIND_PERSON_ID=private-person", "GIT_CONFIG_COUNT=1", "HTTP_PROXY=http://localhost:7897",
	}
	got := cleanGitProbeEnv(parent)
	for _, want := range []string{"PATH=/usr/bin", "GH_TOKEN=operator-login", "HTTP_PROXY=http://localhost:7897", "GIT_OPTIONAL_LOCKS=0"} {
		if !slices.Contains(got, want) {
			t.Fatalf("Git probe environment missing %q: %#v", want, got)
		}
	}
	for _, blocked := range []string{"SELF_GATEWAY_TOKEN=daemon-secret", "SELFMIND_PERSON_ID=private-person", "GIT_CONFIG_COUNT=1"} {
		if slices.Contains(got, blocked) {
			t.Fatalf("Git probe inherited %q", blocked)
		}
	}
}
