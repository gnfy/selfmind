package tools

import (
	"context"
	"fmt"
	"strings"
	"selfmind/internal/kernel/memory"
)

// MemoryTool allows the agent to save durable information.
type MemoryTool struct {
	BaseTool
	mem *memory.MemoryManager
}

func NewMemoryTool(mem *memory.MemoryManager) *MemoryTool {
	return &MemoryTool{
		BaseTool: BaseTool{
			name:        "memory",
			description: "Save durable information to persistent memory that survives across sessions. Memory is injected into future turns, so keep it compact and focused on facts that will still matter later.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type:        "string",
						Description: "The action to perform: 'add', 'replace', or 'remove'.",
						Enum:        []string{"add", "replace", "remove"},
					},
					"target": {
						Type:        "string",
						Description: "Which memory store: 'user' for user preferences/profile, 'memory' for technical notes/environment facts.",
						Enum:        []string{"user", "memory"},
					},
					"content": {
						Type:        "string",
						Description: "The entry content. Required for 'add' and 'replace'.",
					},
					"old_text": {
						Type:        "string",
						Description: "Short unique substring identifying the entry to replace or remove.",
					},
				},
				Required: []string{"action", "target"},
			},
		},
		mem: mem,
	}
}

func (t *MemoryTool) Execute(args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	target, _ := args["target"].(string)
	content, _ := args["content"].(string)
	oldText, _ := args["old_text"].(string)

	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "user1" // Fallback for testing
	}
	ctx := context.Background()

	switch action {
	case "add":
		if content == "" {
			return "", fmt.Errorf("content is required for add")
		}
		err := t.mem.AddFact(ctx, tenantID, target, content)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Added to %s memory: %s", target, content), nil

	case "remove":
		if oldText == "" {
			return "", fmt.Errorf("old_text is required for remove")
		}
		facts, err := t.mem.GetFacts(ctx, tenantID, target)
		if err != nil {
			return "", err
		}
		var targetID string
		for _, f := range facts {
			if strings.Contains(f.Content, oldText) {
				targetID = f.ID
				break
			}
		}
		if targetID == "" {
			return "", fmt.Errorf("could not find memory entry matching %q", oldText)
		}
		err = t.mem.RemoveFact(ctx, tenantID, targetID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed from %s memory: (matched %q)", target, oldText), nil

	case "replace":
		if content == "" || oldText == "" {
			return "", fmt.Errorf("content and old_text are required for replace")
		}
		facts, err := t.mem.GetFacts(ctx, tenantID, target)
		if err != nil {
			return "", err
		}
		var targetID string
		for _, f := range facts {
			if strings.Contains(f.Content, oldText) {
				targetID = f.ID
				break
			}
		}
		if targetID == "" {
			return "", fmt.Errorf("could not find memory entry matching %q", oldText)
		}
		// In SQLite, we can just remove and add, or implement UpdateFact. 
		// For simplicity, we use Remove + Add.
		err = t.mem.RemoveFact(ctx, tenantID, targetID)
		if err != nil {
			return "", err
		}
		err = t.mem.AddFact(ctx, tenantID, target, content)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Replaced in %s memory: (matched %q) with %s", target, oldText, content), nil

	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}
