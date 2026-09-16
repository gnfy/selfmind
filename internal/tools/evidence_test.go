package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/kernel"
)

func TestEvidenceMiddlewareRecordsObservedFileMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observed.txt")
	events := make(chan string, 1)
	ctx := kernel.WithEventChannel(context.Background(), events)

	exec := EvidenceMiddleware()(func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
		return kernel.ToolDispatchResult{Output: "written"}, os.WriteFile(path, []byte("after"), 0644)
	})
	result, err := exec(map[string]interface{}{
		"_context":      ctx,
		"_tool_name":    "write_file",
		"_tool_call_id": "call-1",
		"path":          path,
	})
	if err != nil || result.Output != "written" {
		t.Fatalf("execute: result=%q err=%v", result.Output, err)
	}

	evidence := readEvidenceEvent(t, events)
	if evidence.ToolCallID != "call-1" || evidence.Kind != "mutation" || evidence.Status != "succeeded" {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
	if len(evidence.Files) != 1 || evidence.Files[0].Path != path || evidence.Files[0].AfterSHA256 == "" {
		t.Fatalf("missing file effect: %+v", evidence.Files)
	}
	if evidence.Files[0].BeforeSHA256 == evidence.Files[0].AfterSHA256 {
		t.Fatalf("mutation hashes did not change: %+v", evidence.Files[0])
	}
}

func TestEvidenceMiddlewareRecordsFailedVerificationExitCode(t *testing.T) {
	events := make(chan string, 1)
	ctx := kernel.WithEventChannel(context.Background(), events)
	expectedErr := errors.New("command failed")

	exec := EvidenceMiddleware()(func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
		code := 2
		return kernel.ToolDispatchResult{Output: "failed output", Process: &kernel.ToolProcessResult{Started: true, ExitCode: &code}}, expectedErr
	})
	_, err := exec(map[string]interface{}{
		"_context":      ctx,
		"_tool_name":    "verify",
		"_tool_call_id": "call-2",
		"command":       "go test ./...",
		"cwd":           "/workspace",
		"kind":          "test",
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected wrapped execution error, got %v", err)
	}

	evidence := readEvidenceEvent(t, events)
	if evidence.Kind != "verification" || evidence.Status != "failed" || evidence.Command == nil {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
	if evidence.Command.ExitCode != 2 || evidence.Command.Kind != "test" || evidence.Command.Command != "go test ./..." {
		t.Fatalf("unexpected command evidence: %+v", evidence.Command)
	}
}

func TestEvidenceMiddlewareRecordsTerminalAsCommandEvidence(t *testing.T) {
	events := make(chan string, 1)
	ctx := kernel.WithEventChannel(context.Background(), events)

	exec := EvidenceMiddleware()(func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
		code := 0
		return kernel.ToolDispatchResult{Output: "/workspace/go.mod", Process: &kernel.ToolProcessResult{Started: true, ExitCode: &code}}, nil
	})
	result, err := exec(map[string]interface{}{
		"_context":      ctx,
		"_tool_name":    "terminal",
		"_tool_call_id": "call-3",
		"command":       "go env GOMOD",
		"cwd":           "/workspace",
	})
	if err != nil || result.Output != "/workspace/go.mod" {
		t.Fatalf("execute: result=%q err=%v", result.Output, err)
	}

	evidence := readEvidenceEvent(t, events)
	if evidence.Kind != "command" || evidence.Status != "succeeded" || evidence.Command == nil {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
	if evidence.Command.ExitCode != 0 || evidence.Command.Kind != "command" || evidence.Command.Command != "go env GOMOD" {
		t.Fatalf("unexpected command evidence: %+v", evidence.Command)
	}
}

func readEvidenceEvent(t *testing.T, events <-chan string) kernel.RunEvidence {
	t.Helper()
	raw := <-events
	event, ok := kernel.DecodeAgentEvent(raw)
	if !ok || event.Type != "evidence.recorded" {
		t.Fatalf("unexpected event: %q", raw)
	}
	encoded, err := json.Marshal(event.Payload["evidence"])
	if err != nil {
		t.Fatal(err)
	}
	var evidence kernel.RunEvidence
	if err := json.Unmarshal(encoded, &evidence); err != nil {
		t.Fatal(err)
	}
	return evidence
}
