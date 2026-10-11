package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

// WorkersEnv overrides the daemon's agent worker count.
const WorkersEnv = "SELFMIND_WORKERS"

// maxAgentWorkers caps an explicit SELFMIND_WORKERS value.
const maxAgentWorkers = 16

// ResolveWorkerCount returns how many agent workers the daemon starts for the
// per-person work Run capacity. Unset (or blank), it is one worker per
// admitted Run. An explicit value must be a whole number no lower than the
// capacity; values above the cap, however large, are reduced to it. The
// capacity's own range is checked where admission is configured.
func ResolveWorkerCount(capacity int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(WorkersEnv))
	if raw == "" {
		return min(max(capacity, 1), maxAgentWorkers), nil
	}
	count, err := strconv.Atoi(raw)
	if errors.Is(err, strconv.ErrRange) && unsignedDecimal(raw) {
		// Too large for an int is still a whole number above the cap.
		count, err = maxAgentWorkers, nil
	}
	if err != nil || count < 1 {
		return 0, fmt.Errorf("%s=%q is not a positive whole number of agent workers; unset it to start one worker per gateway.max_active_work_runs (%d), or set a whole number from %d to %d", WorkersEnv, raw, capacity, max(capacity, 1), maxAgentWorkers)
	}
	count = min(count, maxAgentWorkers)
	if count < capacity {
		return 0, fmt.Errorf("%s=%s is below gateway.max_active_work_runs=%d: every admitted Run needs its own agent worker; unset %s, raise it to at least %d, or lower gateway.max_active_work_runs", WorkersEnv, raw, capacity, WorkersEnv, capacity)
	}
	return count, nil
}

// unsignedDecimal reports whether s is decimal digits with an optional plus
// sign. Atoi reports an overflow before it reads a malformed tail, so an
// out-of-range error alone does not show that the value was a number.
func unsignedDecimal(s string) bool {
	digits := strings.TrimPrefix(s, "+")
	return digits != "" && strings.Trim(digits, "0123456789") == ""
}

// AgentWorker is one single-turn agent and the tool dispatcher it owns.
type AgentWorker struct {
	Agent      *kernel.Agent
	Dispatcher *tools.Dispatcher
}

// buildAgentWorker constructs one complete worker: agent, tools, the cron tool
// and the durable background review queue. Every worker, including the
// primary, is built here so none can lack a part the others have.
func buildAgentWorker(opts GatewayOptions, cronSched *cron.Scheduler) (AgentWorker, error) {
	agent, err := InitAgent(opts.Memory, opts.Config, opts.TenantID, opts.Prompts, opts.Control, opts.RequestGate)
	if err != nil {
		return AgentWorker{}, err
	}
	dispatcher, err := InitTools(opts.Memory, opts.Config, agent, opts.TenantID, opts.Prompts, opts.Control, opts.RequestGate)
	if err != nil {
		return AgentWorker{}, err
	}
	registerCronTool(dispatcher, cronSched, opts.Control)
	if err := dispatcher.ValidateInternalToolSchemas(); err != nil {
		return AgentWorker{}, fmt.Errorf("validate registered tool schemas: %w", err)
	}
	agent.SetBackend(dispatcher)
	installDurableReviewQueue(agent, opts.Control, opts.TenantID)
	return AgentWorker{Agent: agent, Dispatcher: dispatcher}, nil
}

// installDurableReviewQueue hands this worker's background reviews to the
// daemon maintenance queue, keyed by the payload hash. A review that is
// already queued is handled by that job; only a failed write leaves the
// engine's in-process fallback.
func installDurableReviewQueue(agent *kernel.Agent, store *control.Store, tenantID string) {
	if agent == nil || agent.ReviewEngine == nil || store == nil {
		return
	}
	agent.ReviewEngine.SetEnqueue(func(_ string, payloadJSON string) bool {
		digest := sha256.Sum256([]byte(payloadJSON))
		key := "skillreview_" + hex.EncodeToString(digest[:12])
		if _, err := store.EnqueueMaintenanceJob(context.Background(), tenantID, key, httpapi.SkillReviewJobVersion, payloadJSON); err != nil {
			log.Warn("background review: durable enqueue failed", "error", err)
			return false
		}
		return true
	})
}

// validateIdenticalWorkers is the startup proof that the pool is homogeneous:
// any worker may run any Run, so each must have the primary's context budget
// and tool catalogue. MCP catalogue changes and Skill tool reloads are held
// while the catalogues are compared, so neither can split them mid-check.
func validateIdenticalWorkers(workers []AgentWorker, mcp *tools.MCPToolManager, skills *tools.SkillToolManager) error {
	if len(workers) < 2 {
		return nil
	}
	agents := make([]*kernel.Agent, 0, len(workers)-1)
	for _, worker := range workers[1:] {
		agents = append(agents, worker.Agent)
	}
	if err := validateWorkerRuntimeContextBudgets(workers[0].Agent.RuntimeContextBudget(), agents); err != nil {
		return err
	}
	var err error
	mcp.HoldCatalog(func() {
		skills.HoldCatalog(func() {
			primary := workerCatalog(workers[0].Dispatcher)
			for index, worker := range workers[1:] {
				if diff := describeCatalogDifference(primary, workerCatalog(worker.Dispatcher)); diff != "" {
					err = fmt.Errorf("agent worker %d tool catalogue differs from the primary agent (%s); every worker must come from the same construction path, so startup stops instead of serving Runs with different tools", index+2, diff)
					return
				}
			}
		})
	})
	return err
}

func validateWorkerRuntimeContextBudgets(primary kernel.RuntimeContextBudget, workers []*kernel.Agent) error {
	for index, worker := range workers {
		budget := worker.RuntimeContextBudget()
		if budget != primary {
			return fmt.Errorf("worker %d runtime context budget differs from the primary agent: primary=%+v worker=%+v", index+2, primary, budget)
		}
	}
	return nil
}

// workerCatalog maps each tool the dispatcher offers to its complete
// definition: name, description, parameter schema and policy metadata.
func workerCatalog(dispatcher *tools.Dispatcher) map[string]string {
	catalog := make(map[string]string)
	for _, definition := range dispatcher.GetToolDefinitions() {
		function, _ := definition["function"].(map[string]interface{})
		name, _ := function["name"].(string)
		encoded, err := json.Marshal(definition)
		if err != nil {
			encoded = []byte(err.Error())
		}
		catalog[name] = string(encoded)
	}
	return catalog
}

func describeCatalogDifference(primary, worker map[string]string) string {
	var missing, unexpected, changed []string
	for name, definition := range primary {
		other, ok := worker[name]
		switch {
		case !ok:
			missing = append(missing, name)
		case other != definition:
			changed = append(changed, name)
		}
	}
	for name := range worker {
		if _, ok := primary[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	var parts []string
	for _, group := range []struct {
		label string
		names []string
	}{{"missing", missing}, {"unexpected", unexpected}, {"definition differs", changed}} {
		if len(group.names) > 0 {
			sort.Strings(group.names)
			parts = append(parts, group.label+": "+strings.Join(group.names, ", "))
		}
	}
	return strings.Join(parts, "; ")
}
