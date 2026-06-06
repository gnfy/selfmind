package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
)

type LearningChange struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Action    string `json:"action"`
	Source    string `json:"source,omitempty"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
	CreatedAt string `json:"created_at"`
}

func recordSkillLearningChange(tenantID, name, action, before, after, source string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	change := LearningChange{
		ID:        "learn_" + time.Now().UTC().Format("20060102T150405.000000000"),
		TenantID:  fallbackTenant(tenantID),
		Kind:      "skill",
		Target:    kernel.SanitizeSkillName(name),
		Action:    action,
		Source:    source,
		Before:    before,
		After:     after,
		CreatedAt: nowRFC3339(),
	}
	_ = appendLearningChange(change)
}

func recordMemoryLearningChange(tenantID, target, action, before, after, source string) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = "memory"
	}
	change := LearningChange{
		ID:        "learn_" + time.Now().UTC().Format("20060102T150405.000000000"),
		TenantID:  fallbackTenant(tenantID),
		Kind:      "memory",
		Target:    target,
		Action:    action,
		Source:    source,
		Before:    before,
		After:     after,
		CreatedAt: nowRFC3339(),
	}
	_ = appendLearningChange(change)
}

func appendLearningChange(change LearningChange) error {
	dir, err := learningDir(change.TenantID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	line, err := json.Marshal(change)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "learning-log.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return writeLearningSnapshot(dir, change)
}

func writeLearningSnapshot(root string, change LearningChange) error {
	if change.Target == "" {
		return nil
	}
	dirName := change.Kind
	if dirName == "" {
		dirName = "change"
	}
	if dirName == "skill" {
		dirName = "skills"
	}
	dir := filepath.Join(root, dirName, snapshotTargetName(change.Target))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(change, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(dir, change.ID+".json"), string(data))
}

func ListSkillLearningChanges(tenantID, name string, limit int) ([]LearningChange, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	target := kernel.SanitizeSkillName(name)
	root, err := learningDir(tenantID)
	if err != nil {
		return nil, err
	}
	return listLearningSnapshots(filepath.Join(root, "skills", target), limit)
}

func ListMemoryLearningChanges(tenantID, target string, limit int) ([]LearningChange, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	root, err := learningDir(tenantID)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, "memory")
	target = strings.TrimSpace(target)
	if target != "" {
		return listLearningSnapshots(filepath.Join(base, snapshotTargetName(target)), limit)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []LearningChange
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		changes, err := listLearningSnapshots(filepath.Join(base, entry.Name()), limit)
		if err != nil {
			return nil, err
		}
		out = append(out, changes...)
	}
	sortLearningChangesDesc(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func listLearningSnapshots(dir string, limit int) ([]LearningChange, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() > entries[j].Name()
	})
	var out []LearningChange
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var change LearningChange
		if err := json.Unmarshal(data, &change); err != nil {
			continue
		}
		out = append(out, change)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func formatSkillLearningChanges(changes []LearningChange) string {
	if len(changes) == 0 {
		return "No learning history for this skill."
	}
	return FormatLearningChanges(changes)
}

func FormatMemoryLearningChanges(changes []LearningChange) string {
	if len(changes) == 0 {
		return "No learning history for memory."
	}
	return FormatLearningChanges(changes)
}

func FormatLearningChanges(changes []LearningChange) string {
	var sb strings.Builder
	for _, change := range changes {
		fmt.Fprintf(&sb, "- `%s` %s %s at %s", change.ID, change.Action, change.Target, change.CreatedAt)
		if change.Source != "" {
			fmt.Fprintf(&sb, " (%s)", change.Source)
		}
		if change.After != "" {
			fmt.Fprintf(&sb, "\n  after: %s", truncateText(toOneLine(change.After), 180))
		} else if change.Before != "" {
			fmt.Fprintf(&sb, "\n  before: %s", truncateText(toOneLine(change.Before), 180))
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

func GetLearningChangeByID(tenantID, changeID string) (*LearningChange, error) {
	changeID = strings.TrimSpace(changeID)
	if changeID == "" {
		return nil, fmt.Errorf("learning change id is required")
	}
	root, err := learningDir(tenantID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, "learning-log.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("learning change not found: %s", changeID)
		}
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var change LearningChange
		if err := json.Unmarshal([]byte(line), &change); err != nil {
			continue
		}
		if change.ID == changeID {
			return &change, nil
		}
	}
	return nil, fmt.Errorf("learning change not found: %s", changeID)
}

func UndoMemoryLearningChange(ctx context.Context, mem *memory.MemoryManager, tenantID, changeID string) (string, error) {
	if mem == nil {
		return "", fmt.Errorf("memory not initialized")
	}
	change, err := GetLearningChangeByID(tenantID, changeID)
	if err != nil {
		return "", err
	}
	if change.Kind != "memory" {
		return "", fmt.Errorf("learning change %s is %s, not memory", change.ID, change.Kind)
	}
	if strings.HasPrefix(change.Action, "undo:") {
		return "", fmt.Errorf("learning change %s is already an undo record", change.ID)
	}
	target := strings.TrimSpace(change.Target)
	if target == "" {
		target = "memory"
	}
	switch change.Action {
	case "add":
		removed, err := removeMemoryFactMatching(ctx, mem, tenantID, target, change.After)
		if err != nil {
			return "", err
		}
		recordMemoryLearningChange(tenantID, target, "undo:add", removed, "", "learning_undo")
		return fmt.Sprintf("Undid memory add `%s`: removed %q.", change.ID, removed), nil
	case "remove":
		if strings.TrimSpace(change.Before) == "" {
			return "", fmt.Errorf("learning change %s has no previous memory content to restore", change.ID)
		}
		if err := mem.AddFact(ctx, tenantID, target, change.Before); err != nil {
			return "", err
		}
		recordMemoryLearningChange(tenantID, target, "undo:remove", "", change.Before, "learning_undo")
		return fmt.Sprintf("Undid memory remove `%s`: restored %q.", change.ID, change.Before), nil
	case "replace":
		if strings.TrimSpace(change.Before) == "" {
			return "", fmt.Errorf("learning change %s has no previous memory content to restore", change.ID)
		}
		if strings.TrimSpace(change.After) != "" {
			_, _ = removeMemoryFactMatching(ctx, mem, tenantID, target, change.After)
		}
		if err := mem.AddFact(ctx, tenantID, target, change.Before); err != nil {
			return "", err
		}
		recordMemoryLearningChange(tenantID, target, "undo:replace", change.After, change.Before, "learning_undo")
		return fmt.Sprintf("Undid memory replace `%s`: restored %q.", change.ID, change.Before), nil
	default:
		return "", fmt.Errorf("memory learning action %q cannot be undone", change.Action)
	}
}

func removeMemoryFactMatching(ctx context.Context, mem *memory.MemoryManager, tenantID, target, needle string) (string, error) {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return "", fmt.Errorf("memory content to remove is empty")
	}
	facts, err := mem.GetFacts(ctx, tenantID, target)
	if err != nil {
		return "", err
	}
	fact, ok := findMatchingMemoryFact(facts, needle)
	if !ok {
		return "", fmt.Errorf("could not find memory entry matching %q", needle)
	}
	if err := mem.RemoveFact(ctx, tenantID, fact.ID); err != nil {
		return "", err
	}
	return fact.Content, nil
}

func findMatchingMemoryFact(facts []memory.Fact, needle string) (memory.Fact, bool) {
	needle = strings.TrimSpace(needle)
	for _, fact := range facts {
		if fact.ID == needle || fact.Content == needle {
			return fact, true
		}
	}
	for _, fact := range facts {
		if strings.Contains(fact.Content, needle) {
			return fact, true
		}
	}
	return memory.Fact{}, false
}

func learningDir(tenantID string) (string, error) {
	skillsDir, err := getSkillsDir(tenantID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(skillsDir), "learning"), nil
}

func fallbackTenant(tenantID string) string {
	if strings.TrimSpace(tenantID) == "" {
		return "default"
	}
	return tenantID
}

func sortLearningChangesDesc(changes []LearningChange) {
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].CreatedAt != changes[j].CreatedAt {
			return changes[i].CreatedAt > changes[j].CreatedAt
		}
		return changes[i].ID > changes[j].ID
	})
}

func snapshotTargetName(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return "memory"
	}
	target = strings.ReplaceAll(target, "/", "-")
	target = strings.ReplaceAll(target, "\\", "-")
	target = strings.ReplaceAll(target, ":", "-")
	clean := filepath.Clean(target)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.IsAbs(clean) {
		return "memory"
	}
	return clean
}

func toOneLine(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	for strings.Contains(value, "  ") {
		value = strings.ReplaceAll(value, "  ", " ")
	}
	return strings.TrimSpace(value)
}

func truncateText(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max] + "..."
}
