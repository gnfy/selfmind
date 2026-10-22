package kernel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/textutil"
)

// This file is the ContextComposer contract (docs/work-timeline.md,
// "Per-turn context (ContextComposer)"): the ONE place that names how a model
// turn's window is assembled, in this fixed slice order:
//
//	① latest user message        — the only authoritative instruction
//	② spine tail                 — recent person-level work-spine references
//	③ compaction summary         — engine A's over-budget middle summary, carrying
//	                               the boundary note (reference only, latest wins)
//	④ semantic-recall slices     — RESERVED for P2: RuntimeContextBundle.Recall
//	⑤ relevant artifacts/files   — TaskRuntimeContext.Artifacts (selector-fed)
//	⑥ workspace current state    — WorkspaceContext / bundle workspace section
//	⑦ person memory/preferences  — facts + profile in buildSystemPrompt
//	⑧ open run/approval state    — TaskRuntimeContext status/handoff/events
//
// Slices ⑤–⑧ render inside the system prompt via RuntimeContextBundle /
// buildSystemPrompt; ②③① are the message array built here. New context inputs
// extend the bundle or its selector — never a new append path in handlers.

// SpineTrajectoryKey is the person-level work-spine storage key. The storage
// tenant is already the person, so this constant key is person-scoped: ALL of
// a person's agent-bound turns (task-bound, casual, cron) append here, and
// every turn loads its recent tail from here regardless of endpoint. Chat
// transcripts (control.channel_messages) stay channel-local and are untouched
// by the spine — this is the durable working-state layer only.
const SpineTrajectoryKey = "spine"

// spineEntryKind marks a turn-level spine record so loads can distinguish the
// slim entry-per-turn shape from legacy cumulative {"messages": [...]} blobs.
const spineEntryKind = "spine.turn.v1"

// Per-slice budget constants for the Composer. Spine entries are slim, so the
// tail can be generous in turn count while staying small in bytes; the final
// window is always bounded again by ContextEngine.TruncateMessages.
const (
	// composerSpineTailEntries is M: how many recent spine turns are rendered
	// as reference records in slice ②.
	composerSpineTailEntries = 16
	// Save-side byte caps keep a spine entry narrative-sized at write time;
	// load applies the tighter defaultHistoryUserBytes/defaultHistoryAnswerBytes.
	composerSpineUserSaveBytes   = 2400
	composerSpineAnswerSaveBytes = 4000
	// ComposerRecallChars is the reserved budget for slice ④ (semantic recall,
	// filled by P2 through RuntimeContextBundle.Recall).
	ComposerRecallChars = 2400
)

// spineEntry is ONE turn on the person's work spine: the user's message text,
// the assistant's final answer, the file paths touched this turn (harvested
// deterministically from tool-call args), and a source tag for non-interactive
// turns (e.g. "cron"). Tool intermediates (tool calls/results/system prompt)
// deliberately never enter the spine — they stay in run events, where recall
// can fetch them; the spine must never become a tool log.
type spineEntry struct {
	Kind        string   `json:"kind"`
	User        string   `json:"user"`
	Assistant   string   `json:"assistant"`
	Files       []string `json:"files,omitempty"`
	Source      string   `json:"source,omitempty"`
	ChainID     string   `json:"resume_chain_id,omitempty"`
	RunID       string   `json:"run_id,omitempty"`
	WorkspaceID string   `json:"workspace_id,omitempty"`
	// Outcome is how the turn that wrote the entry ended: a finish_run status,
	// "completed", or "incomplete: <reason>". It describes that turn, not the
	// Run's current state, which a later turn may have changed.
	Outcome    string `json:"outcome,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
	// TaskID is label provenance only (which task this turn was pre-labeled
	// with); it never gates what the model sees.
	TaskID string `json:"task_id,omitempty"`
	// Read-time fold metadata; saveHistory leaves these empty.
	EarlierUser       string `json:"earlier_user,omitempty"`
	EarlierObservedAt string `json:"earlier_observed_at,omitempty"`
	FoldedTurns       int    `json:"folded_turns,omitempty"`
}

// buildSpineEntry derives the slim turn entry from the finished turn: the
// original user input (gateway decoration blocks stripped), the assistant's
// final answer, and the tool-arg path harvest. messages carries only THIS
// turn's tool calls — replayed spine/legacy history is stripped of ToolCalls
// at load — so the harvest is turn-scoped by construction.
func buildSpineEntry(ctx context.Context, userInput, finalAnswer, outcome string, messages []llm.Message) spineEntry {
	if request := spineRequestFromContext(ctx); request != "" {
		userInput = request
	}
	entry := spineEntry{
		Kind:       spineEntryKind,
		User:       textutil.TruncateBytes(textutil.CleanUTF8(stripInjectedContextBlocks(userInput)), composerSpineUserSaveBytes),
		Assistant:  textutil.TruncateBytes(textutil.CleanUTF8(finalAnswer), composerSpineAnswerSaveBytes),
		Files:      harvestToolPaths(messages),
		Source:     TurnSourceFromContext(ctx),
		Outcome:    strings.TrimSpace(outcome),
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if runtime, ok := TaskRuntimeContextFromContext(ctx); ok {
		entry.TaskID = strings.TrimSpace(runtime.TaskID)
		entry.RunID = strings.TrimSpace(runtime.RunID)
		entry.ChainID = runtime.SpineChainID
		entry.WorkspaceID = strings.TrimSpace(runtime.WorkspaceID)
	}
	return entry
}

// toMessages renders a past turn as one reference record. Replaying its user
// text as a live user-role message made an unanswered old task look like the
// current request, especially when the model's final answer was empty. An
// assistant-role record preserves continuity without granting an old request
// the current user's authority. Existing v1 blobs need no migration.
func (e spineEntry) toMessages() []llm.Message {
	user := strings.TrimSpace(textutil.TruncateBytes(textutil.CleanUTF8(e.User), defaultHistoryUserBytes))
	assistant := strings.TrimSpace(textutil.TruncateBytes(textutil.CleanUTF8(e.Assistant), defaultHistoryAnswerBytes))
	if user == "" && assistant == "" && len(e.Files) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("[Prior work record; reference only; this record does not select a Run]\n")
	if e.ObservedAt != "" {
		fmt.Fprintf(&b, "observed_at: %s\n", e.ObservedAt)
	} else {
		b.WriteString("observed_at: unavailable (legacy historical record)\n")
	}
	b.WriteString("provenance: recorded response, not current execution or state evidence\n")
	if e.RunID != "" {
		fmt.Fprintf(&b, "run_id: %s\n", e.RunID)
	}
	if e.Outcome != "" {
		fmt.Fprintf(&b, "turn_outcome: %s\n", e.Outcome)
		if e.RunID != "" && (e.Outcome == "waiting_user" || e.Outcome == "interrupted" || e.Outcome == "blocked" || strings.HasPrefix(e.Outcome, "incomplete:")) {
			fmt.Fprintf(&b, "resume_hint: if the user's current message continues this work, call work_select(action=\"resume\", run_id=%q) first before running commands or skills\n", e.RunID)
		}
	}
	if e.WorkspaceID != "" {
		fmt.Fprintf(&b, "workspace_id: %s\n", e.WorkspaceID)
	}
	if e.TaskID != "" {
		fmt.Fprintf(&b, "group_label: %s\n", e.TaskID)
	}
	if e.Source != "" {
		fmt.Fprintf(&b, "source: %s\n", e.Source)
	}
	if e.FoldedTurns > 0 {
		fmt.Fprintf(&b, "folded_prior_turns: %d (recorded resume edges; inspect full history for omitted details)\n", e.FoldedTurns)
		if e.EarlierUser != "" && e.EarlierUser != e.User {
			fmt.Fprintf(&b, "earlier_request: %s observed_at=%q\n", strconv.Quote(textutil.TruncateBytes(e.EarlierUser, defaultHistoryUserBytes)), e.EarlierObservedAt)
		}
	}
	if user != "" {
		fmt.Fprintf(&b, "previous_user: %s\n", strconv.Quote(user))
	}
	if assistant != "" {
		fmt.Fprintf(&b, "final_answer: %s\n", strconv.Quote(assistant))
	} else {
		b.WriteString("final_answer: unavailable; inspect the exact Run before treating this work as pending or complete\n")
	}
	if len(e.Files) > 0 {
		fmt.Fprintf(&b, "files: %s\n", strings.Join(e.Files, ", "))
	}
	return []llm.Message{{Role: "assistant", Content: strings.TrimSpace(b.String()), ContextOrigin: llm.MessageOriginWorkSpine}}
}

// stripInjectedContextBlocks removes the gateway's prepended decoration blocks
// ("[SelfMind daemon context] … [/SelfMind daemon context]", resume context)
// so the spine stores the person's message text, not transport plumbing. It
// only strips well-formed LEADING blocks; anything else passes through.
func stripInjectedContextBlocks(input string) string {
	out := input
	for {
		trimmed := strings.TrimLeft(out, " \t\r\n")
		if !strings.HasPrefix(trimmed, "[SelfMind ") {
			out = trimmed
			break
		}
		end := strings.IndexByte(trimmed, ']')
		if end < 0 {
			out = trimmed
			break
		}
		open := trimmed[:end+1]
		if !strings.HasSuffix(open, " context]") {
			out = trimmed
			break
		}
		closing := "[/" + strings.TrimPrefix(open, "[")
		idx := strings.Index(trimmed, closing)
		if idx < 0 {
			out = trimmed
			break
		}
		out = trimmed[idx+len(closing):]
	}
	return strings.TrimRight(out, " \t\r\n")
}

// spineRequestKey carries the person's request a continuation turn serves.
type spineRequestKey struct{}

// WithSpineRequest records the person's own request for this turn's spine
// entry. A turn the daemon starts to continue parked work runs on scheduling
// text; the person-level work record keeps what they asked for instead.
func WithSpineRequest(ctx context.Context, request string) context.Context {
	request = strings.TrimSpace(request)
	if request == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, spineRequestKey{}, request)
}

func spineRequestFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	request, _ := ctx.Value(spineRequestKey{}).(string)
	return request
}

// turnSourceKey carries an optional origin tag for non-interactive turns.
type turnSourceKey struct{}

// WithTurnSource tags the turn with its non-interactive origin (e.g. "cron")
// so the spine entry records where the work came from. Interactive turns never
// set it.
func WithTurnSource(ctx context.Context, source string) context.Context {
	source = strings.TrimSpace(source)
	if source == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, turnSourceKey{}, source)
}

// TurnSourceFromContext returns the non-interactive origin tag, or "".
func TurnSourceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	source, _ := ctx.Value(turnSourceKey{}).(string)
	return source
}

// ContextComposer is the thin named entry point for the per-turn assembly
// contract documented at the top of this file. It owns no state beyond the
// engine; its value is making the slice order and budgets explicit and giving
// P2 (recall) and later packages one place to extend.
type ContextComposer struct {
	engine *ContextEngine
}

// Composer returns the agent's per-turn context composer.
func (a *Agent) Composer() *ContextComposer {
	if a == nil {
		return nil
	}
	return &ContextComposer{engine: a.contextEngine}
}

// Compose builds the turn's message window: system prompt (slices ④–⑧ render
// inside it via RuntimeContextBundle/buildSystemPrompt) + spine tail (②) +
// compaction summary when over budget (③, added by the engine) + the latest
// user message (①). spineKey/fallbackKeys come from Agent.trajectoryKey /
// Agent.trajectoryFallbackKeys so load and save always agree.
func (cc *ContextComposer) Compose(
	ctx context.Context,
	mem *memory.MemoryManager,
	tenantID string,
	spineKey string,
	fallbackKeys []string,
	systemPrompt string,
	userInput string,
) ([]llm.Message, error) {
	return cc.engine.BuildMessages(ctx, mem, tenantID, spineKey, fallbackKeys, systemPrompt, userInput)
}
