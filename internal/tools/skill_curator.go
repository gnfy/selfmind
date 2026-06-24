package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type CuratorOptions struct {
	StaleAfterDays   int
	ArchiveAfterDays int
	DryRun           bool
	WriteReport      bool
}

// CuratorStatusForTenant reports lifecycle metadata without mutating skills.
func CuratorStatusForTenant(tenantID string) (string, error) {
	skills, err := ListSkillsForTenant(tenantID, true)
	if err != nil {
		return "", err
	}
	if len(skills) == 0 {
		return "No skills found.", nil
	}
	counts := map[string]int{}
	var sb strings.Builder
	sb.WriteString("## Skill Curator Status\n\n")
	for _, s := range skills {
		counts[s.State]++
	}
	sb.WriteString(fmt.Sprintf("- active: %d\n- stale: %d\n- archived: %d\n\n", counts[SkillStateActive], counts[SkillStateStale], counts[SkillStateArchived]))
	for _, s := range skills {
		pin := ""
		if s.Pinned {
			pin = " pinned"
		}
		sb.WriteString(fmt.Sprintf("- %s [%s/%s%s] last_used=%s\n", s.Name, s.State, s.Source, pin, emptyDefault(s.LastUsed, "never")))
	}
	return sb.String(), nil
}

// RunCuratorForTenant marks idle agent-created skills stale and archives old ones.
func RunCuratorForTenant(tenantID string, staleAfterDays, archiveAfterDays int) (string, error) {
	return RunCuratorForTenantWithOptions(tenantID, CuratorOptions{
		StaleAfterDays:   staleAfterDays,
		ArchiveAfterDays: archiveAfterDays,
	})
}

func RunCuratorForTenantWithOptions(tenantID string, opts CuratorOptions) (string, error) {
	if opts.StaleAfterDays <= 0 {
		opts.StaleAfterDays = 30
	}
	if opts.ArchiveAfterDays <= 0 {
		opts.ArchiveAfterDays = 90
	}
	skills, err := ListSkillsForTenant(tenantID, false)
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	staleCount := 0
	archivedCount := 0
	skippedCount := 0
	var lines []string

	for _, s := range skills {
		if s.State == SkillStateDisabled {
			skippedCount++
			continue
		}
		if s.Source != SkillSourceAgentCreated {
			skippedCount++
			continue
		}
		if s.Pinned {
			skippedCount++
			continue
		}
		last := parseSkillActivityTime(s)
		if last.IsZero() {
			// Newly-created but never-used skills get a grace period from UpdatedAt in sidecar.
			skippedCount++
			continue
		}
		idleDays := int(now.Sub(last).Hours() / 24)
		switch {
		case idleDays >= opts.ArchiveAfterDays:
			if opts.DryRun {
				archivedCount++
				lines = append(lines, fmt.Sprintf("- would archive %s (%d idle days)", s.Name, idleDays))
			} else {
				if _, err := ArchiveSkillForTenant(tenantID, s.Name); err != nil {
					lines = append(lines, fmt.Sprintf("- %s archive failed: %v", s.Name, err))
					continue
				}
				archivedCount++
				lines = append(lines, fmt.Sprintf("- archived %s (%d idle days)", s.Name, idleDays))
			}
		case idleDays >= opts.StaleAfterDays && s.State != SkillStateStale:
			if opts.DryRun {
				lines = append(lines, fmt.Sprintf("- would mark stale %s (%d idle days)", s.Name, idleDays))
			} else {
				_ = SetSkillState(tenantID, s.Name, SkillStateStale)
				lines = append(lines, fmt.Sprintf("- marked stale %s (%d idle days)", s.Name, idleDays))
			}
			staleCount++
		default:
			skippedCount++
		}
	}

	var sb strings.Builder
	sb.WriteString("## Skill Curator Run\n\n")
	if opts.DryRun {
		sb.WriteString("Mode: dry-run (no files changed)\n\n")
	}
	sb.WriteString(fmt.Sprintf("- stale: %d\n- archived: %d\n- skipped: %d\n", staleCount, archivedCount, skippedCount))
	if len(lines) > 0 {
		sb.WriteString("\n")
		sb.WriteString(strings.Join(lines, "\n"))
	}
	suggestions := CuratorConsolidationSuggestions(skills)
	if len(suggestions) > 0 {
		sb.WriteString("\n\n### Consolidation Suggestions\n")
		sb.WriteString(strings.Join(suggestions, "\n"))
	}
	if opts.WriteReport {
		path, err := WriteCuratorReportForTenant(tenantID, sb.String())
		if err != nil {
			sb.WriteString(fmt.Sprintf("\n\nReport write failed: %v", err))
		} else {
			sb.WriteString(fmt.Sprintf("\n\nReport: %s", path))
		}
	}
	return sb.String(), nil
}

func RestoreSkillForTenant(tenantID, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required for restore")
	}
	dir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", err
	}
	usage, _ := loadSkillUsageForDir(dir)
	archiveDir := filepath.Join(dir, ".archive")
	archived, err := listArchivedSkills(archiveDir, usage, SkillRoot{
		Path:     dir,
		Scope:    SkillScopeUser,
		Source:   SkillSourceManual,
		Writable: true,
	})
	if err != nil {
		return "", err
	}
	safe := strings.ToLower(name)
	for _, s := range archived {
		if strings.ToLower(s.Name) != safe && strings.ToLower(filepath.Base(s.Path)) != safe {
			continue
		}
		dest := filepath.Join(dir, filepath.Base(s.Path))
		if _, err := os.Stat(dest); err == nil {
			return "", fmt.Errorf("restore destination already exists: %s", dest)
		}
		if err := os.Rename(s.Path, dest); err != nil {
			return "", err
		}
		_ = SetSkillState(tenantID, s.Name, SkillStateActive)
		return fmt.Sprintf("Skill %q restored to %s", s.Name, dest), nil
	}
	return "", fmt.Errorf("archived skill not found: %s", name)
}

func CuratorConsolidationSuggestions(skills []SkillInfo) []string {
	groups := map[string][]string{}
	for _, s := range skills {
		if s.Source != SkillSourceAgentCreated || s.State == SkillStateArchived {
			continue
		}
		prefix := skillClusterPrefix(s.Name)
		if prefix == "" {
			continue
		}
		groups[prefix] = append(groups[prefix], s.Name)
	}
	var out []string
	for prefix, names := range groups {
		if len(names) < 2 {
			continue
		}
		out = append(out, fmt.Sprintf("- `%s-*`: consider one umbrella skill with sections for %s", prefix, strings.Join(names, ", ")))
	}
	return out
}

func skillClusterPrefix(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, sep := range []string{"-", "_", "."} {
		if idx := strings.Index(name, sep); idx > 2 {
			return name[:idx]
		}
	}
	return ""
}

func WriteCuratorReportForTenant(tenantID, content string) (string, error) {
	skillsDir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", err
	}
	root := filepath.Join(filepath.Dir(skillsDir), "logs", "curator", time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(root, "REPORT.md")
	if err := atomicWriteFile(path, content); err != nil {
		return "", err
	}
	return path, nil
}

func parseSkillActivityTime(s SkillInfo) time.Time {
	if s.LastUsed == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s.LastUsed)
	if err != nil {
		return time.Time{}
	}
	return t
}
