package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/textutil"
)

const memoryOverviewSectionLimit = 6

type memoryViewSection struct {
	key   string
	title string
}

var memoryViewSections = []memoryViewSection{
	{key: "pinned", title: "Confirmed by you"},
	{key: "communication", title: "Communication preferences"},
	{key: "development", title: "Development and tools"},
	{key: "projects", title: "Projects and workspaces"},
	{key: "goals", title: "Interests and goals"},
	{key: "identity", title: "Identity and environment"},
	{key: "other", title: "Other learned context"},
}

type memoryFactGroup struct {
	category string
	key      string
	facts    []memory.Fact
	display  memory.Fact
}

var memoryPathPattern = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|/)[^\s,'"<>\)\]]+`)
var namedProjectPatterns = []*regexp.Regexp{
	regexp.MustCompile("(?i)(?:project|repo|repository|codebase)\\s+(?:called|named)\\s+['\"`]?([a-z0-9._-]+)"),
	regexp.MustCompile("(?i)refers?\\s+to\\s+['\"`]?([a-z0-9._-]+)['\"`]?\\s+as\\s+(?:a\\s+)?(?:project|repo|repository|codebase)\\b"),
	regexp.MustCompile("(?i)['\"`]?([a-z0-9._-]+)['\"`]?\\s+(?:project|repo|repository|codebase)\\b"),
}

// formatMemoryOverview is the human-facing read model for /memory. Raw facts
// remain the durable evidence; this view only classifies and folds related
// records so storage identifiers and extraction duplication do not dominate
// the normal UX.
func formatMemoryOverview(ctx context.Context, mem *memory.MemoryManager, tenantID string) (string, error) {
	facts, err := loadAllMemoryFacts(ctx, mem, tenantID, false)
	if err != nil {
		return "", err
	}
	groups := groupMemoryFacts(facts)
	byCategory := make(map[string][]memoryFactGroup)
	for _, group := range groups {
		byCategory[group.category] = append(byCategory[group.category], group)
	}

	var sb strings.Builder
	sb.WriteString("## Memory\n")
	if len(facts) == 0 {
		sb.WriteString("\nNo durable memories yet.\n")
		sb.WriteString("\nUse `/memory pin <text>` to save something authoritative.")
		return sb.String(), nil
	}
	sb.WriteString("\nA concise view of what SelfMind currently remembers. Related evidence is grouped; raw records are unchanged.\n")

	for _, section := range memoryViewSections {
		items := byCategory[section.key]
		if len(items) == 0 {
			continue
		}
		sort.SliceStable(items, func(i, j int) bool {
			return memoryGroupRank(items[i]) > memoryGroupRank(items[j])
		})
		sb.WriteString("\n### " + section.title + "\n")
		shown := len(items)
		if shown > memoryOverviewSectionLimit {
			shown = memoryOverviewSectionLimit
		}
		for _, item := range items[:shown] {
			content := textutil.Truncate(strings.TrimSpace(item.display.Content), 260)
			fmt.Fprintf(&sb, "- %s", content)
			if len(item.facts) > 1 {
				fmt.Fprintf(&sb, " (%d related records)", len(item.facts))
			}
			sb.WriteString("\n")
		}
		if hidden := len(items) - shown; hidden > 0 {
			fmt.Fprintf(&sb, "- ... %d more in this category; use `/memory search <query>`\n", hidden)
		}
	}

	sb.WriteString("\nManage: `/memory search <query>` · `/memory show <ref>` · `/memory correct <ref> <text>` · `/memory forget <ref>` · `/memory raw`")
	return strings.TrimSpace(sb.String()), nil
}

func formatMemorySearch(ctx context.Context, mem *memory.MemoryManager, tenantID, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	facts, err := loadAllMemoryFacts(ctx, mem, tenantID, false)
	if err != nil {
		return "", err
	}
	needle := strings.ToLower(query)
	var matches []memory.Fact
	for _, fact := range facts {
		if strings.Contains(strings.ToLower(fact.Content), needle) {
			matches = append(matches, fact)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Target == "pinned" && matches[j].Target != "pinned" {
			return true
		}
		if matches[i].Confidence != matches[j].Confidence {
			return matches[i].Confidence > matches[j].Confidence
		}
		return matches[i].CreatedAt.After(matches[j].CreatedAt)
	})
	if len(matches) == 0 {
		return fmt.Sprintf("No memory matches %q.", query), nil
	}
	const limit = 20
	shown := len(matches)
	if shown > limit {
		shown = limit
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Memory search: %q\n", query)
	for _, fact := range matches[:shown] {
		fmt.Fprintf(&sb, "\n- [%s] %s\n  %s", shortMemoryRef(fact.ID), memoryTargetLabel(fact.Target), strings.TrimSpace(fact.Content))
	}
	if len(matches) > shown {
		fmt.Fprintf(&sb, "\n\n... %d more matches", len(matches)-shown)
	}
	sb.WriteString("\n\nUse `/memory show <ref>` for provenance or `/memory forget <ref>` to remove one record.")
	return sb.String(), nil
}

func formatMemoryDetail(ctx context.Context, mem *memory.MemoryManager, tenantID, ref string) (string, error) {
	fact, err := findMemoryFactByRef(ctx, mem, tenantID, ref)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("## Memory detail\n")
	fmt.Fprintf(&sb, "\nReference: %s\n", shortMemoryRef(fact.ID))
	fmt.Fprintf(&sb, "Type: %s\n", memoryTargetLabel(fact.Target))
	fmt.Fprintf(&sb, "Content: %s\n", fact.Content)
	if fact.Source != "" {
		fmt.Fprintf(&sb, "Source: %s\n", fact.Source)
	}
	if fact.Scope != "" {
		fmt.Fprintf(&sb, "Scope: %s\n", fact.Scope)
	}
	if fact.Confidence > 0 {
		fmt.Fprintf(&sb, "Confidence: %.0f%%\n", fact.Confidence*100)
	}
	if !fact.CreatedAt.IsZero() {
		fmt.Fprintf(&sb, "Created: %s\n", fact.CreatedAt.Local().Format(time.DateOnly))
	}
	if !fact.LastVerifiedAt.IsZero() {
		fmt.Fprintf(&sb, "Last verified: %s\n", fact.LastVerifiedAt.Local().Format(time.DateOnly))
	}
	if fact.CreatedFromRun != "" {
		fmt.Fprintf(&sb, "Run: %s\n", shortMemoryRef(fact.CreatedFromRun))
	}
	sb.WriteString("\nActions: `/memory correct " + shortMemoryRef(fact.ID) + " <text>` · `/memory forget " + shortMemoryRef(fact.ID) + "`")
	return strings.TrimSpace(sb.String()), nil
}

func formatRawMemoryFacts(ctx context.Context, mem *memory.MemoryManager, tenantID string) (string, error) {
	section := func(sb *strings.Builder, title, target string) error {
		facts, err := mem.GetFacts(ctx, tenantID, target)
		if err != nil {
			return err
		}
		sb.WriteString("\n### " + title + "\n")
		if len(facts) == 0 {
			sb.WriteString("- (empty)\n")
			return nil
		}
		for _, fact := range facts {
			fmt.Fprintf(sb, "- [%s] %s%s\n", shortMemoryRef(fact.ID), fact.Content, factProvenanceSuffix(fact))
		}
		return nil
	}
	var sb strings.Builder
	sb.WriteString("## Raw memory evidence\n")
	for _, item := range []struct{ title, target string }{
		{title: "User", target: "user"},
		{title: "Project / Environment", target: "memory"},
		{title: "Pinned", target: "pinned"},
	} {
		if err := section(&sb, item.title, item.target); err != nil {
			return "", err
		}
	}
	sb.WriteString("\nUse `/memory show <ref>` for full metadata. Raw records are evidence; `/memory` is the curated view.")
	return sb.String(), nil
}

func loadAllMemoryFacts(ctx context.Context, mem *memory.MemoryManager, tenantID string, includeProfile bool) ([]memory.Fact, error) {
	if mem == nil {
		return nil, fmt.Errorf("memory provider not initialized")
	}
	// Transition read model (docs/memory-governance.zh-CN.md §5): canonical
	// rows plus legacy facts the canonical layer has never seen. Forgotten/
	// superseded canonicals suppress their legacy shadows.
	facts, _ := memory.ReadModelFacts(ctx, mem, tenantID)
	if includeProfile {
		if profile, err := mem.GetFacts(ctx, tenantID, "profile"); err == nil {
			facts = append(facts, profile...)
		}
	}
	return facts, nil
}

func findMemoryFactByRef(ctx context.Context, mem *memory.MemoryManager, tenantID, ref string) (memory.Fact, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return memory.Fact{}, fmt.Errorf("memory reference is required")
	}
	if len(ref) < 6 {
		return memory.Fact{}, fmt.Errorf("memory reference %q is too short; use the reference shown by /memory search", ref)
	}
	facts, err := loadAllMemoryFacts(ctx, mem, tenantID, false)
	if err != nil {
		return memory.Fact{}, err
	}
	var matches []memory.Fact
	for _, fact := range facts {
		if fact.ID == ref || strings.HasPrefix(fact.ID, ref) {
			matches = append(matches, fact)
		}
	}
	if len(matches) == 0 {
		// Ref stability across the transition: a ref handed out before a
		// canonical shadow appeared still resolves against the raw legacy
		// rows; mutations mirror to the canonical layer by statement identity.
		for _, target := range []string{"pinned", "user", "memory"} {
			legacy, err := mem.GetFacts(ctx, tenantID, target)
			if err != nil {
				continue
			}
			for _, fact := range legacy {
				if fact.ID == ref || strings.HasPrefix(fact.ID, ref) {
					matches = append(matches, fact)
				}
			}
		}
	}
	if len(matches) == 0 {
		return memory.Fact{}, fmt.Errorf("memory %q was not found; use /memory search <query>", ref)
	}
	if len(matches) > 1 {
		return memory.Fact{}, fmt.Errorf("memory reference %q is ambiguous; use a longer prefix", ref)
	}
	return matches[0], nil
}

func groupMemoryFacts(facts []memory.Fact) []memoryFactGroup {
	var groups []memoryFactGroup
	for _, fact := range facts {
		if strings.TrimSpace(fact.Content) == "" || fact.Target == "profile" {
			continue
		}
		category := memoryFactCategory(fact)
		key := memorySemanticKey(category, fact.Content)
		matched := -1
		for i := range groups {
			if groups[i].category != category {
				continue
			}
			if (key != "" && groups[i].key == key) || equivalentMemoryText(groups[i].display.Content, fact.Content) {
				matched = i
				break
			}
		}
		if matched < 0 {
			groups = append(groups, memoryFactGroup{category: category, key: key, facts: []memory.Fact{fact}, display: fact})
			continue
		}
		groups[matched].facts = append(groups[matched].facts, fact)
		if memoryFactDisplayScore(fact) > memoryFactDisplayScore(groups[matched].display) {
			groups[matched].display = fact
		}
	}
	return groups
}

func memoryFactCategory(fact memory.Fact) string {
	if fact.Target == "pinned" {
		return "pinned"
	}
	value := strings.ToLower(fact.Content)
	switch {
	case memoryPathPattern.MatchString(value) || containsAny(value, "project", "repository", "repo ", "codebase", "workspace", "directory", "项目", "仓库", "代码库", "工作区", "目录"):
		return "projects"
	case containsAny(value, "programmer", "developer", "programming", "coding", "golang", " go ", "python", "rust", "javascript", "typescript", "php", "cli", "cicd", "ci/cd", "编程", "开发", "代码", "学习"):
		return "development"
	case containsAny(value, "prefers", "preference", "responses", "response style", "language", "communicat", "中文", "回答", "回复", "语言", "表达"):
		return "communication"
	case containsAny(value, "interested", "interest", "focus", "cares about", "wants to", "goal", "关注", "感兴趣", "目标", "希望"):
		return "goals"
	case containsAny(value, "username", "user id", "platform user", "identity", "account", "用户名", "用户 id", "身份", "账号"):
		return "identity"
	default:
		return "other"
	}
}

func memorySemanticKey(category, content string) string {
	value := strings.ToLower(content)
	if category == "projects" {
		if name := namedProjectName(value); name != "" {
			return "project:" + name
		}
		path := memoryPathPattern.FindString(value)
		if path != "" {
			path = strings.TrimRight(path, ".;:")
			return "path:" + filepath.ToSlash(path)
		}
	}
	return ""
}

func namedProjectName(value string) string {
	ignored := map[string]struct{}{
		"a": {}, "an": {}, "current": {}, "new": {}, "the": {}, "this": {},
	}
	for _, pattern := range namedProjectPatterns {
		for _, match := range pattern.FindAllStringSubmatch(value, -1) {
			if len(match) != 2 {
				continue
			}
			name := strings.ToLower(strings.Trim(match[1], "'\"`"))
			name = strings.TrimRight(name, ".,;:")
			if _, skip := ignored[name]; !skip && name != "" {
				return name
			}
		}
	}
	return ""
}

func equivalentMemoryText(a, b string) bool {
	a, b = normalizeMemoryText(a), normalizeMemoryText(b)
	if a == b {
		return true
	}
	shorter, longer := a, b
	if len([]rune(shorter)) > len([]rune(longer)) {
		shorter, longer = longer, shorter
	}
	if len([]rune(shorter)) >= 20 && strings.Contains(longer, shorter) {
		return true
	}
	at, bt := memoryTokenSet(a), memoryTokenSet(b)
	if len(at) < 3 || len(bt) < 3 {
		return false
	}
	intersection := 0
	for token := range at {
		if _, ok := bt[token]; ok {
			intersection++
		}
	}
	if intersection < 3 {
		return false
	}
	union := len(at) + len(bt) - intersection
	return union > 0 && float64(intersection)/float64(union) >= 0.55
}

func normalizeMemoryText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"the user ", "user is ", "user has ", "user's ", "user wants ", "user prefers "} {
		value = strings.TrimPrefix(value, prefix)
	}
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '/' || r == '.' || r == '_' || r == '-')
	}), " ")
}

func memoryTokenSet(value string) map[string]struct{} {
	stop := map[string]struct{}{
		"a": {}, "an": {}, "and": {}, "as": {}, "at": {}, "called": {}, "for": {}, "in": {}, "is": {}, "of": {}, "the": {}, "their": {}, "to": {}, "user": {}, "with": {},
	}
	out := make(map[string]struct{})
	for _, token := range strings.Fields(value) {
		if _, skip := stop[token]; skip || len([]rune(token)) < 2 {
			continue
		}
		out[token] = struct{}{}
	}
	return out
}

func memoryFactDisplayScore(fact memory.Fact) float64 {
	score := fact.Confidence * 100
	if fact.Target == "pinned" || strings.EqualFold(fact.Source, "user") {
		score += 30
	}
	if memoryPathPattern.MatchString(fact.Content) {
		score += 10
	}
	tokens := len(memoryTokenSet(normalizeMemoryText(fact.Content)))
	if tokens > 12 {
		tokens = 12
	}
	return score + float64(tokens)
}

func memoryGroupRank(group memoryFactGroup) float64 {
	return memoryFactDisplayScore(group.display) + float64(len(group.facts))*2
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func shortMemoryRef(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func memoryTargetLabel(target string) string {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "pinned":
		return "confirmed"
	case "memory":
		return "project/environment"
	default:
		return "user"
	}
}

// FormatCanonicalGovernanceEvents exposes automatic consolidation mutations
// without duplicating every normal intake event already present in the
// learning log. Reversible event ids can be passed to /memory undo.
func FormatCanonicalGovernanceEvents(events []memory.MemoryEvent) string {
	var visible []memory.MemoryEvent
	for _, event := range events {
		switch event.Action {
		case "merge", "archive", "undo":
			visible = append(visible, event)
		}
		if len(visible) >= 20 {
			break
		}
	}
	if len(visible) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Automatic governance history\n")
	for _, event := range visible {
		fmt.Fprintf(&sb, "- `%s` %s", event.ID, event.Action)
		if event.MemoryID != "" {
			fmt.Fprintf(&sb, " memory [%s]", shortMemoryRef(event.MemoryID))
		}
		if !event.CreatedAt.IsZero() {
			fmt.Fprintf(&sb, " at %s", event.CreatedAt.Format(time.RFC3339))
		}
		if event.Action == "merge" || event.Action == "archive" {
			sb.WriteString(" (undoable)")
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}
