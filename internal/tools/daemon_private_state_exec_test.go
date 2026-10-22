package tools

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
)

// The reviewer's exploit, end to end through the real execution engine: a
// confined command whose writable root is the person's home directory still
// cannot read the daemon's state or reach its listeners, because the daemon
// installs that state as private and every isolated backend applies it.
func TestIsolatedCommandCannotReachInstalledDaemonPrivateState(t *testing.T) {
	if !ExecSandboxAvailable() || runtime.GOOS != "darwin" {
		t.Skip("needs the real isolated backend; exercised on the macOS Seatbelt host")
	}
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(true, false, true)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })
	tenant, workspace := scratchExecScope(t)

	// Lay out a realistic data directory beside the workspace and make the
	// workspace's parent the writable root, so the data directory sits inside
	// what the command may write — the worst case the reviewer described.
	home := filepath.Dir(workspace)
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	gatewayDir := filepath.Join(resolvedHome, ".selfmind", "data", "gateway")
	if err := os.MkdirAll(gatewayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(gatewayDir, "local_control.token")
	db := filepath.Join(resolvedHome, ".selfmind", "data", "control.db")
	for path, content := range map[string]string{token: "daemon-secret-token", db: "SQLite format 3"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "reached") })
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	go http.Serve(tcpListener, reached)
	daemonPort := tcpListener.Addr().(*net.TCPAddr).Port

	socketDir, err := os.MkdirTemp("/tmp", "smds")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	// The daemon installs the symlink-resolved path (seatbelt matches the real
	// path); /tmp is a symlink to /private/tmp on macOS.
	if resolved, err := filepath.EvalSymlinks(socketDir); err == nil {
		socketDir = resolved
	}
	socketPath := filepath.Join(socketDir, "control.sock")
	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unixListener.Close()
	go http.Serve(unixListener, reached)

	bindTestExecutionRuntime(t, tenant, NewExecutionRuntime("", executionenv.RuntimeRoot(), sandbox.PrivateState{
		Dirs:     []string{gatewayDir},
		Files:    []string{db},
		Sockets:  []string{socketPath},
		TCPPorts: []sandbox.TCPPort{{Port: daemonPort}},
	}, nil))

	run := func(command string) (string, error) {
		return NewExecuteCommandTool().Execute(map[string]interface{}{
			"_tenant_id": tenant,
			"command":    command,
			// The widest writable root a person can trust: their home.
			"cwd":     resolvedHome,
			"sandbox": "isolated",
			"timeout": 20,
		})
	}

	// Reads of the daemon's own state are denied even inside the writable root.
	if out, err := run("cat " + token); err == nil || strings.Contains(out, "daemon-secret-token") {
		t.Fatalf("sandboxed cat of the token must fail: err=%v out=%q", err, out)
	}
	if out, err := run("cat " + db); err == nil || strings.Contains(out, "SQLite") {
		t.Fatalf("sandboxed read of control.db must fail: err=%v out=%q", err, out)
	}
	if out, err := run("ls " + gatewayDir); err == nil {
		t.Fatalf("listing the runtime directory must fail: %q", out)
	}

	// Moving the directories that hold the state aside would reach it by a name
	// no rule mentions; every one of them is pinned for a command that may write
	// the home directory.
	for _, dir := range []string{filepath.Join(resolvedHome, ".selfmind"), filepath.Join(resolvedHome, ".selfmind", "data")} {
		moved := dir + "-moved"
		out, err := run("mv " + dir + " " + moved + " && cat " + filepath.Join(moved, "control.db"))
		if err == nil || strings.Contains(out, "SQLite") {
			t.Fatalf("moving %s aside must fail: err=%v out=%q", dir, err, out)
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("%s no longer exists after a confined move: %v", dir, statErr)
		}
	}

	// A workspace file in the same tree still reads, so the denial is scoped
	// to the daemon's state, not the whole writable root.
	readme := filepath.Join(workspace, "README")
	if err := os.WriteFile(readme, []byte("workspace content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run("cat " + readme); err != nil || !strings.Contains(out, "workspace content") {
		t.Fatalf("workspace reads must keep working: err=%v out=%q", err, out)
	}

	// Connecting to the daemon's listeners is denied even with shared network.
	if _, err := os.Stat("/usr/bin/curl"); err == nil {
		network := func(args string) (string, error) {
			return NewExecuteCommandTool().Execute(map[string]interface{}{
				"_tenant_id": tenant, "command": "/usr/bin/curl -sS -m 3 " + args,
				"cwd": resolvedHome, "sandbox": "isolated", "_network_shared": true, "timeout": 20,
			})
		}
		for _, target := range []string{
			fmt.Sprintf("http://127.0.0.1:%d/", daemonPort),
			"--unix-socket " + socketPath + " http://daemon/",
		} {
			if out, err := network(target); err == nil || strings.Contains(out, "reached") {
				t.Fatalf("a confined client reached the daemon listener %s: err=%v out=%q", target, err, out)
			}
		}
	}
}
