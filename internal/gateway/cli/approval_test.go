package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/ui/components"
)

// approvalRespondCall records one call to the approval responder so tests can
// assert the decision AND the grant scope reach the existing respond path.
type approvalRespondCall struct {
	id, decision, scope, grantKey string
}

func newApprovalTestModel() (*uiModel, *[]approvalRespondCall) {
	model := NewController(nil, nil, nil, "").model
	model.width = 100
	model.height = 30
	calls := &[]approvalRespondCall{}
	model.approvalResponder = func(id, decision, scope, grantKey string) error {
		*calls = append(*calls, approvalRespondCall{id, decision, scope, grantKey})
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
	if last.NoticeKind != noticeWarning {
		t.Fatalf("approval request notice kind = %v", last.NoticeKind)
	}
	if rendered := stripANSI(renderNoticeMessage(last.Content, last.NoticeKind, 80)); strings.Contains(rendered, "\n") {
		t.Fatalf("transcript record must be ONE line: %q", rendered)
	}
	// The panel renders in the hybrid active region.
	view := stripANSI(model.View())
	if !strings.Contains(view, "Would you like to make the following edits?") || !strings.Contains(view, "Yes, proceed") {
		t.Fatalf("hybrid view missing the panel:\n%s", view)
	}
}

func TestApprovalPromptStaysInActiveRegion(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.processState().Update(processEvent{
		kind: processStreamDelta, content: "UNDERLYING_REASONING", phase: llm.AssistantPhaseCommentary,
	})
	model.activePlanJSON = `{"plan":[{"step":"UNDERLYING_PLAN","status":"in_progress"}]}`
	model.editor.SetValue("UNDERLYING_DRAFT")

	updated, _ := model.Update(sampleApproval("apr_inline"))
	model = updated.(*uiModel)
	view := stripANSI(model.View())

	if !strings.Contains(view, "Would you like to make the following edits?") || !strings.Contains(view, "Yes, proceed") {
		t.Fatalf("inline approval is missing its decision content:\n%s", view)
	}
	for _, visible := range []string{"UNDERLYING_REASONING", "UNDERLYING_PLAN", "UNDERLYING_DRAFT"} {
		if !strings.Contains(view, visible) {
			t.Fatalf("inline approval hid active-region content %q:\n%s", visible, view)
		}
	}
}

func TestInlineApprovalCapturesKeysOverPager(t *testing.T) {
	model, calls := newApprovalTestModel()
	model.openHelp()

	updated, _ := model.Update(sampleApproval("apr_over_pager"))
	model = updated.(*uiModel)
	if view := stripANSI(model.View()); !strings.Contains(view, "Would you like to make the following edits?") {
		t.Fatalf("inline approval should temporarily preempt the pager:\n%s", view)
	}
	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)

	if len(*calls) != 1 || (*calls)[0].id != "apr_over_pager" || (*calls)[0].decision != "approved" {
		t.Fatalf("inline approval did not capture the decision over pager: calls=%+v", *calls)
	}
	if model.approvalPrompt != nil {
		t.Fatal("approved inline prompt remained active")
	}
	if view := stripANSI(model.View()); !strings.Contains(view, "Keyboard") {
		t.Fatalf("pager should return after the approval resolves:\n%s", view)
	}
}

// TestApprovalPanelSuppressesSpinnerLine: while the panel is up, the
// model-wait spinner is redundant noise and must disappear from the active
// region. It returns only when a subsequent structured model_wait arrives.
func TestApprovalPanelSuppressesSpinnerLine(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.startModelWait("Waiting for the model")

	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	if active := stripANSI(model.renderActiveBlock(100)); strings.Contains(active, "Waiting for the model") {
		t.Fatalf("hybrid active region must hide the spinner line while the panel is up:\n%s", active)
	}
	// Resolving the approval resumes the run, but does not guess that the model is
	// waiting. The next structured phase restarts the spinner.
	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)
	if !model.thinking {
		t.Fatal("run should resume as working after the decision")
	}
	if active := stripANSI(model.renderActiveBlock(100)); strings.TrimSpace(active) != "" {
		t.Fatalf("spinner returned before model_wait:\n%s", active)
	}
	updated, _ = model.Update(MsgAgentActivity{Phase: modelWaitPhase, Content: "Waiting for the model to decide after tool results"})
	model = updated.(*uiModel)
	if active := stripANSI(model.renderActiveBlock(100)); !strings.Contains(active, "Waiting for the model") {
		t.Fatalf("spinner did not restart on model_wait:\n%s", active)
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
	msg := sampleApproval("apr_1")
	msg.Options = []components.ApprovalOption{
		{Label: "Yes, proceed", Key: "y", Decision: "approved"},
		{Label: "Yes, and don't ask again for writes in this run", Key: "r", Decision: "approved", Scope: "run"},
		{Label: "No", Key: "n", Decision: "rejected"},
	}
	updated, _ := model.Update(msg)
	model = updated.(*uiModel)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(*uiModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(*uiModel)

	if len(*calls) != 1 || (*calls)[0].scope != "run" || (*calls)[0].decision != "approved" {
		t.Fatalf("down,enter should approve for this run: %v", *calls)
	}
}

// TestApprovalEscRejectsAndStrayKeysDoNotAnswer pins the Codex contract: Esc
// explicitly rejects the current request; other keys are swallowed instead of
// leaking into the composer.
func TestApprovalEscRejectsAndStrayKeysDoNotAnswer(t *testing.T) {
	model, calls := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	for _, key := range []tea.KeyMsg{keyRunes("x"), keyRunes("q"), {Type: tea.KeyTab}} {
		updated, _ = model.Update(key)
		model = updated.(*uiModel)
	}

	if len(*calls) != 0 {
		t.Fatalf("stray keys must not answer: %v", *calls)
	}
	if model.editor.Value() != "" {
		t.Fatalf("stray keys leaked into the composer: %q", model.editor.Value())
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(*uiModel)
	if len(*calls) != 1 || (*calls)[0].decision != "rejected" || (*calls)[0].id != "apr_1" {
		t.Fatalf("Esc must send an explicit rejection: %v", *calls)
	}
	if model.approvalPrompt != nil {
		t.Fatal("panel must close after the rejection is accepted")
	}
	last := model.messages[len(model.messages)-1]
	if last.NoticeKind != noticeError || !strings.Contains(last.Content, "Cancelled approval for write_file") {
		t.Fatalf("cancel record = %+v", last)
	}
}

func TestApprovalDenySendsRejectionImmediately(t *testing.T) {
	model, calls := newApprovalTestModel()
	model.clientMode = true
	model.thinking = true
	var steered bool
	model.steerFn = func(text string) error {
		steered = true
		return nil
	}
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	updated, _ = model.Update(keyRunes("n"))
	model = updated.(*uiModel)
	if model.approvalPrompt != nil {
		t.Fatal("panel should close after denial")
	}
	if len(*calls) != 1 || (*calls)[0].decision != "rejected" || (*calls)[0].scope != "" {
		t.Fatalf("rejection must be sent immediately: %v", *calls)
	}
	if steered {
		t.Fatal("denial must not invent follow-up guidance")
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

func TestApprovalCtrlCRejectsInsteadOfSilentlyDismissing(t *testing.T) {
	model, calls := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	model = updated.(*uiModel)

	if len(*calls) != 1 || (*calls)[0].decision != "rejected" || (*calls)[0].id != "apr_1" {
		t.Fatalf("Ctrl+C must send an explicit rejection: %v", *calls)
	}
	if model.statusMsg != "Approval cancelled." {
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

func TestApprovalCancelRearmsNextWithoutDroppingIt(t *testing.T) {
	model, calls := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgApprovalRequest{ID: "apr_2", Tool: "terminal"})
	model = updated.(*uiModel)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(*uiModel)

	if len(*calls) != 1 || (*calls)[0].id != "apr_1" || (*calls)[0].decision != "rejected" {
		t.Fatalf("cancel must explicitly reject only the current request: %v", *calls)
	}
	if model.approvalPrompt == nil || model.pendingApprovalID != "apr_2" {
		t.Fatalf("cancel dropped the next durable request: pending=%q", model.pendingApprovalID)
	}
}

func TestApprovalPayloadKeepsServerRuleText(t *testing.T) {
	options := approvalOptionsFromPayload(map[string]interface{}{
		"decisions": []interface{}{map[string]interface{}{
			"label":      "Yes, and don't ask again for commands that start with `git status` in this run",
			"decision":   "approved",
			"scope":      "run",
			"grant_key":  "rule:exec_prefix:git status",
			"rule_label": "commands that start with `git status`",
			"key":        "r",
		}},
	})
	if len(options) != 1 || options[0].RuleLabel != "commands that start with `git status`" {
		t.Fatalf("client lost daemon-authored rule text: %+v", options)
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

// TestAgentDoneClearsApprovalFlow: a still-live waiter cannot survive its run;
// stale panel state must not answer into the void.
func TestAgentDoneClearsApprovalFlow(t *testing.T) {
	model, _ := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgApprovalRequest{ID: "apr_2", Tool: "terminal"})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgAgentDone{Response: "run finished"})
	model = updated.(*uiModel)

	if model.approvalPrompt != nil || len(model.approvalQueue) != 0 {
		t.Fatal("approval flow must be cleared when the run ends")
	}
}

func TestAgentDoneKeepsParkedApprovalAnswerable(t *testing.T) {
	model, _ := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgApprovalParked{
		ID: "apr_1", Event: uiEventRef{Source: eventSourceDaemon, EventID: "event-park-1"},
	})
	model = updated.(*uiModel)
	if model.approvalPrompt == nil || !model.approvalPrompt.IsParked() {
		t.Fatal("approval.parked did not relabel the active prompt")
	}

	updated, _ = model.Update(MsgAgentDone{Response: "task parked"})
	model = updated.(*uiModel)
	if model.approvalPrompt == nil || model.pendingApprovalID != "apr_1" || !model.approvalPrompt.IsParked() {
		t.Fatalf("parked approval was lost at run end: pending=%q prompt=%+v", model.pendingApprovalID, model.approvalPrompt)
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
	if model.approvalPrompt == nil || model.pendingApprovalID != "apr_1" {
		t.Fatal("failed delivery must keep the same approval available for retry")
	}
	if model.thinking || strings.Contains(model.statusMsg, "resuming") {
		t.Fatalf("failed delivery must not look resumed: thinking=%v status=%q", model.thinking, model.statusMsg)
	}
	for _, msg := range model.messages {
		if msg.Role == "notice" && strings.Contains(msg.Content, "Approved write_file") {
			t.Fatalf("failed delivery created a false success record: %+v", msg)
		}
	}
}

func TestApprovalResponderFailureKeepsDenyPanelForRetry(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.approvalResponder = func(string, string, string, string) error {
		return errors.New("transport unavailable")
	}
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(keyRunes("n"))
	model = updated.(*uiModel)

	if model.approvalPrompt == nil || model.pendingApprovalID != "apr_1" {
		t.Fatal("failed rejection must retain the same panel for retry")
	}
}

func TestApprovalResolvedElsewhereClosesCurrentAndRearmsQueue(t *testing.T) {
	model, _ := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgApprovalRequest{ID: "apr_2", Tool: "terminal"})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgApprovalResolved{
		ID:     "apr_1",
		Status: "approved",
		Event:  uiEventRef{Source: eventSourceDaemon, EventID: "event-approve-1"},
	})
	model = updated.(*uiModel)

	if model.approvalPrompt == nil || model.pendingApprovalID != "apr_2" {
		t.Fatalf("next queued approval was not armed: pending=%q", model.pendingApprovalID)
	}
	if !model.thinking || model.runStatus != "working" {
		t.Fatalf("resolved run did not resume: thinking=%v status=%q", model.thinking, model.runStatus)
	}
	var resolution *ChatMessage
	for i := range model.messages {
		if strings.Contains(model.messages[i].Content, "Approved write_file elsewhere") {
			resolution = &model.messages[i]
		}
	}
	if resolution == nil || resolution.NoticeKind != noticeSuccess {
		t.Fatalf("external resolution record missing or misclassified: %+v", model.messages)
	}
}

func TestApprovalExpiredClosesCurrentWithoutResumingAndRearmsQueue(t *testing.T) {
	model, _ := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgApprovalRequest{ID: "apr_2", Tool: "terminal"})
	model = updated.(*uiModel)
	model.thinking = false
	model.runStatus = "waiting_user"

	updated, _ = model.Update(MsgApprovalResolved{
		ID:     "apr_1",
		Status: "expired",
		Event:  uiEventRef{Source: eventSourceDaemon, EventID: "event-expire-1"},
	})
	model = updated.(*uiModel)

	if model.approvalPrompt == nil || model.pendingApprovalID != "apr_2" {
		t.Fatalf("next queued approval was not armed: pending=%q", model.pendingApprovalID)
	}
	if model.thinking || model.runStatus != "waiting_user" {
		t.Fatalf("expiration falsely resumed run: thinking=%v status=%q", model.thinking, model.runStatus)
	}
	var resolution *ChatMessage
	for i := range model.messages {
		if strings.Contains(model.messages[i].Content, "Approval expired: write_file") {
			resolution = &model.messages[i]
		}
	}
	if resolution == nil || resolution.NoticeKind != noticeWarning {
		t.Fatalf("expiration record missing or misclassified: %+v", model.messages)
	}
}

func TestApprovalResolvedElsewhereRemovesQueuedRequest(t *testing.T) {
	model, _ := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(MsgApprovalRequest{ID: "apr_2", Tool: "terminal"})
	model = updated.(*uiModel)

	updated, _ = model.Update(MsgApprovalResolved{
		ID:     "apr_2",
		Status: "rejected",
		Event:  uiEventRef{Source: eventSourceDaemon, EventID: "event-reject-2"},
	})
	model = updated.(*uiModel)

	if model.pendingApprovalID != "apr_1" || len(model.approvalQueue) != 0 {
		t.Fatalf("queued external resolution disturbed current request: pending=%q queue=%v", model.pendingApprovalID, model.approvalQueue)
	}
	last := model.messages[len(model.messages)-1]
	if last.NoticeKind != noticeError || !strings.Contains(last.Content, "Denied terminal elsewhere") {
		t.Fatalf("external denial record = %+v", last)
	}
}

func TestLocalApprovalResolutionEchoIsSilent(t *testing.T) {
	model, _ := newApprovalTestModel()
	updated, _ := model.Update(sampleApproval("apr_1"))
	model = updated.(*uiModel)
	updated, _ = model.Update(keyRunes("y"))
	model = updated.(*uiModel)
	wantMessages := len(model.messages)

	updated, _ = model.Update(MsgApprovalResolved{
		ID:     "apr_1",
		Status: "approved",
		Event:  uiEventRef{Source: eventSourceDaemon, EventID: "event-local-echo"},
	})
	model = updated.(*uiModel)

	if len(model.messages) != wantMessages {
		t.Fatalf("local stream echo added a duplicate record: before=%d after=%d", wantMessages, len(model.messages))
	}
}

func TestUnknownApprovalResolutionDoesNotPolluteTranscript(t *testing.T) {
	model, _ := newApprovalTestModel()
	wantMessages := len(model.messages)

	updated, _ := model.Update(MsgApprovalResolved{
		ID:     "apr-from-old-replay",
		Status: "approved",
		Event:  uiEventRef{Source: eventSourceDaemon, EventID: "event-old-replay"},
	})
	model = updated.(*uiModel)

	if len(model.messages) != wantMessages || model.statusMsg != "" {
		t.Fatalf("unknown historical resolution polluted UI: messages=%d status=%q", len(model.messages), model.statusMsg)
	}
}

// This is deliberately a production-chain test: it enters through the
// llm.StreamEvent adapter, crosses Bubble Tea's queue, and exits through the
// reducer. A helper-only test would not catch a resolution switch that was
// implemented but never wired into the live event path.
func TestApprovalResolutionEventClosesPanelThroughProductionPath(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.armApprovalPrompt(sampleApproval("apr_1"))
	var output bytes.Buffer
	program := tea.NewProgram(model,
		tea.WithInput(bytes.NewReader(nil)),
		tea.WithOutput(&output),
		tea.WithoutSignalHandler(),
	)
	model.program = program
	done := make(chan tea.Model, 1)
	errCh := make(chan error, 1)
	go func() {
		final, err := program.Run()
		if err != nil {
			errCh <- err
			return
		}
		done <- final
	}()

	model.forwardGatewayEventFrom(llm.StreamEvent{
		EventType: "approval.approved",
		EventID:   "event-production-resolution",
		Payload:   map[string]interface{}{"approval_id": "apr_1", "scope": "run"},
	}, eventSourceDaemon)
	program.Quit()

	select {
	case err := <-errCh:
		t.Fatalf("bubbletea program: %v", err)
	case final := <-done:
		resolved := final.(*uiModel)
		if resolved.approvalPrompt != nil || resolved.pendingApprovalID != "" {
			t.Fatalf("production event path left stale approval: pending=%q", resolved.pendingApprovalID)
		}
	case <-time.After(3 * time.Second):
		program.Kill()
		t.Fatal("production event path did not drain")
	}
}

// Expiration has its own production-path guard because accepting the event in
// the reducer is insufficient if either the daemon-event adapter or client
// stream mapping forgets the third terminal status.
func TestApprovalExpirationEventClosesPanelThroughProductionPath(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.armApprovalPrompt(sampleApproval("apr_expired"))
	model.thinking = false
	model.runStatus = "waiting_user"
	var output bytes.Buffer
	program := tea.NewProgram(model,
		tea.WithInput(bytes.NewReader(nil)),
		tea.WithOutput(&output),
		tea.WithoutSignalHandler(),
	)
	model.program = program
	done := make(chan tea.Model, 1)
	errCh := make(chan error, 1)
	go func() {
		final, err := program.Run()
		if err != nil {
			errCh <- err
			return
		}
		done <- final
	}()

	model.forwardGatewayEventFrom(llm.StreamEvent{
		EventType: "approval.expired",
		EventID:   "event-production-expiration",
		Payload:   map[string]interface{}{"approval_id": "apr_expired", "reason": "waiter gone"},
	}, eventSourceDaemon)
	program.Quit()

	select {
	case err := <-errCh:
		t.Fatalf("bubbletea program: %v", err)
	case final := <-done:
		resolved := final.(*uiModel)
		if resolved.approvalPrompt != nil || resolved.pendingApprovalID != "" {
			t.Fatalf("production expiration path left stale approval: pending=%q", resolved.pendingApprovalID)
		}
		if resolved.thinking || resolved.runStatus != "waiting_user" {
			t.Fatalf("production expiration path falsely resumed: thinking=%v status=%q", resolved.thinking, resolved.runStatus)
		}
	case <-time.After(3 * time.Second):
		program.Kill()
		t.Fatal("production expiration path did not drain")
	}
}

func TestApprovalParkedSurvivesRunEndThroughProductionPath(t *testing.T) {
	model, _ := newApprovalTestModel()
	model.armApprovalPrompt(sampleApproval("apr_parked"))
	var output bytes.Buffer
	program := tea.NewProgram(model,
		tea.WithInput(bytes.NewReader(nil)),
		tea.WithOutput(&output),
		tea.WithoutSignalHandler(),
	)
	model.program = program
	done := make(chan tea.Model, 1)
	errCh := make(chan error, 1)
	go func() {
		final, err := program.Run()
		if err != nil {
			errCh <- err
			return
		}
		done <- final
	}()

	model.forwardGatewayEventFrom(llm.StreamEvent{
		EventType: "approval.parked",
		EventID:   "event-production-parked",
		Payload:   map[string]interface{}{"approval_id": "apr_parked", "reason": "resource budget elapsed"},
	}, eventSourceDaemon)
	program.Send(MsgAgentDone{Response: "task parked"})
	program.Quit()

	select {
	case err := <-errCh:
		t.Fatalf("bubbletea program: %v", err)
	case final := <-done:
		resolved := final.(*uiModel)
		if resolved.approvalPrompt == nil || resolved.pendingApprovalID != "apr_parked" || !resolved.approvalPrompt.IsParked() {
			t.Fatalf("production parked path lost approval: pending=%q prompt=%+v", resolved.pendingApprovalID, resolved.approvalPrompt)
		}
	case <-time.After(3 * time.Second):
		program.Kill()
		t.Fatal("production parked path did not drain")
	}
}
