package envprofiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression for the 2026-07-30 incident: the AWS profile declared writable
// overlays at ~/.aws/sso/cache and ~/.aws/cli/cache, but the operator's host had
// neither directory. bwrap cannot create a mount point under a read-only root,
// so it aborted with "Can't mkdir parents for .../.aws/sso/cache: Read-only file
// system" BEFORE the command ran — every command matching the profile failed,
// including `aws --version`, and a durable watcher retried that for two hours.
//
// The fix is a declared state root: a writable shell at ~/.aws makes the nested
// mount points creatable, and the named children stay readable.
func TestApplySynthesizesStateRootWhenNestedPathsAreMissing(t *testing.T) {
	home := t.TempDir()
	awsDir := filepath.Join(home, ".aws")
	if err := os.MkdirAll(awsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Exactly the live host layout: config and credentials, no sso/ or cli/.
	for _, name := range []string{"config", "credentials"} {
		if err := os.WriteFile(filepath.Join(awsDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := Apply(Match([]string{"aws"}), ApplyContext{
		Home:      home,
		StateRoot: filepath.Join(t.TempDir(), "state"),
		Trust:     TrustTrusted,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(result.SynthesizedDirs) != 1 {
		t.Fatalf("synthesized dirs = %+v, want the aws state root", result.SynthesizedDirs)
	}
	synth := result.SynthesizedDirs[0]
	if synth.Target != awsDir {
		t.Fatalf("synthesized target = %q, want %q", synth.Target, awsDir)
	}
	for _, name := range []string{"config", "credentials"} {
		want := filepath.Join(awsDir, name)
		found := false
		for _, child := range synth.ReadOnlyChildren {
			if child == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is not readable inside the synthesized root: %v", name, synth.ReadOnlyChildren)
		}
	}

	// The writable caches must still be requested: they are what the tool writes.
	var targets []string
	for _, overlay := range result.OverlayMounts {
		targets = append(targets, overlay.Target)
	}
	for _, want := range []string{filepath.Join(awsDir, "sso", "cache"), filepath.Join(awsDir, "cli", "cache")} {
		found := false
		for _, target := range targets {
			if target == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("overlay for %q missing: %v", want, targets)
		}
	}
	for _, note := range result.Notes {
		if strings.Contains(note, "not a mountable path") {
			t.Fatalf("overlay was dropped even though the state root is synthesized: %s", note)
		}
	}
}

// Without a state root on the host there is nothing to shell in, and mounting
// over a path the host lacks has the same missing-mount-point problem. The
// overlay must then be dropped with a note instead of producing a plan that
// aborts the sandbox.
func TestApplyDropsUnmountableOverlayWithNote(t *testing.T) {
	home := t.TempDir() // no ~/.aws at all
	result, err := Apply(Match([]string{"aws"}), ApplyContext{
		Home:      home,
		StateRoot: filepath.Join(t.TempDir(), "state"),
		Trust:     TrustTrusted,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.SynthesizedDirs) != 0 {
		t.Fatalf("synthesized a root the host does not have: %+v", result.SynthesizedDirs)
	}
	if len(result.OverlayMounts) != 0 {
		t.Fatalf("kept an unmountable overlay: %+v", result.OverlayMounts)
	}
	notes := strings.Join(result.Notes, " | ")
	if !strings.Contains(notes, "not a mountable path") {
		t.Fatalf("dropping the overlay was not reported: %q", notes)
	}
}

func TestMountable(t *testing.T) {
	dir := t.TempDir()
	if !mountable(dir, nil) {
		t.Fatal("an existing directory must be mountable")
	}
	missing := filepath.Join(dir, "sso", "cache")
	if mountable(missing, nil) {
		t.Fatal("a missing nested path must not be mountable on its own")
	}
	if !mountable(missing, []SynthesizedDir{{Target: dir}}) {
		t.Fatal("a missing path inside a synthesized root must be mountable")
	}
	if mountable(missing, []SynthesizedDir{{Target: filepath.Join(dir, "other")}}) {
		t.Fatal("an unrelated synthesized root must not make a path mountable")
	}
}

func TestWithinDir(t *testing.T) {
	if !withinDir("/home/u/.aws", "/home/u/.aws") {
		t.Fatal("a directory is within itself")
	}
	if !withinDir("/home/u/.aws", "/home/u/.aws/sso/cache") {
		t.Fatal("a nested path is within its parent")
	}
	if withinDir("/home/u/.aws", "/home/u/.awsx") {
		t.Fatal("a sibling prefix must not count as nested")
	}
	if withinDir("/home/u/.aws", "/home/u") {
		t.Fatal("a parent must not count as nested")
	}
}

// The inventory is what makes an indirect invocation work: a program reached
// through an interpreter is invisible to shell parsing, so the host's own state
// decides what to prepare.
func TestAvailableOnHostCoversOperatorProfilesWithState(t *testing.T) {
	home := t.TempDir()
	for _, dir := range []string{".config/gcloud", ".aws"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx := ApplyContext{Home: home, ToolchainRoot: filepath.Join(t.TempDir(), "toolchain")}
	ids := map[string]bool{}
	for _, profile := range AvailableOnHost(ctx) {
		ids[profile.ID] = true
	}
	if !ids["gcloud"] || !ids["aws"] {
		t.Fatalf("host state must be discovered, got %v", ids)
	}
	if ids["gh"] || ids["docker"] {
		t.Fatalf("profiles without host state must stay out, got %v", ids)
	}
	// Toolchain caches are meaningless without the tool and stay match-driven.
	if ids["go-toolchain"] || ids["node-toolchain"] {
		t.Fatalf("toolchain profiles must not enter the inventory, got %v", ids)
	}
}

// Apply treats a missing persistent-cache root as a hard failure, which is right
// for a profile the command NAMED and wrong for one the inventory volunteered:
// it would fail commands that never mentioned the tool.
func TestAvailableOnHostSkipsProfilesNeedingAnUnavailableToolchainRoot(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config/helm"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, profile := range AvailableOnHost(ApplyContext{Home: home}) {
		if profile.ID == "helm" {
			t.Fatal("helm needs a toolchain root and must not be volunteered without one")
		}
	}
	found := false
	for _, profile := range AvailableOnHost(ApplyContext{Home: home, ToolchainRoot: t.TempDir()}) {
		if profile.ID == "helm" {
			found = true
		}
	}
	if !found {
		t.Fatal("with a toolchain root available, helm belongs to the inventory")
	}
}

// Applying the whole inventory at once would turn any future catalog entry that
// redirects an already-used variable into a hard failure for EVERY command, so
// uniqueness is asserted at build time instead of discovered in production.
func TestCatalogRedirectVariablesAreUnique(t *testing.T) {
	owner := map[string]string{}
	for i := range Catalog {
		for _, redirect := range Catalog[i].EnvRedirect {
			if first, exists := owner[redirect.Name]; exists {
				t.Fatalf("%s is redirected by both %s and %s; two profiles cannot own one variable",
					redirect.Name, first, Catalog[i].ID)
			}
			owner[redirect.Name] = Catalog[i].ID
		}
	}
}
