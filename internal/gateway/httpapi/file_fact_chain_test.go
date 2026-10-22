package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func TestBatchFileFactsRemainReadableThroughPersonScopedArtifact(t *testing.T) {
	sink, store, _ := newSinkHarness(t)
	run, err := store.GetRun(context.Background(), sink.tenantID, sink.runID)
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.ToolInvocationScope{ControlTenantID: sink.tenantID, PersonID: run.PersonID, RunID: run.ID, ExecutionScopeKey: tools.ExecutionScopeKeyForRun(run.ID)}
	defer tools.SetExecutionScope(run.PersonID, tools.ExecutionScope{TenantID: sink.tenantID, PersonID: run.PersonID, RunID: run.ID})()
	ctx := kernel.WithToolArtifactSink(kernel.WithToolInvocationScope(context.Background(), scope), sink)
	root := t.TempDir()
	path := filepath.Join(root, "facts.txt")
	content := strings.Repeat("a", 6000) + "FACT-KEPT-IN-MIDDLE" + strings.Repeat("z", 6000)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	dispatcher := tools.NewDispatcher()
	tools.RegisterBuiltins(dispatcher)
	batch := tools.NewBatchReadTool(dispatcher.Dispatch)
	out, err := batch.ExecuteContext(ctx, map[string]interface{}{"_invocation_scope": scope, "operations": []interface{}{map[string]interface{}{"tool": "read_file", "path": path}}})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Items []struct {
			ArtifactID string `json:"artifact_id"`
			Output     string `json:"output"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ArtifactID == "" || strings.Contains(result.Items[0].Output, "FACT-KEPT-IN-MIDDLE") {
		t.Fatal("fixture did not require artifact read-back")
	}
	viewer := tools.NewToolOutputViewTool(filepath.Dir(sink.dir))
	args := map[string]interface{}{"artifact_id": result.Items[0].ArtifactID, "offset_bytes": 5900, "limit_bytes": 200, "_invocation_scope": scope}
	read, err := viewer.Execute(args)
	if err != nil || !strings.Contains(read, "FACT-KEPT-IN-MIDDLE") {
		t.Fatalf("middle could not be recovered: %v", err)
	}
	scope.PersonID = "different-person"
	scope.RunID = "different-run"
	scope.ExecutionScopeKey = tools.ExecutionScopeKeyForRun(scope.RunID)
	defer tools.SetExecutionScope(scope.PersonID, tools.ExecutionScope{TenantID: sink.tenantID, PersonID: scope.PersonID, RunID: scope.RunID})()
	args["_invocation_scope"] = scope
	if _, err := viewer.Execute(args); err == nil || !strings.Contains(err.Error(), "not found in this person's partition") {
		t.Fatal("artifact crossed person boundary")
	}
}
