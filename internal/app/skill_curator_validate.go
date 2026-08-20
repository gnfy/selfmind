package app

import (
	"fmt"
	"strings"

	"selfmind/internal/control"
)

func validateCuratedSkillContent(content, name string) error {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > 32*1024 {
		return fmt.Errorf("curated skill content must be non-empty and at most 32 KiB")
	}
	lower := strings.ToLower(content)
	if !strings.HasPrefix(content, "---\n") || !strings.Contains(lower, "name: "+strings.ToLower(name)) {
		return fmt.Errorf("curated skill content must start with front matter for %s", name)
	}
	sectionOrder := control.CanonicalSkillSectionOrder()
	for _, heading := range sectionOrder {
		if !strings.Contains(lower, "# "+heading) && !strings.Contains(lower, "## "+heading) {
			return fmt.Errorf("curated skill content is missing %s heading", heading)
		}
	}
	sections := splitCuratedSkillSections(content)
	if sections.duplicate || strings.Join(sections.order, "\x00") != strings.Join(sectionOrder, "\x00") {
		return fmt.Errorf("curated skill content must contain each required level-two section exactly once and in canonical order")
	}
	return nil
}

func validateNarrowSkillRepair(active, candidate string, changedSections []string) error {
	if strings.TrimSpace(active) == "" {
		return fmt.Errorf("curator PATCH requires the active Skill content")
	}
	if len(changedSections) == 0 || len(changedSections) > 3 {
		return fmt.Errorf("curator PATCH must declare one to three changed_sections")
	}
	allowedHeadings := make(map[string]bool, len(control.CanonicalSkillSectionOrder()))
	for _, heading := range control.CanonicalSkillSectionOrder() {
		allowedHeadings[heading] = true
	}
	declared := map[string]bool{}
	for _, heading := range changedSections {
		heading = strings.ToLower(strings.TrimSpace(heading))
		if !allowedHeadings[heading] || declared[heading] {
			return fmt.Errorf("curator PATCH declared invalid or duplicate changed section %q", heading)
		}
		declared[heading] = true
	}
	activeSections := splitCuratedSkillSections(active)
	candidateSections := splitCuratedSkillSections(candidate)
	if activeSections.prefix != candidateSections.prefix {
		return fmt.Errorf("curator PATCH changed front matter or content outside level-two sections")
	}
	if strings.Join(activeSections.order, "\x00") != strings.Join(candidateSections.order, "\x00") {
		return fmt.Errorf("curator PATCH changed the Skill section topology")
	}
	changed := 0
	for _, heading := range activeSections.order {
		if activeSections.headings[heading] != candidateSections.headings[heading] {
			return fmt.Errorf("curator PATCH changed section heading %q", heading)
		}
		before, after := activeSections.bodies[heading], candidateSections.bodies[heading]
		if before == after {
			continue
		}
		if !declared[heading] {
			return fmt.Errorf("curator PATCH changed undeclared section %q", heading)
		}
		changed++
	}
	if changed == 0 {
		return fmt.Errorf("curator PATCH did not change any declared section")
	}
	if changed != len(declared) {
		return fmt.Errorf("curator PATCH declared a section that was not changed")
	}
	return nil
}

func validateRepairIncidentCoverage(digest control.SkillEvidenceDigest, changedSections []string) error {
	declared := map[string]bool{}
	for _, heading := range changedSections {
		declared[strings.ToLower(strings.TrimSpace(heading))] = true
	}
	required := map[string]bool{}
	for _, observation := range digest.NegativeObservations {
		if !verifiedRepairObservation(observation) {
			continue
		}
		required[repairSectionForFailedStep(observation.Incident.FailedStepID)] = true
	}
	for heading := range required {
		if !declared[heading] {
			return fmt.Errorf("curator PATCH must include failed section %q in changed_sections", heading)
		}
	}
	return nil
}

func repairSectionForFailedStep(failedStepID string) string {
	step := strings.ToLower(strings.TrimSpace(failedStepID))
	aliases := map[string]string{
		"applicability":  "applicability",
		"input":          "inputs",
		"inputs":         "inputs",
		"precondition":   "preconditions",
		"preconditions":  "preconditions",
		"procedure":      "procedure",
		"failure guard":  "failure guards",
		"failure guards": "failure guards",
		"recovery":       "recovery",
		"verification":   "verification",
	}
	if heading := aliases[step]; heading != "" {
		return heading
	}
	return "procedure"
}

type curatedSkillSections struct {
	prefix    string
	order     []string
	headings  map[string]string
	bodies    map[string]string
	duplicate bool
}

func splitCuratedSkillSections(content string) curatedSkillSections {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	result := curatedSkillSections{headings: map[string]string{}, bodies: map[string]string{}}
	var prefix []string
	bodyLines := map[string][]string{}
	current := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			current = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "## ")))
			if _, exists := result.headings[current]; exists {
				result.duplicate = true
			}
			result.order = append(result.order, current)
			result.headings[current] = line
			bodyLines[current] = []string{}
			continue
		}
		if current == "" {
			prefix = append(prefix, line)
			continue
		}
		bodyLines[current] = append(bodyLines[current], line)
	}
	result.prefix = strings.Join(prefix, "\n")
	for heading, body := range bodyLines {
		result.bodies[heading] = strings.Join(body, "\n")
	}
	return result
}
