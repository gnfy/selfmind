//go:build darwin

package sandbox

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startUnder runs a long sleep under policy (or unsandboxed when wrap is
// false) and returns its process, killed at cleanup.
func startUnder(t *testing.T, policy Policy, wrap bool) *exec.Cmd {
	t.Helper()
	argv := []string{"/bin/sleep", "30"}
	if wrap {
		var ok bool
		if argv, ok = WrapSeatbelt(policy, argv); !ok {
			t.Fatal("policy must wrap on a host with sandbox-exec")
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// The canary finds exactly the processes still running under one policy:
// not this unsandboxed test process, not an unsandboxed sleep, not a process
// under another policy with its own canary — and it finds one that has left
// its process group for a session of its own.
func TestCanaryFindsOnlyTheProcessesUnderItsPolicy(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	canary := NewCanary()
	other := NewCanary()
	mine := startUnder(t, Policy{Canary: canary}, true)
	theirs := startUnder(t, Policy{Canary: other}, true)
	plain := startUnder(t, Policy{}, false)
	// A sandboxed process that left its process group for its own session.
	detached := exec.Command("/bin/sh", "-c", "exec /bin/sleep 30")
	if argv, ok := WrapSeatbelt(Policy{Canary: canary}, []string{"/bin/sleep", "30"}); ok {
		detached = exec.Command(argv[0], argv[1:]...)
	}
	detached.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := detached.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = detached.Process.Kill()
		_ = detached.Wait()
	})
	// sandbox-exec applies the profile and then execs the command; wait until
	// both have done so.
	deadline := time.Now().Add(5 * time.Second)
	var found []int
	for time.Now().Before(deadline) {
		var err error
		if found, err = ProcessesUnderCanary(canary); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(found, mine.Process.Pid) && slices.Contains(found, detached.Process.Pid) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range []int{mine.Process.Pid, detached.Process.Pid} {
		if !slices.Contains(found, pid) {
			t.Fatalf("a process under the canary's policy was not found: %d not in %v", pid, found)
		}
	}
	for name, pid := range map[string]int{"this test": os.Getpid(), "unsandboxed": plain.Process.Pid, "other policy": theirs.Process.Pid} {
		if slices.Contains(found, pid) {
			t.Fatalf("%s process %d was taken for one under the canary: %v", name, pid, found)
		}
	}
	killed, err := StopProcessesUnderCanary(canary)
	if err != nil || len(killed) != 2 {
		t.Fatalf("stop: killed %v err %v", killed, err)
	}
	for _, cmd := range []*exec.Cmd{mine, detached} {
		if err := cmd.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
			t.Fatalf("process %d was not killed: %v", cmd.Process.Pid, err)
		}
	}
	// The constraint that must change the result: the other policy's
	// process and the unsandboxed one are still running.
	for _, cmd := range []*exec.Cmd{theirs, plain} {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("process %d outside the canary's policy was stopped: %v", cmd.Process.Pid, err)
		}
	}
}
