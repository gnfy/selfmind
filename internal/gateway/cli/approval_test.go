package cli

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// approvalRespondCall records one call to the approval responder so tests can
// assert the decision AND the grant scope reach the existing respond path.
type approvalRespondCall struct {
	id, decision, scope string
}

func newApprovalTestModel() (*uiModel, *[]approvalRespondCall) {
	model := NewController(nil, nil, nil, "").model
	model.width = 100
	model.height = 30
	model.viewport.Width = 100
	model.viewport.Height = 24
	model.hybrid = true
	calls := &[]approvalRespondCall{}
	model.approvalResponder = func(id, decision, scope string) error {
		*calls = append(*calls, approvalRespondCall{id, decision, scope})
		return nil
	}
	return model, calls
}

func sampleApproval(id string) MsgApprovalRequest {
	return MsgApprovalRequest{
		ID:     id,
		Tool:   "write_file",
		Target: "/mnt/d/wwwroot/ai/game/ember-citadel-tank-battle.html",
		Reason: "accesses path outside project root",
	}
}

func keyRunes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestApprovalRequestArmsPanel(t *testing.T) {
	model, calls := newApprovalTestModel()

	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	if model.approvalPrompt == nil {
		t.Fatal("approval panel should be armed")
	}
	if model.pendingApprovalID != "apr_1" {
		t.Fatalf("pendingApprovalID = %q", model.pendingApprovalID)
	}
	if len(*calls) != 0 {
		t.Fatalf("arming must not answer: %v", *calls)
	}
	// Deduplication: no legacy "type y to allow" notice.
	if strings.Contains(model.statusMsg, "type y") {
		t.Fatalf("legacy text notice must be suppressed, statusMsg = %q", model.statusMsg)
	}
	// Durable transcript record: exactly one compact notice line.
	last := model.messages[len(model.messages)-1]
	if last.Role != "notice" || !strings.Contains(last.Content, "Approval required: write_file") {
		t.Fatalf("compact transcript record missing: %+v", last)
	}
	if rendered := stripANSI(renderNoticeMessage(last.Content, 80)); strings.Contains(rendered, "\n") {
		t.Fatalf("transcript record must be ONE line: %q", rendered)
	}
	// The panel renders in the hybrid active region.
	view := stripANSI(model.View())
	if !strings.Contains(view, "Approval required") || !strings.Contains(view, "Yes, run it once") {
		t.Fatalf("hybrid view missing the panel:\n%s", view)
	}
}

func TestApprovalPanelRendersInLegacyModeToo(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.hybrid = false

	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	view := stripANSI(model.View())
	if !strings.Contains(view, "Approval required") || !strings.Contains(view, "Yes, run it once") {
		t.Fatalf("legacy view missing the panel:\n%s", view)
	}
}

// TestApprovalPanelSuppressesSpinnerLine: while the panel is up, the
// "Preparing to run <tool>" spinner/activity line is redundant noise and must
// disappear in BOTH renderers. It returns once the run resumes.
func TestApprovalPanelSuppressesSpinnerLine(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.thinking = true
	model.activityText = "Preparing to run write_file."

	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	if active := stripANSI(model.renderActiveBlock(100)); strings.Contains(active, "Preparing to run") {
		t.Fatalf("hybrid active region must hide the spinner line while the panel is up:\n%s", active)
	}
	if all := stripANSI(model.renderAllMessages()); strings.Contains(all, "Preparing to run") {
		t.Fatalf("legacy transcript must hide the spinner line while the panel is up:\n%s", all)
	}

	// Resolving the approval resumes the run and restores the working indicator.
	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)
	if !model.thinking {
		t.Fatal("run should resume as working after the decision")
	}
	if active := stripANSI(model.renderActiveBlock(100)); !strings.Contains(active, "Working") {
		t.Fatalf("spinner line should return after the decision:\n%s", active)
	}
}

func TestApprovalShortcutScopes(t *testing.T) {
	cases := []struct {
		key      string
		decision string
		scope    string
		note     string
	}{
		{"y", "approved", "", "(this time)"},
		{"t", "approved", "task", "(allowed for this task)"},
		{"a", "approved", "person", "(always allowed)"},
	}
	for _, tc := range cases {
		model, calls := newApprovalTestModel()
		updated, _ := model.Update(sampleApproval("apr_1"))
		model = updated.(*uiModel)

		updated, _ = model.Update(keyRunes(tc.key))
		model = updated.(*uiModel)

		if len(*calls) != 1 {
			t.Fatalf("key %q: responder calls = %v", tc.key, *calls)
		}
		got := (*calls)[0]
		if got.id != "apr_1" || got.decision != tc.decision || got.scope != tc.scope {
			t.Fatalf("key %q: responder got %+v, want apr_1/%s/%s", tc.key, got, tc.decision, tc.scope)
		}
		if model.approvalPrompt != nil {
			t.Fatalf("key %q: panel should close after the decision", tc.key)
		}
		if !model.thinking || model.runStatus != "working" {
			t.Fatalf("key %q: run should resume (thinking=%v status=%q)", tc.key, model.thinking, model.runStatus)
		}
		last := model.messages[len(model.messages)-1]
		if last.Role != "notice" || !strings.Contains(last.Content, "Approved write_file "+tc.note) {
			t.Fatalf("key %q: decision record = %+v, want note %q", tc.key, last, tc.note)
		}
	}
}

func TestApprovalEnterSelectsHighlightedOption(t *testing.T) {
	model, calls := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	for _, key := range []tea.KeyMsg{{Type: tea.KeyDown}, {Type: tea.KeyDown}} {
		updated, _ = model.Update(key)
		model = updated.(*uiModel)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(*uiModel)

	if len(*calls) != 1 || (*calls)[0].scope != "person" || (*calls)[0].decision != "approved" {
		t.Fatalf("down,down,enter should approve with person scope: %v", *calls)
	}
}

// TestApprovalEscAndStrayKeysDoNotAnswer: an approval is an explicit decision.
// Esc does nothing, and other keys are swallowed instead of leaking into the
// composer.
func TestApprovalEscAndStrayKeysDoNotAnswer(t *testing.T) {
	model, calls := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, keyRunes("x"), keyRunes("q"), {Type: tea.KeyTab}} {
		updated, _ = model.Update(key)
		model = updated.(*uiModel)
	}

	if model.approvalPrompt == nil {
		t.Fatal("panel must stay armed")
	}
	if len(*calls) != 0 {
		t.Fatalf("no decision should have been sent: %v", *calls)
	}
	if model.editor.Value() != "" {
		t.Fatalf("stray keys leaked into the composer: %q", model.editor.Value())
	}
}

func TestApprovalDenyThenGuidanceSendsRejectionAndSteering(t *testing.T) {
	model, calls := newApprovalTestModel()
	model.clientMode = true
	model.thinking = true
	var steered string
	model.steerFn = func(text string) error {
		steered = text
		return nil
	}
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	updated, _ = model.Update(keyRunes("n"))
	model = updated.(*uiModel)
	if model.approvalPrompt != nil {
		t.Fatal("panel should close into the deny follow-up")
	}
	if !model.approvalDenyFollowup {
		t.Fatal("deny follow-up should capture the composer")
	}
	if len(*calls) != 0 {
		t.Fatalf("rejection must wait for the follow-up Enter: %v", *calls)
	}
	if hint := stripANSI(model.composerHint()); !strings.Contains(hint, "Tell the agent what to do instead") {
		t.Fatalf("composer hint missing: %q", hint)
	}

	model.editor.SetValue("use a path inside the workspace instead")
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(*uiModel)

	if len(*calls) != 1 || (*calls)[0].decision != "rejected" || (*calls)[0].scope != "" {
		t.Fatalf("rejection not sent first: %v", *calls)
	}
	if steered != "use a path inside the workspace instead" {
		t.Fatalf("guidance not steered: %q", steered)
	}
	if model.approvalDenyFollowup {
		t.Fatal("deny follow-up should be resolved")
	}
	var sawDenied bool
	for _, msg := range model.messages {
		if msg.Role == "notice" && strings.Contains(msg.Content, "Denied write_file") {
			sawDenied = true
		}
	}
	if !sawDenied {
		t.Fatal("deny decision record missing from transcript")
	}
}

func TestApprovalDenyBareEnterJustDenies(t *testing.T) {
	model, calls := newApprovalTestModel()
	model.clientMode = true
	var steered bool
	model.steerFn = func(string) error {
		steered = true
		return nil
	}
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	updated, _ = model.Update(keyRunes("n"))
	model = updated.(*uiModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(*uiModel)

	if len(*calls) != 1 || (*calls)[0].decision != "rejected" {
		t.Fatalf("bare Enter should send a plain deny: %v", *calls)
	}
	if steered {
		t.Fatal("bare deny must not send steering guidance")
	}
	if model.statusMsg != "Denied." {
		t.Fatalf("statusMsg = %q", model.statusMsg)
	}
}

// TestApprovalQueueRearmsNext: approvals that arrive while one is pending are
// answered one at a time, FIFO.
func TestApprovalQueueRearmsNext(t *testing.T) {
	model, calls := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgApprovalRequest{ID: "apr_2", Tool: "terminal", Reason: "invokes dangerous command: chmod"})
	model = updated.(*uiModel)

	if model.pendingApprovalID != "apr_1" || len(model.approvalQueue) != 1 {
		t.Fatalf("second request should queue: pending=%q queue=%v", model.pendingApprovalID, model.approvalQueue)
	}

	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)

	if len(*calls) != 1 || (*calls)[0].id != "apr_1" {
		t.Fatalf("first answer should target apr_1: %v", *calls)
	}
	if model.approvalPrompt == nil || model.pendingApprovalID != "apr_2" {
		t.Fatalf("panel should re-arm with apr_2: pending=%q", model.pendingApprovalID)
	}
	if model.approvalPrompt.Tool() != "terminal" {
		t.Fatalf("re-armed panel tool = %q", model.approvalPrompt.Tool())
	}

	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)
	if len(*calls) != 2 || (*calls)[1].id != "apr_2" {
		t.Fatalf("second answer should target apr_2: %v", *calls)
	}
	if model.approvalPrompt != nil || len(model.approvalQueue) != 0 {
		t.Fatal("flow should be fully drained")
	}
}

func TestStatusBarShowsWaitingApproval(t *testing.T) {
	model, _ := newApprovalTestModel()
	if line := stripANSI(model.statusLine()); strings.Contains(line, "waiting approval") {
		t.Fatalf("idle status must not claim a pending approval: %q", line)
	}

	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	line := stripANSI(model.statusLine())
	if !strings.Contains(line, "waiting approval") {
		t.Fatalf("status bar must show the waiting-approval state: %q", line)
	}
	if strings.Contains(line, "ready") {
		t.Fatalf("status bar must not say ready while an approval is pending: %q", line)
	}

	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)
	if line := stripANSI(model.statusLine()); strings.Contains(line, "waiting approval") {
		t.Fatalf("waiting state should clear after the decision: %q", line)
	}
}

// TestAgentDoneClearsApprovalFlow: when the run ends, the daemon expires
// unanswered approval rows — stale panel state must not survive to answer into
// the void.
func TestAgentDoneClearsApprovalFlow(t *testing.T) {
	model, _ := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgApprovalRequest{ID: "apr_2", Tool: "terminal"})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgAgentDone{Response: "run finished"})
	model = updated.(*uiModel)

	if model.approvalPrompt != nil || len(model.approvalQueue) != 0 || model.approvalDenyFollowup {
		t.Fatal("approval flow must be cleared when the run ends")
	}
}

// TestApprovalWithoutResponderIsHonest: if no responder is installed (in-process
// session), the decision must fail loudly instead of silently doing nothing.
func TestApprovalWithoutResponderIsHonest(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.approvalResponder = nil
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)

	var sawError bool
	for _, msg := range model.messages {
		if msg.IsError && strings.Contains(msg.Content, "Could not send approval") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("missing responder must surface an honest error")
	}
}
