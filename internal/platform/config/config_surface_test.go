package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The execution engine must not grow the configuration surface.
//
// Execution behaviour is DATA compiled into the binary (the tool environment
// profile catalog, the reusable-grant floor, the failure-class rules), not user
// preference. This test is the executable form of that rule: adding a tool or a
// state primitive must never require a new key, because a configuration key is a
// support burden and an invitation to diverge per install.
func TestExecSandboxConfigSurfaceStaysSmall(t *testing.T) {
	fields := reflect.TypeOf(ExecSandboxConfig{})
	want := map[string]bool{"Enabled": true, "Required": true, "AllowNetwork": true}
	if fields.NumField() != len(want) {
		names := make([]string, 0, fields.NumField())
		for i := 0; i < fields.NumField(); i++ {
			names = append(names, fields.Field(i).Name)
		}
		t.Fatalf("exec_sandbox has %d keys (%s); it must stay at %d. Execution behaviour belongs in "+
			"internal/tools/envprofiles (source data), not in configuration.",
			fields.NumField(), strings.Join(names, ", "), len(want))
	}
	for i := 0; i < fields.NumField(); i++ {
		if !want[fields.Field(i).Name] {
			t.Fatalf("unexpected exec_sandbox key %q", fields.Field(i).Name)
		}
	}
}

// The profile catalog must not be reachable from configuration: a file the user
// can edit would make execution behaviour install-specific and unreviewable.
func TestNoProfileCatalogConfigKey(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "internal", "platform", "config", "loader.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"envprofiles", "env_profiles", "tool_profiles", "state_paths"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("configuration must not reference %q: the profile catalog is source data", forbidden)
		}
	}
}
