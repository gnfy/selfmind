package cliapp

import (
	"os"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

// `env refresh` used to tell the user that `gateway restart` would adopt the
// sampled environment. It would not: a plain restart inherits the CLI's own
// environment — exactly the stale one being replaced. The guidance must name the
// command that actually applies the sample.
func TestEnvRefreshDoesNotClaimAPlainRestartAdoptsTheSample(t *testing.T) {
	source, err := readEnvCommandSource()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "selfmind env refresh --restart") {
		t.Fatal("the guidance must point at the command that applies the sample")
	}
	if !strings.Contains(source, "A plain `selfmind gateway restart` inherits this shell's environment instead.") {
		t.Fatal("the difference between the two commands must be stated, not implied")
	}
}

// The sample must reach the daemon directly and never be written to disk: an
// environment file would persist credential values that must only exist in
// memory.
func TestEnvRefreshPassesTheSampleInMemory(t *testing.T) {
	source, err := readEnvCommandSource()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "a.gatewayRestartWithEnvironment(nil, sampled)") {
		t.Fatal("the sampled environment must be handed to the restart directly")
	}
	for _, forbidden := range []string{"WriteFile", "os.Create", "env-snapshot"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("the sample must not be persisted (%q found)", forbidden)
		}
	}
}

// A world-readable service definition must never carry credentials, by name or
// by a value with inline credentials. The value check remains independent from
// the passthrough allowlist: ordinary proxy URLs are non-secret values, but
// ambient proxy variables are excluded by servicePassthroughEnvironment.
func TestServicePassthroughCredentialFilter(t *testing.T) {
	if !isCredentialShapedEnvName("GITHUB_TOKEN") || !isCredentialShapedEnvName("aws_secret_access_key") {
		t.Fatal("credential-shaped names must be recognized regardless of case")
	}
	if isCredentialShapedEnvName("CLOUDSDK_CONFIG") || isCredentialShapedEnvName("KUBECONFIG") {
		t.Fatal("configuration locations are not credentials")
	}
	if !valueEmbedsCredentials("http://user:pass@proxy.internal:3128") {
		t.Fatal("a proxy URL with inline credentials must be refused")
	}
	if valueEmbedsCredentials("http://proxy.internal:3128") || valueEmbedsCredentials("/home/u/.config/gcloud") {
		t.Fatal("an ordinary proxy URL or path must be allowed")
	}
	// A URL whose PATH contains @ is not an embedded credential.
	if valueEmbedsCredentials("https://example.com/a@b") {
		t.Fatal("an @ outside the authority must not be treated as a credential")
	}
}

// readEnvCommandSource reads this package's own source. The two guarantees under
// test are properties of the CODE PATH (what the user is told, and that the
// sample is never persisted), which a behavioural test cannot observe without
// restarting a real daemon.
func readEnvCommandSource() (string, error) {
	data, err := os.ReadFile("env_commands.go")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// The comparison baseline must be the DAEMON. Comparing against the CLI reported
// "unchanged" in exactly the situation the command exists for: the CLI is
// normally the first process to see a new toolchain, so a fresh shell and a stale
// daemon look identical from here.
func TestEnvRefreshComparesAgainstTheDaemon(t *testing.T) {
	source, err := readEnvCommandSource()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, "a.daemonEnvironmentIdentity()") {
		t.Fatal("the baseline must be read from the running daemon")
	}
	if strings.Contains(source, `before := tools.SampleEnvironmentSnapshot(os.Environ(), "cli")`) {
		t.Fatal("the CLI's own environment must not be the comparison baseline")
	}
	// An operating-system-managed daemon takes its environment from the installed
	// definition, so reporting a successful adoption after a restart would be a lie.
	if !strings.Contains(source, "A restart cannot adopt a sampled environment.") {
		t.Fatal("launchd mode must refuse explicitly instead of appearing to succeed")
	}
}

func TestDaemonEnvironmentDifferencesNamesEachDimension(t *testing.T) {
	daemon := api.GatewayRuntimeInfo{
		PrincipalFingerprint:   "principal-old",
		EnvironmentFingerprint: "env-same",
		CredentialSourceHash:   "cred-old",
	}
	sample := &executionenv.Snapshot{
		PrincipalFingerprint:   "principal-new",
		EnvironmentFingerprint: "env-same",
		CredentialSourceHash:   "cred-new",
	}
	changed := daemonEnvironmentDifferences(daemon, sample)
	if strings.Join(changed, ",") != "account/profile,credential source" {
		t.Fatalf("unexpected differences: %v", changed)
	}
	identical := daemonEnvironmentDifferences(api.GatewayRuntimeInfo{
		PrincipalFingerprint:   sample.PrincipalFingerprint,
		EnvironmentFingerprint: sample.EnvironmentFingerprint,
		CredentialSourceHash:   sample.CredentialSourceHash,
	}, sample)
	if len(identical) != 0 {
		t.Fatalf("an identical environment must report no differences: %v", identical)
	}
}
