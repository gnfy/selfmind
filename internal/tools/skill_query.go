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
		scan, err := discoverSkillPaths(root)
		if err != nil {
			return nil, err
		}
		for _, found := range scan.Paths {
			info, ok := readSkillInfo(found.Path, found.Format, usage, root)
			if !ok {
				continue
			}
			info.PackageName = scan.PackageName
			key := skillPathKey(info.Path)
			if seen[key] {
				continue
			}
			seen[key] = true
			skills = append(skills, info)
		}
		if includeArchived && root.Writable {
			archiveDir := filepath.Join(root.Path, ".archive")
			archived, _ := listArchivedSkills(archiveDir, usage, root)
			skills = append(skills, archived...)
		}
	}
	sort.SliceStable(skills, func(i, j int) bool {
		if skills[i].Name != skills[j].Name {
			return skills[i].Name < skills[j].Name
		}
		return skills[i].Path < skills[j].Path
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

// findSkill resolves one Skill by bare or qualified name. A bare name resolves
// only when it is unambiguous: when several enabled Skills share it, resolution
// fails and names the qualified candidates rather than silently preferring one
// root. Descriptions never take part in the decision, because they are author
// text and on an external package that author is untrusted.
func findSkill(tenantID, name string, invocation ...map[string]interface{}) (SkillInfo, error) {
	skills, err := ListSkillsForTenant(tenantID, false, invocation...)
	if err != nil {
		return SkillInfo{}, err
	}
	matches := matchSkillsByName(skills, name)
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return SkillInfo{}, &skillNotFoundError{Name: name}
	default:
		return SkillInfo{}, &skillAmbiguousError{Name: name, Candidates: qualifiedSkillNames(matches)}
	}
}

// matchSkillsByName returns every Skill a bare name, a qualified name, or a
// discovery path selects. A path is the disambiguator of last resort: two roots
// can share both scope and source, so the qualified form is not guaranteed
// unique while the path always is.
func matchSkillsByName(skills []SkillInfo, name string) []SkillInfo {
	trimmed := strings.TrimSpace(name)
	if strings.ContainsRune(trimmed, filepath.Separator) || strings.Contains(trimmed, "/") {
		if matches := matchSkillsByPath(skills, filepath.FromSlash(trimmed)); len(matches) > 0 {
			return matches
		}
		// A path the person typed may route through a symlink while discovery
		// recorded the resolved location, which is ordinary on macOS where /tmp
		// and /var are links. Resolving is the fallback, not the hot path.
		if resolved, err := filepath.EvalSymlinks(filepath.FromSlash(trimmed)); err == nil {
			if matches := matchSkillsByPath(skills, resolved); len(matches) > 0 {
				return matches
			}
		}
	}
	if source, short, ok := splitQualifiedSkillName(trimmed); ok {
		safe := kernel.SanitizeSkillName(short)
		var matches []SkillInfo
		for _, s := range skills {
			if skillSourceLabel(s) != source {
				continue
			}
			if s.Name == short || kernel.SanitizeSkillName(s.Name) == safe {
				matches = append(matches, s)
			}
		}
		return matches
	}
	safe := kernel.SanitizeSkillName(trimmed)
	var matches []SkillInfo
	for _, s := range skills {
		if s.Name == trimmed || s.Name == safe || kernel.SanitizeSkillName(s.Name) == safe {
			matches = append(matches, s)
		}
	}
	return matches
}

// splitQualifiedSkillName splits a `source:name` reference. Skill names are
// sanitized to lowercase letters, digits, and hyphens, so a colon is an
// unambiguous separator.
func splitQualifiedSkillName(name string) (source, short string, ok bool) {
	idx := strings.Index(name, ":")
	if idx <= 0 || idx == len(name)-1 {
		return "", "", false
	}
	source = normalizeSkillCommandName(name[:idx])
	short = strings.TrimSpace(name[idx+1:])
	if source == "" || short == "" {
		return "", "", false
	}
	return source, short, true
}

// skillSourceLabel is the qualifier half of a qualified name: the
// manifest-declared package name when one governs the root, and the root scope
// otherwise. A relative path is deliberately not used, because it moves
// whenever a category directory is renamed.
func skillSourceLabel(info SkillInfo) string {
	source := strings.TrimSpace(info.PackageName)
	if source == "" && info.Provenance == SkillProvenanceExternal {
		source = SkillProvenanceExternal
	}
	if source == "" {
		source = info.Scope
	}
	if strings.TrimSpace(source) == "" {
		return "unknown"
	}
	return normalizeSkillCommandName(source)
}

// QualifiedSkillName renders the `source:name` form used to disambiguate.
func QualifiedSkillName(info SkillInfo) string {
	return skillSourceLabel(info) + ":" + kernel.SanitizeSkillName(info.Name)
}

// qualifiedSkillNames renders the candidate list for an ambiguity refusal. When
// two candidates share a qualified name their paths are appended, so every
// listed candidate is something the person can actually type back.
func qualifiedSkillNames(skills []SkillInfo) []string {
	counts := make(map[string]int, len(skills))
	for _, s := range skills {
		counts[QualifiedSkillName(s)]++
	}
	out := make([]string, 0, len(skills))
	for _, s := range skills {
		qualified := QualifiedSkillName(s)
		if counts[qualified] > 1 {
			qualified += " (" + filepath.ToSlash(s.Path) + ")"
		}
		out = append(out, qualified)
	}
	sort.Strings(out)
	return out
}

// primarySkillsByName keeps the highest-precedence Skill for each name. Model
// surfaces need one entry per name; listing and lookup keep every entry so a
// collision stays visible.
func primarySkillsByName(skills []SkillInfo) []SkillInfo {
	best := make(map[string]int, len(skills))
	out := make([]SkillInfo, 0, len(skills))
	for _, info := range skills {
		key := normalizeSkillCommandName(info.Name)
		if idx, ok := best[key]; ok {
			if info.Precedence < out[idx].Precedence {
				out[idx] = info
			}
			continue
		}
		best[key] = len(out)
		out = append(out, info)
	}
	return out
}

// findSkillByPrecedence resolves a name to the winning root without refusing a
// collision. Reference-based and stored-identity lookups use it because their
// own identity recheck, not a refusal, is what catches a root that has newly
// won precedence for the same name.
func findSkillByPrecedence(tenantID, name string, invocation ...map[string]interface{}) (SkillInfo, error) {
	skills, err := ListSkillsForTenant(tenantID, false, invocation...)
	if err != nil {
		return SkillInfo{}, err
	}
	matches := matchSkillsByName(skills, name)
	if len(matches) == 0 {
		return SkillInfo{}, &skillNotFoundError{Name: name}
	}
	winner := matches[0]
	for _, candidate := range matches[1:] {
		if candidate.Precedence < winner.Precedence {
			winner = candidate
		}
	}
	return winner, nil
}

func matchSkillsByPath(skills []SkillInfo, path string) []SkillInfo {
	key := skillPathKey(path)
	var matches []SkillInfo
	for _, s := range skills {
		if skillPathKey(s.Path) == key || skillPathKey(skillMainFilePath(s)) == key {
			matches = append(matches, s)
		}
	}
	return matches
}

func skillPathKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return strings.ToLower(filepath.Clean(abs))
}

type skillNotFoundError struct{ Name string }

func (e *skillNotFoundError) Error() string { return fmt.Sprintf("skill not found: %s", e.Name) }

// skillAmbiguousError reports a bare name that several Skills answer to.
type skillAmbiguousError struct {
	Name       string
	Candidates []string
}

func (e *skillAmbiguousError) Error() string {
	return fmt.Sprintf("skill name %q is ambiguous across %d Skills; use a qualified name: %s",
		e.Name, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

func readSkillInfo(path, format string, usage map[string]SkillUsageRecord, root SkillRoot) (SkillInfo, bool) {
	if format == "dir" && isDeveloperAgentOnlySkill(path) {
		return SkillInfo{}, false
	}
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
		Provenance:          root.Provenance,
		ModelInvocable:      def.ModelInvocation == nil || *def.ModelInvocation,
		Precedence:          root.Priority,
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
	shortNameCounts := make(map[string]int, len(skills))
	for _, s := range skills {
		shortNameCounts[normalizeSkillCommandName(s.Name)]++
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
		label := s.Name
		if shortNameCounts[normalizeSkillCommandName(s.Name)] > 1 {
			label = QualifiedSkillName(s)
		}
		sb.WriteString(fmt.Sprintf("- %s [%s/%s/%s/%s%s]: %s\n  %s\n", label, s.State, s.Scope, s.Source, writable, pin, emptyDefault(s.Description, "(no description)"), s.Path))
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
