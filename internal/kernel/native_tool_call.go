package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"selfmind/internal/kernel/llm"
)

type toolExecutionResult struct {
	index     int
	step      string
	msg       llm.Message
	toolName  string
	signature string
	rawResult string
	success   bool
}

type toolLifecycleHandoff struct {
	Status      string
	Summary     string
	Message     string
	Done        []string
	NextSteps   []string
	Files       []string
	Tests       []string
	Risks       []string
	NeedApprove bool
}

type parallelToolSupport interface {
	SupportsParallelTool(name string) bool
}

func (a *Agent) llmToolDefinitions(ctx context.Context, strategy TaskStrategy) []llm.ToolDefinition {
	if a.backend == nil {
		return nil
	}
	return toolDefinitionsForLLM(ctx, a.backend.GetToolDefinitions(), strategy)
}

func toolDefinitionsForLLM(ctx context.Context, defs []map[string]interface{}, strategy TaskStrategy) []llm.ToolDefinition {
	out := make([]llm.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		name := toolDefinitionName(d)
		if name == "" {
			continue
		}
		if !toolDefinitionAvailable(ctx, d) {
			continue
		}
		if !strategy.AllowsTool(name) {
			continue
		}
		params := toolDefinitionParameters(d)
		if params == nil {
			params = map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			}
		}
		out = append(out, llm.ToolDefinition{
			Name:        name,
			Description: toolDefinitionDescription(d),
			Parameters:  params,
		})
	}
	return out
}

func filterToolCallsByStrategy(calls []llm.ToolCall, strategy TaskStrategy) []llm.ToolCall {
	if len(calls) == 0 {
		return calls
	}
	out := make([]llm.ToolCall, 0, len(calls))
	for _, call := range calls {
		if strategy.AllowsTool(call.Function) {
			out = append(out, call)
		}
	}
	return out
}

func filterToolCallsByStrategyAndBudget(calls []llm.ToolCall, strategy TaskStrategy, actionToolsUsed int) ([]llm.ToolCall, int) {
	calls = filterToolCallsByStrategy(calls, strategy)
	if len(calls) == 0 {
		return calls, 0
	}
	strategy = strategy.normalized()
	remaining := strategy.MaxActionTools - actionToolsUsed
	if strategy.ToolMode == ToolModeNone {
		remaining = 0
	}
	out := make([]llm.ToolCall, 0, len(calls))
	dropped := 0
	for _, call := range calls {
		if isLifecycleToolName(call.Function) {
			out = append(out, call)
			continue
		}
		if remaining <= 0 {
			dropped++
			continue
		}
		out = append(out, call)
		remaining--
	}
	return out, dropped
}

func filterToolCallsByLifecycleCaps(calls []llm.ToolCall, toolUseCounts map[string]int) ([]llm.ToolCall, int) {
	if len(calls) == 0 {
		return calls, 0
	}
	counts := make(map[string]int, len(toolUseCounts))
	for name, count := range toolUseCounts {
		counts[strings.TrimSpace(name)] = count
	}
	out := make([]llm.ToolCall, 0, len(calls))
	dropped := 0
	for _, call := range calls {
		name := strings.TrimSpace(call.Function)
		if cap := lifecycleToolCap(name); cap > 0 {
			if counts[name] >= cap {
				dropped++
				continue
			}
			counts[name]++
		}
		out = append(out, call)
	}
	return out, dropped
}

func incrementToolUseCounts(counts map[string]int, calls []llm.ToolCall) {
	for _, call := range calls {
		name := strings.TrimSpace(call.Function)
		if name != "" {
			counts[name]++
		}
	}
}

func countActionToolCalls(calls []llm.ToolCall) int {
	count := 0
	for _, call := range calls {
		if !isLifecycleToolName(call.Function) {
			count++
		}
	}
	return count
}

func actionToolBudgetReached(strategy TaskStrategy, actionToolsUsed int) bool {
	strategy = strategy.normalized()
	return strategy.ToolMode != ToolModeNone && strategy.MaxActionTools > 0 && actionToolsUsed >= strategy.MaxActionTools
}

func lifecycleToolNames() []string {
	return []string{"update_plan", "finish_run"}
}

func lifecycleToolCap(name string) int {
	switch strings.TrimSpace(name) {
	case "update_plan":
		// A visible plan is a phase-level view, not a transcript. Eight calls are
		// enough for an initial snapshot, several real transitions, and closure;
		// identical snapshots are also suppressed by PlanStore.
		return 8
	case "finish_run":
		return 1
	default:
		return 0
	}
}

func unresolvedPlanStepsFromToolCall(call llm.ToolCall) ([]string, bool) {
	if strings.TrimSpace(call.Function) != "update_plan" {
		return nil, false
	}
	args := parseToolCallArgs(call.Args)
	raw, ok := args["plan"].([]interface{})
	if !ok {
		return nil, false
	}
	unresolved := make([]string, 0, len(raw))
	for _, item := range raw {
		step, ok := item.(map[string]interface{})
		if !ok {
			return nil, false
		}
		status := strings.TrimSpace(fmt.Sprintf("%v", step["status"]))
		if status == "completed" || status == "cancelled" {
			continue
		}
		name := strings.TrimSpace(fmt.Sprintf("%v", step["step"]))
		if name == "" {
			name = "unnamed step"
		}
		unresolved = append(unresolved, name)
	}
	return unresolved, true
}

func finishRunStatusFromToolCall(call llm.ToolCall) (string, bool) {
	if strings.TrimSpace(call.Function) != "finish_run" {
		return "", false
	}
	status := strings.TrimSpace(fmt.Sprintf("%v", parseToolCallArgs(call.Args)["status"]))
	return status, status != ""
}

func isLifecycleToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case "update_plan", "finish_run":
		return true
	default:
		return false
	}
}

func legacyToolCallsToLLM(calls []ToolCall, iteration int) []llm.ToolCall {
	out := make([]llm.ToolCall, 0, len(calls))
	for i, c := range calls {
		out = append(out, llm.ToolCall{
			ID:       fmt.Sprintf("legacy-toolcall-%d-%d", iteration, i),
			Function: c.Name,
			Args:     c.Args,
		})
	}
	return out
}

func normalizeToolCallIDs(calls []llm.ToolCall, iteration int) []llm.ToolCall {
	out := make([]llm.ToolCall, len(calls))
	copy(out, calls)
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = fmt.Sprintf("toolcall-%d-%d", iteration, i)
		}
	}
	return out
}

// isolateWorkUnitBoundaryCall makes update_plan a batch boundary. A provider
// may emit update_plan and later tool calls in one assistant response, but the
// later calls belong to the newly projected work unit and must not execute
// with the old unit's Skill/tool activation state. The loop executes only the
// boundary call and asks the model to re-issue still-needed calls next turn.
func isolateWorkUnitBoundaryCall(calls []llm.ToolCall) ([]llm.ToolCall, int) {
	if len(calls) < 2 {
		return calls, 0
	}
	for _, call := range calls {
		if strings.TrimSpace(call.Function) == "update_plan" {
			return []llm.ToolCall{call}, len(calls) - 1
		}
	}
	return calls, 0
}

// isolateExternalWatchHandoffCalls makes the first watcher registration a
// lifecycle boundary. Calls before it still belong to the foreground work and
// every watcher in the same batch is retained, but later non-watcher calls must
// be re-issued only if registration fails. This lets one turn register several
// independent observations without continuing to mutate or poll afterward.
func isolateExternalWatchHandoffCalls(calls []llm.ToolCall) ([]llm.ToolCall, int) {
	if len(calls) < 2 {
		return calls, 0
	}
	firstWatch := -1
	for i, call := range calls {
		if strings.TrimSpace(call.Function) == "watch_external" {
			firstWatch = i
			break
		}
	}
	if firstWatch < 0 {
		return calls, 0
	}
	kept := append([]llm.ToolCall(nil), calls[:firstWatch]...)
	for _, call := range calls[firstWatch:] {
		if strings.TrimSpace(call.Function) == "watch_external" {
			kept = append(kept, call)
		}
	}
	return kept, len(calls) - len(kept)
}

// lifecycleHandoffFromToolResults accepts a lifecycle transition only from
// successful trusted built-in watcher results. External tools and arbitrary
// model-visible JSON can never end a run through this path.
func lifecycleHandoffFromToolResults(results []toolExecutionResult) (toolLifecycleHandoff, bool) {
	var out toolLifecycleHandoff
	registered := 0
	for _, result := range results {
		if !result.success {
			return toolLifecycleHandoff{}, false
		}
		if strings.TrimSpace(result.toolName) != "watch_external" {
			continue
		}
		var decoded struct {
			WatchID    string `json:"watch_id"`
			Registered bool   `json:"registered"`
			Message    string `json:"message"`
			Handoff    struct {
				Status      string   `json:"status"`
				Summary     string   `json:"summary"`
				Done        []string `json:"done"`
				NextSteps   []string `json:"next_steps"`
				Files       []string `json:"files"`
				Tests       []string `json:"tests"`
				Risks       []string `json:"risks"`
				NeedApprove bool     `json:"need_approve"`
			} `json:"lifecycle_handoff"`
		}
		if json.Unmarshal([]byte(result.rawResult), &decoded) != nil ||
			!decoded.Registered || strings.TrimSpace(decoded.WatchID) == "" ||
			strings.TrimSpace(decoded.Message) == "" ||
			strings.TrimSpace(decoded.Handoff.Status) != "waiting_external" ||
			strings.TrimSpace(decoded.Handoff.Summary) == "" {
			continue
		}
		registered++
		out.Status = "waiting_external"
		out.Summary = decoded.Handoff.Summary
		out.Message = decoded.Message
		out.Done = append(out.Done, decoded.Handoff.Done...)
		out.NextSteps = appendUniqueLifecycleItems(out.NextSteps, decoded.Handoff.NextSteps...)
		out.Files = appendUniqueLifecycleItems(out.Files, decoded.Handoff.Files...)
		out.Tests = appendUniqueLifecycleItems(out.Tests, decoded.Handoff.Tests...)
		out.Risks = appendUniqueLifecycleItems(out.Risks, decoded.Handoff.Risks...)
		out.NeedApprove = out.NeedApprove || decoded.Handoff.NeedApprove
	}
	if registered == 0 {
		return toolLifecycleHandoff{}, false
	}
	if registered > 1 {
		out.Summary = fmt.Sprintf("Registered %d durable external watchers.", registered)
		out.Message = fmt.Sprintf("%d watchers are running in the background. You can start another task now; SelfMind will notify you as each reaches a terminal state.", registered)
	}
	return out, true
}

func appendUniqueLifecycleItems(dst []string, items ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(items))
	for _, item := range dst {
		if item = strings.TrimSpace(item); item != "" {
			seen[item] = struct{}{}
		}
	}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		dst = append(dst, item)
	}
	return dst
}

func (a *Agent) executeToolCalls(ctx context.Context, tenantID string, eventCh chan string, calls []llm.ToolCall) []toolExecutionResult {
	results := make([]toolExecutionResult, len(calls))
	if shouldParallelizeToolCalls(calls, a.backend) {
		var wg sync.WaitGroup
		for idx, call := range calls {
			wg.Add(1)
			go func(i int, c llm.ToolCall) {
				defer wg.Done()
				results[i] = a.executeSingleToolCall(ctx, tenantID, eventCh, i, c)
			}(idx, call)
		}
		wg.Wait()
		return results
	}

	for idx, call := range calls {
		results[idx] = a.executeSingleToolCall(ctx, tenantID, eventCh, idx, call)
	}
	return results
}

func (a *Agent) executeSingleToolCall(ctx context.Context, tenantID string, eventCh chan string, idx int, call llm.ToolCall) toolExecutionResult {
	name := call.Function
	signature := strings.TrimSpace(name) + "\x00" + strings.TrimSpace(call.Args)
	if call.ID == "" {
		call.ID = fmt.Sprintf("toolcall-%d", idx)
	}
	if name == "" {
		step := "Error executing tool: missing function name"
		return toolExecutionResult{
			index:     idx,
			step:      step,
			toolName:  name,
			signature: signature,
			msg: llm.Message{
				Role:       "tool",
				Content:    step,
				ToolCallID: call.ID,
			},
		}
	}

	select {
	case <-ctx.Done():
		step := fmt.Sprintf("Error executing %s: %v", name, ctx.Err())
		return toolExecutionResult{
			index:     idx,
			step:      step,
			toolName:  name,
			signature: signature,
			msg: llm.Message{
				Role:       "tool",
				Content:    step,
				Name:       name,
				ToolCallID: call.ID,
			},
		}
	default:
	}
	// One lookup answers both questions (is it exposed, and why not), instead of
	// rebuilding the whole catalogue and then scanning it twice.
	if exposure, known := a.resolveToolExposureForDispatch(name); known {
		switch exposure {
		case "hidden":
			return a.toolDispatchRefused(eventCh, idx, call, signature,
				fmt.Errorf("tool %s is not available in the model tool surface", name))
		case "deferred":
			if !deferredToolActive(ctx, name) {
				return a.toolDispatchRefused(eventCh, idx, call, signature,
					fmt.Errorf("tool %s is deferred in this run; call tool_search to discover and activate matching capabilities before retrying", name))
			}
		}
	}

	args := parseToolCallArgs(call.Args)
	stripModelRuntimeArgs(args)
	args["_tenant_id"] = tenantID
	args["_context"] = ctx
	args["_tool_call_id"] = call.ID
	if invocationScope, ok := ToolInvocationScopeFromContext(ctx); ok {
		args["_invocation_scope"] = invocationScope
	}
	if workspace, ok := WorkspaceContextFromContext(ctx); ok && strings.TrimSpace(workspace.ID) != "" {
		// Mutation tools need the logical workspace id as well as the root used
		// by WorkspaceScopeMiddleware. Memory uses this hidden value to keep
		// project facts out of the person's global preference partition.
		args["_workspace_id"] = strings.TrimSpace(workspace.ID)
	}

	// Claim the dispatch BEFORE execution. For state-changing tools this is a
	// hard correctness boundary: if the claim cannot be persisted, or the same
	// call id was already claimed, do not execute the side effect.
	var ledgerRunID string
	retryClass := ClassifyToolRetry(name)
	if rt, ok := TaskRuntimeContextFromContext(ctx); ok {
		ledgerRunID = rt.RunID
	}
	ledger := ToolLedgerFromContext(ctx)
	if ledgerRunID != "" {
		if ledger == nil && retryClass != ToolRetryReadOnly {
			return a.toolDispatchRefused(eventCh, idx, call, signature,
				fmt.Errorf("durable tool ledger is unavailable; %s was not executed", name))
		}
		if ledger != nil {
			decision, claimErr := ledger.ClaimDispatch(ctx, ToolLedgerEntry{
				RunID:      ledgerRunID,
				ToolCallID: call.ID,
				ToolName:   name,
				ArgsHash:   ToolArgsHash(call.Args),
				RetryClass: retryClass,
			})
			if claimErr != nil && retryClass != ToolRetryReadOnly {
				return a.toolDispatchRefused(eventCh, idx, call, signature,
					fmt.Errorf("durable tool claim failed; %s was not executed: %w", name, claimErr))
			}
			if claimErr == nil && !decision.Execute {
				return a.toolDispatchRefused(eventCh, idx, call, signature,
					fmt.Errorf("tool call %s is already recorded as %s; it was not executed again; inspect current state with read-only tools before deciding the next step", call.ID, decision.Status))
			}
		}
	}

	if eventCh != nil {
		payload := map[string]interface{}{}
		if provider, ok := a.backend.(ToolExecutionMetadataProvider); ok {
			metadata := provider.ToolExecutionMetadata(name, args)
			payload["tool_origin"] = metadata.Origin
			payload["tool_category"] = metadata.Category
			payload["tool_risk_level"] = metadata.RiskLevel
			payload["tool_read_only"] = metadata.ReadOnly
			if len(metadata.OperationClasses) > 0 {
				payload["operation_classes"] = metadata.OperationClasses
			}
		}
		EmitAgentEvent(eventCh, AgentEvent{
			Type:       "tool.started",
			ToolName:   name,
			ToolCallID: call.ID,
			ToolArgs:   call.Args,
			Payload:    payload,
		})
	}

	startTime := time.Now()
	result, err := a.backend.Dispatch(name, args)
	duration := time.Since(startTime).Seconds()
	var completedMetadata []ToolExecutionMetadata
	if provider, ok := a.backend.(ToolExecutionMetadataProvider); ok {
		completedMetadata = append(completedMetadata, provider.ToolExecutionMetadata(name, args))
	}

	var ledgerErr error
	if ledgerRunID != "" && ledger != nil {
		// The execution context may be cancelled precisely because the tool was
		// stopped by /stop or the watchdog. Closing the durable ledger is cleanup,
		// not more execution; it must survive that cancellation or the call remains
		// falsely "started" and every resume treats the side effect as uncertain.
		ledgerErr = ledger.RecordOutcome(context.WithoutCancel(ctx), ledgerRunID, call.ID, err == nil)
	}

	if err != nil {
		if ledgerErr != nil {
			err = fmt.Errorf("%w; durable outcome recording also failed: %v", err, ledgerErr)
		}
		metadata := ToolExecutionMetadata{}
		if len(completedMetadata) > 0 {
			metadata = completedMetadata[0]
		}
		packaged := packageDispatchedToolFailureCtx(ctx, name, result, err, metadata)
		if eventCh != nil {
			emitToolEndEventWithDuration(eventCh, name, call.ID, packaged, duration, err, completedMetadata...)
		}
		return toolExecutionResult{
			index:     idx,
			step:      packaged.ModelContent,
			toolName:  name,
			signature: signature,
			msg: llm.Message{
				Role:       "tool",
				Content:    packaged.ModelContent,
				Name:       name,
				ToolCallID: call.ID,
			},
		}
	}
	if ledgerErr != nil && retryClass != ToolRetryReadOnly {
		return a.toolDispatchRefused(eventCh, idx, call, signature,
			fmt.Errorf("%s returned, but its durable outcome could not be recorded; external state is uncertain and must be verified before any retry: %w", name, ledgerErr))
	}

	modelResult := modelVisibleSkillToolResult(name, result)
	packaged := packageToolResultCtx(ctx, name, modelResult)
	activated := activateToolsFromSearchResult(ctx, name, result)
	if eventCh != nil {
		if len(activated) > 0 {
			EmitAgentEvent(eventCh, AgentEvent{Type: "tool.catalog.activated", Payload: map[string]interface{}{
				"tools": activated, "added": len(activated), "active_total": activatedToolCount(ctx),
			}})
		}
		emitToolEndEventWithDuration(eventCh, name, call.ID, packaged, duration, nil, completedMetadata...)
		emitStructuredToolEvent(eventCh, name, args, result, nil)
	}

	return toolExecutionResult{
		index:     idx,
		step:      toolHistoryStep(name, packaged),
		toolName:  name,
		signature: signature,
		rawResult: result,
		success:   true,
		msg: llm.Message{
			Role:       "tool",
			Content:    packaged.ModelContent,
			Name:       name,
			ToolCallID: call.ID,
		},
	}
}

// modelVisibleSkillToolResult keeps control-plane identity in rawResult/events
// while reducing the provider-visible tool message to the presentation
// contract. This is the final common dispatch boundary, so direct tools cannot
// accidentally reintroduce paths or hashes into a later prompt.
func modelVisibleSkillToolResult(name, raw string) string {
	if name == "skills_list" {
		var source map[string]interface{}
		if json.Unmarshal([]byte(raw), &source) != nil {
			return raw
		}
		out := map[string]interface{}{}
		for _, key := range []string{"success", "count", "total_matches", "truncated", "hint"} {
			if value, exists := source[key]; exists {
				out[key] = value
			}
		}
		var minimal []map[string]interface{}
		if rows, ok := source["skills"].([]interface{}); ok {
			for _, row := range rows {
				item, _ := row.(map[string]interface{})
				presented := map[string]interface{}{}
				for _, key := range []string{"candidate_ref", "name", "description", "scope", "source"} {
					if value, exists := item[key]; exists {
						presented[key] = value
					}
				}
				minimal = append(minimal, presented)
			}
		}
		out["skills"] = minimal
		encoded, err := json.Marshal(out)
		if err == nil {
			return string(encoded)
		}
		return raw
	}
	allowed := map[string]map[string]bool{
		"skill_select": {
			"success": true, "activation_id": true, "name": true, "instructions": true,
			"linked_files": true, "delivery_mode": true, "notice": true, "candidate_notice": true,
		},
		"skill_fallback": {
			"success": true, "activation_id": true, "name": true, "notice": true,
		},
		"skill_view": {
			"success": true, "name": true, "description": true, "content": true,
			"offset_bytes": true, "total_bytes": true, "complete": true, "next_offset_bytes": true,
			"hint": true, "file": true, "section": true, "linked_files": true,
		},
	}
	fields, ok := allowed[name]
	if !ok {
		return raw
	}
	var source map[string]interface{}
	if json.Unmarshal([]byte(raw), &source) != nil {
		return raw
	}
	out := make(map[string]interface{}, len(fields))
	for key := range fields {
		if value, exists := source[key]; exists {
			out[key] = value
		}
	}
	if candidateNotice, _ := out["candidate_notice"].(string); strings.TrimSpace(candidateNotice) != "" {
		notice, _ := out["notice"].(string)
		out["notice"] = strings.TrimSpace(notice + " " + candidateNotice)
		delete(out, "candidate_notice")
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return string(encoded)
}

// stripModelRuntimeArgs enforces the namespace boundary between model input
// and daemon-owned execution metadata. Every underscore-prefixed argument is
// reserved for the runtime and is rebuilt below from authenticated context.
func stripModelRuntimeArgs(args map[string]interface{}) {
	for key := range args {
		if strings.HasPrefix(strings.TrimSpace(key), "_") {
			delete(args, key)
		}
	}
}

func (a *Agent) toolDispatchRefused(eventCh chan string, idx int, call llm.ToolCall, signature string, err error) toolExecutionResult {
	packaged := packageToolError(call.Function, err)
	if eventCh != nil {
		emitToolEndEventWithDuration(eventCh, call.Function, call.ID, packaged, 0, err)
	}
	return toolExecutionResult{
		index: idx, step: packaged.ModelContent, toolName: call.Function, signature: signature,
		msg: llm.Message{Role: "tool", Content: packaged.ModelContent, Name: call.Function, ToolCallID: call.ID},
	}
}

func toolHistoryStep(name string, result ToolResultEnvelope) string {
	preview := strings.TrimSpace(result.Preview)
	if preview == "" {
		preview = "completed"
	}
	if result.Truncated {
		return fmt.Sprintf("Executed tool: %s, preview: %s (model received bounded head/tail of %d-byte output)", name, preview, result.Bytes)
	}
	return fmt.Sprintf("Executed tool: %s, result: %s", name, preview)
}

func parseToolCallArgs(raw string) map[string]interface{} {
	args := make(map[string]interface{})
	if raw == "" {
		return args
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return args
	}
	if args == nil {
		return make(map[string]interface{})
	}
	return args
}

func shouldParallelizeToolCalls(calls []llm.ToolCall, backends ...AgentBackend) bool {
	if len(calls) <= 1 {
		return false
	}
	var backend AgentBackend
	if len(backends) > 0 {
		backend = backends[0]
	}
	for _, call := range calls {
		if !toolSupportsParallel(call.Function, backend) {
			return false
		}
	}
	return true
}

func toolSupportsParallel(name string, backend AgentBackend) bool {
	if support, ok := backend.(parallelToolSupport); ok {
		return support.SupportsParallelTool(name)
	}
	return parallelSafeTool(name)
}

func parallelSafeTool(name string) bool {
	switch name {
	case "read_file", "cat", "ls_r", "list_files", "search_files", "grep",
		"web_search", "web_extract", "session_search", "get_current_time",
		"process_list", "process_poll", "tool_output_view":
		return true
	default:
		return false
	}
}

func emitStructuredToolEvent(eventCh chan string, name string, args map[string]interface{}, result string, err error) {
	if err != nil {
		return
	}
	switch name {
	case "update_plan":
		var update struct {
			Changed *bool `json:"changed"`
		}
		if json.Unmarshal([]byte(result), &update) == nil && update.Changed != nil && !*update.Changed {
			return
		}
		EmitAgentEvent(eventCh, AgentEvent{
			Type: "plan.updated",
			Plan: planItemsFromArgs(args),
			Payload: map[string]interface{}{
				"explanation": stringArg(args, "explanation"),
			},
		})
	case "finish_run":
		payload := map[string]interface{}{}
		if err := json.Unmarshal([]byte(result), &payload); err != nil {
			for k, v := range args {
				if !strings.HasPrefix(k, "_") {
					payload[k] = v
				}
			}
		}
		EmitAgentEvent(eventCh, AgentEvent{
			Type:    "run.outcome",
			Payload: payload,
		})
	}
}

func planItemsFromArgs(args map[string]interface{}) []PlanItem {
	raw, ok := args["plan"].([]interface{})
	if !ok {
		return nil
	}
	items := make([]PlanItem, 0, len(raw))
	for _, item := range raw {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		workUnit, _ := obj["work_unit"].(bool)
		items = append(items, PlanItem{
			Step:          fmt.Sprintf("%v", obj["step"]),
			Status:        fmt.Sprintf("%v", obj["status"]),
			RelatedTaskID: stringArg(obj, "related_task_id"),
			WorkUnitID:    stringArg(obj, "work_unit_id"),
			WorkUnit:      workUnit,
		})
	}
	return items
}

func stringArg(args map[string]interface{}, key string) string {
	if value, ok := args[key].(string); ok {
		return value
	}
	return ""
}
