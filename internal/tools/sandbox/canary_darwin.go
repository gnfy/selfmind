//go:build darwin

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A command's seatbelt profile may carry a canary: a unique mach global name
// that only this profile lets a process look up. No service is registered
// under it, so allowing the lookup grants nothing. It identifies the processes
// still running under that one profile after the command returns — one that
// left the command's process group (setsid, a daemon) included — because the
// kernel answers sandbox_check for any process of this user.

//go:cgo_import_dynamic libc_sandbox_check sandbox_check "/usr/lib/libSystem.B.dylib"

var libc_sandbox_check_trampoline_addr uintptr

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

const (
	// sandboxFilterGlobalName is SANDBOX_FILTER_GLOBAL_NAME: the variadic
	// argument is a mach global name, the filter mach-lookup takes.
	sandboxFilterGlobalName = 2
	// sandboxCheckNoReport is SANDBOX_CHECK_NO_REPORT: a denied check is
	// not logged as a violation.
	sandboxCheckNoReport = 0x40000000
	// processZombie is SZOMB, the p_stat of an exited, unreaped process.
	processZombie = 5
)

// checkCanaryName and leftoverStopSupported are replaced only by tests.
var (
	checkCanaryName = checkGlobalName
	// Stopping leftovers has run only on arm64; amd64 is built and vetted
	// but has never been exercised, so it is not attempted there.
	leftoverStopSupported = runtime.GOARCH == "arm64"
)

// checkGlobalName asks the kernel whether pid's sandbox allows operation on a
// global name. sandbox_check is variadic; the runtime's six-argument libc call
// also passes the fourth argument on the stack, where arm64 darwin reads a
// variadic argument.
func checkGlobalName(pid int, operation, name string) (allowed bool, err error) {
	op, err := syscall.BytePtrFromString(operation)
	if err != nil {
		return false, err
	}
	global, err := syscall.BytePtrFromString(name)
	if err != nil {
		return false, err
	}
	r1, _, errno := syscall_syscall6(libc_sandbox_check_trampoline_addr, uintptr(pid),
		uintptr(unsafe.Pointer(op)), uintptr(sandboxFilterGlobalName|sandboxCheckNoReport),
		uintptr(unsafe.Pointer(global)), 0, 0)
	runtime.KeepAlive(op)
	runtime.KeepAlive(global)
	switch int32(r1) {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("sandbox_check %d: %w", pid, errno)
	}
}

// underCanary reports whether pid runs under the profile that carries
// canary: it may look the canary up but not the decoy beside it. An
// unsandboxed process may look up both, and any other profile cannot look up
// the canary, which no other profile names.
func underCanary(pid int, canary string) (bool, error) {
	allowed, err := checkCanaryName(pid, "mach-lookup", canary)
	if err != nil || !allowed {
		return false, err
	}
	decoy, err := checkCanaryName(pid, "mach-lookup", canary+".decoy")
	if err != nil {
		return false, err
	}
	return !decoy, nil
}

// ProcessesUnderCanary lists this user's processes still running under the
// profile that carries canary. A process the check cannot answer for is not
// taken for one outside the canary: the listing then fails.
func ProcessesUnderCanary(canary string) ([]int, error) {
	if !ValidCanary(canary) {
		return nil, fmt.Errorf("%q is not a canary this runtime made", canary)
	}
	// A check that cannot answer for this process cannot answer for any:
	// say so rather than find nothing.
	if _, err := checkCanaryName(os.Getpid(), "mach-lookup", canary); err != nil {
		return nil, fmt.Errorf("sandbox_check is unusable on this host: %w", err)
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	self := os.Getpid()
	var found []int
	unanswered := 0
	for _, proc := range procs {
		pid := int(proc.Proc.P_pid)
		// A zombie has exited and can no longer act; its parent has not
		// reaped it yet.
		if pid <= 1 || pid == self || proc.Proc.P_stat == processZombie {
			continue
		}
		under, err := underCanary(pid, canary)
		switch {
		case errors.Is(err, syscall.ESRCH):
			// It exited while the list was read.
		case err != nil:
			unanswered++
		case under:
			found = append(found, pid)
		}
	}
	if unanswered > 0 {
		return found, fmt.Errorf("sandbox_check could not answer for %d processes", unanswered)
	}
	return found, nil
}

// StopProcessesUnderCanary kills every process still running under the profile
// that carries canary, repeating while any remain (one may fork while the
// others die), and returns the pids it killed. It first confirms the check
// finds a process known to be under the canary — a sandboxed probe it starts
// — so a check that silently finds nothing can never pass for a clean stop;
// anything it cannot confirm is an error that says so.
func StopProcessesUnderCanary(canary string) ([]int, error) {
	if !ValidCanary(canary) {
		return nil, fmt.Errorf("%q is not a canary this runtime made", canary)
	}
	if !leftoverStopSupported {
		return nil, ErrLeftoverStopUnverified
	}
	unconfirmed := func(format string, args ...interface{}) error {
		return fmt.Errorf("could not confirm the processes left under the command's sandbox were stopped: "+format, args...)
	}
	probe, err := startCanaryProbe(canary)
	if err != nil {
		return nil, unconfirmed("start a probe: %v", err)
	}
	defer func() {
		_ = probe.Process.Kill()
		_ = probe.Wait()
	}()
	if !probeFound(probe.Process.Pid, canary) {
		return nil, unconfirmed("the check did not find a process known to be under the canary")
	}
	seen := map[int]bool{}
	var killed []int
	for pass := 0; pass < 8; pass++ {
		found, err := ProcessesUnderCanary(canary)
		if err != nil {
			return killed, unconfirmed("%v", err)
		}
		if len(found) == 0 {
			return killed, nil
		}
		for _, pid := range found {
			// Asked again right before the signal: the pid may have been
			// reused since the list was read.
			if under, err := underCanary(pid, canary); err != nil || !under {
				continue
			}
			if syscall.Kill(pid, syscall.SIGKILL) == nil && pid != probe.Process.Pid && !seen[pid] {
				seen[pid] = true
				killed = append(killed, pid)
			}
		}
	}
	if found, err := ProcessesUnderCanary(canary); err != nil || len(found) > 0 {
		return killed, unconfirmed("still running: %v %v", found, err)
	}
	return killed, nil
}

// LeftoverStopSupported reports whether StopProcessesUnderCanary stops
// anything on this platform.
func LeftoverStopSupported() bool { return leftoverStopSupported }

// startCanaryProbe runs a sleep under a policy carrying only the canary: a
// process the check must find under it.
func startCanaryProbe(canary string) (*exec.Cmd, error) {
	argv, ok := WrapSeatbelt(Policy{Canary: canary}, []string{"/bin/sleep", "30"})
	if !ok {
		return nil, errors.New("sandbox-exec cannot apply the canary policy")
	}
	probe := exec.Command(argv[0], argv[1:]...)
	if err := probe.Start(); err != nil {
		return nil, err
	}
	return probe, nil
}

// probeFound waits, briefly, until the check finds the probe under the canary
// once sandbox-exec has applied its policy.
func probeFound(pid int, canary string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if under, err := underCanary(pid, canary); err == nil && under {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
