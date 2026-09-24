package tools

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestProcessProxyProjectionFollowsExecutionNetwork(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"HTTP_PROXY=http://127.0.0.1:7897",
		"https_proxy=http://localhost:7897",
		"ALL_PROXY=socks5://proxy.example.test:1080",
		"NO_PROXY=localhost",
	}

	isolationCalls := 0
	isolated, decision := adaptProcessEnvForNetwork(parent, false, func(string) bool {
		isolationCalls++
		return true
	})
	if decision.Mode != proxyModeOmittedForIsolatedNetwork || decision.Suppressed != 3 {
		t.Fatalf("isolated decision = %+v", decision)
	}
	if isolationCalls != 0 {
		t.Fatalf("isolated network performed %d reachability probes", isolationCalls)
	}
	if want := []string{"PATH=/usr/bin", "NO_PROXY=localhost"}; !reflect.DeepEqual(isolated, want) {
		t.Fatalf("isolated env = %#v, want %#v", isolated, want)
	}

	var probed []string
	shared, decision := adaptProcessEnvForNetwork(parent, true, func(address string) bool {
		probed = append(probed, address)
		return false
	})
	if decision.Mode != proxyModeOmittedUnreachableLoopback || decision.Suppressed != 2 {
		t.Fatalf("shared decision = %+v", decision)
	}
	if want := []string{"127.0.0.1:7897", "localhost:7897"}; !reflect.DeepEqual(probed, want) {
		t.Fatalf("probed = %#v, want %#v", probed, want)
	}
	if want := []string{"PATH=/usr/bin", "ALL_PROXY=socks5://proxy.example.test:1080", "NO_PROXY=localhost"}; !reflect.DeepEqual(shared, want) {
		t.Fatalf("shared env = %#v, want %#v", shared, want)
	}
}

func TestProcessProxyProjectionPreservesReachableLoopback(t *testing.T) {
	parent := []string{"HTTPS_PROXY=http://127.0.0.1:7897", "PATH=/usr/bin"}
	got, decision := adaptProcessEnvForNetwork(parent, true, func(address string) bool {
		return address == "127.0.0.1:7897"
	})
	if decision.Mode != proxyModeInherited || decision.Suppressed != 0 {
		t.Fatalf("decision = %+v", decision)
	}
	if !reflect.DeepEqual(got, parent) {
		t.Fatalf("reachable proxy was changed: %#v", got)
	}
}

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

func TestMCPDefaultEnvironmentKeepsOperatorCredentialsButStripsControlState(t *testing.T) {
	t.Setenv("GH_TOKEN", "operator-token")
	t.Setenv("SELFMIND_CHANNEL", "cli:private-channel")

	got := BuildProcessEnv(filterEnv(nil), DefaultProcessEnvPolicy())
	if !slices.Contains(got, "GH_TOKEN=operator-token") {
		t.Fatalf("stdio MCP environment lost an ordinary operator credential: %#v", got)
	}
	if slices.Contains(got, "SELFMIND_CHANNEL=cli:private-channel") {
		t.Fatalf("stdio MCP environment leaked gateway state: %#v", got)
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

// The common real shape is several variables pointing at ONE listener —
// HTTP_PROXY, HTTPS_PROXY and ALL_PROXY all set to the same local port. Each
// probe is a dial on the execution hot path, so the address, not the variable,
// decides how many happen.
func TestProcessProxyProjectionProbesEachAddressOnce(t *testing.T) {
	parent := []string{
		"HTTP_PROXY=http://127.0.0.1:7897",
		"HTTPS_PROXY=http://127.0.0.1:7897",
		"ALL_PROXY=socks5://127.0.0.1:7897",
		"NO_PROXY=localhost",
		"PATH=/usr/bin",
	}
	var probed []string
	got, decision := adaptProcessEnvForNetwork(parent, true, func(address string) bool {
		probed = append(probed, address)
		return false
	})
	if len(probed) != 1 || probed[0] != "127.0.0.1:7897" {
		t.Fatalf("one listener must cost one probe, got %#v", probed)
	}
	if decision.Mode != proxyModeOmittedUnreachableLoopback || decision.Suppressed != 3 {
		t.Fatalf("all three variables name the same dead listener: %+v", decision)
	}
	if want := []string{"NO_PROXY=localhost", "PATH=/usr/bin"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %#v, want %#v", got, want)
	}

	// The constraint that must change the result: a second, distinct listener
	// is a second decision and therefore a second probe.
	probed = nil
	got, decision = adaptProcessEnvForNetwork(
		append(parent, "HTTPS_PROXY=http://127.0.0.1:1080"), true,
		func(address string) bool {
			probed = append(probed, address)
			return address == "127.0.0.1:1080"
		})
	if len(probed) != 2 {
		t.Fatalf("two listeners must cost two probes, got %#v", probed)
	}
	if !slices.Contains(got, "HTTPS_PROXY=http://127.0.0.1:1080") {
		t.Fatalf("the reachable listener must survive: %#v", got)
	}
}
