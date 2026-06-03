package tools

import (
	"fmt"
	"strings"
	"time"
)

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
	if staleAfterDays <= 0 {
		staleAfterDays = 30
	}
	if archiveAfterDays <= 0 {
		archiveAfterDays = 90
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
		case idleDays >= archiveAfterDays:
			if _, err := ArchiveSkillForTenant(tenantID, s.Name); err != nil {
				lines = append(lines, fmt.Sprintf("- %s archive failed: %v", s.Name, err))
				continue
			}
			archivedCount++
			lines = append(lines, fmt.Sprintf("- archived %s (%d idle days)", s.Name, idleDays))
		case idleDays >= staleAfterDays && s.State != SkillStateStale:
			_ = SetSkillState(tenantID, s.Name, SkillStateStale)
			staleCount++
			lines = append(lines, fmt.Sprintf("- marked stale %s (%d idle days)", s.Name, idleDays))
		default:
			skippedCount++
		}
	}

	var sb strings.Builder
	sb.WriteString("## Skill Curator Run\n\n")
	sb.WriteString(fmt.Sprintf("- stale: %d\n- archived: %d\n- skipped: %d\n", staleCount, archivedCount, skippedCount))
	if len(lines) > 0 {
		sb.WriteString("\n")
		sb.WriteString(strings.Join(lines, "\n"))
	}
	return sb.String(), nil
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
