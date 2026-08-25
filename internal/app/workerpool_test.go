package app

import (
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

func TestWorkerCountParsesEnv(t *testing.T) {
	cases := map[string]int{
		"":    1,
		"1":   1,
		"4":   4,
		"16":  16,
		"99":  16, // capped
		"0":   1,  // invalid → default
		"-3":  1,
		"abc": 1,
		" 3 ": 3,
	}
	for in, want := range cases {
		t.Setenv("SELFMIND_WORKERS", in)
		if got := workerCount(); got != want {
			t.Fatalf("workerCount(%q) = %d, want %d", in, got, want)
		}
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
