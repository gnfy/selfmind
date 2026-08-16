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
			name:    "execute later does not block a read-only observation",
			content: "先检查发布状态，暂时不要执行。",
			classes: []tools.OperationClass{tools.OpClassObserve},
			targets: []string{"gcloud builds describe build-1"},
			blocked: false,
		},
		{
			name:    "execute later still blocks a mutating command",
			content: "先检查发布状态，暂时不要执行。",
			classes: []tools.OperationClass{tools.OpClassExecInTurn},
			targets: []string{"gcloud builds triggers run deploy"},
			blocked: true,
		},
		{
			name:    "explicit command ban includes observations",
			content: "不要运行任何命令。",
			classes: []tools.OperationClass{tools.OpClassObserve},
			targets: []string{"gcloud builds describe build-1"},
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

// Real eval-case wording that the first scoping pass still blocked. Each row
// is a sentence a person actually wrote, paired with the operation the agent
// was asked to perform in the very same message.
func TestDenyScopeDoesNotBlockWhatTheSentenceAsksFor(t *testing.T) {
	cases := []struct {
		name    string
		content string
		classes []tools.OperationClass
		blocked bool
	}{
		{
			// "run" used to match inside "rerun", so forbidding a SECOND run
			// blocked the first one the same sentence requested.
			name:    "do not rerun leaves the requested first run alone",
			content: "Run the command false exactly once as the verification step. Do not repair or rerun the verification.",
			classes: []tools.OperationClass{tools.OpClassExecInTurn},
			blocked: false,
		},
		{
			// "foreground" is the manner qualifier here: the person is ruling
			// out an in-turn poll loop and asking for delegation instead.
			name:    "forbidding a foreground loop still allows delegation",
			content: "Register a durable external watch for the deployment. Do not run a foreground polling loop.",
			classes: []tools.OperationClass{tools.OpClassExecDelegated},
			blocked: false,
		},
		{
			name:    "forbidding a foreground loop still stops an in-turn run",
			content: "Register a durable external watch for the deployment. Do not run a foreground polling loop.",
			classes: []tools.OperationClass{tools.OpClassExecInTurn},
			blocked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := denyScopesFor(t, tc.content)
			if !snapshot.HasExplicitDeny() {
				t.Fatalf("fixture should record a prohibition: %q", tc.content)
			}
			if got := snapshot.DenyBlocks(tc.classes, nil); got != tc.blocked {
				t.Fatalf("DenyBlocks(%v) = %v, want %v (scopes=%+v)", tc.classes, got, tc.blocked, snapshot.DenyScopes)
			}
		})
	}
}

// A prohibition governs what follows it. "Finish the run as waiting_user and
// do not invent or resolve that input" forbids inventing input; the noun "run"
// ahead of the marker used to classify the whole clause as an execution ban.
//
// The clause still blocks — it names no operation this code can classify, and
// an unclassifiable prohibition deliberately keeps its blanket effect — but it
// now does so honestly, as "unresolved", rather than by claiming the person
// forbade execution.
func TestDenyScopeIgnoresWordsBeforeTheMarker(t *testing.T) {
	snapshot := denyScopesFor(t, "Finish the run as waiting_user and do not invent or resolve that input.")
	if len(snapshot.DenyScopes) != 1 {
		t.Fatalf("want exactly one prohibition, got %+v", snapshot.DenyScopes)
	}
	scope := snapshot.DenyScopes[0]
	if len(scope.Classes) != 0 {
		t.Fatalf("classes = %v, want none: nothing after the marker names an operation", scope.Classes)
	}
	if scope.Resolved {
		t.Fatal("a clause that names no operation must stay unresolved")
	}
}

// Single-character Chinese verbs are how people actually write this. Matching
// only the two-character compounds left the most ordinary "don't touch the
// code" unclassified, which meant the blanket fail-safe and a run where every
// tool asked for approval nobody was there to give.
func TestDenyScopeClassifiesSingleCharacterVerbs(t *testing.T) {
	cases := []struct {
		content string
		want    tools.OperationClass
	}{
		{"先给一个优化 CLI 交互体验的三步方案，不要改代码。", tools.OpClassWrite},
		{"不要写文件", tools.OpClassWrite},
		{"不要删这个目录", tools.OpClassDelete},
	}
	for _, tc := range cases {
		t.Run(tc.content, func(t *testing.T) {
			snapshot := denyScopesFor(t, tc.content)
			if len(snapshot.DenyScopes) != 1 {
				t.Fatalf("want one prohibition, got %+v", snapshot.DenyScopes)
			}
			scope := snapshot.DenyScopes[0]
			if !scope.Resolved || len(scope.Classes) != 1 || scope.Classes[0] != tc.want {
				t.Fatalf("classes = %v (resolved=%v), want [%s]", scope.Classes, scope.Resolved, tc.want)
			}
			// A plan-only turn touches nothing writable, so it must run.
			if snapshot.DenyBlocks([]tools.OperationClass{tools.OpClassExecInTurn}, []string{"ls"}) {
				t.Fatal("a write prohibition must not block a read-only exec probe")
			}
		})
	}
}
