package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel"
)

func TestTypedCommandResultSurvivesCoercionAndMiddleware(t *testing.T) {
	for _, tool := range []Tool{NewExecuteCommandTool(), NewVerifyTool()} {
		for _, failed := range []bool{false, true} {
			t.Run(tool.Name()+"/"+map[bool]string{true: "failure", false: "success"}[failed], func(t *testing.T) {
				registry := NewRegistry()
				registry.Register(tool)
				registry.UseMiddleware(RedactionMiddleware())
				registry.UseResultMiddleware(EvidenceMiddleware())
				command := "printf observed"
				if failed {
					command += "; exit 7"
				}
				args := map[string]interface{}{"command": command, "cwd": t.TempDir(), "timeout": "5", "_tool_call_id": "call-check"}
				result, err := registry.DispatchResult(tool.Name(), args)
				if (err != nil) != failed || result.Invoked == nil || !*result.Invoked {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if result.Process == nil || !result.Process.Started || result.Process.ExitCode == nil {
					t.Fatalf("lost process facts: %+v", result.Process)
				}
				want := 0
				if failed {
					want = 7
				}
				if *result.Process.ExitCode != want || len(result.Evidence) != 1 || result.Evidence[0].Command.ExitCode != want {
					t.Fatalf("inconsistent facts: %+v evidence=%+v", result.Process, result.Evidence)
				}
				if !strings.Contains(result.Output, "observed") {
					t.Fatal("output lost")
				}
				for _, retired := range []string{"_command_exit_code", "_verification_binding", "_recovery_outcome"} {
					if _, ok := args[retired]; ok {
						t.Fatalf("output escaped into args: %s", retired)
					}
				}
			})
		}
	}
}

func TestTypedStartFailureHasNoSuccessfulExitStatus(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewVerifyTool())
	registry.UseResultMiddleware(EvidenceMiddleware())
	result, err := registry.DispatchResult("verify", map[string]interface{}{"command": "printf unreachable", "cwd": filepath.Join(t.TempDir(), "missing")})
	if err == nil || result.Process == nil || result.Process.Started || result.Process.ExitCode != nil {
		t.Fatalf("result=%+v process=%+v err=%v", result, result.Process, err)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Command.ExitCode != -1 || result.Evidence[0].Status == "succeeded" {
		t.Fatalf("fabricated successful evidence: %+v", result.Evidence)
	}
}

func TestTypedCancellationPreservesPartialEffects(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := NewRegistry()
	registry.Register(NewExecuteCommandTool())
	registry.UseResultMiddleware(EvidenceMiddleware())
	done := make(chan struct{})
	var result kernel.ToolDispatchResult
	var err error
	go func() {
		defer close(done)
		result, err = registry.DispatchResult("terminal", map[string]interface{}{"command": "printf changed > effect; exec sleep 30", "cwd": dir, "_context": ctx})
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	written := false
wait:
	for {
		select {
		case <-tick.C:
			if _, statErr := os.Stat(filepath.Join(dir, "effect")); statErr == nil {
				written = true
				break wait
			}
		case <-done:
			t.Fatalf("command ended before effect: %v", err)
		case <-deadline.C:
			cancel()
			<-done
			t.Fatal("command did not create effect")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled command did not stop")
	}
	if !written || err == nil || result.Process == nil || !result.Process.Started || (result.Process.ExitCode != nil && *result.Process.ExitCode == 0) {
		t.Fatalf("partial effect erased: process=%+v err=%v", result.Process, err)
	}
	if data, readErr := os.ReadFile(filepath.Join(dir, "effect")); readErr != nil || string(data) != "changed" {
		t.Fatalf("effect=%q err=%v", data, readErr)
	}
}

type resultProbeTool struct {
	BaseTool
	calls    int
	external bool
}

func (t *resultProbeTool) SchemaOrigin() ToolSchemaOrigin {
	if t.external {
		return ToolSchemaOriginExternal
	}
	return ToolSchemaOriginBuiltin
}
func (t *resultProbeTool) ExecuteResult(map[string]interface{}) (kernel.ToolDispatchResult, error) {
	t.calls++
	code := 9
	err := newStableToolRecoveryError(errors.New("token=secret-result-value"), "test_failure", "test", "token=secret-result-value", "inspect", "execution", "after_observation", "unknown", true, "inspect")
	return kernel.ToolDispatchResult{Output: "token=secret-result-value", Process: &kernel.ToolProcessResult{Started: true, ExitCode: &code}, Evidence: []kernel.RunEvidence{{ToolCallID: "proof"}}}, err
}

func TestTypedFactsSurviveRedactionAndApprovalRefusal(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		registry := NewRegistry()
		tool := &resultProbeTool{BaseTool: BaseTool{name: "probe", schema: ToolSchema{Type: "object"}}}
		registry.Register(tool)
		registry.UseMiddleware(RedactionMiddleware())
		registry.UseMiddleware(ApprovalMiddleware(blocked))
		result, err := registry.DispatchResult("probe", nil)
		if err == nil || result.Invoked == nil || *result.Invoked == blocked {
			t.Fatalf("invoked=%v err=%v", result.Invoked, err)
		}
		if blocked {
			if tool.calls != 0 || result.Process != nil {
				t.Fatal("approval refusal dispatched")
			}
			continue
		}
		if tool.calls != 1 || result.Process == nil || *result.Process.ExitCode != 9 || len(result.Evidence) != 1 {
			t.Fatalf("facts lost: %+v", result)
		}
		var failure *stableToolError
		if !errors.As(err, &failure) || failure.ToolEffectState() != "unknown" || !failure.ToolStateChanged() || failure.ToolFailurePhase() != "execution" {
			t.Fatalf("redaction lost failure facts: %v", err)
		}
		if strings.Contains(result.Output, "secret-result-value") || strings.Contains(err.Error(), "secret-result-value") || strings.Contains(failure.ModelSafeMessage(), "secret-result-value") {
			t.Fatal("secret leaked")
		}
	}
}

func TestExternalResultCannotAssertLocalExecutionFacts(t *testing.T) {
	registry := NewRegistry()
	tool := &resultProbeTool{BaseTool: BaseTool{name: "external_probe", schema: ToolSchema{Type: "object"}}, external: true}
	registry.Register(tool)
	result, _ := registry.DispatchResult(tool.Name(), nil)
	if tool.calls != 1 || result.Process != nil || len(result.Evidence) != 0 {
		t.Fatalf("untrusted facts accepted: %+v", result)
	}
}

func TestLegacyFileFailurePreservesObservedEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	registry.Register(&BaseTool{name: "write_file", schema: ToolSchema{Type: "object"}, handler: func(map[string]interface{}) (string, error) {
		if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
			return "", err
		}
		return "partial write observed", context.Canceled
	}})
	registry.UseMiddleware(RedactionMiddleware())
	registry.UseResultMiddleware(EvidenceMiddleware())
	result, err := registry.DispatchResult("write_file", map[string]interface{}{"path": path, "_tool_call_id": "partial"})
	if !errors.Is(err, context.Canceled) || result.Invoked == nil || !*result.Invoked || result.Process != nil || len(result.Evidence) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	evidence := result.Evidence[0]
	if evidence.Status != "failed" || len(evidence.Files) != 1 || evidence.Files[0].BeforeSHA256 == evidence.Files[0].AfterSHA256 || evidence.Files[0].AfterSHA256 == "" {
		t.Fatalf("partial mutation erased: %+v", evidence)
	}
}
