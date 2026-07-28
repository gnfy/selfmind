package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Path        string
	Format      string
	State       string
	Source      string
	Scope       string
	Root        string
	Writable    bool
	LastUsed    string
	Pinned      bool
}

// SkillManageTool allows the agent to actively maintain reusable skills.
type SkillManageTool struct {
	BaseTool
}

// NewSkillManageTool creates the skill_manage tool.
func NewSkillManageTool() *SkillManageTool {
	return &SkillManageTool{
		BaseTool: BaseTool{
			name:        "skill_manage",
			description: "Create, search, read, patch, archive, or delete reusable skills. Skills are procedural memory and are stored under ~/.selfmind/<tenant>/skills/.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"action": {
						Type:        "string",
						Description: "Action: list, search, read, history, undo, create, update, edit, patch, delete, archive, write_file, remove_file, pin, unpin, enable, disable, or reload.",
						Enum:        []string{"list", "search", "read", "history", "undo", "create", "update", "edit", "patch", "delete", "archive", "write_file", "remove_file", "pin", "unpin", "enable", "disable", "reload"},
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
				},
				Required: []string{"action"},
			},
		},
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

	tenantID, _ := args["_tenant_id"].(string)
	if tenantID == "" {
		tenantID = "default"
	}

	switch action {
	case "list":
		skills, err := ListSkillsForTenant(tenantID, false)
		if err != nil {
			return "", err
		}
		return formatSkillsList(skills), nil
	case "search":
		if strings.TrimSpace(query) == "" {
			return "", fmt.Errorf("query is required for search")
		}
		skills, err := SearchSkillsForTenant(tenantID, query)
		if err != nil {
			return "", err
		}
		return formatSkillsList(skills), nil
	case "read":
		if name == "" {
			return "", fmt.Errorf("name is required for read")
		}
		return ReadSkillForTenant(tenantID, name)
	case "history":
		if name == "" {
			return "", fmt.Errorf("name is required for history")
		}
		changes, err := ListSkillLearningChanges(tenantID, name, 20)
		if err != nil {
			return "", err
		}
		return formatSkillLearningChanges(changes), nil
	case "undo":
		if changeID == "" {
			return "", fmt.Errorf("change_id is required for undo")
		}
		result, err := UndoSkillLearningChangeForTenant(tenantID, changeID)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "create":
		result, err := createSkill(tenantID, name, content, description, source)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "update", "edit":
		result, err := editSkill(tenantID, name, content, description)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "patch":
		result, err := patchSkill(tenantID, name, oldText, newText, filePath, replaceAll)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "delete":
		result, err := deleteSkill(tenantID, name)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "archive":
		result, err := ArchiveSkillForTenant(tenantID, name)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "write_file":
		result, err := writeSkillSupportFile(tenantID, name, filePath, fileContent)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "remove_file":
		result, err := removeSkillSupportFile(tenantID, name, filePath)
		reloadSkillToolsFromArgs(tenantID, args)
		return result, err
	case "pin":
		if name == "" {
			return "", fmt.Errorf("name is required for pin")
		}
		info, err := findSkill(tenantID, name)
		if err != nil {
			return "", err
		}
		if err := SetSkillPinned(tenantID, info.Name, true); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "pin", "", "pinned", info.Source)
		return fmt.Sprintf("Skill %q pinned.", info.Name), nil
	case "unpin":
		if name == "" {
			return "", fmt.Errorf("name is required for unpin")
		}
		info, err := findSkill(tenantID, name)
		if err != nil {
			return "", err
		}
		if err := SetSkillPinned(tenantID, info.Name, false); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "unpin", "pinned", "unpinned", info.Source)
		return fmt.Sprintf("Skill %q unpinned.", info.Name), nil
	case "enable":
		if name == "" {
			return "", fmt.Errorf("name is required for enable")
		}
		info, err := findSkill(tenantID, name)
		if err != nil {
			return "", err
		}
		if err := SetSkillState(tenantID, info.Name, SkillStateActive); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "enable", info.State, SkillStateActive, info.Source)
		return fmt.Sprintf("Skill %q enabled.", info.Name), nil
	case "disable":
		if name == "" {
			return "", fmt.Errorf("name is required for disable")
		}
		info, err := findSkill(tenantID, name)
		if err != nil {
			return "", err
		}
		if err := SetSkillState(tenantID, info.Name, SkillStateDisabled); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "disable", info.State, SkillStateDisabled, info.Source)
		return fmt.Sprintf("Skill %q disabled.", info.Name), nil
	case "reload":
		registry, _ := args["_registry"].(*Registry)
		loaded, err := ReloadSkillToolsForTenant(tenantID, registry)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Reloaded %d skill tools.", len(loaded)), nil
	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

func reloadSkillToolsFromArgs(tenantID string, args map[string]interface{}) {
	if registry, _ := args["_registry"].(*Registry); registry != nil {
		_, _ = ReloadSkillToolsForTenant(tenantID, registry)
	}
}

func getSkillsDir(tenantID string) (string, error) {
	root, err := WritableSkillRootForTenant(tenantID)
	if err != nil {
		return "", err
	}
	return root.Path, nil
}

func createSkill(tenantID, name, content, description, source string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for create")
	}
	if content == "" {
		return "", fmt.Errorf("content is required for create")
	}
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", err
	}
	safeName := kernel.SanitizeSkillName(name)
	skillDir := filepath.Join(dir, safeName)
	if _, err := os.Stat(skillDir); err == nil {
		return "", fmt.Errorf("skill %q already exists; use patch or edit instead", name)
	}
	if _, err := os.Stat(filepath.Join(dir, safeName+".md")); err == nil {
		return "", fmt.Errorf("legacy skill %q already exists; use patch or edit instead", name)
	}

	content = ensureFrontMatter(content, safeName, description)
	if err := validateSkillEnvironmentDeclarations(content); err != nil {
		return "", err
	}
	if err := kernel.ScanSkillForDangers(content); err != nil {
		return "", fmt.Errorf("security scan failed: %w", err)
	}
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return "", err
	}
	target := filepath.Join(skillDir, "SKILL.md")
	if err := atomicWriteFile(target, content); err != nil {
		return "", err
	}
	if source == "" {
		source = SkillSourceAgentCreated
	}
	_ = MarkSkillCreated(tenantID, safeName, source, "skill_manage")
	recordSkillLearningChange(tenantID, safeName, "create", "", content, source)
	return fmt.Sprintf("Skill %q created at %s", safeName, target), nil
}

func editSkill(tenantID, name, content, description string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for edit")
	}
	if content == "" {
		return "", fmt.Errorf("content is required for edit")
	}
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "editing it"); err != nil {
		return "", err
	}
	content = ensureFrontMatter(content, info.Name, description)
	if err := validateSkillEnvironmentDeclarations(content); err != nil {
		return "", err
	}
	if err := kernel.ScanSkillForDangers(content); err != nil {
		return "", fmt.Errorf("security scan failed: %w", err)
	}
	target := info.Path
	if info.Format == "dir" {
		target = filepath.Join(info.Path, "SKILL.md")
	}
	beforeData, _ := os.ReadFile(target)
	if err := atomicWriteFile(target, content); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name)
	recordSkillLearningChange(tenantID, info.Name, "edit", string(beforeData), content, info.Source)
	return fmt.Sprintf("Skill %q edited at %s", info.Name, target), nil
}

func patchSkill(tenantID, name, oldText, newText, filePath string, replaceAll bool) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for patch")
	}
	if oldText == "" {
		return "", fmt.Errorf("old_text is required for patch")
	}
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "patching it"); err != nil {
		return "", err
	}
	target := info.Path
	auditAction := "patch"
	if info.Format == "dir" {
		target = filepath.Join(info.Path, "SKILL.md")
	}
	if filePath != "" {
		skillDir, err := ensureDirectorySkill(tenantID, info)
		if err != nil {
			return "", err
		}
		target, err = safeSupportPath(skillDir, filePath)
		if err != nil {
			return "", err
		}
		auditAction = "patch:" + filepath.ToSlash(filePath)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	current := string(data)
	updated, matches, strategy, err := fuzzyReplace(current, oldText, newText, replaceAll)
	if err != nil {
		return "", fmt.Errorf("%w\n\n%s", err, patchFailureHint(current, oldText, target))
	}
	if err := kernel.ScanSkillForDangers(updated); err != nil {
		return "", fmt.Errorf("security scan failed: %w", err)
	}
	if filePath == "" {
		if err := validateSkillEnvironmentDeclarations(updated); err != nil {
			return "", err
		}
	}
	if err := atomicWriteFile(target, updated); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name)
	recordSkillLearningChange(tenantID, info.Name, auditAction, current, updated, info.Source)
	return fmt.Sprintf("Skill %q patched at %s (%d replacement, strategy=%s)", info.Name, target, matches, strategy), nil
}

func deleteSkill(tenantID, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for delete")
	}
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "deleting it"); err != nil {
		return "", err
	}
	if info.Pinned {
		return "", fmt.Errorf("skill %q is pinned; unpin it before deleting", info.Name)
	}
	before, _ := readSkillContent(info)
	if info.Format == "dir" {
		err = os.RemoveAll(info.Path)
	} else {
		err = os.Remove(info.Path)
	}
	if err != nil {
		return "", err
	}
	_ = SetSkillState(tenantID, info.Name, SkillStateArchived)
	recordSkillLearningChange(tenantID, info.Name, "delete", before, "", info.Source)
	return fmt.Sprintf("Skill %q deleted.", info.Name), nil
}

func ArchiveSkillForTenant(tenantID, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for archive")
	}
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "archiving it"); err != nil {
		return "", err
	}
	if info.Pinned {
		return "", fmt.Errorf("skill %q is pinned; unpin it before archiving", info.Name)
	}
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", err
	}
	archiveDir := filepath.Join(dir, ".archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return "", err
	}
	destName := filepath.Base(info.Path)
	dest := filepath.Join(archiveDir, destName)
	if _, err := os.Stat(dest); err == nil {
		dest = filepath.Join(archiveDir, fmt.Sprintf("%s-%d", destName, time.Now().Unix()))
	}
	before, _ := readSkillContent(info)
	if err := os.Rename(info.Path, dest); err != nil {
		return "", err
	}
	_ = SetSkillState(tenantID, info.Name, SkillStateArchived)
	recordSkillLearningChange(tenantID, info.Name, "archive", before, "archived to "+dest, info.Source)
	return fmt.Sprintf("Skill %q archived to %s", info.Name, dest), nil
}

func writeSkillSupportFile(tenantID, name, filePath, fileContent string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for write_file")
	}
	if filePath == "" {
		return "", fmt.Errorf("file_path is required for write_file")
	}
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "writing support files"); err != nil {
		return "", err
	}
	skillDir, err := ensureDirectorySkill(tenantID, info)
	if err != nil {
		return "", err
	}
	target, err := safeSupportPath(skillDir, filePath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return "", err
	}
	if err := kernel.ScanSkillForDangers(fileContent); err != nil {
		return "", fmt.Errorf("security scan failed: %w", err)
	}
	beforeData, _ := os.ReadFile(target)
	if err := atomicWriteFile(target, fileContent); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name)
	recordSkillLearningChange(tenantID, info.Name, "write_file:"+filepath.ToSlash(filePath), string(beforeData), fileContent, info.Source)
	return fmt.Sprintf("Wrote support file for skill %q: %s", info.Name, target), nil
}

func removeSkillSupportFile(tenantID, name, filePath string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for remove_file")
	}
	if filePath == "" {
		return "", fmt.Errorf("file_path is required for remove_file")
	}
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "removing support files"); err != nil {
		return "", err
	}
	if info.Format != "dir" {
		return "", fmt.Errorf("skill %q has no support file directory", info.Name)
	}
	target, err := safeSupportPath(info.Path, filePath)
	if err != nil {
		return "", err
	}
	beforeData, _ := os.ReadFile(target)
	if err := os.Remove(target); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name)
	recordSkillLearningChange(tenantID, info.Name, "remove_file:"+filepath.ToSlash(filePath), string(beforeData), "", info.Source)
	return fmt.Sprintf("Removed support file for skill %q: %s", info.Name, target), nil
}

func UndoSkillLearningChangeForTenant(tenantID, changeID string) (string, error) {
	change, err := GetLearningChangeByID(tenantID, changeID)
	if err != nil {
		return "", err
	}
	if change.Kind != "skill" {
		return "", fmt.Errorf("learning change %s is %s, not skill", change.ID, change.Kind)
	}
	if strings.HasPrefix(change.Action, "undo:") {
		return "", fmt.Errorf("learning change %s is already an undo record", change.ID)
	}
	name := kernel.SanitizeSkillName(change.Target)
	if name == "" {
		return "", fmt.Errorf("skill name is missing from learning change %s", change.ID)
	}
	switch {
	case change.Action == "create":
		info, err := findSkill(tenantID, name)
		if err != nil {
			return "", err
		}
		if err := ensureWritableSkill(info, "undoing create"); err != nil {
			return "", err
		}
		current, _ := readSkillContent(info)
		if strings.TrimSpace(change.After) != "" && current != change.After {
			return "", fmt.Errorf("skill %q changed after %s; refusing to undo create", info.Name, change.ID)
		}
		if info.Format == "dir" {
			err = os.RemoveAll(info.Path)
		} else {
			err = os.Remove(info.Path)
		}
		if err != nil {
			return "", err
		}
		_ = SetSkillState(tenantID, info.Name, SkillStateArchived)
		recordSkillLearningChange(tenantID, info.Name, "undo:create", current, "", info.Source)
		return fmt.Sprintf("Undid skill create `%s`: removed %q.", change.ID, info.Name), nil
	case change.Action == "edit" || change.Action == "patch":
		if strings.TrimSpace(change.Before) == "" {
			return "", fmt.Errorf("learning change %s has no previous skill content to restore", change.ID)
		}
		info, err := findSkill(tenantID, name)
		if err != nil {
			return "", err
		}
		if err := ensureWritableSkill(info, "undoing edit"); err != nil {
			return "", err
		}
		target := skillMainFilePath(info)
		current, _ := os.ReadFile(target)
		if err := validateSkillEnvironmentDeclarations(change.Before); err != nil {
			return "", err
		}
		if err := kernel.ScanSkillForDangers(change.Before); err != nil {
			return "", fmt.Errorf("security scan failed: %w", err)
		}
		if err := atomicWriteFile(target, change.Before); err != nil {
			return "", err
		}
		_ = MarkSkillPatched(tenantID, info.Name)
		recordSkillLearningChange(tenantID, info.Name, "undo:"+change.Action, string(current), change.Before, info.Source)
		return fmt.Sprintf("Undid skill %s `%s`: restored %q.", change.Action, change.ID, info.Name), nil
	case strings.HasPrefix(change.Action, "patch:"):
		filePath := strings.TrimPrefix(change.Action, "patch:")
		return undoSkillSupportFileChange(tenantID, name, change, filePath, true)
	case strings.HasPrefix(change.Action, "write_file:"):
		filePath := strings.TrimPrefix(change.Action, "write_file:")
		return undoSkillSupportFileChange(tenantID, name, change, filePath, true)
	case strings.HasPrefix(change.Action, "remove_file:"):
		filePath := strings.TrimPrefix(change.Action, "remove_file:")
		return undoSkillSupportFileChange(tenantID, name, change, filePath, true)
	case change.Action == "delete" || change.Action == "archive":
		if strings.TrimSpace(change.Before) == "" {
			return "", fmt.Errorf("learning change %s has no previous skill content to restore", change.ID)
		}
		target, info, err := activeSkillMainPathForRestore(tenantID, name)
		if err != nil {
			return "", err
		}
		current, _ := os.ReadFile(target)
		if err := validateSkillEnvironmentDeclarations(change.Before); err != nil {
			return "", err
		}
		if err := kernel.ScanSkillForDangers(change.Before); err != nil {
			return "", fmt.Errorf("security scan failed: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return "", err
		}
		if err := atomicWriteFile(target, change.Before); err != nil {
			return "", err
		}
		_ = MarkSkillCreated(tenantID, name, emptyDefault(info.Source, change.Source), "learning_undo")
		_ = SetSkillState(tenantID, name, SkillStateActive)
		recordSkillLearningChange(tenantID, name, "undo:"+change.Action, string(current), change.Before, emptyDefault(info.Source, change.Source))
		return fmt.Sprintf("Undid skill %s `%s`: restored %q.", change.Action, change.ID, name), nil
	case change.Action == "pin":
		if err := SetSkillPinned(tenantID, name, false); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, name, "undo:pin", "pinned", "unpinned", change.Source)
		return fmt.Sprintf("Undid skill pin `%s`: unpinned %q.", change.ID, name), nil
	case change.Action == "unpin":
		if err := SetSkillPinned(tenantID, name, true); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, name, "undo:unpin", "unpinned", "pinned", change.Source)
		return fmt.Sprintf("Undid skill unpin `%s`: pinned %q.", change.ID, name), nil
	case change.Action == "enable" || change.Action == "disable":
		state := strings.TrimSpace(change.Before)
		if state == "" {
			state = SkillStateActive
		}
		if err := SetSkillState(tenantID, name, state); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, name, "undo:"+change.Action, change.After, state, change.Source)
		return fmt.Sprintf("Undid skill %s `%s`: restored %q to %s.", change.Action, change.ID, name, state), nil
	default:
		return "", fmt.Errorf("skill learning action %q cannot be undone", change.Action)
	}
}

func undoSkillSupportFileChange(tenantID, name string, change *LearningChange, filePath string, restoreBefore bool) (string, error) {
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "undoing support file changes"); err != nil {
		return "", err
	}
	skillDir, err := ensureDirectorySkill(tenantID, info)
	if err != nil {
		return "", err
	}
	target, err := safeSupportPath(skillDir, filePath)
	if err != nil {
		return "", err
	}
	current, _ := os.ReadFile(target)
	if restoreBefore && strings.TrimSpace(change.Before) != "" {
		if err := kernel.ScanSkillForDangers(change.Before); err != nil {
			return "", fmt.Errorf("security scan failed: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return "", err
		}
		if err := atomicWriteFile(target, change.Before); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, info.Name, "undo:"+change.Action, string(current), change.Before, info.Source)
		return fmt.Sprintf("Undid skill support-file change `%s`: restored %s.", change.ID, filepath.ToSlash(filePath)), nil
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	recordSkillLearningChange(tenantID, info.Name, "undo:"+change.Action, string(current), "", info.Source)
	return fmt.Sprintf("Undid skill support-file change `%s`: removed %s.", change.ID, filepath.ToSlash(filePath)), nil
}

func skillMainFilePath(info SkillInfo) string {
	if info.Format == "dir" {
		return filepath.Join(info.Path, "SKILL.md")
	}
	return info.Path
}

func activeSkillMainPathForRestore(tenantID, name string) (string, SkillInfo, error) {
	if info, err := findSkill(tenantID, name); err == nil {
		if info.Writable {
			return skillMainFilePath(info), info, nil
		}
	}
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", SkillInfo{}, err
	}
	safeName := kernel.SanitizeSkillName(name)
	path := filepath.Join(dir, safeName, "SKILL.md")
	return path, SkillInfo{Name: safeName, Path: filepath.Dir(path), Format: "dir", Source: SkillSourceAgentCreated, Scope: SkillScopeUser, Root: dir, Writable: true}, nil
}

func ListSkillsForTenant(tenantID string, includeArchived bool) ([]SkillInfo, error) {
	roots, err := SkillRootsForTenant(tenantID)
	if err != nil {
		return nil, err
	}
	userUsage := map[string]SkillUsageRecord{}
	if userDir, err := userSkillsDirForTenant(tenantID); err == nil {
		userUsage, _ = loadSkillUsageForDir(userDir)
	}
	var skills []SkillInfo
	seen := map[string]bool{}
	for _, root := range roots {
		if root.Writable {
			_ = os.MkdirAll(root.Path, 0755)
		}
		usage, _ := loadSkillUsageForDir(root.Path)
		for name, rec := range userUsage {
			if _, ok := usage[name]; !ok {
				usage[name] = rec
			}
		}
		entries, err := os.ReadDir(root.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			path := filepath.Join(root.Path, name)
			format := ""
			if entry.IsDir() {
				format = "dir"
			} else if strings.HasSuffix(name, ".md") {
				format = "file"
			}
			if format == "" {
				continue
			}
			info, ok := readSkillInfo(path, format, usage, root)
			if ok && !seen[normalizeSkillCommandName(info.Name)] {
				seen[normalizeSkillCommandName(info.Name)] = true
				skills = append(skills, info)
			}
		}
		if includeArchived && root.Writable {
			archiveDir := filepath.Join(root.Path, ".archive")
			archived, _ := listArchivedSkills(archiveDir, usage, root)
			skills = append(skills, archived...)
		}
	}
	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})
	return skills, nil
}

func SearchSkillsForTenant(tenantID, query string) ([]SkillInfo, error) {
	skills, err := ListSkillsForTenant(tenantID, false)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	var matches []SkillInfo
	for _, s := range skills {
		haystack := strings.ToLower(s.Name + "\n" + s.Description)
		content, _ := readSkillContent(s)
		if strings.Contains(haystack, q) || strings.Contains(strings.ToLower(content), q) {
			matches = append(matches, s)
		}
	}
	return matches, nil
}

func ReadSkillForTenant(tenantID, name string) (string, error) {
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	content, err := readSkillContent(info)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	writable := "read-only"
	if info.Writable {
		writable = "writable"
	}
	sb.WriteString(fmt.Sprintf("# %s\n\nPath: %s\nState: %s\nSource: %s\nScope: %s\nAccess: %s\n\n", info.Name, info.Path, info.State, info.Source, emptyDefault(info.Scope, "unknown"), writable))
	sb.WriteString(content)
	if info.Format == "dir" {
		files := listSupportFiles(info.Path)
		if len(files) > 0 {
			sb.WriteString("\n\n## Support files\n")
			for _, f := range files {
				sb.WriteString("- " + f + "\n")
			}
		}
	}
	return sb.String(), nil
}

func findSkill(tenantID, name string) (SkillInfo, error) {
	skills, err := ListSkillsForTenant(tenantID, false)
	if err != nil {
		return SkillInfo{}, err
	}
	safe := kernel.SanitizeSkillName(name)
	for _, s := range skills {
		if s.Name == name || s.Name == safe || kernel.SanitizeSkillName(s.Name) == safe {
			return s, nil
		}
	}
	return SkillInfo{}, fmt.Errorf("skill not found: %s", name)
}

func readSkillInfo(path, format string, usage map[string]SkillUsageRecord, root SkillRoot) (SkillInfo, bool) {
	contentPath := path
	if format == "dir" {
		contentPath = filepath.Join(path, "SKILL.md")
	}
	data, err := os.ReadFile(contentPath)
	if err != nil {
		return SkillInfo{}, false
	}
	def, _, _ := parseFrontMatter(string(data))
	name := def.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	rec := usage[name]
	if rec.Name == "" {
		rec = usage[kernel.SanitizeSkillName(name)]
	}
	state := rec.State
	if state == "" {
		state = SkillStateActive
	}
	source := rec.Source
	if source == "" {
		source = root.Source
	}
	if source == "" {
		source = SkillSourceManual
	}
	return SkillInfo{
		Name:        name,
		Description: def.Description,
		Path:        path,
		Format:      format,
		State:       state,
		Source:      source,
		Scope:       root.Scope,
		Root:        root.Path,
		Writable:    root.Writable,
		LastUsed:    rec.LastUsed,
		Pinned:      rec.Pinned,
	}, true
}

func listArchivedSkills(archiveDir string, usage map[string]SkillUsageRecord, root SkillRoot) ([]SkillInfo, error) {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return nil, err
	}
	var skills []SkillInfo
	for _, entry := range entries {
		path := filepath.Join(archiveDir, entry.Name())
		if entry.IsDir() {
			if info, ok := readSkillInfo(path, "dir", usage, root); ok {
				info.State = SkillStateArchived
				skills = append(skills, info)
			}
		} else if strings.HasSuffix(entry.Name(), ".md") {
			if info, ok := readSkillInfo(path, "file", usage, root); ok {
				info.State = SkillStateArchived
				skills = append(skills, info)
			}
		}
	}
	return skills, nil
}

func readSkillContent(info SkillInfo) (string, error) {
	path := info.Path
	if info.Format == "dir" {
		path = filepath.Join(info.Path, "SKILL.md")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func ensureDirectorySkill(tenantID string, info SkillInfo) (string, error) {
	if err := ensureWritableSkill(info, "converting it to a directory skill"); err != nil {
		return "", err
	}
	if info.Format == "dir" {
		return info.Path, nil
	}
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", err
	}
	safeName := kernel.SanitizeSkillName(info.Name)
	skillDir := filepath.Join(dir, safeName)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(info.Path)
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(filepath.Join(skillDir, "SKILL.md"), string(data)); err != nil {
		return "", err
	}
	if err := os.Remove(info.Path); err != nil {
		return "", err
	}
	return skillDir, nil
}

func safeSupportPath(skillDir, filePath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(filePath))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." {
		return "", fmt.Errorf("invalid support file path: %s", filePath)
	}
	parts := strings.Split(clean, string(os.PathSeparator))
	if len(parts) < 2 || !allowedSkillSubdirs[parts[0]] {
		return "", fmt.Errorf("support files must be under references/, templates/, scripts/, or assets/")
	}
	target := filepath.Join(skillDir, clean)
	rel, err := filepath.Rel(skillDir, target)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("support file path escapes skill directory")
	}
	return target, nil
}

func listSupportFiles(skillDir string) []string {
	var files []string
	for subdir := range allowedSkillSubdirs {
		root := filepath.Join(skillDir, subdir)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(skillDir, path)
			if err == nil {
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	sort.Strings(files)
	return files
}

func formatSkillsList(skills []SkillInfo) string {
	if len(skills) == 0 {
		return "No skills found."
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d skills:\n\n", len(skills)))
	for _, s := range skills {
		pin := ""
		if s.Pinned {
			pin = " pinned"
		}
		writable := "read-only"
		if s.Writable {
			writable = "writable"
		}
		sb.WriteString(fmt.Sprintf("- %s [%s/%s/%s/%s%s]: %s\n  %s\n", s.Name, s.State, s.Scope, s.Source, writable, pin, emptyDefault(s.Description, "(no description)"), s.Path))
	}
	return sb.String()
}

// ensureFrontMatter wraps raw content with YAML front matter if missing.
func ensureFrontMatter(content, name, description string) string {
	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		return content
	}
	if description == "" {
		description = name
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", kernel.SanitizeSkillName(name)))
	sb.WriteString(fmt.Sprintf("description: %s\n", description))
	sb.WriteString("---\n\n")
	sb.WriteString(content)
	return sb.String()
}

func emptyDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
