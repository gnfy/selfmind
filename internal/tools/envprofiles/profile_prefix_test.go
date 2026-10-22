package envprofiles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCLIProfilePrefixesKeepStateRequirements(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		mounts bool
	}{
		{"snapshot-profile-wins", []string{"aws", "sts", "get-caller-identity"}, false},
		{"full-profile", []string{"aws", "--profile", "session", "sts", "get-caller-identity"}, true},
		{"unique-prefix", []string{"aws", "--prof", "session", "sts", "get-caller-identity"}, true},
		{"unique-prefix-equals", []string{"aws", "--prof=session", "sts", "get-caller-identity"}, true},
		{"ambiguous-prefix", []string{"aws", "--pro", "session", "sts", "get-caller-identity"}, true},
		{"prefix-static-unknown", []string{"aws", "--prof", "static", "sts", "get-caller-identity"}, true},
		{"after-options-end", []string{"aws", "--", "--prof", "session"}, false},
		{"unrelated-option", []string{"aws", "--region", "us-east-1", "sts", "get-caller-identity"}, false},
		{"explicit-static", []string{"aws", "--profile", "static", "sts", "get-caller-identity"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".aws"), 0700); err != nil {
				t.Fatal(err)
			}
			config := "[default]\nregion=us-east-1\n[profile static]\nregion=eu-west-1\n[profile session]\nsso_session=work\n"
			if err := os.WriteFile(filepath.Join(home, ".aws/config"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			vars := map[string]string{"AWS_PROFILE": "static", "AWS_DEFAULT_PROFILE": "session"}
			ctx := ApplyContext{Home: home, StateRoot: t.TempDir(), Trust: TrustTrusted, ProgramArguments: map[string][][]string{"aws": {tc.args}}, Lookup: func(name string) (string, bool) { v, ok := vars[name]; return v, ok }}
			got, err := Apply(Match([]string{"aws"}), ctx)
			if err != nil {
				t.Fatal(err)
			}
			mounts := len(got.OverlayMounts) > 0 || len(got.SynthesizedDirs) > 0
			if mounts != tc.mounts {
				t.Fatalf("required state mounts=%v; actual CLI profile spelling requires %v", mounts, tc.mounts)
			}
		})
	}
}
