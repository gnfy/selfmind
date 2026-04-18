package tools

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/kernel/memory"
)

// =============================================================================
// Checkpoint Tool
// =============================================================================

// CheckpointTool 保存 / 恢复 / 管理会话快照
type CheckpointTool struct {
	BaseTool
	memFn func() (*memory.MemoryManager, string, string, error)
	msgFn func() ([]byte, error)
}

func NewCheckpointTool(memFn func() (*memory.MemoryManager, string, string, error), msgFn func() ([]byte, error)) *CheckpointTool {
	return &CheckpointTool{
		BaseTool: BaseTool{
			name:        "checkpoint",
			description: "保存、列出、恢复或删除会话快照（checkpoint）",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type:        "string",
						Description: "操作：save / list / load / delete",
					},
					"name": {
						Type:        "string",
						Description: "快照名称（save/load/delete 时必需）",
					},
				},
				Required: []string{"action"},
			},
		},
		memFn: memFn,
		msgFn: msgFn,
	}
}

func (t *CheckpointTool) Execute(args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	name, _ := args["name"].(string)

	mem, tenantID, channel, err := t.memFn()
	if err != nil || mem == nil {
		return "", fmt.Errorf("checkpoint manager not initialized")
	}

	ctx := context.Background()

	switch action {
	case "save":
		if name == "" {
			return "", fmt.Errorf("name is required for save")
		}
		msgs, err := t.msgFn()
		if err != nil {
			return "", fmt.Errorf("get messages: %w", err)
		}
		if err := mem.SaveCheckpoint(ctx, tenantID, channel, name, msgs); err != nil {
			return "", fmt.Errorf("save checkpoint: %w", err)
		}
		return fmt.Sprintf("Checkpoint %q saved successfully", name), nil

	case "list":
		checkpoints, err := mem.ListCheckpoints(ctx, tenantID, channel)
		if err != nil {
			return "", fmt.Errorf("list checkpoints: %w", err)
		}
		if len(checkpoints) == 0 {
			return "No checkpoints found", nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%d checkpoint(s):\n\n", len(checkpoints)))
		for _, cp := range checkpoints {
			sb.WriteString(fmt.Sprintf("  - %s  (created: %s)\n", cp.Name, cp.CreatedAt.Format("2006-01-02 15:04")))
		}
		return sb.String(), nil

	case "load":
		if name == "" {
			return "", fmt.Errorf("name is required for load")
		}
		msgs, err := mem.LoadCheckpoint(ctx, tenantID, channel, name)
		if err != nil {
			return "", fmt.Errorf("load checkpoint: %w", err)
		}
		return fmt.Sprintf("MEDIA:checkpoint:load:%s:%s", name, string(msgs)), nil

	case "delete":
		if name == "" {
			return "", fmt.Errorf("name is required for delete")
		}
		if err := mem.DeleteCheckpoint(ctx, tenantID, channel, name); err != nil {
			return "", fmt.Errorf("delete checkpoint: %w", err)
		}
		return fmt.Sprintf("Checkpoint %q deleted", name), nil

	default:
		return "", fmt.Errorf("unknown action %q, use: save / list / load / delete", action)
	}
}

// InjectCheckpointFns injects memory and message functions into the dispatcher.
// Called from main.go after all components are created.
func InjectCheckpointFns(
	disp *Dispatcher,
	memFn func() (*memory.MemoryManager, string, string, error),
	msgFn func() ([]byte, error),
) {
	disp.RegisterTool(NewCheckpointTool(memFn, msgFn))
}
