package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"selfmind/internal/kernel/llm"
)

type toolExecutionResult struct {
	index int
	step  string
	msg   llm.Message
}

func (a *Agent) llmToolDefinitions() []llm.ToolDefinition {
	if a.backend == nil {
		return nil
	}
	return toolDefinitionsForLLM(a.backend.GetToolDefinitions())
}

func toolDefinitionsForLLM(defs []map[string]interface{}) []llm.ToolDefinition {
	out := make([]llm.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		name := toolDefinitionName(d)
		if name == "" {
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

func (a *Agent) executeToolCalls(ctx context.Context, tenantID string, calls []llm.ToolCall) []toolExecutionResult {
	results := make([]toolExecutionResult, len(calls))
	if shouldParallelizeToolCalls(calls) {
		var wg sync.WaitGroup
		for idx, call := range calls {
			wg.Add(1)
			go func(i int, c llm.ToolCall) {
				defer wg.Done()
				results[i] = a.executeSingleToolCall(ctx, tenantID, i, c)
			}(idx, call)
		}
		wg.Wait()
		return results
	}

	for idx, call := range calls {
		results[idx] = a.executeSingleToolCall(ctx, tenantID, idx, call)
	}
	return results
}

func (a *Agent) executeSingleToolCall(ctx context.Context, tenantID string, idx int, call llm.ToolCall) toolExecutionResult {
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

	if a.EventChannel != nil {
		a.EventChannel <- fmt.Sprintf("tool_start:%s:%s", name, call.Args)
	}

	startTime := time.Now()
	result, err := a.backend.Dispatch(name, args)
	duration := time.Since(startTime).Seconds()

	if a.EventChannel != nil {
		emitToolEndEventWithDuration(a.EventChannel, name, result, duration, err)
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

func shouldParallelizeToolCalls(calls []llm.ToolCall) bool {
	if len(calls) <= 1 {
		return false
	}
	for _, call := range calls {
		if !parallelSafeTool(call.Function) {
			return false
		}
	}
	return true
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
