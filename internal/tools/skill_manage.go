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
						Description: "Action: list, search, read, create, update, edit, patch, delete, archive, write_file, remove_file, pin, or unpin.",
						Enum:        []string{"list", "search", "read", "create", "update", "edit", "patch", "delete", "archive", "write_file", "remove_file", "pin", "unpin"},
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
	filePath, _ := args["file_path"].(string)
	fileContent, _ := args["file_content"].(string)
	source, _ := args["source"].(string)

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
	case "create":
		return createSkill(tenantID, name, content, description, source)
	case "update", "edit":
		return editSkill(tenantID, name, content, description)
	case "patch":
		return patchSkill(tenantID, name, oldText, newText, filePath)
	case "delete":
		return deleteSkill(tenantID, name)
	case "archive":
		return ArchiveSkillForTenant(tenantID, name)
	case "write_file":
		return writeSkillSupportFile(tenantID, name, filePath, fileContent)
	case "remove_file":
		return removeSkillSupportFile(tenantID, name, filePath)
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
		return fmt.Sprintf("Skill %q unpinned.", info.Name), nil
	default:
		return "", fmt.Errorf("unknown action: %s", action)
	}
}

func getSkillsDir(tenantID string) (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("get home dir: %w", err)
		}
	}
	baseDir := filepath.Join(home, ".selfmind")
	dir := SkillsDirForTenant(baseDir, tenantID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create skills dir: %w", err)
	}
	return dir, nil
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
	content = ensureFrontMatter(content, info.Name, description)
	if err := kernel.ScanSkillForDangers(content); err != nil {
		return "", fmt.Errorf("security scan failed: %w", err)
	}
	target := info.Path
	if info.Format == "dir" {
		target = filepath.Join(info.Path, "SKILL.md")
	}
	if err := atomicWriteFile(target, content); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name)
	return fmt.Sprintf("Skill %q edited at %s", info.Name, target), nil
}

func patchSkill(tenantID, name, oldText, newText, filePath string) (string, error) {
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
	target := info.Path
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
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	current := string(data)
	if !strings.Contains(current, oldText) {
		return "", fmt.Errorf("old_text not found in %s", target)
	}
	updated := strings.Replace(current, oldText, newText, 1)
	if err := kernel.ScanSkillForDangers(updated); err != nil {
		return "", fmt.Errorf("security scan failed: %w", err)
	}
	if err := atomicWriteFile(target, updated); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name)
	return fmt.Sprintf("Skill %q patched at %s", info.Name, target), nil
}

func deleteSkill(tenantID, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for delete")
	}
	info, err := findSkill(tenantID, name)
	if err != nil {
		return "", err
	}
	if info.Pinned {
		return "", fmt.Errorf("skill %q is pinned; unpin it before deleting", info.Name)
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
	if err := os.Rename(info.Path, dest); err != nil {
		return "", err
	}
	_ = SetSkillState(tenantID, info.Name, SkillStateArchived)
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
	if err := atomicWriteFile(target, fileContent); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name)
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
	if info.Format != "dir" {
		return "", fmt.Errorf("skill %q has no support file directory", info.Name)
	}
	target, err := safeSupportPath(info.Path, filePath)
	if err != nil {
		return "", err
	}
	if err := os.Remove(target); err != nil {
		return "", err
	}
	_ = MarkSkillPatched(tenantID, info.Name)
	return fmt.Sprintf("Removed support file for skill %q: %s", info.Name, target), nil
}

func ListSkillsForTenant(tenantID string, includeArchived bool) ([]SkillInfo, error) {
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return nil, err
	}
	usage, _ := loadSkillUsageForDir(dir)
	var skills []SkillInfo
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		if entry.IsDir() {
			info, ok := readSkillInfo(path, "dir", usage)
			if ok {
				skills = append(skills, info)
			}
			continue
		}
		if strings.HasSuffix(name, ".md") {
			info, ok := readSkillInfo(path, "file", usage)
			if ok {
				skills = append(skills, info)
			}
		}
	}
	if includeArchived {
		archiveDir := filepath.Join(dir, ".archive")
		archived, _ := listArchivedSkills(archiveDir, usage)
		skills = append(skills, archived...)
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
	sb.WriteString(fmt.Sprintf("# %s\n\nPath: %s\nState: %s\nSource: %s\n\n", info.Name, info.Path, info.State, info.Source))
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

func readSkillInfo(path, format string, usage map[string]SkillUsageRecord) (SkillInfo, bool) {
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
		source = SkillSourceManual
	}
	return SkillInfo{
		Name:        name,
		Description: def.Description,
		Path:        path,
		Format:      format,
		State:       state,
		Source:      source,
		LastUsed:    rec.LastUsed,
		Pinned:      rec.Pinned,
	}, true
}

func listArchivedSkills(archiveDir string, usage map[string]SkillUsageRecord) ([]SkillInfo, error) {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return nil, err
	}
	var skills []SkillInfo
	for _, entry := range entries {
		path := filepath.Join(archiveDir, entry.Name())
		if entry.IsDir() {
			if info, ok := readSkillInfo(path, "dir", usage); ok {
				info.State = SkillStateArchived
				skills = append(skills, info)
			}
		} else if strings.HasSuffix(entry.Name(), ".md") {
			if info, ok := readSkillInfo(path, "file", usage); ok {
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
		sb.WriteString(fmt.Sprintf("- %s [%s/%s%s]: %s\n  %s\n", s.Name, s.State, s.Source, pin, emptyDefault(s.Description, "(no description)"), s.Path))
	}
	return sb.String()
}

func atomicWriteFile(path, content string) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write tmp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename file: %w", err)
	}
	return nil
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
