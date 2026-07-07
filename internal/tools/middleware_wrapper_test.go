package tools

import (
	"context"
	"strings"
	"testing"
)

// TestHardlineUnwrapsWrappers pins the wrapper-bypass fix: the hard floor must
// see the real inner program behind a shell `-c`, a priv wrapper (sudo/doas),
// and exec prefixes (env/timeout/nice/...), not just the leading token.
func TestHardlineUnwrapsWrappers(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"bash -c rm root", `bash -c "rm -rf /"`, true},
		{"sh -c rm root single quotes", `sh -c 'rm -rf /'`, true},
		{"bash -lc combined flag", `bash -lc "rm -rf /"`, true},
		{"sudo rm root", "sudo rm -rf /", true},
		{"sudo -u value rm root", "sudo -u root rm -rf /", true},
		{"doas rm root", "doas rm -rf /", true},
		{"env rm home var", "env rm -rf $HOME", true},
		{"env assignment rm root", "env FOO=bar rm -rf /", true},
		{"timeout duration rm root", "timeout 5 rm -rf /", true},
		{"timeout suffix duration rm root", "timeout 30s rm -rf /", true},
		{"nice value flag rm root", "nice -n 10 rm -rf /", true},
		{"nested sudo bash rm", `sudo bash -c "rm -rf /"`, true},
		{"bash -c mkfs", `bash -c "mkfs.ext4 /dev/sda1"`, true},
		{"bash -c shutdown", `bash -c "shutdown -h now"`, true},
		{"embedded shell in code style", "os.system('rm -rf /')", true},

		// Benign wrapped commands must NOT trip the floor.
		{"bash -c ls", `bash -c "ls -la"`, false},
		{"sudo ls", "sudo ls -la", false},
		{"timeout duration ls", "timeout 5 ls", false},
		{"env assignment build", "env GOFLAGS=-mod=mod go build ./...", false},
		{"opaque bash script not hardline", "bash deploy.sh", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := hardlineToolCall("", "terminal", map[string]interface{}{"command": tc.cmd})
			if got != tc.want {
				t.Fatalf("hardlineToolCall(%q) = %v (reason %q), want %v", tc.cmd, got, reason, tc.want)
			}
		})
	}
}

// TestDangerousUnwrapsWrappersAndOpaque proves the dangerous heuristic also
// unwraps wrappers and flags an opaque (unparseable) wrapped command.
func TestDangerousUnwrapsWrappersAndOpaque(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"bash -c rm subdir", `bash -c "rm -rf build"`, true},
		{"sudo chmod", "sudo chmod 777 x", true},
		{"env kill", "env kill -9 123", true},
		{"opaque bash script", "bash deploy.sh", true}, // unparsed -> dangerous
		{"benign bash -c ls", `bash -c "ls -la"`, false},
		{"benign sudo ls", "sudo ls", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := dangerousToolCall("", "terminal", map[string]interface{}{"command": tc.cmd})
			if got != tc.want {
				t.Fatalf("dangerousToolCall(%q) = %v (reason %q), want %v", tc.cmd, got, reason, tc.want)
			}
		})
	}
}

// TestExecuteCodePayloadIsClassified proves execute_code's args["code"] payload
// is now visible to BOTH the hard floor and the dangerous heuristic (previously
// they only read args["command"], so execute_code slid past both entirely).
func TestExecuteCodePayloadIsClassified(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	if blocked, reason := hardlineToolCall("", "execute_code", map[string]interface{}{
		"code": "import os\nos.system('rm -rf /')",
	}); !blocked {
		t.Fatalf("execute_code code payload with rm -rf / must hit the hard floor; reason=%q", reason)
	}
	if d, _ := dangerousToolCall("", "execute_code", map[string]interface{}{
		"code": "import os\nos.system('rm -rf build')",
	}); !d {
		t.Fatal("execute_code code payload invoking rm must be flagged dangerous")
	}
	if blocked, _ := hardlineToolCall("", "execute_code", map[string]interface{}{
		"code": "print('hello world')",
	}); blocked {
		t.Fatal("benign execute_code payload must not be hard-blocked")
	}
}

// TestExecuteCodeRequiresApprovalOnRequest is the core P0-1 assertion: in the
// DEFAULT on-request mode, execute_code (arbitrary code) must reach the human
// ask even though its payload is benign — it is never auto-run unprompted.
func TestExecuteCodeRequiresApprovalOnRequest(t *testing.T) {
	asked := 0
	cleanup := SetExecutionScope("person-ec", ExecutionScope{
		TenantID: "tenant-ec", PersonID: "person-ec", TaskID: "task-ec",
		ApprovalMode: ApprovalOnRequest,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			asked++
			if req.ToolName != "execute_code" {
				t.Fatalf("unexpected tool asked: %q", req.ToolName)
			}
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr"}, nil
		},
	})
	defer cleanup()

	ran := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	})
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-ec",
		"_tool_name": "execute_code",
		"code":       "print('totally benign')",
	}); err != nil {
		t.Fatalf("approved execute_code should run: %v", err)
	}
	if asked != 1 {
		t.Fatalf("execute_code must ask exactly once in on-request mode, got %d", asked)
	}
	if !ran {
		t.Fatal("execute_code should run after approval")
	}
}

// TestExecuteCodeSmartModeTriaged proves smart mode routes a benign
// execute_code through the LLM judge (not a silent auto-run).
func TestExecuteCodeSmartModeTriaged(t *testing.T) {
	judge := &fakeJudge{reply: "APPROVE"}
	cleanup := SetExecutionScope("person-ecs", ExecutionScope{
		TenantID: "tenant-ecs", PersonID: "person-ecs", TaskID: "task-ecs",
		ApprovalMode: ApprovalSmart, Grants: newFakeGrantStore(), Judge: judge,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			t.Fatal("human ask must not be reached on an APPROVE verdict")
			return ToolApprovalDecision{}, nil
		},
	})
	defer cleanup()

	ran := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	})
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-ecs",
		"_tool_name": "execute_code",
		"code":       "print('hi')",
	}); err != nil {
		t.Fatalf("triage-approved execute_code should run: %v", err)
	}
	if judge.calls != 1 {
		t.Fatalf("smart mode must triage execute_code exactly once, got %d", judge.calls)
	}
	if !ran {
		t.Fatal("execute_code should run after an APPROVE verdict")
	}
}

// TestExecuteCodeHardFloorInCodePayload proves the hard floor fires for a
// destructive command hidden in the code payload, even under full-auto — and
// with the safety-policy wording, not the user-rejection contract.
func TestExecuteCodeHardFloorEvenFullAuto(t *testing.T) {
	cleanup := SetExecutionScope("person-echf", ExecutionScope{
		TenantID: "tenant-echf", PersonID: "person-echf",
		ApprovalMode: ApprovalFullAuto,
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			t.Fatal("approval must not be consulted for a hard-floor op")
			return ToolApprovalDecision{}, nil
		},
	})
	defer cleanup()

	ran := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		ran = true
		return "ran", nil
	})
	_, err := exec(map[string]interface{}{
		"_tenant_id": "person-echf",
		"_tool_name": "execute_code",
		"code":       "import os; os.system('rm -rf /')",
	})
	if err == nil || ran {
		t.Fatalf("hard-floor code payload must be blocked even under full-auto: err=%v ran=%v", err, ran)
	}
	if !strings.Contains(err.Error(), "blocked by safety policy") {
		t.Fatalf("must read as a safety-policy block, got: %v", err)
	}
}
