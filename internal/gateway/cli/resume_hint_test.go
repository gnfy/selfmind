package cli

import (
	"testing"
	"time"
)

// TestHasConversationHistoryIgnoresComposerHistory pins the resume-hint
// regression: composer history is all-time typing, not this session transcript,
// so a zero-input open-and-close must NOT advertise a resume command.
func TestHasConversationHistoryIgnoresPersistedInputHistory(t *testing.T) {
	c := NewController("", "", nil, "")
	c.model.editor.SeedHistory([]string{"prior session command", "another one"}, 1024)
	if c.HasConversationHistory() {
		t.Fatal("persisted input history alone must not report conversation history")
	}
}

// TestHasConversationHistoryIgnoresAssistantOnlyContent: startup content the
// user never responded to (digest, tips, notices) is not a resumable session.
func TestHasConversationHistoryIgnoresAssistantOnlyContent(t *testing.T) {
	c := &Controller{model: &uiModel{
		messages: []ChatMessage{
			{Role: "assistant", Content: "While you were away: 2 tasks finished.", Timestamp: time.Now()},
			{Role: "system", Content: "notice", Timestamp: time.Now()},
		},
	}}
	if c.HasConversationHistory() {
		t.Fatal("assistant/system-only transcript must not report conversation history")
	}
}

// TestHasConversationHistoryDetectsUserTurn: one non-empty user message —
// however it was submitted — makes the session worth a resume hint.
func TestHasConversationHistoryDetectsUserTurn(t *testing.T) {
	c := &Controller{model: &uiModel{
		messages: []ChatMessage{
			{Role: "assistant", Content: "hello", Timestamp: time.Now()},
			{Role: "user", Content: "查一下部署状态", Timestamp: time.Now()},
		},
	}}
	if !c.HasConversationHistory() {
		t.Fatal("a user turn must report conversation history")
	}

	// Whitespace-only user content does not count.
	c = &Controller{model: &uiModel{
		messages: []ChatMessage{{Role: "user", Content: "   ", Timestamp: time.Now()}},
	}}
	if c.HasConversationHistory() {
		t.Fatal("whitespace-only user content must not report conversation history")
	}
}

// TestHasConversationHistoryNilSafety mirrors the callers' nil tolerance.
func TestHasConversationHistoryNilSafety(t *testing.T) {
	var c *Controller
	if c.HasConversationHistory() {
		t.Fatal("nil controller must report false")
	}
	if (&Controller{}).HasConversationHistory() {
		t.Fatal("nil model must report false")
	}
}
