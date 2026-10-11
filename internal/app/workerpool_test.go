package app

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

// Without SELFMIND_WORKERS the daemon starts one agent worker per
// admitted Run, so the worker count follows gateway.max_active_work_runs.
func TestResolveWorkerCountFollowsCapacityWhenUnset(t *testing.T) {
	for _, capacity := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("capacity %d", capacity), func(t *testing.T) {
			unsetWorkersEnv(t)
			got, err := ResolveWorkerCount(capacity)
			if err != nil || got != capacity {
				t.Fatalf("unset SELFMIND_WORKERS with capacity %d = %d, %v; want %d workers", capacity, got, err, capacity)
			}
			// An empty value is how service managers and shells spell
			// "not set"; it must not be read as an invalid count.
			t.Setenv(WorkersEnv, "  ")
			got, err = ResolveWorkerCount(capacity)
			if err != nil || got != capacity {
				t.Fatalf("blank SELFMIND_WORKERS with capacity %d = %d, %v; want %d workers", capacity, got, err, capacity)
			}
		})
	}
}

func TestResolveWorkerCountHonorsExplicitCountAtOrAboveCapacity(t *testing.T) {
	for _, tc := range []struct {
		value    string
		capacity int
		want     int
	}{
		{"1", 1, 1}, {"2", 2, 2}, {" 3 ", 2, 3}, {"4", 1, 4},
		{"16", 3, 16}, {"99", 2, 16}, // the existing upper cap is kept
	} {
		t.Setenv(WorkersEnv, tc.value)
		got, err := ResolveWorkerCount(tc.capacity)
		if err != nil || got != tc.want {
			t.Fatalf("SELFMIND_WORKERS=%q capacity %d = %d, %v; want %d", tc.value, tc.capacity, got, err, tc.want)
		}
	}
}

// An explicit count below the capacity cannot serve every admitted Run. The
// error must name both settings so the operator knows which two to reconcile.
func TestResolveWorkerCountRejectsExplicitCountBelowCapacity(t *testing.T) {
	for _, tc := range []struct {
		value    string
		capacity int
	}{{"1", 2}, {"2", 3}, {"1", 3}} {
		t.Setenv(WorkersEnv, tc.value)
		got, err := ResolveWorkerCount(tc.capacity)
		if err == nil {
			t.Fatalf("SELFMIND_WORKERS=%s with capacity %d was accepted as %d workers", tc.value, tc.capacity, got)
		}
		message := err.Error()
		for _, want := range []string{"SELFMIND_WORKERS=" + tc.value, "gateway.max_active_work_runs", fmt.Sprint(tc.capacity)} {
			if !strings.Contains(message, want) {
				t.Fatalf("SELFMIND_WORKERS=%s capacity %d error %q does not name %q", tc.value, tc.capacity, message, want)
			}
		}
	}
}

// A whole number too large for an int is still a whole number above the cap,
// so it is capped like 99 rather than reported as not a whole number.
func TestResolveWorkerCountCapsAnOverflowingCount(t *testing.T) {
	for _, tc := range []struct {
		value    string
		capacity int
	}{
		{"99999999999999999999", 2},
		{"18446744073709551616", 3},
		{"+99999999999999999999", 1},
	} {
		t.Setenv(WorkersEnv, tc.value)
		got, err := ResolveWorkerCount(tc.capacity)
		if err != nil || got != maxAgentWorkers {
			t.Fatalf("SELFMIND_WORKERS=%s capacity %d = %d, %v; want the cap %d like any count above it", tc.value, tc.capacity, got, err, maxAgentWorkers)
		}
	}
}

// A malformed, zero or negative count fails startup instead of silently
// becoming a default the operator never chose.
func TestResolveWorkerCountRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"0", "-3", "abc", "2.5", "two", "0x2", "-99999999999999999999", "99999999999999999999x"} {
		t.Setenv(WorkersEnv, value)
		got, err := ResolveWorkerCount(2)
		if err == nil {
			t.Fatalf("SELFMIND_WORKERS=%q was accepted as %d workers", value, got)
		}
		if !strings.Contains(err.Error(), "SELFMIND_WORKERS") || !strings.Contains(err.Error(), value) {
			t.Fatalf("SELFMIND_WORKERS=%q error %q does not name the variable and its value", value, err)
		}
	}
}

func unsetWorkersEnv(t *testing.T) {
	t.Helper()
	t.Setenv(WorkersEnv, "")
	if err := os.Unsetenv(WorkersEnv); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerPoolRequiresHomogeneousRuntimeContextBudgets(t *testing.T) {
	primary := kernel.NewAgent(nil, nil, nil, "primary", 1, 1, nil)
	primary.SetContextWindow(32 * 1024)
	same := kernel.NewAgent(nil, nil, nil, "same", 1, 1, nil)
	same.SetContextWindow(32 * 1024)
	if err := validateWorkerRuntimeContextBudgets(primary.RuntimeContextBudget(), []*kernel.Agent{same}); err != nil {
		t.Fatalf("homogeneous budgets rejected: %v", err)
	}
	different := kernel.NewAgent(nil, nil, nil, "different", 1, 1, nil)
	different.SetContextWindow(128 * 1024)
	if err := validateWorkerRuntimeContextBudgets(primary.RuntimeContextBudget(), []*kernel.Agent{same, different}); err == nil || !strings.Contains(err.Error(), "worker 3") {
		t.Fatalf("heterogeneous budgets were not rejected precisely: %v", err)
	}
}
