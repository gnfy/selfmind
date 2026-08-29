package components

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

const maxComposerHistoryEntries = 200

// ComposerAction is the controller-visible result of one composer key event.
// History, completion, and textarea ownership stay inside Editor; callers only
// need to know whether the key was consumed or requested submission.
type ComposerAction uint8

const (
	ComposerActionUnhandled ComposerAction = iota
	ComposerActionHandled
	ComposerActionSubmit
)

type ComposerResult struct {
	Action ComposerAction
	Cmd    tea.Cmd
}

func (r ComposerResult) Handled() bool {
	return r.Action != ComposerActionUnhandled
}

// ComposerHistoryDisposition tells Submit whether the accepted draft belongs
// in history. Persistent history is still limited to payload-free text;
// paste/image drafts remain recallable only for the lifetime of this Editor.
type ComposerHistoryDisposition uint8

const (
	ComposerHistoryNone ComposerHistoryDisposition = iota
	ComposerHistorySessionOnly
	ComposerHistoryPersistent
)

// ComposerSubmission is the immutable value handed to the controller after
// the composer has decided how to record and clear the draft.
type ComposerSubmission struct {
	Display        string
	Expanded       string
	Unresolved     string
	PersistentText string
	Persist        bool
}

// composerDraft is a restorable, current-process composer entry. Persistent
// history seeds are text-only; locally recorded entries may also own paste and
// image payloads.
type composerDraft struct {
	Text     string
	Snippets []PasteSnippet
	Images   []ImageAttachment
}

func (d composerDraft) sizeBytes() int64 {
	size := int64(len(d.Text))
	for _, snippet := range d.Snippets {
		size += int64(len(snippet.Token) + len(snippet.Text))
	}
	for _, image := range d.Images {
		size += int64(len(image.Token) + len(image.Path) + len(image.Name))
	}
	return size
}

// SeedHistory installs the persistent, text-only prefix used by this composer.
// Entries are oldest-first. maxBytes bounds the combined in-memory history;
// non-positive values use the standard 512 KiB default.
func (e *Editor) SeedHistory(entries []string, maxBytes int64) {
	if maxBytes <= 0 {
		maxBytes = 524288
	}
	e.history = nil
	e.historyBytes = 0
	e.historyMaxBytes = maxBytes
	e.historyIndex = -1
	e.dismissedToken = ""
	for _, text := range entries {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		e.appendHistoryDraft(composerDraft{Text: text})
	}
}

// HandleKey owns composer-local key arbitration. Recalled history gets first
// claim on Up/Down; a manually typed completion prefix gets the next claim;
// otherwise the textarea handles normal cursor movement.
func (e *Editor) HandleKey(msg tea.KeyMsg) ComposerResult {
	switch msg.Type {
	case tea.KeyUp:
		if e.navigateHistory(-1) {
			return ComposerResult{Action: ComposerActionHandled}
		}
		if e.SuggestionsVisible() {
			e.MoveSuggestion(-1)
			return ComposerResult{Action: ComposerActionHandled}
		}
		return ComposerResult{Action: ComposerActionHandled, Cmd: e.Update(msg)}
	case tea.KeyDown:
		if e.navigateHistory(1) {
			return ComposerResult{Action: ComposerActionHandled}
		}
		if e.SuggestionsVisible() {
			e.MoveSuggestion(1)
			return ComposerResult{Action: ComposerActionHandled}
		}
		return ComposerResult{Action: ComposerActionHandled, Cmd: e.Update(msg)}
	case tea.KeyTab:
		if e.AcceptSuggestion() {
			return ComposerResult{Action: ComposerActionHandled}
		}
		return ComposerResult{Action: ComposerActionHandled, Cmd: e.Update(msg)}
	case tea.KeyEnter:
		e.AcceptSuggestion()
		return ComposerResult{Action: ComposerActionSubmit}
	case tea.KeyEsc:
		if e.SuggestionsVisible() {
			e.dismissedToken = e.Value()
			e.hintIndex = 0
			return ComposerResult{Action: ComposerActionHandled}
		}
		return ComposerResult{Action: ComposerActionUnhandled}
	default:
		return ComposerResult{Action: ComposerActionHandled, Cmd: e.Update(msg)}
	}
}

// PreviewSubmission resolves a draft without changing editor or history
// state. Controllers use it for validation that must preserve rejected input.
func (e *Editor) PreviewSubmission() ComposerSubmission {
	if e == nil {
		return ComposerSubmission{}
	}
	display := strings.TrimSpace(e.Value())
	expanded := e.ExpandValue()
	if display == "" {
		display = expanded
	}
	return ComposerSubmission{
		Display:    display,
		Expanded:   expanded,
		Unresolved: e.UnresolvedToken(),
	}
}

// Submit records the current draft according to disposition, returns the
// resolved value, and resets the visible composer. Rich payload ownership is
// retained only by the in-process history snapshot.
func (e *Editor) Submit(disposition ComposerHistoryDisposition) ComposerSubmission {
	submission := e.PreviewSubmission()
	if e == nil {
		return submission
	}
	draft := e.currentComposerDraft()
	if disposition != ComposerHistoryNone && !e.secure {
		e.appendHistoryDraft(draft)
	}
	if disposition == ComposerHistoryPersistent && !e.secure && submission.Unresolved == "" &&
		len(draft.Snippets) == 0 && len(draft.Images) == 0 {
		text := strings.TrimSpace(draft.Text)
		if text != "" {
			submission.PersistentText = text
			submission.Persist = true
		}
	}
	e.Reset()
	return submission
}

// Clear discards the visible draft and optionally retains a session-local
// snapshot. It is the Ctrl+C path: no cleared draft is ever marked persistent.
func (e *Editor) Clear(disposition ComposerHistoryDisposition) bool {
	if e == nil || (e.Value() == "" && len(e.snippets) == 0 && len(e.images) == 0) {
		return false
	}
	if disposition != ComposerHistoryNone && !e.secure {
		e.appendHistoryDraft(e.currentComposerDraft())
	}
	e.Reset()
	return true
}

func (e *Editor) browsingHistory() bool {
	if e == nil || e.secure || e.historyIndex < 0 || e.historyIndex >= len(e.history) {
		return false
	}
	return e.Value() == e.history[e.historyIndex].Text && e.CursorAtTextBoundary()
}

func (e *Editor) navigateHistory(delta int) bool {
	if e == nil || e.secure || len(e.history) == 0 {
		return false
	}
	current := e.Value()
	if current != "" && !e.browsingHistory() {
		return false
	}

	switch {
	case delta < 0:
		if e.historyIndex == -1 {
			e.historyIndex = len(e.history) - 1
		} else if e.historyIndex > 0 {
			e.historyIndex--
		}
		e.applyHistoryDraft(e.history[e.historyIndex])
		return true
	case delta > 0:
		if e.historyIndex == -1 {
			return false
		}
		if e.historyIndex < len(e.history)-1 {
			e.historyIndex++
			e.applyHistoryDraft(e.history[e.historyIndex])
			return true
		}
		e.historyIndex = -1
		e.applyHistoryDraft(composerDraft{})
		return true
	default:
		return false
	}
}

func (e *Editor) applyHistoryDraft(draft composerDraft) {
	e.textarea.SetValue(draft.Text)
	e.textarea.CursorEnd()
	e.snippets = append([]PasteSnippet(nil), draft.Snippets...)
	e.images = append([]ImageAttachment(nil), draft.Images...)
	e.hintIndex = 0
	e.dismissedToken = ""
}

func (e *Editor) currentComposerDraft() composerDraft {
	if e == nil || e.secure {
		return composerDraft{}
	}
	return composerDraft{
		Text:     e.textarea.Value(),
		Snippets: append([]PasteSnippet(nil), e.snippets...),
		Images:   append([]ImageAttachment(nil), e.images...),
	}
}

func (e *Editor) appendHistoryDraft(draft composerDraft) {
	if draft.Text == "" && len(draft.Snippets) == 0 && len(draft.Images) == 0 {
		return
	}
	if len(e.history) > 0 && equalComposerDraft(e.history[len(e.history)-1], draft) {
		e.historyIndex = -1
		return
	}
	e.history = append(e.history, draft)
	e.historyBytes += draft.sizeBytes()
	e.historyIndex = -1
	for len(e.history) > 1 && (len(e.history) > maxComposerHistoryEntries ||
		(e.historyMaxBytes > 0 && e.historyBytes > e.historyMaxBytes)) {
		e.historyBytes -= e.history[0].sizeBytes()
		e.history = e.history[1:]
	}
}

func equalComposerDraft(a, b composerDraft) bool {
	if a.Text != b.Text || len(a.Snippets) != len(b.Snippets) || len(a.Images) != len(b.Images) {
		return false
	}
	for i := range a.Snippets {
		if a.Snippets[i] != b.Snippets[i] {
			return false
		}
	}
	for i := range a.Images {
		if a.Images[i] != b.Images[i] {
			return false
		}
	}
	return true
}
