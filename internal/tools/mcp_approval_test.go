package tools

import (
	"context"
	"fmt"
	"testing"
)

type externalApprovalTestTool struct{ BaseTool }

func newExternalApprovalTestTool() *externalApprovalTestTool {
	return &externalApprovalTestTool{BaseTool: BaseTool{
		name: "external_delete", description: "untrusted external tool",
		schema: ToolSchema{Type: "object", Properties: map[string]PropertyDef{
			"target": {Type: "string"},
		}},
		metadata: ToolMetadata{Category: "mcp", RiskLevel: ToolRiskMedium},
		handler:  func(map[string]interface{}) (string, error) { return "executed", nil },
	}}
}

func (t *externalApprovalTestTool) SchemaOrigin() ToolSchemaOrigin {
	return ToolSchemaOriginExternal
}

func TestUnclassifiedExternalToolAlwaysRequiresOnceOnlyHumanApproval(t *testing.T) {
	modes := []ApprovalMode{ApprovalOnRequest, ApprovalReadOnly, ApprovalAutoEdit, ApprovalSmart, ApprovalFullAuto}
	for index, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			personID := fmt.Sprintf("person-mcp-%d", index)
			asks := 0
			judge := &fakeJudge{reply: "APPROVE"}
			cleanup := SetExecutionScope(personID, ExecutionScope{
				TenantID: "tenant-mcp", PersonID: personID, TaskID: "task-mcp", RunID: "run-mcp",
				ApprovalMode: mode, Judge: judge,
				Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
					asks++
					if req.DecisionPolicy != ApprovalDecisionPolicyOnceOnly {
						t.Fatalf("decision policy = %q", req.DecisionPolicy)
					}
					if req.Reason != unclassifiedExternalApprovalReason {
						t.Fatalf("reason = %q", req.Reason)
					}
					if len(req.Args) != 1 || req.Args["target"] != "repo" {
						t.Fatalf("approval args = %#v", req.Args)
					}
					return ToolApprovalDecision{Approved: true, ApprovalID: "apr-mcp"}, nil
				},
			})
			defer cleanup()

			dispatcher := NewDispatcherWithRegistry(NewRegistry())
			dispatcher.InjectMiddleware(SmartApprovalMiddleware(""))
			dispatcher.RegisterTool(newExternalApprovalTestTool())
			result, err := dispatcher.Dispatch("external_delete", map[string]interface{}{
				"_tenant_id": personID,
				"target":     "repo",
			})
			if err != nil || result != "executed" {
				t.Fatalf("result=%q err=%v", result, err)
			}
			if asks != 1 {
				t.Fatalf("human asks = %d", asks)
			}
			if judge.calls != 0 {
				t.Fatalf("untrusted external descriptions must not reach smart triage; judge calls=%d", judge.calls)
			}
		})
	}
}

func TestExternalToolPolicyComesFromRegisteredToolNotName(t *testing.T) {
	tool := newExternalApprovalTestTool()
	tool.name = "harmless_looking_name"
	registry := NewRegistry()
	registry.Register(tool)
	seen := false
	exec := registry.Wrap(tool, []Middleware{func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			seen = unclassifiedExternalToolCall(args)
			return next(args)
		}
	}})
	if _, err := exec(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("external origin was not injected into the dispatch policy")
	}
}
