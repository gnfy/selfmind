package executionenv

import (
	"strings"
	"testing"
)

// TestScratchRedirectsTMPDIR pins the promise the terminal tool's description
// now makes to the model: `mktemp` keeps throwaway files out of the workspace.
// That is only true while TMPDIR points at the run's scratch. A run once wrote
// a helper script into the user's repository and, although it deleted it, the
// deleted `.py` alone held an otherwise finished run at verification_partial —
// the capability existed all along and nothing told the model about it.
func TestScratchRedirectsTMPDIR(t *testing.T) {
	s := LeaseScratch{LeaseID: "l1", Root: "/r/l1", TmpDir: "/r/l1/tmp", StateDir: "/r/l1/state"}
	env := s.EnvOverrides()
	var sawTmpdir, sawRunTmp bool
	for _, e := range env {
		if strings.HasPrefix(e, "TMPDIR=") && strings.HasSuffix(e, "/r/l1/tmp") {
			sawTmpdir = true
		}
		if strings.HasPrefix(e, ScratchTmpEnvVar+"=") && strings.HasSuffix(e, "/r/l1/tmp") {
			sawRunTmp = true
		}
	}
	if !sawTmpdir {
		t.Fatalf("TMPDIR must point at the run scratch, so mktemp lands there: %v", env)
	}
	if !sawRunTmp {
		t.Fatalf("%s must be exported: %v", ScratchTmpEnvVar, env)
	}
}
