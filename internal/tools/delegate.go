package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
)

type DelegateTool struct {
	BaseTool
	delegateFn func(goal string, context string, toolsets []string) (string, llm.UsageStats, error)
	batchFn    func(tasks []DelegateTaskSpec) ([]DelegateTaskResult, error)
}

type DelegateTaskSpec struct {
	Goal     string   `json:"goal"`
	Context  string   `json:"context,omitempty"`
	Toolsets []string `json:"toolsets,omitempty"`
}

type DelegateTaskResult struct {
	Goal     string         `json:"goal,omitempty"`
	Response string         `json:"response,omitempty"`
	Usage    llm.UsageStats `json:"usage,omitempty"`
	Error    string         `json:"error,omitempty"`
}

func NewDelegateTool() *DelegateTool {
	return &DelegateTool{
		BaseTool: BaseTool{
			name:        "delegate_task",
			description: "Delegate one task, or a batch of independent goals, to sub-agents.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"goal": {
						Type:        "string",
						Description: "Single sub-agent goal.",
					},
					"goals": {
						Type:        "array",
						Description: "Optional batch of independent sub-agent goals.",
						Items:       &PropertyDef{Type: "string"},
					},
					"context": {
						Type:        "string",
						Description: "Shared background context for the delegated work.",
					},
					"toolsets": {
						Type:        "string",
						Description: "Comma-separated toolsets, such as terminal,file,web,memory,skill.",
					},
				},
			},
			metadata: ToolMetadata{
				Category:  "task",
				RiskLevel: ToolRiskMedium,
			},
		},
	}
}

func (t *DelegateTool) RegisterDelegateFn(fn func(goal string, context string, toolsets []string) (string, llm.UsageStats, error)) {
	t.delegateFn = fn
}

func (t *DelegateTool) RegisterBatchDelegateFn(fn func(tasks []DelegateTaskSpec) ([]DelegateTaskResult, error)) {
	t.batchFn = fn
}

func (t *DelegateTool) Execute(args map[string]interface{}) (string, error) {
	context, _ := args["context"].(string)
	toolsets, _ := args["toolsets"].(string)
	toolList := splitToolsets(toolsets)

	if goals := delegateGoals(args["goals"]); len(goals) > 0 {
		tasks := make([]DelegateTaskSpec, 0, len(goals))
		for _, goal := range goals {
			tasks = append(tasks, DelegateTaskSpec{
				Goal:     goal,
				Context:  context,
				Toolsets: toolList,
			})
		}
		results, err := t.executeBatch(tasks)
		if err != nil {
			return "", err
		}
		data, _ := json.MarshalIndent(results, "", "  ")
		return string(data), nil
	}

	goal, _ := args["goal"].(string)
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return "", fmt.Errorf("goal or goals is required")
	}
	if t.delegateFn == nil {
		return "", fmt.Errorf("delegate not initialized")
	}
	resp, _, err := t.delegateFn(goal, context, toolList)
	return resp, err
}

func (t *DelegateTool) executeBatch(tasks []DelegateTaskSpec) ([]DelegateTaskResult, error) {
	if t.batchFn != nil {
		return t.batchFn(tasks)
	}
	if t.delegateFn == nil {
		return nil, fmt.Errorf("delegate not initialized")
	}
	results := make([]DelegateTaskResult, 0, len(tasks))
	for _, task := range tasks {
		resp, usage, err := t.delegateFn(task.Goal, task.Context, task.Toolsets)
		item := DelegateTaskResult{Goal: task.Goal, Response: resp, Usage: usage}
		if err != nil {
			item.Error = err.Error()
		}
		results = append(results, item)
	}
	return results, nil
}

func delegateGoals(raw interface{}) []string {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(fmt.Sprintf("%v", item))
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

func splitToolsets(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
