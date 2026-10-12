//go:build linux

package cliapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func foreignHomeWithSystemdUnit(t *testing.T) (home, unit string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(serviceHomeEnv, "")
	unit = gatewaySystemdUnitPath(home)
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, unit
}

func TestSystemdChangesRefuseAHomeThatIsNotTheAccountsOwn(t *testing.T) {
	verbs := fakeServiceManager(t)
	home, unit := foreignHomeWithSystemdUnit(t)
	assertForeignHomeCannotChangeService(t, home, unit, verbs)
}

func TestSystemdWithoutAUnitFileIsUnaffectedByHome(t *testing.T) {
	assertHomeWithoutServiceIsUnaffected(t, fakeServiceManager(t))
}

func TestSystemdChangesProceedFromTheHomeTheOperatorNamed(t *testing.T) {
	verbs := fakeServiceManager(t)
	home, _ := foreignHomeWithSystemdUnit(t)
	t.Setenv(serviceHomeEnv, home)
	_, _, err := gatewayServiceStartIfInstalled("")
	if err == nil || strings.Contains(err.Error(), "refusing") {
		t.Fatalf("the named HOME did not reach the service manager: %v", err)
	}
	if got := verbs(); len(got) != 1 || got[0] != "start" {
		t.Fatalf("service manager verbs = %v, want one start", got)
	}
}
