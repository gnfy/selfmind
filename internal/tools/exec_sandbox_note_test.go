package tools

import (
	"errors"
	"strings"
	"testing"
)

// withExecSandboxPolicy installs a policy for one test and restores the
// process-wide default afterwards (the policy is a package-level singleton).
func withExecSandboxPolicy(t *testing.T, enabled, required, network bool) {
	t.Helper()
	prevEnabled, prevRequired, prevNetwork := execSandboxPolicy()
	SetExecSandbox(enabled, required, network)
	t.Cleanup(func() { SetExecSandbox(prevEnabled, prevRequired, prevNetwork) })
}

// TestExecSandboxPromptNote pins the model-facing environment awareness: the
// note must state the network posture (the live failure mode was the model
// blind-retrying network commands in a network-less sandbox), and stay empty
// when commands run directly on the host.
func TestExecSandboxPromptNote(t *testing.T) {
	// Disabled sandbox → no note, regardless of host capability.
	withExecSandboxPolicy(t, false, false, false)
	if note := ExecSandboxPromptNote(); note != "" {
		t.Fatalf("disabled sandbox must yield no note, got %q", note)
	}

	if !ExecSandboxAvailable() {
		t.Skip("isolated sandbox unavailable on this host; enabled-path notes not renderable")
	}

	withExecSandboxPolicy(t, true, false, true)
	note := ExecSandboxPromptNote()
	if !strings.Contains(note, "daemon host namespace") || !strings.Contains(note, "proxy and DNS") {
		t.Fatalf("networked sandbox note must state shared network, got %q", note)
	}

	withExecSandboxPolicy(t, true, false, false)
	note = ExecSandboxPromptNote()
	if !strings.Contains(note, "network is disabled by default") || !strings.Contains(note, "network:shared") || strings.Contains(note, "sandbox=host") {
		t.Fatalf("network-less sandbox note must describe the narrow capability, got %q", note)
	}
}

// TestIsolatedSandboxTimeoutHint: the hint appears ONLY for the trap case —
// isolated mode with network disabled. A networked sandbox timeout is a real
// timeout and must not be blamed on the sandbox; host mode never hints.
func TestIsolatedSandboxTimeoutHint(t *testing.T) {
	withExecSandboxPolicy(t, true, false, false)
	hint := isolatedSandboxTimeoutHint(SandboxDecision{Mode: SandboxIsolated})
	if !strings.Contains(hint, "sandbox_context: isolated network-disabled") || strings.Contains(hint, "sandbox=host") {
		t.Fatalf("isolated+no-network timeout must stay diagnostic, got %q", hint)
	}

	if hint := isolatedSandboxTimeoutHint(SandboxDecision{Mode: SandboxHost}); hint != "" {
		t.Fatalf("host mode must not hint, got %q", hint)
	}

	withExecSandboxPolicy(t, true, false, true)
	if hint := isolatedSandboxTimeoutHint(SandboxDecision{Mode: SandboxIsolated, NetworkShared: true}); hint != "" {
		t.Fatalf("networked sandbox must not blame the sandbox for a timeout, got %q", hint)
	}
}

func TestEnrichSandboxTimeoutEmitsOneClassification(t *testing.T) {
	withExecSandboxPolicy(t, true, false, false)
	err := enrichSandboxTimeout(
		"terminal",
		errors.New("command timed out after 1 seconds"),
		"",
		SandboxDecision{Mode: SandboxIsolated},
	)
	if err == nil || !strings.Contains(err.Error(), "sandbox_context: isolated network-disabled") {
		t.Fatalf("network-less timeout must include sandbox context, got %v", err)
	}
	if count := strings.Count(err.Error(), "error_class:"); count != 0 {
		t.Fatalf("timeout alone must not become a network classification; got %d: %v", count, err)
	}

	withExecSandboxPolicy(t, true, false, true)
	err = enrichSandboxTimeout(
		"terminal",
		errors.New("command timed out after 1 seconds"),
		"",
		SandboxDecision{Mode: SandboxIsolated, NetworkShared: true},
	)
	if err == nil || !strings.Contains(err.Error(), "error_class: timeout") {
		t.Fatalf("networked timeout must remain a normal timeout, got %v", err)
	}
	if count := strings.Count(err.Error(), "error_class:"); count != 1 {
		t.Fatalf("networked timeout emitted %d classifications: %v", count, err)
	}
}
