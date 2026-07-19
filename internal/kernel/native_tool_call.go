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
	success   bool
}

type parallelToolSupport interface {
	SupportsParallelTool(name string) bool
}

func (a *Agent) llmToolDefinitions(strategy TaskStrategy) []llm.ToolDefinition {
	if a.backend == nil {
		return nil
	}
	return toolDefinitionsForLLM(a.backend.GetToolDefinitions(), strategy)
}

func toolDefinitionsForLLM(defs []map[string]interface{}, strategy TaskStrategy) []llm.ToolDefinition {
	out := make([]llm.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		name := toolDefinitionName(d)
		if name == "" {
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

	args := parseToolCallArgs(call.Args)
	args["_tenant_id"] = tenantID
	args["_context"] = ctx
	args["_tool_call_id"] = call.ID
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
		EmitAgentEvent(eventCh, AgentEvent{
			Type:       "tool.started",
			ToolName:   name,
			ToolCallID: call.ID,
			ToolArgs:   call.Args,
		})
	}

	startTime := time.Now()
	result, err := a.backend.Dispatch(name, args)
	duration := time.Since(startTime).Seconds()

	var ledgerErr error
	if ledgerRunID != "" && ledger != nil {
		ledgerErr = ledger.RecordOutcome(ctx, ledgerRunID, call.ID, err == nil)
	}

	if err != nil {
		if ledgerErr != nil {
			err = fmt.Errorf("%w; durable outcome recording also failed: %v", err, ledgerErr)
		}
		packaged := packageToolError(name, err)
		if eventCh != nil {
			emitToolEndEventWithDuration(eventCh, name, call.ID, packaged, duration, err)
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

	packaged := packageToolResultCtx(ctx, name, result)
	if eventCh != nil {
		emitToolEndEventWithDuration(eventCh, name, call.ID, packaged, duration, nil)
		emitStructuredToolEvent(eventCh, name, args, result, nil)
	}

	return toolExecutionResult{
		index:     idx,
		step:      toolHistoryStep(name, packaged),
		toolName:  name,
		signature: signature,
		success:   true,
		msg: llm.Message{
			Role:       "tool",
			Content:    packaged.ModelContent,
			Name:       name,
			ToolCallID: call.ID,
		},
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
		items = append(items, PlanItem{
			Step:   fmt.Sprintf("%v", obj["step"]),
			Status: fmt.Sprintf("%v", obj["status"]),
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
