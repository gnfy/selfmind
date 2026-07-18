package cliapp

import (
	"errors"
	"strings"
	"testing"
	"time"

	appcore "selfmind/internal/app"
)

func TestFormatModelRoleProbesGroupsResultsAndReportsFailure(t *testing.T) {
	section, failed := formatModelRoleProbes([]appcore.ModelRoleProbe{
		{Roles: []string{"background_review", "memory_extract"}, Provider: "kimi-coding", Model: "kimi-for-coding", Latency: 1250 * time.Millisecond},
		{Roles: []string{"summarizer"}, Provider: "minimax", Model: "MiniMax-M3", Latency: time.Second, Err: errors.New("provider 403: secret-token")},
	})
	if !failed {
		t.Fatal("failed probe must make doctor fail")
	}
	for _, want := range []string{
		"OK roles=background_review,memory_extract provider=kimi-coding model=kimi-for-coding",
		"FAIL roles=summarizer provider=minimax model=MiniMax-M3",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("probe section missing %q:\n%s", want, section)
		}
	}
}

func TestFormatModelRoleProbesHandlesNoConfiguredRoles(t *testing.T) {
	section, failed := formatModelRoleProbes(nil)
	if failed || !strings.Contains(section, "no explicitly configured roles") {
		t.Fatalf("section=%q failed=%v", section, failed)
	}
}
