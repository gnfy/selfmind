package tools

import (
	"reflect"
	"testing"
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
		{"AWS_PROFILE=session aws sts get-caller-identity", true},
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

func TestEnvironmentOverridesRetainUncertainAuthWithoutProxyCoupling(t *testing.T) {
	for _, tc := range []struct{ command, variable string }{
		{"AWS_PROFILE=session aws sts get-caller-identity", "AWS_PROFILE"},
		{"env 'AWS_CONFIG_FILE=/other/config' aws sts get-caller-identity", "AWS_CONFIG_FILE"},
		{"env -u AWS_PROFILE aws sts get-caller-identity", "AWS_PROFILE"},
		{"env --unset=AWS_PROFILE aws sts get-caller-identity", "AWS_PROFILE"},
		{"env $CONFIG aws sts get-caller-identity", "*"},
		{"unset AWS_PROFILE; aws sts get-caller-identity", "AWS_PROFILE"},
		{"env -i aws sts get-caller-identity", "*"},
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
