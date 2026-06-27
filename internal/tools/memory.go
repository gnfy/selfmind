package tools

import (
	"context"
	"fmt"
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
						Description: "The action to perform: add, replace, remove, history, or undo.",
						Enum:        []string{"add", "replace", "remove", "history", "undo"},
					},
					"target": {
						Type:        "string",
						Description: "Which memory store: 'user' for user preferences/profile, 'memory' for technical notes/environment facts, 'pinned' for authoritative facts the user confirmed (the profile synthesizer must not contradict these). Optional for history and undo.",
						Enum:        []string{"user", "memory", "pinned"},
					},
					"content": {
						Type:        "string",
						Description: "The entry content. Required for 'add' and 'replace'.",
					},
					"old_text": {
						Type:        "string",
						Description: "Short unique substring identifying the entry to replace or remove.",
					},
					"change_id": {
						Type:        "string",
						Description: "Learning history change id to undo.",
					},
				},
				Required: []string{"action"},
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
	changeID, _ := args["change_id"].(string)

	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "default"
	}
	ctx := context.Background()

	switch action {
	case "add":
		if target == "" {
			return "", fmt.Errorf("target is required for add")
		}
		if content == "" {
			return "", fmt.Errorf("content is required for add")
		}
		err := t.mem.AddFact(ctx, tenantID, target, content)
		if err != nil {
			return "", err
		}
		recordMemoryLearningChange(tenantID, target, "add", "", content, "memory_tool")
		return fmt.Sprintf("Added to %s memory: %s", target, content), nil

	case "remove":
		if target == "" {
			return "", fmt.Errorf("target is required for remove")
		}
		if oldText == "" {
			return "", fmt.Errorf("old_text is required for remove")
		}
		facts, err := t.mem.GetFacts(ctx, tenantID, target)
		if err != nil {
			return "", err
		}
		fact, ok := findMatchingMemoryFact(facts, oldText)
		if !ok {
			return "", fmt.Errorf("could not find memory entry matching %q", oldText)
		}
		err = t.mem.RemoveFact(ctx, tenantID, fact.ID)
		if err != nil {
			return "", err
		}
		recordMemoryLearningChange(tenantID, target, "remove", fact.Content, "", "memory_tool")
		return fmt.Sprintf("Removed from %s memory: %s", target, fact.Content), nil

	case "replace":
		if target == "" {
			return "", fmt.Errorf("target is required for replace")
		}
		if content == "" || oldText == "" {
			return "", fmt.Errorf("content and old_text are required for replace")
		}
		facts, err := t.mem.GetFacts(ctx, tenantID, target)
		if err != nil {
			return "", err
		}
		fact, ok := findMatchingMemoryFact(facts, oldText)
		if !ok {
			return "", fmt.Errorf("could not find memory entry matching %q", oldText)
		}
		// In SQLite, we can just remove and add, or implement UpdateFact.
		// For simplicity, we use Remove + Add.
		err = t.mem.RemoveFact(ctx, tenantID, fact.ID)
		if err != nil {
			return "", err
		}
		err = t.mem.AddFact(ctx, tenantID, target, content)
		if err != nil {
			return "", err
		}
		recordMemoryLearningChange(tenantID, target, "replace", fact.Content, content, "memory_tool")
		return fmt.Sprintf("Replaced in %s memory: %s -> %s", target, fact.Content, content), nil

	case "history":
		changes, err := ListMemoryLearningChanges(tenantID, target, 20)
		if err != nil {
			return "", err
		}
		return FormatMemoryLearningChanges(changes), nil

	case "undo":
		if changeID == "" {
			return "", fmt.Errorf("change_id is required for undo")
		}
		return UndoMemoryLearningChange(ctx, t.mem, tenantID, changeID)

	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}
