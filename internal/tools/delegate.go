package tools

import (
	"selfmind/internal/kernel/llm"
	"fmt"
	"strings"
)

// =============================================================================
// Delegate Tool
// =============================================================================

// DelegateTool 派生子 Agent
type DelegateTool struct {
	BaseTool
	delegateFn func(goal string, context string, toolsets []string) (string, llm.UsageStats, error)
}

func NewDelegateTool() *DelegateTool {
	return &DelegateTool{
		BaseTool: BaseTool{
			name:        "delegate_task",
			description: "派生子 Agent 执行复杂任务",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"goal": {
						Type:        "string",
						Description: "子 Agent 的任务目标",
					},
					"context": {
						Type:        "string",
						Description: "传递给子 Agent 的背景信息",
					},
					"toolsets": {
						Type:        "string",
						Description: "逗号分隔的工具集名称，如 terminal,file,web",
					},
				},
				Required: []string{"goal"},
			},
		},
	}
}

func (t *DelegateTool) RegisterDelegateFn(fn func(goal string, context string, toolsets []string) (string, llm.UsageStats, error)) {
	t.delegateFn = fn
}

func (t *DelegateTool) Execute(args map[string]interface{}) (string, error) {
	goal, _ := args["goal"].(string)
	if goal == "" {
		return "", fmt.Errorf("goal is required")
	}
	context, _ := args["context"].(string)
	toolsets, _ := args["toolsets"].(string)

	var toolList []string
	if toolsets != "" {
		toolList = strings.Split(toolsets, ",")
	}

	if t.delegateFn == nil {
		return "", fmt.Errorf("delegate not initialized")
	}
	resp, _, err := t.delegateFn(goal, context, toolList)
	return resp, err
}
