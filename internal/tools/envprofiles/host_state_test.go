package envprofiles

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mkfifo plants a named pipe through the host's own utility.
func mkfifo(path string) error { return exec.Command("mkfifo", path).Run() }

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// awsHost is a host home whose ~/.aws holds config (and optionally more).
func awsHost(t *testing.T, config string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte("[default]\naws_access_key_id=AKIAEXAMPLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func inPlaceContext(home, trust, profile string) ApplyContext {
	return ApplyContext{
		Home: home, Trust: trust, InPlaceState: true,
		ProgramArguments: map[string][][]string{"aws": {{"aws", "--profile", profile, "sts", "get-caller-identity"}}},
	}
}

func writePaths(plan HostStatePlan) []string {
	paths := make([]string, 0, len(plan.Writes))
	for _, write := range plan.Writes {
		paths = append(paths, write.Path)
	}
	return paths
}

// D3: on a backend that cannot mount, each mount-backed write requirement
// becomes an in-place write allowance on exactly its declared subpath. The
// state root itself and the configuration and credential files are never
// among them.
func TestPlanHostStateWritesNamesOnlyTheDeclaredSubpaths(t *testing.T) {
	for _, tc := range []struct {
		name, config, profile string
		existing              string
	}{
		{name: "sso", config: "[default]\nregion=us-east-1\n[profile sso-dev]\nsso_session=dev\n", profile: "sso-dev"},
		// Another profile name and authentication kind, on a host whose SSO
		// cache already exists.
		{name: "role", config: "[profile ops-admin]\nrole_arn=arn:aws:iam::1:role/ops\n", profile: "ops-admin", existing: "sso/cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := awsHost(t, tc.config)
			if tc.existing != "" {
				if err := os.MkdirAll(filepath.Join(home, ".aws", filepath.FromSlash(tc.existing)), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := PlanHostStateWrites(Match([]string{"aws"}), inPlaceContext(home, TrustTrusted, tc.profile))
			if err != nil {
				t.Fatal(err)
			}
			root := canonical(t, filepath.Join(home, ".aws"))
			want := []string{filepath.Join(root, "sso", "cache"), filepath.Join(root, "cli", "cache")}
			if got := writePaths(plan); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("writes = %v, want %v", got, want)
			}
			for _, write := range plan.Writes {
				if write.Root != root || write.Profile != "aws" {
					t.Fatalf("write %+v must be bounded by the declared root %s", write, root)
				}
				existed := tc.existing != "" && strings.HasSuffix(write.Path, filepath.FromSlash(tc.existing))
				if write.Missing == existed {
					t.Fatalf("write %s missing=%v, existed=%v", write.Path, write.Missing, existed)
				}
				for _, readOnly := range []string{root, filepath.Join(root, "config"), filepath.Join(root, "credentials")} {
					if write.Path == readOnly || strings.HasPrefix(readOnly, write.Path+string(filepath.Separator)) {
						t.Fatalf("write %s covers read-only state %s", write.Path, readOnly)
					}
				}
			}
			// Planning is side-effect free.
			for _, write := range plan.Writes {
				if _, err := os.Stat(write.Path); write.Missing && err == nil {
					t.Fatalf("planning created %s", write.Path)
				}
			}
		})
	}
}

// The inputs that must change the result: static authentication needs no
// writable state, and withheld operator credentials get no host write at all.
func TestPlanHostStateWritesFollowsAuthenticationAndCredentialPolicy(t *testing.T) {
	home := awsHost(t, "[default]\nregion=us-east-1\n[profile static]\nregion=eu-west-1\n[profile sso-dev]\nsso_session=dev\n")
	plan, err := PlanHostStateWrites(Match([]string{"aws"}), inPlaceContext(home, TrustTrusted, "static"))
	if err != nil || len(plan.Writes) != 0 {
		t.Fatalf("static authentication acquired writable host state: %+v %v", plan, err)
	}
	plan, err = PlanHostStateWrites(Match([]string{"aws"}), inPlaceContext(home, TrustUntrusted, "sso-dev"))
	if err != nil || len(plan.Writes) != 0 {
		t.Fatalf("withheld credentials acquired writable host state: %+v %v", plan, err)
	}
	if !strings.Contains(strings.Join(plan.Notes, " "), "withheld") {
		t.Fatalf("withholding must be reported: %v", plan.Notes)
	}
	ctx := inPlaceContext(home, TrustUntrusted, "sso-dev")
	ctx.HasCredentialRead = true
	if plan, err = PlanHostStateWrites(Match([]string{"aws"}), ctx); err != nil || len(plan.Writes) != 2 {
		t.Fatalf("credential:read must restore the declared writes: %+v %v", plan, err)
	}
	// No state root on the host: nothing to write, and nothing to create.
	bare := t.TempDir()
	if plan, err = PlanHostStateWrites(Match([]string{"aws"}), inPlaceContext(bare, TrustTrusted, "sso-dev")); err != nil || len(plan.Writes) != 0 {
		t.Fatalf("a host without the state root gained writes: %+v %v", plan, err)
	}
}

// A requirement that subpath writes cannot express is refused with its
// specific reason, never widened.
func TestPlanHostStateWritesRefusesWhatSubpathsCannotExpress(t *testing.T) {
	config := "[profile sso-dev]\nsso_session=dev\n"
	outsideLink := func(t *testing.T, rel string) string {
		home := awsHost(t, config)
		outside := t.TempDir()
		link := filepath.Join(home, ".aws", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		return home
	}
	for _, tc := range []struct {
		name   string
		home   func(t *testing.T) string
		reason string
	}{
		{"cache-link-escapes", func(t *testing.T) string { return outsideLink(t, "sso/cache") }, "outside"},
		{"parent-link-escapes", func(t *testing.T) string { return outsideLink(t, "sso") }, "outside"},
		{"cache-is-a-file", func(t *testing.T) string {
			home := awsHost(t, config)
			if err := os.MkdirAll(filepath.Join(home, ".aws", "sso"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".aws", "sso", "cache"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return home
		}, "not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanHostStateWrites(Match([]string{"aws"}), inPlaceContext(tc.home(t), TrustTrusted, "sso-dev"))
			var unexpressible *UnexpressibleStateError
			if !errors.As(err, &unexpressible) || unexpressible.Profile != "aws" || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want an unexpressible-state refusal naming %q, got %v", tc.reason, err)
			}
		})
	}

	// A link that stays inside the declared root is refused too: its target can
	// be wider than declared (`sso/`), and whoever can repoint the link steers
	// later grants. Every component below the root must be a real directory.
	for _, tc := range []struct{ name, link, target string }{
		{"cache-link-inside", "sso/cache", "sso-cache-real"},
		// The same rule one component up.
		{"parent-link-inside", "sso", "sso-real"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := awsHost(t, config)
			real := filepath.Join(home, ".aws", tc.target)
			if err := os.MkdirAll(filepath.Join(real, "cache"), 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(home, ".aws", filepath.FromSlash(tc.link))
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			_, err := PlanHostStateWrites(Match([]string{"aws"}), inPlaceContext(home, TrustTrusted, "sso-dev"))
			var unexpressible *UnexpressibleStateError
			if !errors.As(err, &unexpressible) || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("a linked component must be refused by name, got %v", err)
			}
		})
	}

	// Generic data, generic refusals: a write requirement without a declared
	// state root, or one that would cover the profile's read-only state.
	for _, tc := range []struct {
		name    string
		profile *EnvProfile
		reason  string
	}{
		{"no-declared-root", &EnvProfile{ID: "rootless", MapRWAt: []MapRWAt{{Key: "cache", At: StateSource{HomeRelPath: ".tool/cache"}, Flat: true}}}, "declared state root"},
		{"covers-read-only", &EnvProfile{ID: "covering",
			SynthesizeDir: []SynthesizeDir{{At: StateSource{HomeRelPath: ".tool"}, KeepReadOnly: []StateSource{{HomeRelPath: ".tool/state/config"}}}},
			MapRWAt:       []MapRWAt{{Key: "state", At: StateSource{HomeRelPath: ".tool/state"}, Flat: true}}}, "read-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".tool", "cache"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(home, ".tool", "state"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".tool", "state", "config"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := PlanHostStateWrites([]*EnvProfile{tc.profile}, ApplyContext{Home: home, Trust: TrustTrusted, InPlaceState: true})
			var unexpressible *UnexpressibleStateError
			if !errors.As(err, &unexpressible) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want a refusal naming %q, got %v", tc.reason, err)
			}
		})
	}
}

// nestingAWS is the aws profile with its caches not declared flat, for the
// rules that apply to a cache that may nest.
func nestingAWS() []*EnvProfile {
	var out []*EnvProfile
	for _, profile := range Match([]string{"aws"}) {
		copied := *profile
		copied.MapRWAt = append([]MapRWAt(nil), profile.MapRWAt...)
		for i := range copied.MapRWAt {
			copied.MapRWAt[i].Flat = false
		}
		out = append(out, &copied)
	}
	return out
}

// A cache written in place is listed, never walked: its repair must not
// depend on directories a sandboxed process could exchange underneath it.
// Only a cache declared flat is written in place; the same requirement not
// declared flat is refused.
func TestPlanHostStateWritesRefusesCachesNotDeclaredFlat(t *testing.T) {
	home := awsHost(t, "[profile sso-dev]\nsso_session=dev\n")
	_, err := PlanHostStateWrites(nestingAWS(), inPlaceContext(home, TrustTrusted, "sso-dev"))
	var unexpressible *UnexpressibleStateError
	if !errors.As(err, &unexpressible) || !strings.Contains(err.Error(), "not declared flat") {
		t.Fatalf("a cache not declared flat must not be written in place, got %v", err)
	}
	// The constraint that must change the result: the catalog's flat caches.
	if plan, err := PlanHostStateWrites(Match([]string{"aws"}), inPlaceContext(home, TrustTrusted, "sso-dev")); err != nil || len(plan.Writes) != 2 {
		t.Fatalf("the catalog's flat caches must be written in place: %+v %v", plan, err)
	}
}

// What a cache already holds is no reason to refuse the command: a planted
// entry is moved out before the command runs, as after it
// (RepairHostStateWrites). Planning plans the writes and leaves the cache as
// it found it.
func TestPlanHostStateWritesLeavesPlantedEntriesToTheRepair(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, cache string) (string, error)
	}{
		{"symlink", func(t *testing.T, cache string) (string, error) {
			path := filepath.Join(cache, "a1b2c3.json")
			return path, os.Symlink(filepath.Join(t.TempDir(), "victim"), path)
		}},
		{"fifo", func(t *testing.T, cache string) (string, error) {
			path := filepath.Join(cache, "pipe")
			return path, mkfifo(path)
		}},
		{"hard link to a file outside", func(t *testing.T, cache string) (string, error) {
			outside := filepath.Join(t.TempDir(), "workspace-file")
			if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
				return "", err
			}
			path := filepath.Join(cache, "alias.json")
			return path, os.Link(outside, path)
		}},
		{"directory", func(t *testing.T, cache string) (string, error) {
			path := filepath.Join(cache, "sub")
			return path, os.Mkdir(path, 0o700)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := awsHost(t, "[profile sso-dev]\nsso_session=dev\n")
			cache := filepath.Join(home, ".aws", "sso", "cache")
			if err := os.MkdirAll(cache, 0o700); err != nil {
				t.Fatal(err)
			}
			planted, err := tc.plant(t, cache)
			if err != nil {
				t.Skipf("cannot create the entry on this host: %v", err)
			}
			plan, err := PlanHostStateWrites(Match([]string{"aws"}), inPlaceContext(home, TrustTrusted, "sso-dev"))
			if err != nil || len(plan.Writes) != 2 {
				t.Fatalf("a planted %s must not refuse the command: %+v %v", tc.name, plan, err)
			}
			if _, err := os.Lstat(planted); err != nil {
				t.Fatalf("planning must not touch the cache: %v", err)
			}
		})
	}
}

// Missing declared subpaths are created inside their root only, and a path
// that escaped after planning is refused rather than created elsewhere.
func TestMaterializeHostStateWritesCreatesOnlyInsideTheRoot(t *testing.T) {
	home := awsHost(t, "[profile sso-dev]\nsso_session=dev\n")
	ctx := inPlaceContext(home, TrustTrusted, "sso-dev")
	plan, err := PlanHostStateWrites(Match([]string{"aws"}), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := MaterializeHostStateWrites(plan); err != nil {
		t.Fatal(err)
	}
	for _, write := range plan.Writes {
		info, err := os.Stat(write.Path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s was not created as a private directory: %v %v", write.Path, info, err)
		}
	}

	// The same plan against a root whose subdirectory was swapped for an
	// escaping link after planning: nothing may be created outside.
	home = awsHost(t, "[profile sso-dev]\nsso_session=dev\n")
	ctx = inPlaceContext(home, TrustTrusted, "sso-dev")
	plan, err = PlanHostStateWrites(Match([]string{"aws"}), ctx)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".aws", "sso")); err != nil {
		t.Fatal(err)
	}
	var unexpressible *UnexpressibleStateError
	if err := MaterializeHostStateWrites(plan); !errors.As(err, &unexpressible) {
		t.Fatalf("an escape after planning must be refused, got %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("created state outside the root: %v", entries)
	}
}

// In place, nothing is mounted: no synthesized root, no overlay, no private
// cache directory and no seeded token copy. Everything else — read-only
// mappings, other profiles' copies and redirects — is unchanged.
func TestApplyInPlaceStateMaterializesNoMountState(t *testing.T) {
	home := awsHost(t, "[profile sso-dev]\nsso_session=dev\n")
	if err := os.MkdirAll(filepath.Join(home, ".aws", "sso", "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "sso", "cache", "token.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "gh", "hosts.yml"), []byte("github.com: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	ctx := inPlaceContext(home, TrustTrusted, "sso-dev")
	ctx.StateRoot = state
	result, err := Apply(Match([]string{"aws", "gh"}), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.OverlayMounts)+len(result.SynthesizedDirs) != 0 {
		t.Fatalf("in-place preparation produced mounts: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(state, "aws")); err == nil {
		t.Fatal("in-place preparation copied credential state into the run overlay")
	}
	if len(result.ReadOnlyPaths) != 2 {
		t.Fatalf("read-only configuration must stay mapped: %v", result.ReadOnlyPaths)
	}
	if _, err := os.Stat(filepath.Join(state, "gh", "hosts.yml")); err != nil {
		t.Fatalf("another profile's copy must be unaffected: %v", err)
	}

	// The constraint that must change the result: the mounting form keeps
	// both the synthesized root and the overlays.
	ctx.InPlaceState = false
	ctx.StateRoot = filepath.Join(t.TempDir(), "state")
	result, err = Apply(Match([]string{"aws"}), ctx)
	if err != nil || len(result.OverlayMounts) != 2 || len(result.SynthesizedDirs) != 1 {
		t.Fatalf("mount preparation changed: %+v %v", result, err)
	}
}
