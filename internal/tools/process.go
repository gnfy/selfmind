package tools

import (
	"encoding/json"
	"fmt"
)

// ProcessTool 管理后台进程
type ProcessTool struct {
	BaseTool
}

func NewProcessTool() *ProcessTool {
	return &ProcessTool{
		BaseTool: BaseTool{
			name:        "process",
			description: "管理后台运行的任务（list, poll, kill）",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type:        "string",
						Description: "操作类型：list (列出), poll (获取输出), kill (停止进程)",
						Enum:        []string{"list", "poll", "kill"},
					},
					"id": {
						Type:        "string",
						Description: "进程 ID（用于 poll 和 kill）",
					},
				},
				Required: []string{"action"},
			},
		},
	}
}

func (t *ProcessTool) Execute(args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	id, _ := args["id"].(string)

	registry := GetProcessRegistry()

	switch action {
	case "list":
		list := registry.List()
		b, _ := json.Marshal(list)
		return string(b), nil

	case "poll":
		if id == "" {
			return "", fmt.Errorf("id is required for poll")
		}
		output, status, err := registry.Poll(id)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Status: %s\nOutput:\n%s", status, output), nil

	case "kill":
		if id == "" {
			return "", fmt.Errorf("id is required for kill")
		}
		if err := registry.Kill(id); err != nil {
			return "", err
		}
		return fmt.Sprintf("Process %s killed", id), nil

	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}
