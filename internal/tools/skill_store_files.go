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

func getSkillsDir(tenantID string, invocation ...map[string]interface{}) (string, error) {
	root, err := ResolveWritableSkillRootForTenant(tenantID, invocation...)
	if err != nil {
		return "", err
	}
	return root.Path, nil
}

func createSkill(tenantID, name, content, description, source string, invocation ...map[string]interface{}) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for create")
	}
	if content == "" {
		return "", fmt.Errorf("content is required for create")
	}
	dir, err := getSkillsDir(tenantID, invocation...)
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
	if err := ValidateManagedSkillDescription(content); err != nil {
		return "", err
	}
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
	_ = MarkSkillCreated(tenantID, safeName, source, "skill_manage", invocation...)
	recordSkillLearningChange(tenantID, safeName, "create", "", content, source, invocation...)
	return fmt.Sprintf("Skill %q created at %s", safeName, target), nil
}

func editSkill(tenantID, name, content, description string, invocation ...map[string]interface{}) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for edit")
	}
	if content == "" {
		return "", fmt.Errorf("content is required for edit")
	}
	info, err := findSkill(tenantID, name, invocation...)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "editing it"); err != nil {
		return "", err
	}
	content = ensureFrontMatter(content, info.Name, description)
	if err := ValidateManagedSkillDescription(content); err != nil {
		return "", err
	}
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
	_ = MarkSkillPatched(tenantID, info.Name, invocation...)
	recordSkillLearningChange(tenantID, info.Name, "edit", string(beforeData), content, info.Source, invocation...)
	return fmt.Sprintf("Skill %q edited at %s", info.Name, target), nil
}

func patchSkill(tenantID, name, oldText, newText, filePath string, replaceAll bool, invocation ...map[string]interface{}) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for patch")
	}
	if oldText == "" {
		return "", fmt.Errorf("old_text is required for patch")
	}
	info, err := findSkill(tenantID, name, invocation...)
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
		skillDir, err := ensureDirectorySkill(tenantID, info, invocation...)
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
		if err := ValidateManagedSkillDescription(updated); err != nil {
			return "", err
		}
		if err := validateSkillEnvironmentDeclarations(updated); err != nil {
			return "", err
		}
	}
	if err := atomicWriteFile(target, updated); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name, invocation...)
	recordSkillLearningChange(tenantID, info.Name, auditAction, current, updated, info.Source, invocation...)
	return fmt.Sprintf("Skill %q patched at %s (%d replacement, strategy=%s)", info.Name, target, matches, strategy), nil
}

func deleteSkill(tenantID, name string, invocation ...map[string]interface{}) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for delete")
	}
	info, err := findSkill(tenantID, name, invocation...)
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
	_ = SetSkillState(tenantID, info.Name, SkillStateArchived, invocation...)
	recordSkillLearningChange(tenantID, info.Name, "delete", before, "", info.Source, invocation...)
	return fmt.Sprintf("Skill %q deleted.", info.Name), nil
}

func ArchiveSkillForTenant(tenantID, name string, invocation ...map[string]interface{}) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for archive")
	}
	info, err := findSkill(tenantID, name, invocation...)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "archiving it"); err != nil {
		return "", err
	}
	if info.Pinned {
		return "", fmt.Errorf("skill %q is pinned; unpin it before archiving", info.Name)
	}
	dir, err := getSkillsDir(tenantID, invocation...)
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
	_ = SetSkillState(tenantID, info.Name, SkillStateArchived, invocation...)
	recordSkillLearningChange(tenantID, info.Name, "archive", before, "archived to "+dest, info.Source, invocation...)
	return fmt.Sprintf("Skill %q archived to %s", info.Name, dest), nil
}

func writeSkillSupportFile(tenantID, name, filePath, fileContent string, invocation ...map[string]interface{}) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for write_file")
	}
	if filePath == "" {
		return "", fmt.Errorf("file_path is required for write_file")
	}
	info, err := findSkill(tenantID, name, invocation...)
	if err != nil {
		return "", err
	}
	if err := ensureWritableSkill(info, "writing support files"); err != nil {
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
	_ = MarkSkillPatched(tenantID, info.Name, invocation...)
	recordSkillLearningChange(tenantID, info.Name, "write_file:"+filepath.ToSlash(filePath), string(beforeData), fileContent, info.Source, invocation...)
	return fmt.Sprintf("Wrote support file for skill %q: %s", info.Name, target), nil
}

func removeSkillSupportFile(tenantID, name, filePath string, invocation ...map[string]interface{}) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for remove_file")
	}
	if filePath == "" {
		return "", fmt.Errorf("file_path is required for remove_file")
	}
	info, err := findSkill(tenantID, name, invocation...)
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
	_ = MarkSkillPatched(tenantID, info.Name, invocation...)
	recordSkillLearningChange(tenantID, info.Name, "remove_file:"+filepath.ToSlash(filePath), string(beforeData), "", info.Source, invocation...)
	return fmt.Sprintf("Removed support file for skill %q: %s", info.Name, target), nil
}

func ensureDirectorySkill(tenantID string, info SkillInfo, invocation ...map[string]interface{}) (string, error) {
	if err := ensureWritableSkill(info, "converting it to a directory skill"); err != nil {
		return "", err
	}
	if info.Format == "dir" {
		return info.Path, nil
	}
	dir, err := getSkillsDir(tenantID, invocation...)
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
