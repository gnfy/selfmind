package envprofiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuthenticationSelectsOnlyRequiredMountState(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0700); err != nil {
		t.Fatal(err)
	}
	config := "[default]\nregion=us-east-1\n[profile static]\nregion=eu-west-1\n[profile session]\nsso_session=work\n[profile role]\nrole_arn=arn:example:role\n"
	if err := os.WriteFile(filepath.Join(home, ".aws/config"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		opaque bool
		mounts bool
	}{
		{"default", []string{"aws", "sts", "get-caller-identity"}, false, false},
		{"static", []string{"aws", "sts", "get-caller-identity", "--profile", "static"}, false, false},
		{"session", []string{"aws", "sts", "get-caller-identity", "--profile=session"}, false, true},
		{"role", []string{"aws", "sts", "get-caller-identity", "--profile", "role"}, false, true},
		{"unknown-selection", []string{"aws", "sts", "get-caller-identity", "--profile", "$PROFILE"}, false, true},
		{"unknown-arguments", nil, false, true},
		{"opaque-script", nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Apply(Match([]string{"aws"}), ApplyContext{Home: home, StateRoot: t.TempDir(), Trust: TrustTrusted, ProgramArguments: map[string][][]string{"aws": {tc.args}}, OpaquePrograms: tc.opaque})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(result.OverlayMounts) > 0 || len(result.SynthesizedDirs) > 0; got != tc.mounts {
				t.Fatalf("mount requirements=%v, want %v", got, tc.mounts)
			}
			if len(result.ReadOnlyPaths) != 1 {
				t.Fatalf("lost read-only configuration: %+v", result)
			}
		})
	}
}

func TestOpaqueStaticAuthenticationNeedsNoMounts(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".aws/config")
	if err := os.WriteFile(path, []byte("[default]\nregion=us-east-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := ApplyContext{Home: home, StateRoot: t.TempDir(), Trust: TrustTrusted, OpaquePrograms: true}
	result, err := Apply(AvailableOnHost(ctx), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || len(result.OverlayMounts)+len(result.SynthesizedDirs) != 0 {
		t.Fatalf("unrelated mount requirements: %+v", result)
	}
	// A malformed/oversized configuration must not silently drop writable state.
	if err := os.WriteFile(path, []byte("not valid configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = Apply(Match([]string{"aws"}), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.OverlayMounts) != 2 || len(result.SynthesizedDirs) != 1 {
		t.Fatal("unknown auth requirements were discarded")
	}
}

func TestMountSelectionHonorsSnapshotFallbackAndOverrides(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws/config"), []byte("[default]\nregion=us-east-1\n[profile session]\nsso_session=work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := ApplyContext{Home: home, StateRoot: t.TempDir(), Trust: TrustTrusted, ProgramArguments: map[string][][]string{"aws": {{"aws", "sts", "get-caller-identity"}}}, Lookup: func(name string) (string, bool) { return "session", name == "AWS_DEFAULT_PROFILE" }}
	result, err := Apply(Match([]string{"aws"}), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.OverlayMounts) == 0 {
		t.Fatal("snapshot profile selector was ignored")
	}
	ctx.Lookup = nil
	ctx.EnvironmentOverrides = map[string]bool{"AWS_CONFIG_FILE": true}
	result, err = Apply(Match([]string{"aws"}), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.OverlayMounts) == 0 {
		t.Fatal("unresolved configuration override lost required state")
	}
	// The conditions are configuration data, not an AWS branch in the engine.
	generic := &EnvProfile{ID: "custom-state", MatchExecutables: []string{"custom-tool"}, MapRWAt: []MapRWAt{{Key: "cache", At: StateSource{HomeRelPath: ".aws/cache"}}}, SynthesizeDir: []SynthesizeDir{{At: StateSource{HomeRelPath: ".aws"}}}, MountStateWhen: []MountStateCondition{{From: StateSource{HomeRelPath: ".aws/config"}, Keys: []string{"sso_session"}, Selection: &ConfigurationSelection{Option: "--identity", DefaultSection: "default", SectionPrefix: "profile "}}}}
	ctx.EnvironmentOverrides = nil
	ctx.ProgramArguments = map[string][][]string{"custom-tool": {{"custom-tool", "--identity", "session"}}}
	result, err = Apply([]*EnvProfile{generic}, ctx)
	if err != nil || len(result.OverlayMounts) != 1 {
		t.Fatalf("catalog condition isn't generic: %+v %v", result, err)
	}
}
