package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel/memory"

	"github.com/google/uuid"
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
						Description: "The action to perform: list, category, conflicts, stats, raw, search, show, explain, add, pin, unpin, correct, forget, replace, remove, history, or undo.",
						Enum:        []string{"list", "category", "conflicts", "stats", "raw", "search", "show", "explain", "add", "pin", "unpin", "correct", "forget", "replace", "remove", "history", "undo"},
					},
					"target": {
						Type:        "string",
						Description: "Which memory store: 'user' for user preferences/profile, 'memory' for technical notes/environment facts, 'pinned' for authoritative facts the user confirmed (always injected into prompts, immune to automatic maintenance). Optional for history and undo.",
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
					"query": {
						Type:        "string",
						Description: "Case-insensitive text to search for in saved memories.",
					},
					"ref": {
						Type:        "string",
						Description: "A full memory ID or the short reference shown by search/show.",
					},
					"category": {
						Type:        "string",
						Description: "Memory category key shown by list, such as communication, development, projects, goals, identity, or other.",
					},
					"page": {
						Type:        "integer",
						Description: "One-based category page number.",
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
	query, _ := args["query"].(string)
	ref, _ := args["ref"].(string)
	category, _ := args["category"].(string)
	page := 1
	switch value := args["page"].(type) {
	case int:
		page = value
	case float64:
		page = int(value)
	}

	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "default"
	}
	workspaceID, _ := args["_workspace_id"].(string)
	workspaceID = strings.TrimSpace(workspaceID)
	ctx := context.Background()

	switch action {
	case "add":
		if target == "" {
			return "", fmt.Errorf("target is required for add")
		}
		if content == "" {
			return "", fmt.Errorf("content is required for add")
		}
		// The pinned target is user authority by definition (/memory pin routes
		// here); everything else the agent saves on its own initiative.
		source := memory.SourceAgent
		if target == "pinned" {
			source = memory.SourceUser
		}
		fact := memory.Fact{
			ID:             uuid.New().String(),
			Target:         target,
			Content:        content,
			Source:         source,
			Scope:          memory.DeriveFactScope(target, workspaceID),
			Confidence:     memory.BaseConfidence(source),
			LastVerifiedAt: time.Now(),
		}
		if err := t.mem.AddFactMeta(ctx, tenantID, fact); err != nil {
			return "", err
		}
		if store, ok := t.mem.Canonical(); ok {
			if err := store.ApplyIntakeWrite(ctx, tenantID, memory.IntakeWrite{
				Decision: "ADD", Target: target, Scope: fact.Scope,
				Source: source, Content: content,
			}); err != nil {
				_ = t.mem.RemoveFact(ctx, tenantID, fact.ID)
				return "", fmt.Errorf("write canonical memory: %w", err)
			}
		}
		recordMemoryLearningChangeScoped(tenantID, target, fact.Scope, "add", "", content, "memory_tool")
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
		if store, ok := t.mem.Canonical(); ok {
			if err := store.SetCanonicalStatusByHash(ctx, tenantID, canonicalTargetOf(fact), fact.Scope, fact.Content, memory.CanonicalForgotten, "agent"); err != nil {
				return "", err
			}
		}
		err = t.mem.RemoveFact(ctx, tenantID, fact.ID)
		if err != nil {
			if store, ok := t.mem.Canonical(); ok {
				_ = store.SetCanonicalStatusByHash(ctx, tenantID, canonicalTargetOf(fact), fact.Scope, fact.Content, memory.CanonicalActive, "agent_rollback")
			}
			return "", err
		}
		recordMemoryLearningChangeScoped(tenantID, target, fact.Scope, "remove", fact.Content, "", "memory_tool")
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
		// Remove + re-add under the SAME id so references, provenance, and scope
		// survive the rewrite; only content, source, and freshness change.
		if err := t.mem.RemoveFact(ctx, tenantID, fact.ID); err != nil {
			return "", err
		}
		replaced := memory.Fact{
			ID:             fact.ID,
			Target:         fact.Target,
			Content:        content,
			Source:         memory.SourceAgent,
			Scope:          fact.Scope,
			Confidence:     memory.BaseConfidence(memory.SourceAgent),
			CreatedFromRun: fact.CreatedFromRun,
			LastVerifiedAt: time.Now(),
		}
		if replaced.Scope == "" {
			replaced.Scope = memory.DeriveFactScope(replaced.Target, workspaceID)
		}
		if err := t.mem.AddFactMeta(ctx, tenantID, replaced); err != nil {
			// Keep a failed replace from silently deleting the original.
			_ = t.mem.AddFactMeta(ctx, tenantID, fact)
			return "", err
		}
		if store, ok := t.mem.Canonical(); ok {
			if err := store.ApplyIntakeWrite(ctx, tenantID, memory.IntakeWrite{
				Decision: "SUPERSEDE", Target: canonicalTargetOf(fact), Scope: replaced.Scope,
				Source: memory.SourceAgent, Content: content, RefContent: fact.Content,
			}); err != nil {
				_ = t.mem.RemoveFact(ctx, tenantID, replaced.ID)
				_ = t.mem.AddFactMeta(ctx, tenantID, fact)
				return "", fmt.Errorf("replace canonical memory: %w", err)
			}
		}
		recordMemoryLearningChangeScoped(tenantID, target, replaced.Scope, "replace", fact.Content, content, "memory_tool")
		return fmt.Sprintf("Replaced in %s memory: %s -> %s", target, fact.Content, content), nil

	case "history":
		changes, err := ListMemoryLearningChanges(tenantID, target, 20)
		if err != nil {
			return "", err
		}
		out := FormatMemoryLearningChanges(changes)
		if store, ok := t.mem.Canonical(); ok {
			events, eventErr := store.ListMemoryEvents(ctx, tenantID, 30)
			if eventErr == nil {
				if governance := FormatCanonicalGovernanceEvents(events); governance != "" {
					out += "\n\n" + governance
				}
			}
		}
		return out, nil

	case "undo":
		if changeID == "" {
			return "", fmt.Errorf("change_id is required for undo")
		}
		out, err := UndoMemoryLearningChange(ctx, t.mem, tenantID, changeID)
		if err == nil {
			return out, nil
		}
		if store, ok := t.mem.Canonical(); ok && strings.Contains(err.Error(), "learning change not found") {
			if undoErr := store.UndoMemoryEvent(ctx, tenantID, changeID, "user"); undoErr == nil {
				return fmt.Sprintf("Undid memory governance event `%s`.", changeID), nil
			} else {
				return "", undoErr
			}
		}
		return "", err

	case "list":
		return formatMemoryOverview(ctx, t.mem, tenantID)

	case "category":
		return formatMemoryCategory(ctx, t.mem, tenantID, category, page)

	case "conflicts":
		return formatMemoryConflicts(ctx, t.mem, tenantID)

	case "stats":
		return formatMemoryDiagnostics(ctx, t.mem, tenantID)

	case "raw":
		return formatRawMemoryFacts(ctx, t.mem, tenantID)

	case "search":
		return formatMemorySearch(ctx, t.mem, tenantID, query)

	case "show", "explain":
		return formatMemoryDetail(ctx, t.mem, tenantID, ref)

	case "pin", "unpin":
		fact, err := findMemoryFactByRef(ctx, t.mem, tenantID, ref)
		if err != nil {
			return "", err
		}
		store, ok := t.mem.Canonical()
		if !ok {
			return "", fmt.Errorf("pinning existing memory requires the canonical memory store")
		}
		pinned := action == "pin"
		if err := store.SetCanonicalPinned(ctx, tenantID, fact.ID, pinned, "user"); err != nil {
			return "", err
		}
		verb := "Unpinned"
		if pinned {
			verb = "Pinned"
		}
		return fmt.Sprintf("%s memory [%s]: %s", verb, shortMemoryRef(fact.ID), fact.Content), nil

	case "forget":
		fact, err := findMemoryFactByRef(ctx, t.mem, tenantID, ref)
		if err != nil {
			return "", err
		}
		// Forget must stop recall IMMEDIATELY on both layers: the canonical
		// row (authoritative read model) flips to forgotten by id or by
		// statement identity, and the legacy fact row is removed.
		if store, ok := t.mem.Canonical(); ok {
			if err := store.SetCanonicalStatus(ctx, tenantID, fact.ID, memory.CanonicalForgotten, "user"); err != nil {
				return "", err
			}
			if err := store.SetCanonicalStatusByHash(ctx, tenantID, canonicalTargetOf(fact), fact.Scope, fact.Content, memory.CanonicalForgotten, "user"); err != nil {
				return "", err
			}
		}
		if err := t.mem.RemoveFact(ctx, tenantID, fact.ID); err != nil {
			return "", err
		}
		removeLegacyFactByContent(ctx, t.mem, tenantID, fact)
		recordMemoryLearningChangeScoped(tenantID, fact.Target, fact.Scope, "remove", fact.Content, "", "memory_user")
		return fmt.Sprintf("Forgot memory [%s]: %s", shortMemoryRef(fact.ID), fact.Content), nil

	case "correct":
		if strings.TrimSpace(content) == "" {
			return "", fmt.Errorf("content is required for correct")
		}
		fact, err := findMemoryFactByRef(ctx, t.mem, tenantID, ref)
		if err != nil {
			return "", err
		}
		if err := t.mem.RemoveFact(ctx, tenantID, fact.ID); err != nil {
			return "", err
		}
		corrected := memory.Fact{
			ID:             fact.ID,
			Target:         fact.Target,
			Content:        strings.TrimSpace(content),
			Source:         memory.SourceUser,
			Scope:          fact.Scope,
			Confidence:     memory.BaseConfidence(memory.SourceUser),
			CreatedFromRun: fact.CreatedFromRun,
			LastVerifiedAt: time.Now(),
		}
		if corrected.Scope == "" {
			corrected.Scope = memory.DeriveFactScope(corrected.Target, workspaceID)
		}
		if err := t.mem.AddFactMeta(ctx, tenantID, corrected); err != nil {
			// Keep a failed correction from silently deleting its evidence.
			_ = t.mem.AddFactMeta(ctx, tenantID, fact)
			return "", err
		}
		// User authority supersedes on the canonical layer too — corrections
		// may retire ANY belief, including previously user-confirmed ones.
		if store, ok := t.mem.Canonical(); ok {
			if err := store.SetCanonicalStatus(ctx, tenantID, fact.ID, memory.CanonicalSuperseded, "user"); err != nil {
				_ = t.mem.RemoveFact(ctx, tenantID, corrected.ID)
				_ = t.mem.AddFactMeta(ctx, tenantID, fact)
				return "", err
			}
			if err := store.ApplyIntakeWrite(ctx, tenantID, memory.IntakeWrite{
				Decision: "SUPERSEDE", Target: canonicalTargetOf(fact), Scope: corrected.Scope,
				Source: memory.SourceUser, Content: corrected.Content, RefContent: fact.Content,
				Confidence: 1,
			}); err != nil {
				_ = store.SetCanonicalStatus(ctx, tenantID, fact.ID, memory.CanonicalActive, "user_rollback")
				_ = t.mem.RemoveFact(ctx, tenantID, corrected.ID)
				_ = t.mem.AddFactMeta(ctx, tenantID, fact)
				return "", fmt.Errorf("correct canonical memory: %w", err)
			}
		}
		recordMemoryLearningChangeScoped(tenantID, fact.Target, corrected.Scope, "replace", fact.Content, corrected.Content, "memory_user")
		return fmt.Sprintf("Corrected memory [%s]: %s", shortMemoryRef(fact.ID), corrected.Content), nil

	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

// factProvenanceSuffix is the compact "why is this remembered" suffix
// (source / scope / confidence / age) used by the explicit raw evidence view.
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

// canonicalTargetOf maps a view fact back to the canonical row's stored
// target. Pinned rows retain target=pinned in both the legacy and canonical
// stores; the pinned flag is an additional protection bit.
func canonicalTargetOf(f memory.Fact) string {
	return f.Target
}

// removeLegacyFactByContent clears a legacy fact row that matches the
// forgotten statement exactly. Best-effort: when the read model served a
// canonical id, the legacy row has a different id and is found by content.
func removeLegacyFactByContent(ctx context.Context, mem *memory.MemoryManager, tenantID string, fact memory.Fact) {
	for _, target := range []string{"pinned", "user", "memory"} {
		facts, err := mem.GetFacts(ctx, tenantID, target)
		if err != nil {
			continue
		}
		for _, f := range facts {
			if strings.EqualFold(strings.TrimSpace(f.Content), strings.TrimSpace(fact.Content)) {
				_ = mem.RemoveFact(ctx, tenantID, f.ID)
			}
		}
	}
}
