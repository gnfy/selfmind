package kernel

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"selfmind/internal/platform/textutil"
)

const (
	SkillDeliveryContractV1 = 1
	SkillDeliveryModeFull   = "full"
	SkillDeliveryModePaged  = "paged"

	minSkillMainTokens       = 512
	maxSkillMainTokens       = 2048
	maxSkillMainBytes        = 8 * 1024
	maxSkillCatalogBytes     = 8000
	fallbackSkillSliceTokens = 512
)

// SkillMainDelivery is the immutable model-visible main page fixed when a
// Skill is activated. SourceBytes describes the instruction body after YAML
// front matter is removed; DeliveredHash always covers Content exactly.
type SkillMainDelivery struct {
	ContractVersion int
	Mode            string
	Content         string
	DeliveredHash   string
	DeliveredBytes  int
	SourceBytes     int
	SourceTokens    int
	Sections        []string
}

// ValidateSkillMainDeliveryReceipt verifies the immutable bytes fixed when an
// activation is created. It is deliberately independent of the stored source:
// the receipt describes what the model actually receives, not what happened to
// be present on disk before or after activation.
func ValidateSkillMainDeliveryReceipt(contractVersion int, mode, content, deliveredHash string, deliveredBytes int) error {
	if contractVersion != SkillDeliveryContractV1 {
		return fmt.Errorf("unsupported Skill delivery contract version %d", contractVersion)
	}
	if mode != SkillDeliveryModeFull && mode != SkillDeliveryModePaged {
		return fmt.Errorf("invalid Skill delivery mode %q", mode)
	}
	if content == "" {
		return fmt.Errorf("Skill delivered main is empty")
	}
	if deliveredBytes != len(content) {
		return fmt.Errorf("Skill delivered byte receipt is %d, actual %d", deliveredBytes, len(content))
	}
	digest := sha256.Sum256([]byte(content))
	actualHash := fmt.Sprintf("%x", digest[:])
	if strings.TrimSpace(deliveredHash) != actualHash {
		return fmt.Errorf("Skill delivered hash receipt does not match delivered main")
	}
	return nil
}

// RuntimeContextBudgetForContextTokens keeps Skill presentation proportional
// to the provider's usable context while preserving the existing 8 KiB budget
// for non-Skill runtime context. Unknown model metadata uses an explicit 512
// token fallback; it is never silently treated as a 128K model.
func RuntimeContextBudgetForContextTokens(contextTokens int) RuntimeContextBudget {
	mainTokens := fallbackSkillSliceTokens
	catalogTokens := fallbackSkillSliceTokens
	if contextTokens > 0 {
		mainTokens = contextTokens * 3 / 100
		if mainTokens < minSkillMainTokens {
			mainTokens = minSkillMainTokens
		}
		if mainTokens > maxSkillMainTokens {
			mainTokens = maxSkillMainTokens
		}
		catalogTokens = contextTokens * 2 / 100
		if catalogTokens < 1 {
			catalogTokens = 1
		}
	}
	mainBytes := mainTokens * 4
	if mainBytes > maxSkillMainBytes {
		mainBytes = maxSkillMainBytes
	}
	catalogBytes := catalogTokens * 4
	if catalogBytes > maxSkillCatalogBytes {
		catalogBytes = maxSkillCatalogBytes
	}
	skillPeak := mainBytes
	if catalogBytes > skillPeak {
		skillPeak = catalogBytes
	}
	return RuntimeContextBudget{
		TotalChars:         8000 + skillPeak,
		WorkspaceChars:     700,
		TaskChars:          2500,
		MemoryChars:        700,
		SkillMainTokens:    mainTokens,
		SkillMainBytes:     mainBytes,
		SkillCatalogTokens: catalogTokens,
		SkillCatalogBytes:  catalogBytes,
	}
}

// SkillInstructionBody removes metadata-only YAML front matter. Version and
// package hashes continue to cover stored source; this body is the authority
// actually delivered to the model under contract v1.
func SkillInstructionBody(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return strings.TrimSpace(content)
	}
	if end := strings.Index(content[4:], "\n---\n"); end >= 0 {
		return strings.TrimSpace(content[4+end+5:])
	}
	return strings.TrimSpace(content)
}

// ActiveSkillDeliveryBodyBudget reserves a deterministic envelope and linked
// resource manifest inside the Skill slice. Callers build the delivery once;
// Prompt then reproduces those exact bytes instead of truncating per turn.
func ActiveSkillDeliveryBodyBudget(skillSliceChars int, linkedFiles []string) int {
	if skillSliceChars <= 0 {
		skillSliceChars = maxSkillMainBytes
	}
	reserve := 256
	for index, file := range linkedFiles {
		if index >= 12 {
			break
		}
		reserve += len(textutil.TruncateBytes(strings.TrimSpace(file), 160)) + 4
	}
	budget := skillSliceChars - reserve
	if budget < 512 {
		budget = 512
	}
	return budget
}

// BuildSkillMainDelivery returns either the complete instruction body or an
// explicit page index. Oversized instructions are never presented as though a
// truncated prefix were complete; skill_view supplies the named/offset pages.
func BuildSkillMainDelivery(content string, maxBodyBytes int) SkillMainDelivery {
	return BuildSkillMainDeliveryWithinBudget(content, maxBodyBytes, 0)
}

// BuildSkillMainDeliveryWithinBudget enforces both the estimated-token limit
// and the exact UTF-8 byte ceiling. The complete body is used only when it fits
// both; otherwise the result is an explicit page index.
func BuildSkillMainDeliveryWithinBudget(content string, maxBodyBytes, maxBodyTokens int) SkillMainDelivery {
	if maxBodyBytes <= 0 {
		maxBodyBytes = maxSkillMainBytes
	}
	body := SkillInstructionBody(content)
	delivery := SkillMainDelivery{
		ContractVersion: SkillDeliveryContractV1,
		Mode:            SkillDeliveryModeFull,
		Content:         body,
		SourceBytes:     len(body),
		SourceTokens:    estimateTokens(body),
	}
	if len(body) > maxBodyBytes || (maxBodyTokens > 0 && delivery.SourceTokens > maxBodyTokens) {
		delivery.Mode = SkillDeliveryModePaged
		delivery.Sections = skillLevelTwoSections(body)
		delivery.Content = renderPagedSkillIndex(len(body), maxBodyBytes, maxBodyTokens, delivery.Sections)
	}
	digest := sha256.Sum256([]byte(delivery.Content))
	delivery.DeliveredHash = fmt.Sprintf("%x", digest[:])
	delivery.DeliveredBytes = len(delivery.Content)
	return delivery
}

// SkillTextTokens exposes the same deterministic estimate used by Skill
// delivery so aggregate surfaces and curator validation cannot drift onto a
// separate token heuristic.
func SkillTextTokens(content string) int {
	return estimateTokens(content)
}

func skillLevelTwoSections(body string) []string {
	var sections []string
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimPrefix(line, "## "))
		key := strings.ToLower(heading)
		if heading == "" || seen[key] {
			continue
		}
		seen[key] = true
		sections = append(sections, heading)
	}
	return sections
}

func renderPagedSkillIndex(sourceBytes, maxBodyBytes, maxBodyTokens int, sections []string) string {
	var b strings.Builder
	b.WriteString("[PAGED SKILL MAIN]\n")
	fmt.Fprintf(&b, "The stored instruction body is %d bytes and exceeds this activation's %d-byte main-page budget. No instruction prefix is being treated as complete. Load the required pages with skill_view before applying them.\n", sourceBytes, maxBodyBytes)
	if len(sections) == 0 {
		b.WriteString("This document has no level-two section boundaries. Read it with skill_view using offset_bytes and limit_bytes, starting at offset 0.\n")
	} else {
		b.WriteString("Available section pages:\n")
		omitted := 0
		for _, section := range sections {
			line := fmt.Sprintf("- %s (skill_view section=%q)\n", section, section)
			if !skillTextFits(b.String()+line, maxBodyBytes, maxBodyTokens, 64) {
				omitted++
				continue
			}
			b.WriteString(line)
		}
		if omitted > 0 {
			fmt.Fprintf(&b, "- %d additional section name(s) omitted from this index; use skill_view with offset paging to inspect the full main body.\n", omitted)
		}
	}
	result := strings.TrimSpace(b.String())
	if !skillTextFits(result, maxBodyBytes, maxBodyTokens, 0) {
		// The fallback is intentionally a complete actionable sentence, never a
		// prefix masquerading as a complete Skill or a cut-off page index.
		result = "[PAGED SKILL MAIN]\nLoad this Skill main with skill_view using offset_bytes and limit_bytes, starting at offset 0."
		if !skillTextFits(result, maxBodyBytes, maxBodyTokens, 0) {
			return ""
		}
	}
	return result
}

func skillTextFits(value string, maxBytes, maxTokens, reserveBytes int) bool {
	if maxBytes > 0 && len(value)+reserveBytes > maxBytes {
		return false
	}
	return maxTokens <= 0 || estimateTokens(value) <= maxTokens
}

// SkillSectionPage returns one exact level-two section, including its heading.
func SkillSectionPage(content, requested string) (string, bool) {
	body := SkillInstructionBody(content)
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return body, true
	}
	lines := strings.Split(body, "\n")
	start := -1
	for index, line := range lines {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimPrefix(line, "## "))
		if start < 0 {
			if strings.EqualFold(heading, requested) {
				start = index
			}
			continue
		}
		return strings.TrimSpace(strings.Join(lines[start:index], "\n")), true
	}
	if start >= 0 {
		return strings.TrimSpace(strings.Join(lines[start:], "\n")), true
	}
	return "", false
}
