package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"selfmind/internal/executionenv"
)

func TestEnvironmentDependenciesDistinguishNonExecutingInterpreterModes(t *testing.T) {
	for _, tc := range []struct {
		command string
		opaque  bool
	}{
		{"bash --version | head -1", false},
		{"bash -n inspect.sh", false},
		{"python3 --version", false},
		{"bash -c 'bash --version'", false},
		{"bash inspect.sh", true},
		{"bash -n -c 'echo test'", false},
		{"python3 inspect.py", true},
		{"node script.js", true},
		// A configuration selector is a state input, not hidden programs; it
		// selects state through EnvironmentOverrides (see below).
		{"AWS_PROFILE=session aws sts get-caller-identity", false},
		{"echo \"$MESSAGE\"", false},
		{"aws --profile \"$PROFILE\" sts get-caller-identity", false},
	} {
		_, opaque := execCommandProgramSet("terminal", map[string]interface{}{"command": tc.command})
		if opaque != tc.opaque {
			t.Errorf("%q opaque=%v, want %v", tc.command, opaque, tc.opaque)
		}
	}
	// Environment dependency analysis doesn't change observation/approval proof.
	args := map[string]interface{}{"command": "bash -n inspect.sh"}
	if observationOnlyExec("terminal", args) {
		t.Fatal("dependency optimization widened observation authority")
	}
}

// A leading VAR=value changes the environment a program sees, never which
// program runs. Marking it undecidable used to hand every such command the
// whole host inventory.
func TestLeadingAssignmentsKeepTheProgramSetDecidable(t *testing.T) {
	for _, tc := range []struct {
		command, program string
		opaque           bool
	}{
		{"TZ=UTC date", "date", false},
		{"CGO_ENABLED=0 go build ./...", "go", false},
		{"LC_ALL=C sort notes.txt", "sort", false},
		// echo and printf are builtins, not programs; only decidability matters.
		{"FOO=1 echo hi", "", false},
		{"A=1 B=two printf ok", "", false},
		{"bash -c 'LC_ALL=C sort notes.txt'", "sort", false},
		{"env FOO=1 go test ./...", "go", false},
		{"export FOO=1; go test ./...", "go", false},
		// The assignment never makes an executed script decidable.
		{"FOO=1 ./deploy.sh", "", true},
		{"FOO=1 python3 tool.py", "python3", true},
	} {
		programs, opaque := execCommandProgramSet("terminal", map[string]interface{}{"command": tc.command})
		if opaque != tc.opaque {
			t.Errorf("%q opaque=%v, want %v", tc.command, opaque, tc.opaque)
		}
		if tc.program != "" && !stringInList(tc.program, programs) {
			t.Errorf("%q lost its program %s: %v", tc.command, tc.program, programs)
		}
	}
}

// Selecting a profile inline must still prepare that profile's state, through
// the override that the snapshot cannot resolve, without borrowing every other
// operator profile the host happens to have.
func TestLeadingProfileSelectorPreparesOnlyItsProfileState(t *testing.T) {
	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[default]\nregion=us-east-1\n[profile session]\nsso_session=work\n"
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	tenant, workspace := profileExecScope(t, home, "", executionenv.TrustTrusted)
	prepare := func(command string) execMaterial {
		t.Helper()
		material := execMaterialForArgs(map[string]interface{}{"_tenant_id": tenant, "_tool_name": "terminal", "command": command}, workspace)
		if material.ProfileError != nil {
			t.Fatalf("%q: %v", command, material.ProfileError)
		}
		return material
	}
	plain := prepare("aws sts get-caller-identity")
	if len(plain.OverlayMounts)+len(plain.SynthesizedDirs) != 0 {
		t.Fatalf("static default profile acquired writable state: %+v", plain)
	}
	for _, command := range []string{
		"AWS_PROFILE=session aws sts get-caller-identity",
		"AWS_DEFAULT_PROFILE=session aws sts get-caller-identity",
		"aws --profile session sts get-caller-identity",
	} {
		selected := prepare(command)
		if !reflect.DeepEqual(selected.Profiles, []string{"aws"}) || len(selected.ProfilesFromInventory) != 0 {
			t.Errorf("%q widened to the host inventory: profiles=%v inventory=%v", command, selected.Profiles, selected.ProfilesFromInventory)
		}
		if len(selected.OverlayMounts) == 0 {
			t.Errorf("%q lost the selected profile's writable state", command)
		}
	}
	// An assignment unrelated to selection changes nothing.
	unrelated := prepare("FOO=1 aws sts get-caller-identity")
	if !reflect.DeepEqual(preparationView(unrelated), preparationView(plain)) {
		t.Fatalf("unrelated assignment changed preparation:\n%+v\n%+v", preparationView(unrelated), preparationView(plain))
	}
}

// preparationView is everything profile preparation contributes to a command.
func preparationView(material execMaterial) map[string]interface{} {
	return map[string]interface{}{
		"error": material.ProfileError, "profiles": material.Profiles, "inventory": material.ProfilesFromInventory,
		"read_only": material.ReadOnlyPaths, "overlays": material.OverlayMounts, "synthesized": material.SynthesizedDirs,
		"env": material.Env,
	}
}

func TestEnvironmentOverridesRetainUncertainAuthWithoutProxyCoupling(t *testing.T) {
	for _, tc := range []struct{ command, variable string }{
		{"AWS_PROFILE=session aws sts get-caller-identity", "AWS_PROFILE"},
		{"env 'AWS_CONFIG_FILE=/other/config' aws sts get-caller-identity", "AWS_CONFIG_FILE"},
		{"env -u AWS_PROFILE aws sts get-caller-identity", "AWS_PROFILE"},
		{"env --unset=AWS_PROFILE aws sts get-caller-identity", "AWS_PROFILE"},
		{"env $CONFIG aws sts get-caller-identity", "*"},
		{"unset AWS_PROFILE; aws sts get-caller-identity", "AWS_PROFILE"},
		{"env -i aws sts get-caller-identity", "*"},
		// Program names normalize as the prover reads them.
		{"Env -u AWS_PROFILE aws sts get-caller-identity", "AWS_PROFILE"},
		{"/usr/bin/ENV AWS_CONFIG_FILE=/other/config aws sts get-caller-identity", "AWS_CONFIG_FILE"},
		{"UNSET AWS_PROFILE; aws sts get-caller-identity", "AWS_PROFILE"},
	} {
		if !execEnvironmentOverrides(tc.command)[tc.variable] {
			t.Errorf("lost %s override in %s", tc.variable, tc.command)
		}
	}
	overrides := execEnvironmentOverrides("env -u HTTPS_PROXY aws sts get-caller-identity")
	if overrides["*"] || overrides["AWS_PROFILE"] || overrides["AWS_CONFIG_FILE"] {
		t.Fatal("proxy edit widened authentication dependencies")
	}
}

func TestEnvironmentArgumentsPreserveLiteralBoundaries(t *testing.T) {
	for _, command := range []string{
		`aws --profile "work session" sts get-caller-identity`,
		`bash -c 'aws --profile "work session" sts get-caller-identity'`,
		`env -u HTTPS_PROXY aws --profile "work session" sts get-caller-identity`,
	} {
		_, arguments, _ := execCommandAnalysis("terminal", map[string]interface{}{"command": command})
		want := []string{"aws", "--profile", "work session", "sts", "get-caller-identity"}
		if got := arguments["aws"]; len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Errorf("literal argument boundaries lost for %s: %#v", command, got)
		}
	}
}
