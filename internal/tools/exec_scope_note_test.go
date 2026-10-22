package tools

import (
	"context"
	"strings"
	"testing"
)

func TestExecutionNoteUsesExactRunPolicy(t *testing.T) {
	run := "run-note"
	cleanup := SetExecutionScope("person-note", ExecutionScope{RunID: run, ParallelWork: true, SandboxPolicy: &ExecSandboxPolicy{Enabled: true, Required: true, AllowNetwork: true}})
	defer cleanup()
	ctx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun(run))
	note := ExecSandboxPromptNoteForContext(ctx)
	if !strings.Contains(note, "regardless of occupied worker slots") || strings.Contains(note, "can request sandbox=host") {
		t.Fatalf("misleading run capability: %s", note)
	}
}

func TestExecutionNoteUsesSerialRunFrozenNetworkPolicy(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("enforced sandbox unavailable")
	}
	cleanup := SetExecutionScope("person-serial-note", ExecutionScope{RunID: "run-serial-note", SandboxPolicy: &ExecSandboxPolicy{Enabled: true, AllowNetwork: false}})
	defer cleanup()
	ctx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun("run-serial-note"))
	note := ExecSandboxPromptNoteForContext(ctx)
	if !strings.Contains(note, "network is disabled by default") || strings.Contains(note, "network shares") {
		t.Fatalf("ignored frozen Run network policy: %s", note)
	}
}
