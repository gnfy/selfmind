package app

import (
	"os"
	"strconv"
	"strings"

	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
)

// workerCount reads SELFMIND_WORKERS (default 1). 1 keeps the single-agent
// serialized path unchanged; >1 enables the worker pool. Capped to a sane max.
func workerCount() int {
	v := strings.TrimSpace(os.Getenv("SELFMIND_WORKERS"))
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 1
	}
	if n > 16 {
		n = 16
	}
	return n
}

// MaybeEnableWorkerPool turns on multi-worker execution on the gateway when
// SELFMIND_WORKERS>1. It builds N-1 fully independent worker agents (each its
// own InitAgent + InitTools, sharing only the concurrency-safe memory/skill
// stores and the process-global auth manager) and hands them to the gateway.
// A no-op at the default (N=1), so the default path is unchanged.
func MaybeEnableWorkerPool(gw *router.Gateway, mem *memory.MemoryManager, cfg *config.Config, skillStore *kernel.SkillStore, tenantID string) (int, error) {
	n := workerCount()
	if gw == nil || n <= 1 {
		return 1, nil
	}
	extra := make([]*kernel.Agent, 0, n-1)
	for i := 1; i < n; i++ {
		a, err := InitAgent(mem, cfg, tenantID)
		if err != nil {
			return 1 + len(extra), err
		}
		d, err := InitTools(mem, cfg, a, skillStore, tenantID)
		if err != nil {
			return 1 + len(extra), err
		}
		a.SetBackend(d)
		extra = append(extra, a)
	}
	gw.EnableWorkerPool(extra)
	return 1 + len(extra), nil
}
