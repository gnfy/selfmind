package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

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
						Description: "The action to perform: add, replace, remove, history, undo, or list.",
						Enum:        []string{"add", "replace", "remove", "history", "undo", "list"},
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

	case "list":
		return t.formatFactList(ctx, tenantID), nil

	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

// formatFactList renders the user/project/pinned fact stores with compact
// provenance. It backs `/memory list` for daemon clients (which have no
// in-process store) via the dispatch path; the in-process TUI keeps its own
// richer view that also includes the synthesized profile.
func (t *MemoryTool) formatFactList(ctx context.Context, tenantID string) string {
	section := func(sb *strings.Builder, title, target string) {
		facts, _ := t.mem.GetFacts(ctx, tenantID, target)
		sb.WriteString("\n### " + title + "\n")
		if len(facts) == 0 {
			sb.WriteString("- (empty)\n")
			return
		}
		for _, f := range facts {
			sb.WriteString(fmt.Sprintf("- `%s` %s%s\n", f.ID, f.Content, factProvenanceSuffix(f)))
		}
	}
	var sb strings.Builder
	sb.WriteString("## Memory\n")
	section(&sb, "User", "user")
	section(&sb, "Project / Environment", "memory")
	section(&sb, "Pinned (authoritative — synthesis won't override)", "pinned")
	sb.WriteString("\nCommands: /memory pin <text> · /memory remove <user|memory|pinned> <id-or-text> · /memory history · /memory undo <change_id>")
	return sb.String()
}

// factProvenanceSuffix is the compact "why is this remembered" suffix
// (source / scope / confidence / age). Mirrors the TUI's factProvenance so the
// daemon-client view matches the in-process one.
func factProvenanceSuffix(f memory.Fact) string {
	var parts []string
	if f.Source != "" {
		parts = append(parts, "src="+f.Source)
	}
	if f.Scope != "" && f.Scope != "global" {
		parts = append(parts, "scope="+f.Scope)
	}
	if f.Confidence > 0 {
		parts = append(parts, fmt.Sprintf("conf=%.2f", f.Confidence))
	}
	if !f.CreatedAt.IsZero() {
		parts = append(parts, humanizeFactAge(time.Since(f.CreatedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "  · " + strings.Join(parts, " ")
}

func humanizeFactAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
