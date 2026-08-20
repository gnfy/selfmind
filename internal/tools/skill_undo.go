package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/kernel"
)

func UndoSkillLearningChangeForTenant(tenantID, changeID string, invocation ...map[string]interface{}) (string, error) {
	change, err := GetLearningChangeByID(tenantID, changeID, invocation...)
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
		info, err := findSkill(tenantID, name, invocation...)
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
		_ = SetSkillState(tenantID, info.Name, SkillStateArchived, invocation...)
		recordSkillLearningChange(tenantID, info.Name, "undo:create", current, "", info.Source, invocation...)
		return fmt.Sprintf("Undid skill create `%s`: removed %q.", change.ID, info.Name), nil
	case change.Action == "edit" || change.Action == "patch":
		if strings.TrimSpace(change.Before) == "" {
			return "", fmt.Errorf("learning change %s has no previous skill content to restore", change.ID)
		}
		info, err := findSkill(tenantID, name, invocation...)
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
		_ = MarkSkillPatched(tenantID, info.Name, invocation...)
		recordSkillLearningChange(tenantID, info.Name, "undo:"+change.Action, string(current), change.Before, info.Source, invocation...)
		return fmt.Sprintf("Undid skill %s `%s`: restored %q.", change.Action, change.ID, info.Name), nil
	case strings.HasPrefix(change.Action, "patch:"):
		filePath := strings.TrimPrefix(change.Action, "patch:")
		return undoSkillSupportFileChange(tenantID, name, change, filePath, true, invocation...)
	case strings.HasPrefix(change.Action, "write_file:"):
		filePath := strings.TrimPrefix(change.Action, "write_file:")
		return undoSkillSupportFileChange(tenantID, name, change, filePath, true, invocation...)
	case strings.HasPrefix(change.Action, "remove_file:"):
		filePath := strings.TrimPrefix(change.Action, "remove_file:")
		return undoSkillSupportFileChange(tenantID, name, change, filePath, true, invocation...)
	case change.Action == "delete" || change.Action == "archive":
		if strings.TrimSpace(change.Before) == "" {
			return "", fmt.Errorf("learning change %s has no previous skill content to restore", change.ID)
		}
		target, info, err := activeSkillMainPathForRestore(tenantID, name, invocation...)
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
		_ = MarkSkillCreated(tenantID, name, emptyDefault(info.Source, change.Source), "learning_undo", invocation...)
		_ = SetSkillState(tenantID, name, SkillStateActive, invocation...)
		recordSkillLearningChange(tenantID, name, "undo:"+change.Action, string(current), change.Before, emptyDefault(info.Source, change.Source), invocation...)
		return fmt.Sprintf("Undid skill %s `%s`: restored %q.", change.Action, change.ID, name), nil
	case change.Action == "pin":
		if err := SetSkillPinned(tenantID, name, false, invocation...); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, name, "undo:pin", "pinned", "unpinned", change.Source, invocation...)
		return fmt.Sprintf("Undid skill pin `%s`: unpinned %q.", change.ID, name), nil
	case change.Action == "unpin":
		if err := SetSkillPinned(tenantID, name, true, invocation...); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, name, "undo:unpin", "unpinned", "pinned", change.Source, invocation...)
		return fmt.Sprintf("Undid skill unpin `%s`: pinned %q.", change.ID, name), nil
	case change.Action == "enable" || change.Action == "disable":
		state := strings.TrimSpace(change.Before)
		if state == "" {
			state = SkillStateActive
		}
		if err := SetSkillState(tenantID, name, state, invocation...); err != nil {
			return "", err
		}
		recordSkillLearningChange(tenantID, name, "undo:"+change.Action, change.After, state, change.Source, invocation...)
		return fmt.Sprintf("Undid skill %s `%s`: restored %q to %s.", change.Action, change.ID, name, state), nil
	default:
		return "", fmt.Errorf("skill learning action %q cannot be undone", change.Action)
	}
}

func undoSkillSupportFileChange(tenantID, name string, change *LearningChange, filePath string, restoreBefore bool, invocation ...map[string]interface{}) (string, error) {
	info, err := findSkill(tenantID, name, invocation...)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "undoing support file changes"); err != nil {
		return "", err
	}
	skillDir, err := ensureDirectorySkill(tenantID, info, invocation...)
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
		recordSkillLearningChange(tenantID, info.Name, "undo:"+change.Action, string(current), change.Before, info.Source, invocation...)
		return fmt.Sprintf("Undid skill support-file change `%s`: restored %s.", change.ID, filepath.ToSlash(filePath)), nil
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	recordSkillLearningChange(tenantID, info.Name, "undo:"+change.Action, string(current), "", info.Source, invocation...)
	return fmt.Sprintf("Undid skill support-file change `%s`: removed %s.", change.ID, filepath.ToSlash(filePath)), nil
}

func skillMainFilePath(info SkillInfo) string {
	if info.Format == "dir" {
		return filepath.Join(info.Path, "SKILL.md")
	}
	return info.Path
}

func activeSkillMainPathForRestore(tenantID, name string, invocation ...map[string]interface{}) (string, SkillInfo, error) {
	if info, err := findSkill(tenantID, name, invocation...); err == nil {
		if info.Writable {
			return skillMainFilePath(info), info, nil
		}
	}
	dir, err := getSkillsDir(tenantID, invocation...)
	if err != nil {
		return "", SkillInfo{}, err
	}
	safeName := kernel.SanitizeSkillName(name)
	path := filepath.Join(dir, safeName, "SKILL.md")
	return path, SkillInfo{Name: safeName, Path: filepath.Dir(path), Format: "dir", Source: SkillSourceAgentCreated, Scope: SkillScopeUser, Root: dir, Writable: true}, nil
}
