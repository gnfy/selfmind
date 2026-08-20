package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"selfmind/internal/kernel"
)

func ListSkillsForTenant(tenantID string, includeArchived bool, invocation ...map[string]interface{}) ([]SkillInfo, error) {
	roots, err := SkillRootsForTenant(tenantID, invocation...)
	if err != nil {
		return nil, err
	}
	userUsage := map[string]SkillUsageRecord{}
	if userDir, err := userSkillsDirForTenant(tenantID, invocation...); err == nil {
		userUsage, _ = loadSkillUsageForDir(userDir)
	}
	var skills []SkillInfo
	seen := map[string]bool{}
	for _, root := range roots {
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

func SearchSkillsForTenant(tenantID, query string, invocation ...map[string]interface{}) ([]SkillInfo, error) {
	matches, _, err := searchSkillsForTenantDetailed(tenantID, query, invocation...)
	return matches, err
}

func searchSkillsForTenantDetailed(tenantID, query string, invocation ...map[string]interface{}) ([]SkillInfo, int, error) {
	skills, err := ListSkillsForTenant(tenantID, false, invocation...)
	if err != nil {
		return nil, 0, err
	}
	ranked := rankSkillsBM25F(query, skills, len(skills))
	total := len(ranked)
	if len(ranked) > maxSkillSearchResults {
		ranked = ranked[:maxSkillSearchResults]
	}
	return ranked, total, nil
}

func ReadSkillForTenant(tenantID, name string, invocation ...map[string]interface{}) (string, error) {
	info, err := findSkill(tenantID, name, invocation...)
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

func findSkill(tenantID, name string, invocation ...map[string]interface{}) (SkillInfo, error) {
	skills, err := ListSkillsForTenant(tenantID, false, invocation...)
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
		Name:                name,
		Description:         def.Description,
		Path:                path,
		Format:              format,
		State:               state,
		Source:              source,
		Scope:               root.Scope,
		Root:                root.Path,
		Writable:            root.Writable,
		LastUsed:            rec.LastUsed,
		Pinned:              rec.Pinned,
		GovernanceNotBefore: rec.GovernanceNotBefore,
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

func formatSkillSearchResults(skills []SkillInfo, total int) string {
	formatted := formatSkillsList(skills)
	if total <= len(skills) {
		return formatted
	}
	return fmt.Sprintf("Showing %d of %d matching skills. Refine the query to see omitted matches.\n\n%s", len(skills), total, formatted)
}
