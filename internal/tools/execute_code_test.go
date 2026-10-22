package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/tools/sandbox"
)

func TestExecuteCodeTripleQuoteIsNotInjectable(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	tool := NewExecuteCodeTool()

	// Code containing triple quotes used to break out of the exec('''...''')
	// wrapper and could inject arbitrary statements. With direct file execution
	// it must run verbatim and simply print the literal text.
	code := "print('start')\nx = '''embedded ''' + 'quotes'\nprint(x)\nprint('end')"
	out, err := tool.Execute(map[string]interface{}{"code": code, "timeout": 30})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"start", "embedded quotes", "end"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q; got: %q", want, out)
		}
	}
}

func TestExecuteCodeReportsErrors(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	tool := NewExecuteCodeTool()
	_, err := tool.Execute(map[string]interface{}{"code": "raise ValueError('boom')", "timeout": 30})
	if err == nil {
		t.Fatal("expected error from failing script")
	}
}

// Every call runs in a directory of its own, created private and gone afterwards.
// A directory shared by calls was a writable root of all of them.
func TestExecuteCodeCallsGetPrivateDirectoriesThatAreRemoved(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	shared := filepath.Join(home, ".selfmind", "code_sandbox")
	var dirs []string
	for i := 0; i < 2; i++ {
		out, err := NewExecuteCodeTool().Execute(map[string]interface{}{
			"code": "import os\nprint(os.getcwd())\nprint(oct(os.stat('.').st_mode & 0o777))", "timeout": 30,
		})
		fields := strings.Fields(out)
		if err != nil || len(fields) < 2 {
			t.Fatalf("call %d: out=%q err=%v", i, out, err)
		}
		dirs = append(dirs, fields[0])
		if fields[1] != "0o700" {
			t.Fatalf("call %d: its directory has mode %s, want 0o700", i, fields[1])
		}
	}
	realShared, err := filepath.EvalSymlinks(shared)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		if filepath.Dir(dir) != realShared || !strings.HasPrefix(filepath.Base(dir), "call-") {
			t.Fatalf("a call ran in %s, want a call-* directory inside %s", dir, realShared)
		}
	}
	if dirs[0] == dirs[1] {
		t.Fatalf("two calls shared the directory %s", dirs[0])
	}
	if left, _ := os.ReadDir(shared); len(left) != 0 {
		t.Fatalf("calls left files behind: %v", left)
	}
}

// Sandboxed code of one Run must not be able to replace another call's script:
// the shared parent used to be a writable root of every call, so a polling script
// could overwrite a pending one before python opened it.
func TestExecuteCodeDirectoriesAreOutOfReachOfAnotherRunsSandbox(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if !ExecSandboxAvailable() {
		t.Skip("isolated sandbox unavailable on this host")
	}
	withExecSandboxPolicy(t, true, false, false)
	// Outside /tmp: the Linux sandbox replaces /tmp with a private one and creates the
	// mount points of its writable roots there, so a shared parent under /tmp would be a
	// writable directory of that private view rather than the host's read-only one.
	base := fixtureBase(t)
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "root")
	for _, dir := range []string{home, root} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	shared := filepath.Join(home, ".selfmind", "code_sandbox")
	pending := filepath.Join(shared, "call-pending")
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	victims := []string{filepath.Join(shared, "script_pending.py"), filepath.Join(pending, "script.py")}
	for _, victim := range victims {
		if err := os.WriteFile(victim, []byte("print('original')"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	planted := filepath.Join(shared, "planted.py")
	const run = "execute-code-isolation"
	runtime := NewExecutionRuntime(run, t.TempDir(), sandbox.PrivateState{}, nil)
	t.Cleanup(SetExecutionScope(run, ExecutionScope{TenantID: "t", PersonID: run, RunID: run, WorkspaceRoot: root, AllowedRoots: []string{root},
		SandboxPolicy: &ExecSandboxPolicy{Enabled: true}, Runtime: runtime}))
	code := fmt.Sprintf(`import os
for path in %q, %q, %q:
    try:
        open(path, "w").write("print('HIJACKED')")
        print("wrote", os.path.basename(path))
    except OSError:
        print("blocked", os.path.basename(path))
open("own.txt", "w").write("x")
print("own directory writable")
`, victims[0], victims[1], planted)
	args := map[string]interface{}{
		"_context": WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun(run)), "_tool_name": "execute_code",
		"_effective_sandbox_mode": string(SandboxIsolated), "code": code,
	}
	out, err := NewExecuteCodeTool().Execute(args)
	if err != nil || args["_sandbox_mode"] != string(SandboxIsolated) {
		t.Fatalf("the call did not run isolated: mode=%v out=%q err=%v", args["_sandbox_mode"], out, err)
	}
	for _, want := range []string{"blocked script_pending.py", "blocked script.py", "blocked planted.py", "own directory writable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	for _, victim := range victims {
		if data, _ := os.ReadFile(victim); string(data) != "print('original')" {
			t.Fatalf("%s was replaced: %q", victim, data)
		}
	}
	if _, err := os.Stat(planted); err == nil {
		t.Fatal("another Run's sandboxed code created a file in the shared directory")
	}
}
