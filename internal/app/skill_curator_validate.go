package app

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func validateCuratedSkillContent(content, name string) error {
	return validateCuratedSkillPackageShape(content, nil, name)
}

func validateCuratedSkillPackageShape(content string, resources map[string]string, name string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("curated Skill main must be non-empty")
	}
	if err := tools.ValidateManagedSkillDescription(content); err != nil {
		return fmt.Errorf("curated %w", err)
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
	total := len(content)
	for path, body := range resources {
		clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
		if clean == "." || strings.HasPrefix(clean, "../") || !strings.HasPrefix(clean, "references/") {
			return fmt.Errorf("curated Skill resources must use relative references/ paths: %q", path)
		}
		if strings.TrimSpace(body) == "" {
			return fmt.Errorf("curated Skill resource %q is empty", path)
		}
		total += len(body)
	}
	if total > 32*1024 {
		return fmt.Errorf("curated Skill package must be at most 32 KiB")
	}
	return nil
}

func curatedSkillDelivery(content string, resources map[string]string, budget kernel.RuntimeContextBudget) kernel.SkillMainDelivery {
	if budget.SkillMainBytes <= 0 || budget.SkillMainTokens <= 0 {
		budget = kernel.DefaultRuntimeContextBudget()
	}
	paths := make([]string, 0, len(resources))
	for path := range resources {
		paths = append(paths, filepath.ToSlash(filepath.Clean(strings.TrimSpace(path))))
	}
	sort.Strings(paths)
	return kernel.BuildSkillMainDeliveryWithinBudget(content,
		kernel.ActiveSkillDeliveryBodyBudget(budget.SkillMainBytes, paths), budget.SkillMainTokens)
}

func validateCuratedSkillCreateDelivery(content string, resources map[string]string, budget kernel.RuntimeContextBudget) error {
	delivery := curatedSkillDelivery(content, resources, budget)
	if delivery.Mode != kernel.SkillDeliveryModeFull {
		return fmt.Errorf("curated Skill main must be delivered in full under the production byte, token, envelope, and resource-manifest budget; move optional detail into references/ resources")
	}
	return nil
}

func validateCuratedSkillPatchDelivery(active, candidate string, resources map[string]string, budget kernel.RuntimeContextBudget) error {
	activeDelivery := curatedSkillDelivery(active, resources, budget)
	candidateDelivery := curatedSkillDelivery(candidate, resources, budget)
	if activeDelivery.Mode == kernel.SkillDeliveryModeFull {
		if candidateDelivery.Mode != kernel.SkillDeliveryModeFull {
			return fmt.Errorf("curator PATCH cannot make a fully delivered Skill main require paging")
		}
		return nil
	}
	if candidateDelivery.SourceBytes > activeDelivery.SourceBytes || candidateDelivery.SourceTokens > activeDelivery.SourceTokens {
		return fmt.Errorf("curator PATCH cannot grow an already paged legacy Skill main: bytes %d>%d or tokens %d>%d",
			candidateDelivery.SourceBytes, activeDelivery.SourceBytes, candidateDelivery.SourceTokens, activeDelivery.SourceTokens)
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
