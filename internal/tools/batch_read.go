package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/platform/textutil"
)

const (
	batchReadMaxOperations = 8
	batchReadItemBytes     = 8 * 1024
)

type BatchReadTool struct {
	BaseTool
	dispatch func(string, map[string]interface{}) (string, error)
	metadata func(string, map[string]interface{}) kernel.ToolExecutionMetadata
}

type batchReadOperation struct {
	Tool         string `json:"tool"`
	Path         string `json:"path,omitempty"`
	Pattern      string `json:"pattern,omitempty"`
	FileGlob     string `json:"file_glob,omitempty"`
	Recursive    bool   `json:"recursive,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	MaxEntries   int    `json:"max_entries,omitempty"`
	MaxBytes     int    `json:"max_bytes,omitempty"`
	MaxFileBytes int    `json:"max_file_bytes,omitempty"`
	Timeout      int    `json:"timeout,omitempty"`
}

type batchReadItemResult struct {
	Index     int    `json:"index"`
	Tool      string `json:"tool"`
	Success   bool   `json:"success"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	Bytes     int    `json:"bytes"`
	Hash      string `json:"sha256,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func NewBatchReadTool(dispatch func(string, map[string]interface{}) (string, error), metadata ...func(string, map[string]interface{}) kernel.ToolExecutionMetadata) *BatchReadTool {
	var metadataProvider func(string, map[string]interface{}) kernel.ToolExecutionMetadata
	if len(metadata) > 0 {
		metadataProvider = metadata[0]
	}
	return &BatchReadTool{
		BaseTool: BaseTool{
			name:        "batch_read",
			description: "Run up to 8 independent local read-only file operations in one bounded call. Only read_file, search_files, and ls_r are allowed. Any failed item is reported with fallback_required=true; use ordinary tools to recover.",
			metadata: ToolMetadata{
				Exposure: ToolExposureDirect, SupportsParallel: false, ReadOnly: true,
				RiskLevel: ToolRiskLow, Category: "filesystem", OperationClasses: []OperationClass{OpClassObserve}, TimeoutSeconds: 120,
			},
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"candidate_id": {Type: "string", Description: "Optional enabled evolution candidate id shown in daemon context."},
					"operations": {
						Type: "array", Description: "Independent local read-only operations, maximum 8.",
						Items: &PropertyDef{
							Type: "object",
							Properties: map[string]PropertyDef{
								"tool":           {Type: "string", Enum: []string{"read_file", "search_files", "ls_r"}},
								"path":           {Type: "string"},
								"pattern":        {Type: "string"},
								"file_glob":      {Type: "string"},
								"recursive":      {Type: "boolean"},
								"limit":          {Type: "integer"},
								"max_entries":    {Type: "integer"},
								"max_bytes":      {Type: "integer"},
								"max_file_bytes": {Type: "integer"},
								"timeout":        {Type: "integer"},
							},
							Required: []string{"tool"},
						},
					},
				},
				Required: []string{"operations"},
			},
		},
		dispatch: dispatch, metadata: metadataProvider,
	}
}

func (t *BatchReadTool) Execute(args map[string]interface{}) (string, error) {
	return t.ExecuteContext(ContextFromArgs(args), args)
}

func (t *BatchReadTool) ExecuteContext(ctx context.Context, args map[string]interface{}) (string, error) {
	if t.dispatch == nil {
		return "", fmt.Errorf("batch_read dispatcher is unavailable")
	}
	rawOperations, ok := args["operations"].([]interface{})
	if !ok || len(rawOperations) == 0 {
		return "", fmt.Errorf("operations must contain at least one item")
	}
	if len(rawOperations) > batchReadMaxOperations {
		return "", fmt.Errorf("operations exceeds the maximum of %d", batchReadMaxOperations)
	}
	candidateID, _ := args["candidate_id"].(string)
	results := make([]batchReadItemResult, 0, len(rawOperations))
	failures := 0
	for index, raw := range rawOperations {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		encoded, _ := json.Marshal(raw)
		var operation batchReadOperation
		if err := json.Unmarshal(encoded, &operation); err != nil {
			return "", fmt.Errorf("operation %d is invalid: %w", index+1, err)
		}
		if !batchReadAllowedTool(operation.Tool) {
			return "", fmt.Errorf("operation %d uses disallowed tool %q", index+1, operation.Tool)
		}
		if !kernel.ConsumeNestedActionTool(ctx) {
			failures++
			results = append(results, batchReadItemResult{
				Index: index + 1, Tool: operation.Tool, Success: false,
				Error: "nested tool budget exhausted; continue with ordinary tools after the budget is extended",
			})
			kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{
				Type: "evolution.batch_budget_exhausted",
				Payload: map[string]interface{}{
					"candidate_id": candidateID, "index": index + 1, "tool": operation.Tool,
				},
			})
			break
		}
		childArgs := batchReadChildArgs(operation, args, index)
		childCallID := batchReadParentCallID(childArgs)
		parentCallID := batchReadParentCallID(args)
		eventArgs, _ := json.Marshal(batchReadPublicArgs(childArgs))
		startedPayload := map[string]interface{}{
			"tool": operation.Tool, "args": string(eventArgs), "nested": true,
			"parent_tool": "batch_read", "parent_tool_call_id": parentCallID, "candidate_id": candidateID,
		}
		if t.metadata != nil {
			metadata := t.metadata(operation.Tool, childArgs)
			startedPayload["tool_origin"] = metadata.Origin
			startedPayload["tool_category"] = metadata.Category
			startedPayload["tool_risk_level"] = metadata.RiskLevel
			startedPayload["tool_read_only"] = metadata.ReadOnly
			if len(metadata.OperationClasses) > 0 {
				startedPayload["operation_classes"] = metadata.OperationClasses
			}
		}
		kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{
			Type:       "tool.started",
			ToolName:   operation.Tool,
			ToolCallID: childCallID,
			ToolArgs:   string(eventArgs),
			Payload:    startedPayload,
		})
		startedAt := time.Now()
		output, err := t.dispatch(operation.Tool, childArgs)
		duration := time.Since(startedAt).Seconds()
		item := batchReadItemResult{Index: index + 1, Tool: operation.Tool, Success: err == nil, Bytes: len(output)}
		digest := sha256.Sum256([]byte(output))
		item.Hash = fmt.Sprintf("%x", digest[:])
		if len(output) > batchReadItemBytes {
			item.Output = textutil.HeadTail(output, batchReadItemBytes/2, "\n... [batch item truncated] ...\n")
			item.Truncated = true
		} else {
			item.Output = output
		}
		if err != nil {
			failures++
			item.Error = err.Error()
		}
		results = append(results, item)
		completionPayload := map[string]interface{}{
			"tool": operation.Tool, "nested": true, "parent_tool": "batch_read",
			"parent_tool_call_id": parentCallID, "candidate_id": candidateID,
			"result": item.Output, "result_truncated": item.Truncated,
		}
		completionEvent := kernel.AgentEvent{
			Type:            "tool.completed",
			ToolName:        operation.Tool,
			ToolCallID:      childCallID,
			ToolResult:      item.Output,
			DurationSeconds: duration,
			Payload:         completionPayload,
		}
		if err != nil {
			completionPayload["error"] = err.Error()
			completionPayload["error_category"] = ClassifyToolError(operation.Tool, err, output)
			completionEvent.Error = err.Error()
		}
		kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), completionEvent)
		kernel.EmitAgentEvent(kernel.EventChannelFromContext(ctx), kernel.AgentEvent{
			Type: "evolution.batch_item",
			Payload: map[string]interface{}{
				"candidate_id": candidateID, "index": index + 1, "tool": operation.Tool,
				"success": err == nil, "bytes": len(output), "sha256": item.Hash,
				"truncated": item.Truncated, "error": item.Error,
			},
		})
	}
	payload := map[string]interface{}{
		"success": failures == 0, "candidate_id": candidateID, "operations": len(results),
		"failures": failures, "fallback_required": failures > 0, "items": results,
	}
	data, _ := json.Marshal(payload)
	return string(data), nil
}

func batchReadPublicArgs(args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(args))
	for key, value := range args {
		if !strings.HasPrefix(key, "_") {
			out[key] = value
		}
	}
	return out
}

func batchReadAllowedTool(tool string) bool {
	switch strings.TrimSpace(tool) {
	case "read_file", "search_files", "ls_r":
		return true
	default:
		return false
	}
}

func batchReadParentCallID(args map[string]interface{}) string {
	value, ok := args["_tool_call_id"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func batchReadChildArgs(operation batchReadOperation, parent map[string]interface{}, index int) map[string]interface{} {
	args := map[string]interface{}{}
	for key, value := range parent {
		if strings.HasPrefix(key, "_") {
			args[key] = value
		}
	}
	if parentCallID := batchReadParentCallID(parent); parentCallID != "" {
		args["_tool_call_id"] = fmt.Sprintf("%s:batch:%d", parentCallID, index+1)
	} else {
		delete(args, "_tool_call_id")
	}
	if operation.Path != "" {
		args["path"] = operation.Path
	}
	if operation.Pattern != "" {
		args["pattern"] = operation.Pattern
	}
	if operation.FileGlob != "" {
		args["file_glob"] = operation.FileGlob
	}
	if operation.Recursive {
		args["recursive"] = true
	}
	if operation.Limit > 0 {
		args["limit"] = operation.Limit
	}
	if operation.MaxEntries > 0 {
		args["max_entries"] = operation.MaxEntries
	}
	if operation.MaxBytes > 0 {
		args["max_bytes"] = operation.MaxBytes
	}
	if operation.MaxFileBytes > 0 {
		args["max_file_bytes"] = operation.MaxFileBytes
	}
	if operation.Timeout > 0 {
		args["timeout"] = operation.Timeout
	}
	return args
}
