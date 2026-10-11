package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// runtimeFacts are the daemon-runtime observations behind the multi-Run
// predicates: the workers' tool catalogues, MCP stub processes, worker leases,
// and the person's Runs, queue rows and turn choices. Each fact is collected
// only when a predicate reads it.
type runtimeFacts struct {
	catalogs  []map[string]string
	mcpStarts func(server string) (int, error)
	leases    []workerLease
	toolCalls func(tool string) (int, error)
	runs      []control.RunDigest
	queued    map[string]int
	choices   map[string]string
	outbound  map[string]int
	err       map[string]error
}

// runtimePredicateTargets are the predicate targets that read runtimeFacts.
var runtimePredicateTargets = map[string]bool{
	"workers": true, "worker_tool": true, "worker_catalogs": true, "mcp_server_starts": true,
	"worker_leases": true, "leased_workers": true, "worker_overlap": true, "worker_tool_calls": true,
	"runs": true, "queued": true, "turn_choices": true, "outbound": true,
}

// queueStatuses are the durable queue row states counted by `queued`.
var queueStatuses = []string{
	control.QueueStatusQueued, control.QueueStatusStarted, control.QueueStatusDone,
	control.QueueStatusFailed, control.QueueStatusCancelled,
}

// worldState snapshots the world for preds, adding the runtime facts they read.
func (h *runtimeHarness) worldState(ctx context.Context, identity *control.IdentityContext, taskID, runID, workspace string, preds []StatePredicate) WorldState {
	world := CollectWorldState(ctx, h.controlStore, h.mem, identity, taskID, runID, workspace)
	world.runtime = h.collectRuntimeFacts(ctx, identity, preds)
	return world
}

// collectRuntimeFacts gathers what preds read for the person of identity.
func (h *runtimeHarness) collectRuntimeFacts(ctx context.Context, identity *control.IdentityContext, preds []StatePredicate) *runtimeFacts {
	needs := map[string]bool{}
	for _, p := range preds {
		on := strings.ToLower(strings.TrimSpace(p.On))
		if runtimePredicateTargets[on] {
			needs[on] = true
		}
	}
	if len(needs) == 0 {
		return nil
	}
	facts := &runtimeFacts{err: map[string]error{}}
	if needs["workers"] || needs["worker_tool"] || needs["worker_catalogs"] || needs["worker_tool_calls"] {
		for _, worker := range h.gateway.Workers {
			facts.catalogs = append(facts.catalogs, toolCatalog(worker.Dispatcher))
		}
	}
	facts.mcpStarts = func(server string) (int, error) { return countMCPStubStarts(h.stubDir, server) }
	facts.leases = h.leases.snapshot()
	facts.toolCalls = func(tool string) (int, error) { return h.workersThatCompleted(ctx, facts.leases, tool) }
	if identity == nil {
		// A count of zero would pass a count_lte assertion vacuously.
		for _, on := range []string{"runs", "queued", "turn_choices", "outbound"} {
			if needs[on] {
				facts.err[on] = fmt.Errorf("the turn resolved no person to count %s for", on)
			}
		}
		return facts
	}
	if needs["runs"] {
		facts.runs, facts.err["runs"] = h.controlStore.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, 100)
	}
	if needs["outbound"] {
		facts.outbound, facts.err["outbound"] = h.controlStore.CountOutboundByStatusSince(ctx, identity.TenantID, identity.PersonID, time.Unix(0, 0))
	}
	if needs["queued"] {
		facts.queued = map[string]int{}
		for _, status := range queueStatuses {
			count, err := h.controlStore.CountQueued(ctx, identity.TenantID, identity.PersonID, status)
			if err != nil {
				facts.err["queued"] = err
				break
			}
			facts.queued[status] = count
		}
	}
	if needs["turn_choices"] {
		facts.choices = map[string]string{}
		for id, personID := range h.choices {
			if personID != identity.PersonID {
				continue
			}
			status := "pending"
			if _, err := h.controlStore.PeekPendingTurnChoice(ctx, identity.TenantID, identity.PersonID, id, time.Now()); err != nil {
				if !errors.Is(err, control.ErrTurnChoiceNotFound) {
					facts.err["turn_choices"] = err
					break
				}
				status = "resolved"
			}
			facts.choices[id] = status
		}
	}
	return facts
}

// toolCatalog maps each tool a worker's dispatcher registers to its complete
// definition, the same comparison the daemon makes before it serves Runs.
func toolCatalog(dispatcher *tools.Dispatcher) map[string]string {
	catalog := map[string]string{}
	if dispatcher == nil {
		return catalog
	}
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

// optionalWorkerTools are built-in tools that a worker registers only when the
// case's runtime enables them, so a count may name one the case left off.
var optionalWorkerTools = []string{"cron"}

// knownWorkerTool refuses a tool name that no worker registers and the runtime
// could not add: a mistyped name would count zero and pass a negative
// assertion. Built-in names are checked against the workers' actual
// catalogues, so the list cannot drift from the daemon.
func knownWorkerTool(tool string, catalogs []map[string]string) error {
	if containsString(optionalWorkerTools, tool) {
		return nil
	}
	for _, catalog := range catalogs {
		if _, ok := catalog[tool]; ok {
			return nil
		}
	}
	return fmt.Errorf("no agent worker registers a tool named %q", tool)
}

// resolveToolName turns `mcp:<server>/<tool>` into the registered name of an
// MCP tool and leaves any other name unchanged.
func resolveToolName(target string) string {
	target = strings.TrimSpace(target)
	if rest, ok := strings.CutPrefix(target, "mcp:"); ok {
		if server, tool, found := strings.Cut(rest, "/"); found {
			return tools.MCPToolLocalName(server, tool)
		}
	}
	return target
}

// workersThatCompleted counts the distinct workers on which a leased Run
// completed a call to tool without an error.
func (h *runtimeHarness) workersThatCompleted(ctx context.Context, leases []workerLease, tool string) (int, error) {
	workers := map[int]bool{}
	for _, lease := range leases {
		if lease.runID == "" {
			continue
		}
		run, err := h.controlStore.GetRun(ctx, h.tenantID, lease.runID)
		if err != nil {
			return 0, err
		}
		if run == nil {
			continue
		}
		events, err := subjectTaskEvents(ctx, h.controlStore, run.TaskID)
		if err != nil {
			return 0, err
		}
		for _, event := range events {
			if event.RunID != run.ID || event.Type != "tool.completed" {
				continue
			}
			var payload struct {
				Tool  string `json:"tool"`
				Error string `json:"error"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Tool == tool && payload.Error == "" {
				workers[lease.worker] = true
			}
		}
	}
	return len(workers), nil
}

// evalRuntimePredicate answers the multi-Run predicates. handled is false for
// any other target.
func evalRuntimePredicate(p StatePredicate, facts *runtimeFacts) (handled, ok bool, message string) {
	on := strings.ToLower(strings.TrimSpace(p.On))
	if !runtimePredicateTargets[on] {
		return false, false, ""
	}
	if facts == nil {
		return true, false, "runtime facts were not collected"
	}
	if err := facts.err[on]; err != nil {
		return true, false, err.Error()
	}
	count, err := runtimeCount(on, p, facts)
	if err != nil {
		return true, false, err.Error()
	}
	ok, message = evalCount(p, count)
	return true, ok, message
}

func runtimeCount(on string, p StatePredicate, facts *runtimeFacts) (int, error) {
	status := strings.ToLower(strings.TrimSpace(p.Status))
	switch on {
	case "workers":
		return len(facts.catalogs), nil
	case "worker_tool":
		tool := resolveToolName(p.Target)
		if tool == "" {
			return 0, fmt.Errorf("worker_tool needs target: <tool> or mcp:<server>/<tool>")
		}
		if err := knownWorkerTool(tool, facts.catalogs); err != nil {
			return 0, err
		}
		offering := 0
		for _, catalog := range facts.catalogs {
			if _, ok := catalog[tool]; ok {
				offering++
			}
		}
		return offering, nil
	case "worker_catalogs":
		distinct := map[string]bool{}
		for _, catalog := range facts.catalogs {
			names := make([]string, 0, len(catalog))
			for name := range catalog {
				names = append(names, name)
			}
			sort.Strings(names)
			var key strings.Builder
			for _, name := range names {
				key.WriteString(catalog[name])
				key.WriteByte('\n')
			}
			distinct[key.String()] = true
		}
		return len(distinct), nil
	case "mcp_server_starts":
		if strings.TrimSpace(p.Target) == "" {
			return 0, fmt.Errorf("mcp_server_starts needs target: <runtime.mcp_stubs name>")
		}
		return facts.mcpStarts(strings.TrimSpace(p.Target))
	case "worker_leases":
		return len(facts.leases), nil
	case "leased_workers":
		return leasedWorkers(facts.leases), nil
	case "worker_overlap":
		return peakLeaseOverlap(facts.leases), nil
	case "worker_tool_calls":
		tool := resolveToolName(p.Target)
		if tool == "" {
			return 0, fmt.Errorf("worker_tool_calls needs target: <tool> or mcp:<server>/<tool>")
		}
		if err := knownWorkerTool(tool, facts.catalogs); err != nil {
			return 0, err
		}
		return facts.toolCalls(tool)
	case "runs":
		count := 0
		for _, run := range facts.runs {
			if status == "" || strings.EqualFold(run.Status, status) {
				count++
			}
		}
		return count, nil
	case "queued":
		if status != "" {
			return facts.queued[status], nil
		}
		total := 0
		for _, count := range facts.queued {
			total += count
		}
		return total, nil
	case "outbound":
		if status != "" {
			return facts.outbound[status], nil
		}
		total := 0
		for _, count := range facts.outbound {
			total += count
		}
		return total, nil
	case "turn_choices":
		count := 0
		for _, choice := range facts.choices {
			if status == "" || choice == status {
				count++
			}
		}
		return count, nil
	}
	return 0, fmt.Errorf("unknown runtime predicate %s", on)
}
