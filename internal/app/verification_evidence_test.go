package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

func TestConfiguredDispatcherRetainsVerificationFailureReference(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Evolution: config.EvolutionConfig{SkillsDir: t.TempDir()}}
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dispatcher, err := InitTools(memory.NewMemoryManager(nil), cfg, nil, "evidence", nil, store)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := tools.SetExecutionScope("evidence", tools.ExecutionScope{TenantID: "evidence", RunID: "run-evidence", WorkspaceRoot: root, AllowedRoots: []string{root}, ApprovalMode: tools.ApprovalFullAuto})
	defer cleanup()
	events := make(chan string, 64)
	result, err := dispatcher.Dispatch("verify", map[string]interface{}{"_tenant_id": "evidence", "_context": kernel.WithEventChannel(context.Background(), events), "_tool_call_id": "check-failed", "command": "exit 7", "check": map[string]interface{}{"criterion": "exit is zero", "target": "fixture"}})
	if err == nil || !strings.Contains(result, "Verification evidence: check-failed") {
		t.Fatalf("lost failed-check reference: %q %v", result, err)
	}
	for len(events) > 0 {
		event, ok := kernel.DecodeAgentEvent(<-events)
		if !ok || event.Type != "evidence.recorded" {
			continue
		}
		data, _ := json.Marshal(event.Payload["evidence"])
		var evidence kernel.RunEvidence
		_ = json.Unmarshal(data, &evidence)
		if evidence.Command == nil || evidence.Command.Binding == nil || evidence.Command.ExitCode != 7 || evidence.Command.Binding.Criterion != "exit is zero" {
			t.Fatalf("lost invocation facts: %s", data)
		}
		return
	}
	t.Fatal("no verification evidence")
}
