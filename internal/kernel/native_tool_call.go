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
	index int
	step  string
	msg   llm.Message
}

type parallelToolSupport interface {
	SupportsParallelTool(name string) bool
}

func (a *Agent) llmToolDefinitions(excluded map[string]bool) []llm.ToolDefinition {
	if a.backend == nil {
		return nil
	}
	return toolDefinitionsForLLM(a.backend.GetToolDefinitions(), excluded)
}

func toolDefinitionsForLLM(defs []map[string]interface{}, excluded map[string]bool) []llm.ToolDefinition {
	out := make([]llm.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		name := toolDefinitionName(d)
		if name == "" {
			continue
		}
		if excluded != nil && excluded[name] {
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
	if call.ID == "" {
		call.ID = fmt.Sprintf("toolcall-%d", idx)
	}
	if name == "" {
		step := "Error executing tool: missing function name"
		return toolExecutionResult{
			index: idx,
			step:  step,
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
			index: idx,
			step:  step,
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

	if eventCh != nil {
		EmitAgentEvent(eventCh, AgentEvent{
			Type:     "tool.started",
			ToolName: name,
			ToolArgs: call.Args,
		})
	}

	startTime := time.Now()
	result, err := a.backend.Dispatch(name, args)
	duration := time.Since(startTime).Seconds()

	if eventCh != nil {
		emitToolEndEventWithDuration(eventCh, name, result, duration, err)
		emitStructuredToolEvent(eventCh, name, args, result, err)
	}

	if err != nil {
		errorMsg := fmt.Sprintf("Error executing %s: %v", name, err)
		if len(errorMsg) > 2000 {
			errorMsg = errorMsg[:2000] + "...(error message truncated)"
		}
		return toolExecutionResult{
			index: idx,
			step:  errorMsg,
			msg: llm.Message{
				Role:       "tool",
				Content:    errorMsg,
				Name:       name,
				ToolCallID: call.ID,
			},
		}
	}

	llmResult := result
	if len(llmResult) > 10000 {
		llmResult = fmt.Sprintf("%s\n\n... (Content truncated: total %d chars) ...\n\n%s",
			llmResult[:5000], len(llmResult), llmResult[len(llmResult)-5000:])
	}

	return toolExecutionResult{
		index: idx,
		step:  fmt.Sprintf("Executed tool: %s, result: %s", name, llmResult),
		msg: llm.Message{
			Role:       "tool",
			Content:    llmResult,
			Name:       name,
			ToolCallID: call.ID,
		},
	}
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
		"process_list", "process_poll":
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
