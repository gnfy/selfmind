package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
)

// Cross-endpoint explicit memory commands (simplification P3 step 1):
// /remember <text> and /forget <text|ref> are deterministic, model-free, and
// available from every endpoint. They are the PRIMARY preference intake — the
// background analyzer only proposes auto-candidates for explicitly stated
// preferences, and user commands always win over anything it wrote.

const rememberUsage = "Usage: /remember <preference> — save an explicit personal preference (e.g. /remember 回复先给结论再给论据)."
const forgetUsage = "Usage: /forget <text|ref> — forget a remembered preference by its text or its /memory ref."

func (d *Server) handleRememberCommand(ctx context.Context, identity *control.IdentityContext, raw string) (string, error) {
	content := strings.TrimSpace(raw)
	if content == "" {
		return rememberUsage, nil
	}
	if d == nil || d.Memory == nil || identity == nil {
		return "Long-term memory is not available on this daemon.", nil
	}
	if memory.ClassifyTransientContent(content) == memory.TransientConfirmed {
		return "That looks like transient run/build state, which never enters long-term memory. Task progress lives in the task's runs and handoffs; if you really need it verbatim, /memory pin <text> stores it under your explicit authority.", nil
	}
	if d.SkillStorage == nil {
		return "", fmt.Errorf("memory asset storage is unavailable")
	}
	args := map[string]interface{}{
		"action": "remember", "target": "user", "content": content,
		"_tenant_id": memoryPartition(identity),
	}
	args = tools.WithSkillStorage(args, d.SkillStorage)
	if _, err := tools.NewMemoryTool(d.Memory).Execute(args); err != nil {
		return "", fmt.Errorf("save preference: %w", err)
	}
	return "Remembered: " + textutil.Truncate(content, 200) + "\nIt applies across all your endpoints. /forget removes it; /memory lists everything.", nil
}

func (d *Server) handleForgetCommand(ctx context.Context, identity *control.IdentityContext, raw string) (string, error) {
	needle := strings.TrimSpace(raw)
	if needle == "" {
		return forgetUsage, nil
	}
	if d == nil || d.Memory == nil || identity == nil {
		return "Long-term memory is not available on this daemon.", nil
	}
	store, ok := d.Memory.Canonical()
	if !ok {
		return "Long-term memory is not available on this daemon.", nil
	}
	partition := memoryPartition(identity)
	rows, err := store.ListCanonicalMemories(ctx, partition, memory.CanonicalFilter{Limit: 500})
	if err != nil {
		return "", fmt.Errorf("list memory: %w", err)
	}
	matches := matchCanonicalMemories(rows, needle)
	switch len(matches) {
	case 0:
		return "No remembered memory matches " + fmt.Sprintf("%q", textutil.Truncate(needle, 80)) + ". /memory lists what is stored.", nil
	case 1:
		if d.SkillStorage == nil {
			return "", fmt.Errorf("memory asset storage is unavailable")
		}
		target := matches[0]
		args := map[string]interface{}{
			"action": "forget", "ref": target.ID, "_tenant_id": partition,
		}
		args = tools.WithSkillStorage(args, d.SkillStorage)
		if _, err := tools.NewMemoryTool(d.Memory).Execute(args); err != nil {
			return "", fmt.Errorf("forget memory: %w", err)
		}
		return "Forgotten: " + textutil.Truncate(target.Content, 200), nil
	default:
		var sb strings.Builder
		sb.WriteString("Several memories match. Use /forget <ref> with one of:\n")
		for i, row := range matches {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&sb, "%d. [%s] %s\n", i+1, shortCanonicalRef(row.ID), textutil.Truncate(oneLine(row.Content), 100))
		}
		return strings.TrimRight(sb.String(), "\n"), nil
	}
}

// memoryPartition is the person-memory partition key: person memory follows
// the person, so PersonID is authoritative and TenantID only a fallback for
// degenerate identities.
func memoryPartition(identity *control.IdentityContext) string {
	if partition := strings.TrimSpace(identity.PersonID); partition != "" {
		return partition
	}
	return strings.TrimSpace(identity.TenantID)
}

// matchCanonicalMemories resolves a /forget argument deterministically: an
// id-prefix ref (8+ leading characters of the canonical id, as /memory shows)
// wins; otherwise case-insensitive substring match on the stored content.
func matchCanonicalMemories(rows []memory.CanonicalMemory, needle string) []memory.CanonicalMemory {
	trimmed := strings.TrimSpace(needle)
	if looksLikeCanonicalRef(trimmed) {
		var byRef []memory.CanonicalMemory
		for _, row := range rows {
			if strings.HasPrefix(strings.ToLower(row.ID), strings.ToLower(trimmed)) {
				byRef = append(byRef, row)
			}
		}
		if len(byRef) > 0 {
			return byRef
		}
	}
	lowered := strings.ToLower(trimmed)
	var out []memory.CanonicalMemory
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row.Content), lowered) {
			out = append(out, row)
		}
	}
	return out
}

// looksLikeCanonicalRef mirrors the /memory ref shape: at least 8 characters
// of a UUID-ish id, no spaces.
func looksLikeCanonicalRef(value string) bool {
	if len(value) < 8 || strings.ContainsAny(value, " \t") {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F', r == '-':
		default:
			return false
		}
	}
	return true
}

func shortCanonicalRef(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
