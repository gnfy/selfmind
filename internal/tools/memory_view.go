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
	"unicode/utf8"

	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/textutil"
)

const memoryCategoryPageSize = 10

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
	sb.WriteString("Memory\n")
	if len(facts) == 0 {
		sb.WriteString("\nSelfMind has no durable memories yet.\n")
		sb.WriteString("\nUse `/memory pin <ref>` to protect an existing memory, or `/memory pin <text>` to save something authoritative.")
		return sb.String(), nil
	}

	conflicts, protected := memoryOverviewHealth(ctx, mem, tenantID)
	fmt.Fprintf(&sb, "\nSelfMind is managing %d topics from %d active memory records.\n", len(groups), len(facts))
	if conflicts > 0 {
		fmt.Fprintf(&sb, "\nAttention: %d conflict(s) need review. Open `/memory conflicts`.\n", conflicts)
	} else {
		sb.WriteString("\nStatus: healthy; no memory conflicts need your attention.\n")
	}
	if protected > 0 {
		fmt.Fprintf(&sb, "Protected by you: %d\n", protected)
	}
	sb.WriteString("\nCategories\n")

	for _, section := range memoryViewSections {
		items := byCategory[section.key]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "- `%s`  %d  %s\n", section.key, len(items), section.title)
	}

	sb.WriteString("\nOpen a category: `/memory category <name>`\n")
	sb.WriteString("Find something: `/memory search <query>`\n")
	sb.WriteString("Advanced: `/memory history` | `/memory raw`")
	return strings.TrimSpace(sb.String()), nil
}

func memoryOverviewHealth(ctx context.Context, mem *memory.MemoryManager, tenantID string) (conflicts, protected int) {
	store, ok := mem.Canonical()
	if !ok {
		return 0, 0
	}
	rows, err := store.ListCanonicalMemories(ctx, tenantID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalActive, memory.CanonicalConflicted},
	})
	if err != nil {
		return 0, 0
	}
	for _, row := range rows {
		if row.Status == memory.CanonicalConflicted {
			conflicts++
		}
		if row.Pinned || row.UserConfirmed {
			protected++
		}
	}
	return conflicts, protected
}

func formatMemoryCategory(ctx context.Context, mem *memory.MemoryManager, tenantID, category string, page int) (string, error) {
	section, ok := findMemoryViewSection(category)
	if !ok {
		return "", fmt.Errorf("unknown category %q; use /memory to list categories", category)
	}
	if page < 1 {
		page = 1
	}
	facts, err := loadAllMemoryFacts(ctx, mem, tenantID, false)
	if err != nil {
		return "", err
	}
	var groups []memoryFactGroup
	for _, group := range groupMemoryFacts(facts) {
		if group.category == section.key {
			groups = append(groups, group)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return memoryGroupRank(groups[i]) > memoryGroupRank(groups[j]) })
	if len(groups) == 0 {
		return fmt.Sprintf("%s\n\nNo memories in this category.", section.title), nil
	}
	pages := (len(groups) + memoryCategoryPageSize - 1) / memoryCategoryPageSize
	if page > pages {
		page = pages
	}
	start := (page - 1) * memoryCategoryPageSize
	end := start + memoryCategoryPageSize
	if end > len(groups) {
		end = len(groups)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n\n%d topics | page %d/%d\n", section.title, len(groups), page, pages)
	sb.WriteString("Similar memories are folded for browsing only; actions affect the selected reference.\n")
	for i, group := range groups[start:end] {
		fact := group.display
		fmt.Fprintf(&sb, "\n%d. [%s] %s\n", start+i+1, shortMemoryRef(fact.ID), textutil.Truncate(strings.TrimSpace(displayMemoryText(fact.Content)), 220))
		if meta := memoryGroupMetadata(group); meta != "" {
			fmt.Fprintf(&sb, "   %s\n", meta)
		}
	}
	if page < pages {
		fmt.Fprintf(&sb, "\nNext: `/memory category %s %d`\n", section.key, page+1)
	}
	if page > 1 {
		fmt.Fprintf(&sb, "Previous: `/memory category %s %d`\n", section.key, page-1)
	}
	sb.WriteString("\nManage: `/memory show <ref>` | `/memory correct <ref> <text>` | `/memory forget <ref>`")
	return strings.TrimSpace(sb.String()), nil
}

func formatMemoryConflicts(ctx context.Context, mem *memory.MemoryManager, tenantID string) (string, error) {
	store, ok := mem.Canonical()
	if !ok {
		return "Memory conflicts are unavailable with the current storage backend.", nil
	}
	rows, err := store.ListCanonicalMemories(ctx, tenantID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalConflicted},
	})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "Memory conflicts\n\nNo unresolved conflicts.", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Memory conflicts\n\n%d item(s) need review. Conflicts are not treated as settled truth.\n", len(rows))
	for i, row := range rows {
		fmt.Fprintf(&sb, "\n%d. [%s] %s\n", i+1, shortMemoryRef(row.ID), textutil.Truncate(strings.TrimSpace(displayMemoryText(row.Content)), 220))
		fmt.Fprintf(&sb, "   confidence %.0f%% | scope %s | evidence %d\n", row.Confidence*100, displayMemoryScope(row.Scope), row.EvidenceCount)
	}
	sb.WriteString("\nReview: `/memory show <ref>` | resolve with `/memory correct <ref> <text>` or `/memory forget <ref>`")
	return strings.TrimSpace(sb.String()), nil
}

// formatMemoryDiagnostics exposes the storage facts needed to calibrate the
// shadow governance gate. It deliberately reports aggregate counts only: the
// phone-friendly /diag surface must not leak remembered content or ids.
func formatMemoryDiagnostics(ctx context.Context, mem *memory.MemoryManager, tenantID string) (string, error) {
	if mem == nil {
		return "Memory diagnostics unavailable.", nil
	}
	store, ok := mem.Canonical()
	if !ok {
		facts, err := loadAllMemoryFacts(ctx, mem, tenantID, false)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Memory diagnostics\nCanonical store: unavailable\nLegacy records: %d", len(facts)), nil
	}
	rows, err := store.ListCanonicalMemories(ctx, tenantID, memory.CanonicalFilter{Statuses: []string{
		memory.CanonicalActive, memory.CanonicalConflicted, memory.CanonicalArchived,
		memory.CanonicalSuperseded, memory.CanonicalForgotten,
	}})
	if err != nil {
		return "", err
	}
	status := map[string]int{}
	global, workspace, pinned, confirmed := 0, 0, 0, 0
	visibleFacts := make([]memory.Fact, 0, len(rows))
	for _, row := range rows {
		status[row.Status]++
		if row.Pinned {
			pinned++
		}
		if row.UserConfirmed {
			confirmed++
		}
		if strings.HasPrefix(row.Scope, "workspace:") {
			workspace++
		} else {
			global++
		}
		if row.Status != memory.CanonicalActive && row.Status != memory.CanonicalConflicted {
			continue
		}
		target := row.Target
		if row.Pinned {
			target = "pinned"
		}
		visibleFacts = append(visibleFacts, memory.Fact{
			ID: row.ID, Target: target, Content: row.Content, Scope: row.Scope,
			Confidence: row.Confidence, CreatedAt: row.CreatedAt, LastVerifiedAt: row.LastVerifiedAt,
		})
	}
	report := memory.BuildConsolidationDryRun(visibleFacts, memory.ConsolidationDryRunConfig{}, time.Now())
	var sb strings.Builder
	sb.WriteString("Memory diagnostics\n")
	fmt.Fprintf(&sb, "Records: active %d, conflicted %d, archived %d, superseded %d, forgotten %d\n",
		status[memory.CanonicalActive], status[memory.CanonicalConflicted], status[memory.CanonicalArchived],
		status[memory.CanonicalSuperseded], status[memory.CanonicalForgotten])
	fmt.Fprintf(&sb, "Visible topics: %d from %d active records\n", len(groupMemoryFacts(visibleFacts)), len(visibleFacts))
	fmt.Fprintf(&sb, "Protected: pinned %d, user-confirmed %d\n", pinned, confirmed)
	fmt.Fprintf(&sb, "Scopes: global %d, workspace %d\n", global, workspace)
	fmt.Fprintf(&sb, "Consolidation candidates: %d cluster(s)", len(report.CandidateClusters))
	return sb.String(), nil
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
		if strings.Contains(strings.ToLower(fact.Content), needle) || strings.Contains(strings.ToLower(displayMemoryText(fact.Content)), needle) {
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
	fmt.Fprintf(&sb, "Memory search: %q\n", query)
	for _, fact := range matches[:shown] {
		fmt.Fprintf(&sb, "\n- [%s] %s\n  %s", shortMemoryRef(fact.ID), memoryTargetLabel(fact.Target), strings.TrimSpace(displayMemoryText(fact.Content)))
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
	if store, ok := mem.Canonical(); ok {
		rows, listErr := store.ListCanonicalMemories(ctx, tenantID, memory.CanonicalFilter{
			Statuses: []string{memory.CanonicalActive, memory.CanonicalConflicted, memory.CanonicalSuperseded, memory.CanonicalArchived},
		})
		if listErr == nil {
			for _, row := range rows {
				if row.ID == fact.ID {
					return formatCanonicalMemoryDetail(ctx, store, tenantID, row)
				}
			}
		}
	}
	var sb strings.Builder
	sb.WriteString("Memory detail\n")
	fmt.Fprintf(&sb, "\nReference: %s\n", shortMemoryRef(fact.ID))
	fmt.Fprintf(&sb, "Type: %s\n", memoryTargetLabel(fact.Target))
	fmt.Fprintf(&sb, "Content: %s\n", displayMemoryText(fact.Content))
	if displayMemoryText(fact.Content) != fact.Content {
		sb.WriteString("Display note: repaired legacy text encoding for readability; use correct to persist clean text.\n")
	}
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
	sb.WriteString("\nActions: `/memory correct " + shortMemoryRef(fact.ID) + " <text>` | `/memory forget " + shortMemoryRef(fact.ID) + "`")
	return strings.TrimSpace(sb.String()), nil
}

func formatCanonicalMemoryDetail(ctx context.Context, store memory.CanonicalStore, tenantID string, row memory.CanonicalMemory) (string, error) {
	observations, err := store.ObservationsForMemory(ctx, tenantID, row.ID)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("Memory detail\n")
	fmt.Fprintf(&sb, "\nReference: %s\n", shortMemoryRef(row.ID))
	fmt.Fprintf(&sb, "Status: %s\n", row.Status)
	fmt.Fprintf(&sb, "Category: %s\n", memoryCategoryTitle(memoryFactCategory(memory.Fact{Target: row.Target, Content: row.Content})))
	fmt.Fprintf(&sb, "Content: %s\n", displayMemoryText(row.Content))
	if displayMemoryText(row.Content) != row.Content {
		sb.WriteString("Display note: repaired legacy text encoding for readability; use correct to persist clean text.\n")
	}
	fmt.Fprintf(&sb, "Scope: %s\n", displayMemoryScope(row.Scope))
	fmt.Fprintf(&sb, "Confidence: %.0f%%\n", row.Confidence*100)
	fmt.Fprintf(&sb, "Evidence: %d record(s)\n", len(observations))
	if row.Pinned || row.UserConfirmed {
		labels := make([]string, 0, 2)
		if row.Pinned {
			labels = append(labels, "pinned")
		}
		if row.UserConfirmed {
			labels = append(labels, "confirmed by user")
		}
		fmt.Fprintf(&sb, "Protection: %s\n", strings.Join(labels, ", "))
	}
	if !row.LastVerifiedAt.IsZero() {
		fmt.Fprintf(&sb, "Last verified: %s\n", row.LastVerifiedAt.Local().Format(time.DateOnly))
	}
	if !row.LastAccessedAt.IsZero() {
		fmt.Fprintf(&sb, "Last used: %s\n", row.LastAccessedAt.Local().Format(time.DateOnly))
	}
	if len(observations) > 0 {
		sb.WriteString("\nSupporting evidence\n")
		shown := len(observations)
		if shown > 5 {
			shown = 5
		}
		for _, observation := range observations[:shown] {
			fmt.Fprintf(&sb, "- %s | %s", memorySourceLabel(observation.Source), observation.CreatedAt.Local().Format(time.DateOnly))
			if strings.TrimSpace(observation.Content) != strings.TrimSpace(row.Content) {
				fmt.Fprintf(&sb, " | %s", textutil.Truncate(strings.TrimSpace(displayMemoryText(observation.Content)), 140))
			}
			sb.WriteString("\n")
		}
		if len(observations) > shown {
			fmt.Fprintf(&sb, "- ... %d more evidence record(s)\n", len(observations)-shown)
		}
	}
	actionRef := shortMemoryRef(row.ID)
	sb.WriteString("\nActions: `/memory correct " + actionRef + " <text>` | `/memory forget " + actionRef + "`")
	if row.Pinned {
		sb.WriteString(" | `/memory unpin " + actionRef + "`")
	} else {
		sb.WriteString(" | `/memory pin " + actionRef + "`")
	}
	return strings.TrimSpace(sb.String()), nil
}

func formatRawMemoryFacts(ctx context.Context, mem *memory.MemoryManager, tenantID string) (string, error) {
	section := func(sb *strings.Builder, title, target string) error {
		facts, err := mem.GetFacts(ctx, tenantID, target)
		if err != nil {
			return err
		}
		sb.WriteString("\n" + title + "\n")
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
	sb.WriteString("Raw memory evidence\n")
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
	type classifiedFact struct {
		fact     memory.Fact
		category string
		key      string
		sig      memory.SimilaritySignature
	}
	var items []classifiedFact
	for _, fact := range facts {
		if strings.TrimSpace(fact.Content) == "" || fact.Target == "profile" {
			continue
		}
		category := memoryFactCategory(fact)
		items = append(items, classifiedFact{
			fact:     fact,
			category: category,
			key:      memorySemanticKey(category, fact.Content),
			// Signature built ONCE per fact: the pair loop below is O(n²) in
			// comparisons and must never redo normalization/tokenization —
			// that regression made /memory take seconds at a few hundred facts.
			sig: memory.BuildSimilaritySignature(displayMemoryText(fact.Content)),
		})
	}
	parent := make([]int, len(items))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	byCategory := make(map[string][]int)
	for i, item := range items {
		byCategory[item.category] = append(byCategory[item.category], i)
	}
	for _, bucket := range byCategory {
		for a := 0; a < len(bucket); a++ {
			for b := a + 1; b < len(bucket); b++ {
				i, j := bucket[a], bucket[b]
				if (items[i].key != "" && items[i].key == items[j].key) ||
					memory.SignatureSimilarity(items[i].sig, items[j].sig) >= memoryEquivalenceThreshold {
					union(i, j)
				}
			}
		}
	}

	groupIndex := make(map[int]int)
	var groups []memoryFactGroup
	for i, item := range items {
		root := find(i)
		index, ok := groupIndex[root]
		if !ok {
			groupIndex[root] = len(groups)
			groups = append(groups, memoryFactGroup{category: item.category, key: item.key, facts: []memory.Fact{item.fact}, display: item.fact})
			continue
		}
		groups[index].facts = append(groups[index].facts, item.fact)
		if memoryFactDisplayScore(item.fact) > memoryFactDisplayScore(groups[index].display) {
			groups[index].display = item.fact
		}
	}
	return groups
}

func memoryFactCategory(fact memory.Fact) string {
	if fact.Target == "pinned" {
		return "pinned"
	}
	value := strings.ToLower(displayMemoryText(fact.Content))
	switch {
	case memoryPathPattern.MatchString(value) || containsAny(value, "project", "repository", "repo ", "codebase", "workspace", "directory", "项目", "仓库", "代码库", "工作区", "目录"):
		return "projects"
	case containsAny(value, "programmer", "developer", "programming", "coding", "golang", " go ", "python", "rust", "javascript", "typescript", "php", "cli", "cicd", "ci/cd", "编程", "开发", "代码", "学习"):
		return "development"
	case containsAny(value, "prefers", "preference", "responses", "response style", "language", "communicat", "中文", "回答", "回复", "语言", "表达", "偏好"):
		return "communication"
	case containsAny(value, "interested", "interest", "focus", "cares about", "wants to", "goal", "关注", "感兴趣", "目标", "希望"):
		return "goals"
	case containsAny(value, "username", "user id", "platform user", "identity", "account", "用户名", "用户 id", "身份", "账号", "环境"):
		return "identity"
	default:
		return "other"
	}
}

func findMemoryViewSection(value string) (memoryViewSection, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	aliases := map[string]string{
		"confirmed": "pinned", "confirm": "pinned",
		"preference": "communication", "preferences": "communication",
		"dev": "development", "tools": "development",
		"project": "projects", "workspace": "projects", "workspaces": "projects",
		"goal": "goals", "interest": "goals", "interests": "goals",
		"environment": "identity",
	}
	if alias := aliases[value]; alias != "" {
		value = alias
	}
	for _, section := range memoryViewSections {
		if section.key == value {
			return section, true
		}
	}
	return memoryViewSection{}, false
}

func memoryCategoryTitle(category string) string {
	if section, ok := findMemoryViewSection(category); ok {
		return section.title
	}
	return "Other learned context"
}

func memoryGroupMetadata(group memoryFactGroup) string {
	fact := group.display
	parts := []string{fmt.Sprintf("confidence %.0f%%", fact.Confidence*100)}
	if len(group.facts) > 1 {
		parts = append(parts, fmt.Sprintf("related memories %d", len(group.facts)))
	}
	if fact.Scope != "" {
		parts = append(parts, "scope "+displayMemoryScope(fact.Scope))
	}
	if !fact.LastVerifiedAt.IsZero() {
		parts = append(parts, "verified "+fact.LastVerifiedAt.Local().Format(time.DateOnly))
	}
	return strings.Join(parts, " | ")
}

func displayMemoryScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == "global" {
		return "global"
	}
	return textutil.Truncate(scope, 48)
}

func memorySourceLabel(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case memory.SourceUser:
		return "user confirmed"
	case memory.SourceAgent:
		return "agent"
	case memory.SourceTurnExtractor:
		return "turn extraction"
	case memory.SourceFactExtractor:
		return "fact extraction"
	case "legacy", "":
		return "legacy import"
	default:
		return source
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

// memoryEquivalenceThreshold matches the consolidation candidate threshold so
// the browsing fold and the background clustering agree on "same topic".
const memoryEquivalenceThreshold = 0.42

func equivalentMemoryText(a, b string) bool {
	return memory.ConsolidationSimilarity(displayMemoryText(a), displayMemoryText(b)) >= memoryEquivalenceThreshold
}

// displayMemoryText repairs the common legacy failure where UTF-8 bytes were
// decoded as Latin-1 and then stored as UTF-8 (for example, ç»§ç»­ instead of
// 继续). It is deliberately display-only: raw evidence stays byte-for-byte
// auditable, and /memory show tells the user when a correction should be
// persisted. Text that cannot be losslessly recovered is returned unchanged.
func displayMemoryText(value string) string {
	current := value
	for attempt := 0; attempt < 2; attempt++ {
		if !looksLikeUTF8Mojibake(current) {
			break
		}
		bytes := make([]byte, 0, len(current))
		validLatin1 := true
		for _, r := range current {
			if r > 255 {
				validLatin1 = false
				break
			}
			bytes = append(bytes, byte(r))
		}
		if !validLatin1 || !utf8.Valid(bytes) {
			break
		}
		repaired := string(bytes)
		if repaired == current {
			break
		}
		current = repaired
	}
	return current
}

func looksLikeUTF8Mojibake(value string) bool {
	return containsAny(value, "Ã", "Â", "â", "ð", "ç", "æ", "å", "ä")
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
