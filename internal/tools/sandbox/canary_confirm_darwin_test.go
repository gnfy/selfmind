//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

// Item 2: only a canary NewCanary could have made is ever looked up or acted
// on; a marker someone else wrote names nothing to stop.
func TestCanaryAcceptsOnlyTheGeneratedForm(t *testing.T) {
	if !ValidCanary(NewCanary()) || NewCanary() == NewCanary() {
		t.Fatal("NewCanary must make a fresh canary of the generated form")
	}
	for _, other := range []string{"", "com.selfmind.exec.0123abcd", "com.selfmind.exec.0123456789ABCDEF01234567",
		"com.apple.Safari", "com.selfmind.exec.0123456789abcdef01234567.decoy", "../com.selfmind.exec.0123456789abcdef01234567"} {
		if ValidCanary(other) {
			t.Fatalf("%q must not pass for a canary", other)
		}
		if _, err := ProcessesUnderCanary(other); err == nil && other != "" {
			t.Fatalf("looking up processes under %q must be refused", other)
		}
		if _, err := StopProcessesUnderCanary(other); err == nil && other != "" {
			t.Fatalf("stopping processes under %q must be refused", other)
		}
	}
}

// Item 3: a per-process check that fails looks the same as "not under the
// canary", so stopping is confirmed by a known process the check must find —
// a sandboxed probe under the same canary — and a check that cannot answer
// for a process fails closed instead of claiming nothing was left.
func TestStoppingFailsClosedWhenTheCheckCannotConfirm(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	for _, tc := range []struct {
		name  string
		check func(pid int, operation, name string) (bool, error)
	}{
		// A check that never finds anything under the canary.
		{"finds nothing", func(pid int, operation, name string) (bool, error) {
			return pid == os.Getpid(), nil
		}},
		// A check that cannot answer for anyone but this process.
		{"cannot answer", func(pid int, operation, name string) (bool, error) {
			if pid == os.Getpid() {
				return true, nil
			}
			return false, syscall.EPERM
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := checkCanaryName
			checkCanaryName = tc.check
			t.Cleanup(func() { checkCanaryName = previous })
			if _, err := StopProcessesUnderCanary(NewCanary()); err == nil || !strings.Contains(err.Error(), "could not confirm") {
				t.Fatalf("an unconfirmed stop must fail closed, got %v", err)
			}
		})
	}
	// The constraint that must change the result: the real check confirms.
	if killed, err := StopProcessesUnderCanary(NewCanary()); err != nil || len(killed) != 0 {
		t.Fatalf("a confirmed stop with nothing left must succeed: %v %v", killed, err)
	}
}

// Item 4: where stopping leftovers has not been verified (darwin/amd64 has
// never run it), nothing is killed and the caller is told so.
func TestLeftoverStopIsOffWhereItIsUnverified(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	canary := NewCanary()
	leftover := startUnder(t, Policy{Canary: canary}, true)
	previous := leftoverStopSupported
	leftoverStopSupported = false
	t.Cleanup(func() { leftoverStopSupported = previous })
	killed, err := StopProcessesUnderCanary(canary)
	if !errors.Is(err, ErrLeftoverStopUnverified) || len(killed) != 0 {
		t.Fatalf("an unverified platform must not stop anything: %v %v", killed, err)
	}
	if err := leftover.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("a process was stopped on an unverified platform: %v", err)
	}
}
