package tools

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildProcessEnvKeepsToolCredentialsAndStripsControlPlane(t *testing.T) {
	parent := []string{
		"PATH=/usr/local/bin:/usr/bin",
		"GH_TOKEN=operator-token",
		"AWS_PROFILE=production",
		"SELF_GATEWAY_TOKEN=daemon-secret-token",
		"SELF_TENANT_ID=tenant-private",
		"SELFMIND_FUTURE_CONTROL_SECRET=future-daemon-secret",
	}
	got := BuildProcessEnv(parent, DefaultProcessEnvPolicy())

	for _, want := range []string{
		"PATH=/usr/local/bin:/usr/bin",
		"GH_TOKEN=operator-token",
		"AWS_PROFILE=production",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("process environment missing %q: %#v", want, got)
		}
	}
	for _, blocked := range []string{
		"SELF_GATEWAY_TOKEN=daemon-secret-token",
		"SELF_TENANT_ID=tenant-private",
		"SELFMIND_FUTURE_CONTROL_SECRET=future-daemon-secret",
	} {
		if slices.Contains(got, blocked) {
			t.Fatalf("control-plane environment leaked: %q", blocked)
		}
	}
}

func TestCredentialShapedNameUsesBoundedSuffixes(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "APP_DSN", "GITHUB_PAT", "SIGNING_KEY"} {
		if !isCredentialShapedName(name) {
			t.Fatalf("%s should be credential-shaped", name)
		}
	}
	for _, name := range []string{"SELF_GATEWAY_ADDR", "SELFMIND_FLIGHT_DIR", "MONKEY", "HOTKEY"} {
		if isCredentialShapedName(name) {
			t.Fatalf("%s must not be registered as a secret", name)
		}
	}
}

func TestMCPEnvFilterCannotReintroduceSelfMindControlState(t *testing.T) {
	t.Setenv("SELFMIND_CHANNEL", "cli:private-channel")
	t.Setenv("MCP_PUBLIC_SETTING", "enabled")

	got := BuildProcessEnv(
		filterEnv([]string{"SELFMIND_CHANNEL", "MCP_PUBLIC_SETTING"}),
		DefaultProcessEnvPolicy(),
	)
	if slices.Contains(got, "SELFMIND_CHANNEL=cli:private-channel") {
		t.Fatalf("stdio MCP environment leaked gateway state: %#v", got)
	}
	if !slices.Contains(got, "MCP_PUBLIC_SETTING=enabled") {
		t.Fatalf("stdio MCP environment lost an explicitly allowed ordinary value: %#v", got)
	}
}

func TestSnapshotCredentialRefsDoesNotPersistValuesAndSurvivesRotation(t *testing.T) {
	firstRefs, firstPrincipal := SnapshotCredentialRefs([]string{
		"AWS_PROFILE=production",
		"AWS_ACCESS_KEY_ID=access-key-one",
		"AWS_SECRET_ACCESS_KEY=secret-one",
		"GH_TOKEN=github-one",
	})
	secondRefs, secondPrincipal := SnapshotCredentialRefs([]string{
		"AWS_PROFILE=production",
		"AWS_ACCESS_KEY_ID=access-key-two",
		"AWS_SECRET_ACCESS_KEY=secret-two",
		"GH_TOKEN=github-two",
	})
	if firstPrincipal == "" || firstPrincipal != secondPrincipal {
		t.Fatalf("ordinary token rotation changed principal fingerprint: %q != %q", firstPrincipal, secondPrincipal)
	}
	if len(firstRefs) != len(secondRefs) || len(firstRefs) != 3 {
		t.Fatalf("credential refs = %#v / %#v", firstRefs, secondRefs)
	}
	rendered := strings.Join([]string{
		firstPrincipal,
		firstRefs[0].Source,
		firstRefs[1].Source,
		firstRefs[2].Source,
	}, "\n")
	for _, secret := range []string{"access-key-one", "secret-one", "github-one"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("credential value leaked into durable snapshot material: %q", secret)
		}
	}
}
