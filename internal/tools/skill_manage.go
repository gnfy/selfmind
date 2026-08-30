package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

var allowedSkillSubdirs = map[string]bool{
	"references": true,
	"templates":  true,
	"scripts":    true,
	"assets":     true,
}

// SkillInfo is the filesystem + usage view used by skill_manage and the CLI.
type SkillInfo struct {
	Name        string
	Description string
	// PackageName is the manifest-declared source name when a package manifest
	// governs the owning root. It is empty otherwise, and qualified names then
	// fall back to the root scope.
	PackageName string
	Path        string
	Format      string
	State       string
	Source      string
	Scope       string
	Root        string
	// Provenance is the authorship tier of the owning root, independent of
	// scope. See SkillProvenanceFirstParty and SkillProvenanceExternal.
	Provenance string
	// ModelInvocable reports whether the model may select this Skill on its
	// own. An author may switch it off to keep a Skill user-invocable only.
	ModelInvocable bool
	// Precedence is the owning root's lookup priority, lower winning. It is
	// carried on the entry because listing sorts for presentation and would
	// otherwise lose the root order.
	Precedence          int
	Writable            bool
	LastUsed            string
	Pinned              bool
	GovernanceNotBefore string
}

// SkillManageTool allows the agent to actively maintain reusable skills.
type SkillManageTool struct {
	BaseTool
	store *control.Store
}

// NewSkillManageTool creates the skill_manage tool.
func NewSkillManageTool(stores ...*control.Store) *SkillManageTool {
	var store *control.Store
	if len(stores) > 0 {
		store = stores[0]
	}
	return &SkillManageTool{
		BaseTool: BaseTool{
			name:        "skill_manage",
			description: "Create, search, read, patch, archive, restore, or govern reusable skills in the configured control-tenant skill store.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type:        "string",
						Description: "Action: list, search, read, stats, history, undo, create, update, edit, patch, delete, archive, restore, write_file, remove_file, pin, unpin, enable, disable, curator_status, curator_run, or reload.",
						Enum:        []string{"list", "search", "read", "stats", "history", "undo", "create", "update", "edit", "patch", "delete", "archive", "restore", "write_file", "remove_file", "pin", "unpin", "enable", "disable", "curator_status", "curator_run", "reload", "completion"},
					},
					"name": {
						Type:        "string",
						Description: "Skill name.",
					},
					"query": {
						Type:        "string",
						Description: "Search text for action=search.",
					},
					"content": {
						Type:        "string",
						Description: "Full SKILL.md content for create/update/edit.",
					},
					"description": {
						Type:        "string",
						Description: "Short description used when wrapping raw content with front matter.",
					},
					"old_text": {
						Type:        "string",
						Description: "Exact text to replace for action=patch.",
					},
					"new_text": {
						Type:        "string",
						Description: "Replacement text for action=patch.",
					},
					"replace_all": {
						Type:        "boolean",
						Description: "For action=patch, replace all matches instead of requiring a single match.",
					},
					"file_path": {
						Type:        "string",
						Description: "Support file path under references/, templates/, scripts/, or assets/.",
					},
					"file_content": {
						Type:        "string",
						Description: "Content for action=write_file.",
					},
					"source": {
						Type:        "string",
						Description: "Optional skill source metadata, usually agent-created or manual.",
					},
					"change_id": {
						Type:        "string",
						Description: "Learning history change id to undo.",
					},
					"stale_after_days":   {Type: "integer", Description: "Idle days before curator_run marks an agent-created Skill stale."},
					"archive_after_days": {Type: "integer", Description: "Idle days before curator_run archives an agent-created Skill."},
					"dry_run":            {Type: "boolean", Description: "Preview curator_run without changing files."},
					"write_report":       {Type: "boolean", Description: "Write the curator report beside the configured Skill store."},
				},
				Required: []string{"action"},
			},
		},
		store: store,
	}
}

func (t *SkillManageTool) Execute(args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	name, _ := args["name"].(string)
	query, _ := args["query"].(string)
	content, _ := args["content"].(string)
	description, _ := args["description"].(string)
	oldText, _ := args["old_text"].(string)
	newText, _ := args["new_text"].(string)
	replaceAll, _ := args["replace_all"].(bool)
	filePath, _ := args["file_path"].(string)
	fileContent, _ := args["file_content"].(string)
	source, _ := args["source"].(string)
	changeID, _ := args["change_id"].(string)
	if skillMutationAction(action) {
		if err := authorizeSkillMutation(args, action); err != nil {
			return "", err
		}
	}

	tenantID := skillStorageTenantID(args)

	switch action {
	case "list":
		skills, err := ListSkillsForTenant(tenantID, false, args)
		if err != nil {
			return "", err
		}
		return formatSkillsList(t.overlayAttributionRecency(tenantID, skills, args)), nil
	case "completion":
		// Read-only projection for the local `$` completion. It runs here rather
		// than in the client because usage recency for a read-only root lives in
		// the control store, and the client has none.
		skills, err := ListSkillsForTenant(tenantID, false, args)
		if err != nil {
			return "", err
		}
		payload, err := json.Marshal(BuildSkillCompletionCandidates(t.overlayAttributionRecency(tenantID, skills, args)))
		if err != nil {
			return "", err
		}
		return string(payload), nil
	case "search":
		if strings.TrimSpace(query) == "" {
			return "", fmt.Errorf("query is required for search")
		}
		skills, total, err := searchSkillsForTenantDetailed(tenantID, query, args)
		if err != nil {
			return "", err
		}
		return formatSkillSearchResults(skills, total), nil
	case "read":
		if name == "" {
			return "", fmt.Errorf("name is required for read")
		}
		return ReadSkillForTenant(tenantID, name, args)
	case "stats":
		if t.store == nil {
			return "", fmt.Errorf("durable Skill stats are unavailable without the control store")
		}
		stats, err := t.store.SkillUsageStats(ContextFromArgs(args), tenantID)
		if err != nil {
			return "", err
		}
		return formatDurableSkillStats(stats, time.Now()), nil
	case "history":
		if name == "" {
			return "", fmt.Errorf("name is required for history")
		}
		changes, err := ListSkillLearningChanges(tenantID, name, 20, args)
		if err != nil {
			return "", err
		}
		return formatSkillLearningChanges(changes), nil
	case "undo":
		if changeID == "" {
			return "", fmt.Errorf("change_id is required for undo")
		}
		result, err := UndoSkillLearningChangeForTenant(tenantID, changeID, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "create":
		result, err := createSkill(tenantID, name, content, description, source, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "update", "edit":
		result, err := editSkill(tenantID, name, content, description, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "patch":
		result, err := patchSkill(tenantID, name, oldText, newText, filePath, replaceAll, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "delete":
		result, err := deleteSkill(tenantID, name, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "archive":
		result, err := ArchiveSkillForTenant(tenantID, name, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "restore":
		result, err := RestoreSkillForTenant(tenantID, name, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "curator_status":
		return CuratorStatusForTenant(tenantID, args)
	case "curator_run":
		dryRun, _ := args["dry_run"].(bool)
		writeReport, _ := args["write_report"].(bool)
		return RunCuratorForTenantWithOptions(tenantID, CuratorOptions{
			StaleAfterDays: intArg(args, "stale_after_days", 30), ArchiveAfterDays: intArg(args, "archive_after_days", 90),
			DryRun: dryRun, WriteReport: writeReport,
		}, args)
	case "write_file":
		result, err := writeSkillSupportFile(tenantID, name, filePath, fileContent, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "remove_file":
		result, err := removeSkillSupportFile(tenantID, name, filePath, args)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "pin":
		if name == "" {
			return "", fmt.Errorf("name is required for pin")
		}
		info, err := findSkill(tenantID, name, args)
		if err != nil {
			return "", err
		}
		if err := SetSkillPinned(tenantID, info.Name, true, args); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "pin", "", "pinned", info.Source, args)
		return fmt.Sprintf("Skill %q pinned.", info.Name), nil
	case "unpin":
		if name == "" {
			return "", fmt.Errorf("name is required for unpin")
		}
		info, err := findSkill(tenantID, name, args)
		if err != nil {
			return "", err
		}
		if err := SetSkillPinned(tenantID, info.Name, false, args); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "unpin", "pinned", "unpinned", info.Source, args)
		return fmt.Sprintf("Skill %q unpinned.", info.Name), nil
	case "enable":
		if name == "" {
			return "", fmt.Errorf("name is required for enable")
		}
		info, err := findSkill(tenantID, name, args)
		if err != nil {
			return "", err
		}
		if err := SetSkillState(tenantID, info.Name, SkillStateActive, args); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "enable", info.State, SkillStateActive, info.Source, args)
		return fmt.Sprintf("Skill %q enabled.", info.Name), nil
	case "disable":
		if name == "" {
			return "", fmt.Errorf("name is required for disable")
		}
		info, err := findSkill(tenantID, name, args)
		if err != nil {
			return "", err
		}
		if err := SetSkillState(tenantID, info.Name, SkillStateDisabled, args); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "disable", info.State, SkillStateDisabled, info.Source, args)
		return fmt.Sprintf("Skill %q disabled.", info.Name), nil
	case "reload":
		registry, _ := args["_registry"].(*Registry)
		loaded, err := ReloadSkillToolsForTenant(tenantID, registry, args)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Reloaded %d skill tools.", len(loaded)), nil
	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

// overlayAttributionRecency fills the last-used value from the control store.
// A sidecar usage file cannot be written on a read-only root, which is where
// most implicit use happens, so recency for those Skills lives in the control
// store instead. Catalog ranking is untouched: it is metadata-only and must stay
// reproducible, and usage heat must not become a way for an external package to
// rank itself up.
func (t *SkillManageTool) overlayAttributionRecency(tenantID string, skills []SkillInfo, args map[string]interface{}) []SkillInfo {
	if t.store == nil || len(skills) == 0 {
		return skills
	}
	summaries, err := t.store.SkillAttributionSummaries(ContextFromArgs(args), tenantID)
	if err != nil || len(summaries) == 0 {
		return skills
	}
	observed := make(map[string]time.Time, len(summaries))
	for _, summary := range summaries {
		observed[summary.SkillKey] = summary.LastObservedAt
	}
	for i := range skills {
		key, keyErr := resolvedSkillKey(tenantID, skills[i])
		if keyErr != nil {
			continue
		}
		at, ok := observed[key]
		if !ok || at.IsZero() {
			continue
		}
		candidate := at.Format(time.RFC3339)
		if existing, parseErr := time.Parse(time.RFC3339, skills[i].LastUsed); parseErr == nil && !at.After(existing) {
			continue
		}
		skills[i].LastUsed = candidate
	}
	return skills
}

func formatDurableSkillStats(stats []control.SkillUsageStat, now time.Time) string {
	if len(stats) == 0 {
		return "No durable Skill activations or attributions recorded. Legacy skill_metrics rows are historical and excluded."
	}
	var lines []string
	lines = append(lines, "## Skill usage (durable activations and attributions)")
	lines = append(lines, fmt.Sprintf("%-30s %7s %9s %9s %8s %7s %9s %9s", "Skill", "Calls", "Complete", "Fallback", "Failed", "Parked", "Cancelled", "Implicit"))
	lines = append(lines, "---")
	for _, stat := range stats {
		ago := now.Sub(stat.LastUsedAt).Truncate(time.Minute)
		if ago < 0 {
			ago = 0
		}
		lines = append(lines, fmt.Sprintf("%-30s %7d %9d %9d %8d %7d %9d %9d  last used %s ago",
			stat.SkillName, stat.Calls, stat.Completed, stat.Fallbacks, stat.Failures, stat.Parked, stat.Cancelled, stat.Attributions, ago))
	}
	lines = append(lines, "",
		"Calls counts explicit activations; Implicit counts work units that read a Skill's content without activating it.",
		"Only activation evidence is admissible for curator cohorts and repair thresholds.",
		"Legacy skill_metrics counters are historical and do not drive ranking, curation, degradation, or retention.")
	return strings.Join(lines, "\n")
}

func skillMutationAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "undo", "create", "update", "edit", "patch", "delete", "archive", "restore", "curator_run",
		"write_file", "remove_file", "pin", "unpin", "enable", "disable":
		return true
	default:
		return false
	}
}

func authorizeSkillMutation(args map[string]interface{}, action string) error {
	mode := kernel.SkillMutationNone
	if scope, ok := InvocationScopeFromArgs(args); ok && strings.TrimSpace(scope.SkillMutationMode) != "" {
		mode = strings.TrimSpace(scope.SkillMutationMode)
	}
	switch mode {
	case kernel.SkillMutationDirect:
		return nil
	case kernel.SkillMutationCandidateOnly:
		if strings.EqualFold(strings.TrimSpace(action), "candidate_create") {
			return nil
		}
	case kernel.SkillMutationNone:
		// Fail closed below.
	default:
		mode = "unknown:" + mode
	}
	return fmt.Errorf("skill mutation %q is not allowed for this invocation (mode=%s)", action, mode)
}

func reloadSkillToolsFromArgs(tenantID string, args map[string]interface{}) {
	if registry, _ := args["_registry"].(*Registry); registry != nil {
		_, _ = ReloadSkillToolsForTenant(tenantID, registry, args)
	}
}
