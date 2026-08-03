package cli

import (
	"strings"
	"testing"
	"time"

	"selfmind/internal/tools"
)

// TestApprovalHeldWhileTypingThenArms pins the batch-A misfire guard: an approval
// that arrives mid-keystroke must NOT arm the panel, because the panel consumes
// every key and "y" is both a common letter and "yes, run it". The request is
// durable in the daemon, so holding the LOCAL prompt loses nothing.
func TestApprovalHeldWhileTypingThenArms(t *testing.T) {
	model, calls := newApprovalTestModel()

	updated, _ := model.Update(keyRunes("h"))
	model = updated.(*uiModel)

	updated, cmd := model.Update(sampleApproval("apr_typing"))
	model = updated.(*uiModel)
	if model.approvalPrompt != nil {
		t.Fatal("panel must not arm while the person is typing")
	}
	if len(model.delayedApprovals) != 1 {
		t.Fatalf("request should be held, delayed = %d", len(model.delayedApprovals))
	}
	if cmd == nil {
		t.Fatal("holding an approval must schedule the re-check tick")
	}
	// A keystroke arriving in the hold window still goes to the composer, never
	// to a panel that is not up yet.
	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)
	if len(*calls) != 0 {
		t.Fatalf("a keystroke must not answer a held approval: %v", *calls)
	}

	// Once typing goes idle, the tick arms the panel with the same request.
	model.lastInputActivityAt = time.Now().Add(-2 * approvalTypingIdleDelay)
	updated, _ = model.Update(MsgApprovalDelayElapsed{})
	model = updated.(*uiModel)
	if model.approvalPrompt == nil {
		t.Fatal("panel should arm once input goes idle")
	}
	if model.pendingApprovalID != "apr_typing" {
		t.Fatalf("pendingApprovalID = %q", model.pendingApprovalID)
	}
	if len(model.delayedApprovals) != 0 {
		t.Fatalf("held queue should drain, delayed = %d", len(model.delayedApprovals))
	}
}

// TestApprovalArmsImmediatelyWhenIdle proves the guard only delays a panel that
// would actually race a keystroke: with no recent typing the panel is immediate,
// which is the common case (the person is watching, not typing).
func TestApprovalArmsImmediatelyWhenIdle(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.lastInputActivityAt = time.Now().Add(-time.Minute)

	updated, _ := model.Update(sampleApproval("apr_idle"))
	model = updated.(*uiModel)
	if model.approvalPrompt == nil {
		t.Fatal("an approval arriving while input is idle should arm at once")
	}
	if len(model.delayedApprovals) != 0 {
		t.Fatalf("nothing should be held, delayed = %d", len(model.delayedApprovals))
	}
}

// TestHeldApprovalQueuesBehindActivePanel proves a held request is never lost
// when a different approval armed first: it falls back to the normal FIFO queue.
func TestHeldApprovalQueuesBehindActivePanel(t *testing.T) {
	model, _ := newApprovalTestModel()

	updated, _ := model.Update(keyRunes("x"))
	model = updated.(*uiModel)
	updated, _ = model.Update(sampleApproval("apr_held"))
	model = updated.(*uiModel)

	// A second approval arms directly (e.g. delivered after input went idle).
	model.lastInputActivityAt = time.Time{}
	model.armApprovalPrompt(sampleApproval("apr_active"))

	model.lastInputActivityAt = time.Now().Add(-2 * approvalTypingIdleDelay)
	updated, _ = model.Update(MsgApprovalDelayElapsed{})
	model = updated.(*uiModel)

	if model.pendingApprovalID != "apr_active" {
		t.Fatalf("active panel must keep its request, got %q", model.pendingApprovalID)
	}
	if len(model.approvalQueue) != 1 || model.approvalQueue[0].ID != "apr_held" {
		t.Fatalf("held request should fall back to the FIFO queue, queue = %+v", model.approvalQueue)
	}
}

// TestApprovalPanelShowsDaemonContext proves the daemon's decision context
// reaches the rendered panel: this is the whole point of publishing cwd,
// environment, change size, and triage state with the approval event.
func TestApprovalPanelShowsDaemonContext(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.lastInputActivityAt = time.Time{}

	msg := sampleApproval("apr_ctx")
	msg.Tool = "terminal"
	msg.Target = "python3 scripts/report.py"
	msg.Cwd = "/mnt/d/wwwroot/ai/selfmind"
	msg.Environment = "envsnap_1_789c4317"
	msg.ChangeSummary = "1 file +12/-0"
	msg.GrantClass = `"python3" commands`
	msg.TriageState = tools.TriageStateUnavailable

	updated, _ := model.Update(msg)
	model = updated.(*uiModel)
	if model.approvalPrompt == nil {
		t.Fatal("panel should be armed")
	}
	view := model.approvalPrompt.View(100)
	for _, want := range []string{
		"/mnt/d/wwwroot/ai/selfmind",
		"env envsnap_1_789c4317",
		"1 file +12/-0",
		`"python3" commands`,
		"automatic triage unavailable",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("panel missing %q:\n%s", want, view)
		}
	}
}

// TestEscalatedTriageStateShowsNoUnavailableNotice keeps the notice honest: a
// deliberate escalation is the funnel working, and mislabelling it "unavailable"
// would teach the person to ignore the line that matters.
func TestEscalatedTriageStateShowsNoUnavailableNotice(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.lastInputActivityAt = time.Time{}

	msg := sampleApproval("apr_esc")
	msg.TriageState = tools.TriageStateEscalated
	updated, _ := model.Update(msg)
	model = updated.(*uiModel)

	if view := model.approvalPrompt.View(100); strings.Contains(view, "automatic triage unavailable") {
		t.Fatalf("escalation must not claim triage was unavailable:\n%s", view)
	}
}
