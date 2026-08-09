package httpapi

import (
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

func denyScopesFor(t *testing.T, content string) tools.RunIntentSnapshot {
	t.Helper()
	return runIntentSnapshot(api.MessageRequest{Content: content}, nil, nil, nil)
}

// The three real messages that deadlocked CI, plus the two shapes that must
// keep blocking. Each row states what the person forbade and what the agent
// then tried to do.
func TestDenyScopeConstrainsOnlyWhatItNames(t *testing.T) {
	cases := []struct {
		name    string
		content string
		classes []tools.OperationClass
		targets []string
		blocked bool
	}{
		{
			name:    "modify-files does not stop a read-only probe",
			content: "检查当前项目应该如何运行测试。先做最小化只读探测,如果命令失败请分析原因,不要修改文件。",
			classes: []tools.OperationClass{tools.OpClassExecInTurn},
			targets: []string{"go test ./..."},
			blocked: false,
		},
		{
			name:    "modify-files still stops a write",
			content: "检查当前项目应该如何运行测试。不要修改文件。",
			classes: []tools.OperationClass{tools.OpClassWrite},
			targets: []string{"/repo/main.go"},
			blocked: true,
		},
		{
			name:    "execute-directly asks for delegation, not a ban",
			content: "Register a durable external watch named CI completion. Do not execute the polling command directly.",
			classes: []tools.OperationClass{tools.OpClassExecDelegated},
			targets: []string{"printf 'SUCCEEDED\\n'"},
			blocked: false,
		},
		{
			name:    "execute-directly still stops the agent running it now",
			content: "Register a durable external watch. Do not execute the polling command directly.",
			classes: []tools.OperationClass{tools.OpClassExecInTurn},
			targets: []string{"printf 'SUCCEEDED\\n'"},
			blocked: true,
		},
		{
			name:    "an unqualified execution ban covers delegation too",
			content: "不要跑任何命令。",
			classes: []tools.OperationClass{tools.OpClassExecDelegated},
			targets: []string{"printf 'SUCCEEDED'"},
			blocked: true,
		},
		{
			name:    "a named file is the only file protected",
			content: "不要修改 config.yaml。",
			classes: []tools.OperationClass{tools.OpClassWrite},
			targets: []string{"/repo/README.md"},
			blocked: false,
		},
		{
			name:    "a named file is protected by any path that contains it",
			content: "不要修改 config.yaml。",
			classes: []tools.OperationClass{tools.OpClassWrite},
			targets: []string{"/repo/config.yaml"},
			blocked: true,
		},
		{
			name:    "an unclassifiable prohibition keeps the blanket effect",
			content: "不要乱来。",
			classes: []tools.OperationClass{tools.OpClassExecInTurn},
			targets: []string{"ls"},
			blocked: true,
		},
		{
			name:    "the dangerous heuristic alone never activates an unrelated deny",
			content: "不要修改文件。",
			classes: []tools.OperationClass{tools.OpClassDangerous},
			targets: []string{"curl https://example.com"},
			blocked: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := denyScopesFor(t, tc.content)
			if !snapshot.HasExplicitDeny() {
				t.Fatalf("fixture should record a prohibition: %q", tc.content)
			}
			if got := snapshot.DenyBlocks(tc.classes, tc.targets); got != tc.blocked {
				t.Fatalf("DenyBlocks(%v, %v) = %v, want %v (scopes=%+v)",
					tc.classes, tc.targets, got, tc.blocked, snapshot.DenyScopes)
			}
		})
	}
}

// A prohibition binds to its own clause. Without this, the read-only half of
// "probe read-only, then do not modify files" is what gets blocked.
func TestDenyScopeBindsToItsOwnClause(t *testing.T) {
	snapshot := denyScopesFor(t, "先运行测试看看,不要修改文件。")
	if len(snapshot.DenyScopes) != 1 {
		t.Fatalf("want exactly one prohibition, got %+v", snapshot.DenyScopes)
	}
	scope := snapshot.DenyScopes[0]
	if strings.Contains(scope.Clause, "运行") {
		t.Fatalf("prohibition absorbed the neighbouring instruction: %q", scope.Clause)
	}
	if len(scope.Classes) != 1 || scope.Classes[0] != tools.OpClassWrite {
		t.Fatalf("classes = %v, want [write]", scope.Classes)
	}
}

// Text the person did not author this turn is not a current prohibition.
func TestDenyScopeIgnoresNonUserAuthoredSource(t *testing.T) {
	snapshot := runIntentSnapshot(api.MessageRequest{
		Content: "不要修改文件",
		Origin:  "cron",
	}, nil, nil, nil)
	if snapshot.UserAuthored() {
		t.Fatal("a cron-originated turn is not user-authored")
	}
}

// Prohibitions that do not apply must still reach the judge: smart mode should
// weigh what the person said even when the deterministic gate stays open.
func TestUnmatchedDenyStillReachesTheJudge(t *testing.T) {
	snapshot := denyScopesFor(t, "不要修改文件。")
	if snapshot.DenyBlocks([]tools.OperationClass{tools.OpClassExecInTurn}, []string{"go test ./..."}) {
		t.Fatal("a write prohibition must not block an exec probe")
	}
	if len(snapshot.ExplicitDeny) == 0 {
		t.Fatal("the prohibition must remain visible as judge context")
	}
}
