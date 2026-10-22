//go:build !windows

package tools

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
)

// recordedGroups stands in for the daemon's process-group tracker.
type recordedGroups struct {
	mu    sync.Mutex
	pgids []int
}

func (r *recordedGroups) Track(pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pgids = append(r.pgids, pgid)
}

func (r *recordedGroups) has(pgid int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, seen := range r.pgids {
		if seen == pgid {
			return true
		}
	}
	return false
}

func (r *recordedGroups) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pgids)
}

// The daemon refuses a control-socket connection from any process group it
// started for a tool, which still names a detached descendant after its parent
// exits. That only holds if every way a tool command starts reports its group.
func TestEveryToolCommandReportsItsProcessGroupToTheDaemon(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("process groups are a Unix concept")
	}
	recorder := &recordedGroups{}
	tenant, workspace := scratchExecScope(t)
	bindTestExecutionRuntime(t, tenant, NewExecutionRuntime("", executionenv.RuntimeRoot(), sandbox.PrivateState{}, recorder))

	// Foreground host command: the shell leads its group, so the group id is
	// the shell's own pid, which the command prints.
	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant, "command": "echo $$", "cwd": workspace, "sandbox": "host", "timeout": 20,
	})
	if err != nil {
		t.Fatalf("foreground command failed: %v (%s)", err, out)
	}
	shell, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("unexpected output %q", out)
	}
	if !recorder.has(shell) {
		t.Fatalf("the foreground command's group %d was not reported", shell)
	}
	if shell == syscall.Getpgrp() {
		t.Fatal("a tool command must lead its own group, never share the daemon's")
	}

	// Background host command: the same, through the process registry.
	args := map[string]interface{}{
		"_tenant_id": tenant, "command": "sleep 5", "cwd": workspace, "sandbox": "host", "background": true,
	}
	before := recorder.count()
	started, err := NewExecuteCommandTool().Execute(args)
	if err != nil || !strings.Contains(started, "Started background process") {
		t.Fatalf("background start failed: %v (%s)", err, started)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(started, "\n", 2)[0], "Started background process with ID:"))
	registry := ProcessRegistryForArgs(map[string]interface{}{"_tenant_id": tenant})
	registry.mu.RLock()
	process := registry.processes[id]
	registry.mu.RUnlock()
	if process == nil || process.Cmd.Process == nil {
		t.Fatalf("background process %q is not registered", id)
	}
	t.Cleanup(func() { _ = registry.Kill(id) })
	if recorder.count() != before+1 || !recorder.has(process.Cmd.Process.Pid) {
		t.Fatalf("the background command's group %d was not reported (%d new groups)",
			process.Cmd.Process.Pid, recorder.count()-before)
	}
}

// An isolated command is wrapped (sandbox-exec, bwrap); the wrapper leads the
// group and every descendant inherits it, so the group is reported the same way.
func TestIsolatedToolCommandReportsItsProcessGroupToTheDaemon(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("no isolation backend on this host")
	}
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(true, false, false)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })
	recorder := &recordedGroups{}
	tenant, workspace := scratchExecScope(t)
	bindTestExecutionRuntime(t, tenant, NewExecutionRuntime("", executionenv.RuntimeRoot(), sandbox.PrivateState{}, recorder))

	if out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant, "command": "echo isolated", "cwd": workspace, "sandbox": "isolated", "timeout": 20,
	}); err != nil || !strings.Contains(out, "isolated") {
		t.Fatalf("isolated command failed: %v (%s)", err, out)
	}
	if recorder.count() != 1 {
		t.Fatalf("an isolated command must report exactly its own group, got %d", recorder.count())
	}
	if recorder.has(syscall.Getpgrp()) {
		t.Fatal("the daemon's own group must never be recorded")
	}
}

// The installed private state is copied on the way in and out, so neither the
// installer nor a backend can edit what the daemon owns.
func TestDaemonPrivateStateIsCopiedAtTheBoundary(t *testing.T) {
	dirs := []string{"/data/gateway"}
	prefixes := []string{"/home/u/.selfmind/config.yaml."}
	links := []string{"/home/u/.selfmind"}
	r := NewExecutionRuntime("", "", sandbox.PrivateState{Dirs: dirs, FilePrefixes: prefixes, Symlinks: links, TCPPorts: []sandbox.TCPPort{{Port: 8765}}}, nil)
	dirs[0] = "/elsewhere"
	prefixes[0] = "/elsewhere."
	links[0] = "/elsewhere"
	got := r.PrivateState()
	if got.Dirs[0] != "/data/gateway" || got.FilePrefixes[0] != "/home/u/.selfmind/config.yaml." || got.Symlinks[0] != "/home/u/.selfmind" {
		t.Fatalf("the installer's slice leaked into the daemon's state: %v %v %v", got.Dirs, got.FilePrefixes, got.Symlinks)
	}
	got.Dirs[0] = "/tampered"
	got.FilePrefixes[0] = "/tampered."
	got.Symlinks[0] = "/tampered"
	got.TCPPorts[0].Port = 1
	again := r.PrivateState()
	if again.Dirs[0] != "/data/gateway" || again.FilePrefixes[0] != "/home/u/.selfmind/config.yaml." || again.Symlinks[0] != "/home/u/.selfmind" || again.TCPPorts[0].Port != 8765 {
		t.Fatalf("a caller edited the daemon's state through a returned copy: %+v", again)
	}
}

// Bubblewrap cannot create a mount point under the read-only root, so entries
// that do not exist are dropped rather than aborting the command; a missing
// credentials file, for one, is an ordinary state.
func TestExistingPrivateStateDropsWhatBubblewrapCouldNotMount(t *testing.T) {
	root := t.TempDir()
	present := root + "/gateway"
	if err := os.MkdirAll(present, 0o700); err != nil {
		t.Fatal(err)
	}
	file := root + "/control.db"
	if err := os.WriteFile(file, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := existingPrivateState(sandbox.PrivateState{
		Dirs:  []string{present, root + "/missing-dir"},
		Files: []string{file, root + "/auth.json"},
		// A prefix is matched against the directory when the command starts, so
		// one whose directory is gone costs nothing and is kept.
		FilePrefixes: []string{root + "/config.yaml.", root + "/no-such-dir/config.yaml."},
		Sockets:      []string{present + "/control.sock", root + "/no-such-dir/control.sock"},
		TCPPorts:     []sandbox.TCPPort{{Port: 8765}},
	})
	if len(state.Dirs) != 1 || state.Dirs[0] != present || len(state.Files) != 1 || state.Files[0] != file ||
		len(state.FilePrefixes) != 2 ||
		len(state.Sockets) != 1 || state.Sockets[0] != present+"/control.sock" || len(state.TCPPorts) != 1 {
		t.Fatalf("existing private state = %+v", state)
	}
}
