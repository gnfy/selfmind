package kernel

import (
	"context"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/textutil"
)

// This file is the ContextComposer contract (docs/work-timeline.md,
// "Per-turn context (ContextComposer)"): the ONE place that names how a model
// turn's window is assembled, in this fixed slice order:
//
//	① latest user message        — the only authoritative instruction
//	② spine tail                 — recent person-level work-spine turns, verbatim
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
	// composerSpineTailEntries is M: how many recent spine turns are replayed
	// verbatim as slice ② (alternating user/assistant messages).
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
	Kind      string   `json:"kind"`
	User      string   `json:"user"`
	Assistant string   `json:"assistant"`
	Files     []string `json:"files,omitempty"`
	Source    string   `json:"source,omitempty"`
	// TaskID is label provenance only (which task this turn was pre-labeled
	// with); it never gates what the model sees.
	TaskID string `json:"task_id,omitempty"`
}

// buildSpineEntry derives the slim turn entry from the finished turn: the
// original user input (gateway decoration blocks stripped), the assistant's
// final answer, and the tool-arg path harvest. messages carries only THIS
// turn's tool calls — replayed spine/legacy history is stripped of ToolCalls
// at load — so the harvest is turn-scoped by construction.
func buildSpineEntry(ctx context.Context, userInput, finalAnswer string, messages []llm.Message) spineEntry {
	entry := spineEntry{
		Kind:      spineEntryKind,
		User:      textutil.TruncateBytes(textutil.CleanUTF8(stripInjectedContextBlocks(userInput)), composerSpineUserSaveBytes),
		Assistant: textutil.TruncateBytes(textutil.CleanUTF8(finalAnswer), composerSpineAnswerSaveBytes),
		Files:     harvestToolPaths(messages),
		Source:    TurnSourceFromContext(ctx),
	}
	if runtime, ok := TaskRuntimeContextFromContext(ctx); ok {
		entry.TaskID = strings.TrimSpace(runtime.TaskID)
	}
	return entry
}

// toMessages renders one spine entry as the user/assistant message pair the
// tail replays. Truncation happens before the files suffix so the touched-path
// list survives even a long answer.
func (e spineEntry) toMessages() []llm.Message {
	var out []llm.Message
	user := strings.TrimSpace(textutil.TruncateBytes(textutil.CleanUTF8(e.User), defaultHistoryUserBytes))
	if src := strings.TrimSpace(e.Source); src != "" && user != "" {
		user = "[" + src + "] " + user
	}
	if user != "" {
		out = append(out, llm.Message{Role: "user", Content: user})
	}
	assistant := strings.TrimSpace(textutil.TruncateBytes(textutil.CleanUTF8(e.Assistant), defaultHistoryAnswerBytes))
	if len(e.Files) > 0 {
		suffix := "[files: " + strings.Join(e.Files, ", ") + "]"
		if assistant != "" {
			assistant += "\n" + suffix
		} else {
			assistant = suffix
		}
	}
	if assistant != "" {
		out = append(out, llm.Message{Role: "assistant", Content: assistant})
	}
	return out
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
