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
	sb.WriteString(attentionListActions(items))
	return sb.String(), nil
}

// attentionListActions names every operation that applies to the list just
// rendered, one per line.
//
// One sentence naming two of them left the rest invisible: that the run id
// printed on each row is a reference in its own right, that continuing is a
// SELECTION whose work starts with the next message rather than immediately,
// how to put down the item that is running, and how to refresh. A line appears
// only when the list holds an item it applies to, so the block never offers an
// action that would answer "there is nothing to do that to".
//
// The lines are a Markdown bullet list because the terminal renders this reply
// through a CommonMark parser: indented lines under a text line are lazy
// paragraph continuations and get folded into one long line, which is already
// visible above, where each item and its metadata line arrive joined.
func attentionListActions(items []control.AttentionItem) string {
	lines := []string{
		"",
		"Actions:",
		"",
		"- `/resume <n|run_id>` continues that run; your next message goes to it",
		"- `/stop <n|run_id>` clears that item without running it",
	}
	if attentionListHasActivity(items, control.ThreadActivityActive) {
		lines = append(lines, "- `/stop` cancels the run that is running now")
	}
	if attentionListHasActivity(items, control.ThreadActivityMonitoring) {
		lines = append(lines, "- `/watchers` lists or cancels a background watcher")
	}
	return strings.Join(append(lines, "- `/resume` refreshes this list"), "\n")
}

func attentionListHasActivity(items []control.AttentionItem, activity string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Activity), activity) {
			return true
		}
	}
	return false
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
