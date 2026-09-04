package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/textutil"
)

// attentionListLimit bounds one rendered Attention list. Attention is what
// needs the person NOW; a list long enough to page through is a sign the
// derivation is wrong, not a paging requirement.
const attentionListLimit = 20

// attentionListReply renders what currently needs the person, newest signal
// first, and remembers the ordinals so `/resume <n>` resolves against exactly
// the list they saw.
//
// This replaces the `/tasks` card view. That view drew Task rows — title,
// pin state, run counts, duplicate suggestions — and its numbering was the
// only place the ordinal snapshot was recorded, which quietly made "continue
// number 2" depend on a Task listing. Attention is derived per exact Run, so
// the list names runs and the person continues one of them.
func (d *Server) attentionListReply(ctx context.Context, identity *control.IdentityContext, channel string) (string, error) {
	items, total, err := d.openAttentionPage(ctx, identity, "", channel, attentionListLimit, 0)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "Nothing needs attention.", nil
	}
	d.taskLists.rememberAttention(identity, channel, items, time.Now())
	// Why a run stopped is the part a person needs to decide whether to
	// continue it. Best-effort: a failed lookup costs the reason, never the
	// list.
	outcomes, _ := d.Control.LatestRunOutcomesByPerson(ctx, identity.TenantID, identity.PersonID)

	var sb strings.Builder
	sb.WriteString("Needs attention:\n")
	for i, item := range items {
		summary := strings.TrimSpace(item.RunSummary)
		if summary == "" {
			summary = strings.TrimSpace(item.Thread.Title)
		}
		if summary == "" {
			summary = "(no summary)"
		}
		fmt.Fprintf(&sb, "%d. %s\n", i+1, textutil.Truncate(toOneLine(summary), 96))
		meta := []string{attentionActivityLabel(item, outcomes)}
		if channel := displayChannel(item.Channel); channel != "" {
			meta = append(meta, channel)
		}
		meta = append(meta, shortRunID(item.RunID))
		fmt.Fprintf(&sb, "   %s\n", strings.Join(meta, " · "))
	}
	if total > len(items) {
		fmt.Fprintf(&sb, "... and %d more\n", total-len(items))
	}
	sb.WriteString("Use /resume <number> to continue one exactly, or /stop to dismiss the active one.")
	return sb.String(), nil
}

// attentionActivityLabel says why an item is asking for the person, in their
// words rather than the derivation's. An interrupted run reports what actually
// interrupted it, because "interrupted" alone does not tell anyone whether it
// is worth continuing.
func attentionActivityLabel(item control.AttentionItem, outcomes map[string]control.LatestRunOutcome) string {
	switch strings.ToLower(strings.TrimSpace(item.Activity)) {
	case "active":
		return "running"
	case "needs_attention":
		return "waiting for you"
	case "monitoring":
		return "watching"
	case "resumable":
		if strings.EqualFold(strings.TrimSpace(item.RunStatus), "interrupted") {
			if outcome, ok := outcomes[item.Thread.ID]; ok {
				return interruptedTaskSuffix(outcome)
			}
			return "interrupted"
		}
		if explicitResumeRunStatus(item.RunStatus) {
			return "resumable"
		}
		return strings.TrimSpace(item.RunStatus)
	}
	return strings.TrimSpace(item.Activity)
}

// interruptedTaskSuffix renders a completion reason as the phrase a person can
// act on.
func interruptedTaskSuffix(outcome control.LatestRunOutcome) string {
	resumable := ""
	if outcome.Resumable {
		resumable = " - resumable"
	}
	switch strings.ToLower(strings.TrimSpace(outcome.CompletionReason)) {
	case "daemon_recovery":
		return "daemon restarted" + resumable
	case "provider_or_transport_error", "transport_error", "provider_error":
		return "provider connection interrupted" + resumable
	case "context_overflow":
		return "context limit reached" + resumable
	case "verification_incomplete", "verification_failed":
		return "verification incomplete" + resumable
	default:
		return "interrupted" + resumable
	}
}

// displayChannel hides a channel that is not a name a person recognizes. The
// terminal mints a fresh UUID per launch and stores it as the channel, so
// printing it verbatim put a meaningless identifier on every line of the
// listing and pushed the part that matters off the end.
func displayChannel(channel string) string {
	channel = strings.TrimSpace(channel)
	if looksLikeSessionUUID(channel) {
		return ""
	}
	return channel
}

// looksLikeSessionUUID reports whether s is a bare RFC-4122 UUID.
func looksLikeSessionUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
