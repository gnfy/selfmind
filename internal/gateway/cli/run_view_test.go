package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
)

func TestRunViewIgnoresStaleLoadsAndClosedRefresh(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.runViewer = func(_ context.Context, id string) (api.RunView, error) {
		return api.RunView{RunID: id, Status: "waiting_user", Content: id}, nil
	}
	first, second := m.loadRunView("first"), m.loadRunView("second")
	m.Update(second())
	m.Update(first())
	if m.runViewID != "second" {
		t.Fatal("late response replaced the selected Run")
	}
	refresh := m.loadRunView("second")
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.Update(refresh())
	if m.pager != nil || m.runViewID != "" {
		t.Fatal("late refresh reopened a closed view")
	}
}

func TestRunViewChooserKeepsSelectionVisible(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.width, m.height = 100, 24
	options := make([]api.RunViewOption, 25)
	for i := range options {
		options[i] = api.RunViewOption{RunID: fmt.Sprintf("run_%02d", i), Title: fmt.Sprintf("work_%02d", i), Status: "waiting_user"}
	}
	m.showRunView(MsgRunViewLoaded{View: api.RunView{Options: options}})
	for range 24 {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if !strings.Contains(stripANSI(m.pager.View()), "work_24") {
		t.Fatal("selected work scrolled outside the visible chooser")
	}
}

func TestOwnHumanWaitTakesPrecedenceOverRunView(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.channel = "origin"
	m.runViewer = func(context.Context, string) (api.RunView, error) {
		return api.RunView{RunID: "foreign", Status: "waiting_user"}, nil
	}
	m.Update(m.loadRunView("foreign")())
	refresh := m.loadRunView("foreign")
	m.Update(MsgClarifyRequest{ID: "own-question", Question: "Which target?", Channel: "origin"})
	m.Update(refresh())
	if m.pager != nil || !m.clarifyMode || m.clarifyReq.ID != "own-question" || m.runViewID != "" {
		t.Fatal("own question hidden by work view or late refresh")
	}
}

func TestAttachChooserViewsExactParkedRunWithoutResuming(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.width, m.height = 100, 30
	m.applyPlanSnapshot(sessionTestPlan, uiEventRef{RunID: "own"})
	originalPlan, messages := m.activePlanJSON, len(m.messages)
	var viewed []string
	m.runViewer = func(_ context.Context, id string) (api.RunView, error) {
		viewed = append(viewed, id)
		if id == "" {
			return api.RunView{Options: []api.RunViewOption{{RunID: "run_a", Title: "old release", Status: "waiting_user"}, {RunID: "run_b", Title: "second work", Status: "running"}}}, nil
		}
		return api.RunView{RunID: id, Title: "old release", Status: "waiting_user", Content: "Plan\n- ✓ inspected\nApproval pending", CanContinue: true}, nil
	}
	m.resumePickerArmed = false
	m.Update(m.handleAttach(nil)())
	if m.pager == nil || !strings.Contains(stripANSI(m.pager.View()), "old release") {
		t.Fatal("/attach did not expose parked work")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not select a view")
	}
	m.Update(cmd())
	if len(viewed) != 2 || viewed[1] != "run_a" || m.runViewID != "run_a" {
		t.Fatalf("selection drifted: %v", viewed)
	}
	if m.pager == nil || !strings.Contains(stripANSI(m.pager.View()), "Approval pending") {
		t.Fatal("parked state and plan not shown")
	}
	if m.watchingRun || m.approvalPrompt != nil || m.resumePickerArmed || m.activePlanJSON != originalPlan || len(m.messages) != messages {
		t.Fatal("viewing mutated execution, input authority, plan or transcript")
	}
	if handled, cmd := m.runViewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}}); handled || cmd != nil {
		t.Fatal("compatibility /attach granted continuation")
	}
	// Esc leaves the detail page for the chooser it came from; a second Esc
	// closes the viewer and restores this terminal's own UI.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pager == nil || m.runViewID != "" || !strings.Contains(stripANSI(m.pager.View()), "old release") {
		t.Fatal("Esc on a detail page did not return to the chooser")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pager != nil || m.activePlanJSON != originalPlan {
		t.Fatal("closing view did not restore own UI")
	}
}

// workViewModel opens bare /resume over three Runs, the third with a very long
// page, and returns the model positioned on the chooser.
func workViewModel(t *testing.T, canContinue func(id string) bool) *uiModel {
	t.Helper()
	m, _ := newApprovalTestModel()
	m.width, m.height = 100, 24
	m.runViewer = func(_ context.Context, id string) (api.RunView, error) {
		if id == "" {
			return api.RunView{Content: "Waiting and recent work", Options: []api.RunViewOption{
				{RunID: "run_a", Title: "first", Status: "waiting_user"},
				{RunID: "run_b", Title: "second", Status: "running"},
				{RunID: "run_c", Title: "third", Status: "interrupted"},
			}}, nil
		}
		return api.RunView{RunID: id, Title: id, Status: "interrupted", Content: strings.Repeat("a long plan line\n", 200), CanContinue: canContinue(id)}, nil
	}
	m.Update(m.handleResumeSelect(nil)())
	if m.pager == nil || len(m.runViewOptions) != 3 {
		t.Fatal("bare /resume did not open the chooser")
	}
	return m
}

// footerOf is the pager's fixed last row, which no scrolling moves.
func footerOf(m *uiModel) string {
	lines := strings.Split(strings.TrimRight(stripANSI(m.pager.View()), "\n"), "\n")
	return lines[len(lines)-1]
}

func openDetail(t *testing.T, m *uiModel, downs int) {
	t.Helper()
	for range downs {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not open a detail page")
	}
	m.Update(cmd())
}

// A page that is long enough to scroll used to list its keys at its very end,
// where they were never seen: the footer said only "q/Esc close", so the page
// read as having nothing to do but leave. The footer now names the choice and
// stays put wherever the page is scrolled.
func TestRunViewDetailFooterOffersSelectOrBackAndNeverScrollsAway(t *testing.T) {
	m := workViewModel(t, func(string) bool { return true })
	if footer := footerOf(m); !strings.Contains(footer, "Enter view") || !strings.Contains(footer, "q/Esc close") {
		t.Fatalf("chooser footer = %q", footer)
	}
	openDetail(t, m, 1)
	for _, offset := range []int{0, 1, 2} {
		for range offset * 40 {
			m.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
		footer := footerOf(m)
		for _, want := range []string{"c select for your next message", "Esc/b back", "r refresh", "q close"} {
			if !strings.Contains(footer, want) {
				t.Fatalf("footer at scroll step %d lacks %q: %q", offset, want, footer)
			}
		}
	}
	if !strings.Contains(footerOf(m), "line ") || strings.Contains(footerOf(m), "line 1 ") {
		t.Fatalf("the footer must still say where the page is scrolled: %q", footerOf(m))
	}
}

// Esc or b on a detail page is "not this one": it returns to the chooser with
// the row that was opened still selected, and nothing was chosen. q closes the
// whole viewer.
func TestRunViewEscAndBReturnToTheChooserOnTheOpenedRow(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyRunes, Runes: []rune{'b'}}} {
		t.Run(key.String(), func(t *testing.T) {
			var selected string
			m := workViewModel(t, func(string) bool { return true })
			m.messageProcessor = func(_ context.Context, req api.MessageRequest) (api.MessageResponse, int) {
				selected = req.Content
				return api.MessageResponse{Content: "Selected run", Turn: &api.TurnStatus{Status: "completed"}}, 200
			}
			openDetail(t, m, 2)
			if m.runViewID != "run_c" {
				t.Fatalf("opened %q", m.runViewID)
			}
			m.Update(key)
			if m.pager == nil || m.runViewID != "" || len(m.runViewOptions) != 3 || m.runViewIndex != 2 {
				t.Fatalf("not back on the third chooser row: pager=%v id=%q options=%d index=%d", m.pager != nil, m.runViewID, len(m.runViewOptions), m.runViewIndex)
			}
			if !strings.Contains(stripANSI(m.pager.View()), "› - ") && !strings.Contains(stripANSI(m.pager.View()), "› interrupted · third") {
				t.Fatalf("the opened row is not marked selected:\n%s", stripANSI(m.pager.View()))
			}
			if selected != "" {
				t.Fatalf("going back selected a Run: %q", selected)
			}
			// Another Run can be opened from the same chooser, and q leaves entirely.
			m.Update(tea.KeyMsg{Type: tea.KeyUp})
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m.Update(cmd())
			if m.runViewID != "run_b" {
				t.Fatalf("second pick opened %q", m.runViewID)
			}
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
			if m.pager != nil || m.runViewID != "" || m.runViewOptions != nil {
				t.Fatal("q did not close the whole viewer")
			}
		})
	}
}

// A refresh that was on its way when the person went back must not pull the
// detail page over the chooser again.
func TestRunViewLateRefreshDoesNotReopenTheDetailAfterGoingBack(t *testing.T) {
	m := workViewModel(t, func(string) bool { return true })
	openDetail(t, m, 0)
	refresh := m.loadRunView(m.runViewID)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.Update(refresh())
	if m.runViewID != "" || len(m.runViewOptions) != 3 {
		t.Fatalf("late refresh reopened the detail page: id=%q options=%d", m.runViewID, len(m.runViewOptions))
	}
}

// A page opened for an exact Run has no chooser behind it, so the footer does
// not offer to go back and Esc closes. One the person cannot continue (it waits
// on their own decision) says it is view only and offers no selection.
func TestRunViewFooterMatchesWhatThePageCanDo(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.width, m.height = 100, 24
	m.runViewer = func(_ context.Context, id string) (api.RunView, error) {
		return api.RunView{RunID: id, Status: "waiting_user", Content: "Approval pending", CanContinue: false}, nil
	}
	m.runViewResumeActions = true
	m.Update(m.loadRunView("exact-run")())
	footer := footerOf(m)
	if !strings.Contains(footer, "view only") || strings.Contains(footer, "select") || strings.Contains(footer, "back") || !strings.Contains(footer, "q/Esc close") {
		t.Fatalf("view-only direct page footer = %q", footer)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pager != nil {
		t.Fatal("Esc on a page with no chooser behind it must close")
	}

	chooser := workViewModel(t, func(string) bool { return false })
	openDetail(t, chooser, 0)
	if footer := footerOf(chooser); !strings.Contains(footer, "view only") || !strings.Contains(footer, "Esc/b back") || strings.Contains(footer, "select") {
		t.Fatalf("view-only page opened from the chooser, footer = %q", footer)
	}
}

func TestViewingAnotherActiveRunDoesNotMergeOwnForeground(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.thinking, m.daemonRunActive = true, true
	m.daemonRunID = "own"
	m.applyPlanSnapshot(sessionTestPlan, uiEventRef{RunID: "own"})
	plan := m.activePlanJSON
	m.runWatcher = func(context.Context, string, httpapi.StreamObserver) (api.RunObservation, error) {
		t.Fatal("foreign watcher started over own work")
		return api.RunObservation{}, nil
	}
	cmd := m.showRunView(MsgRunViewLoaded{View: api.RunView{RunID: "foreign", Status: "running", Content: "Other work"}})
	if cmd != nil || m.watchingRun || m.daemonRunID != "own" || m.activePlanJSON != plan {
		t.Fatal("observation hijacked foreground")
	}
}

func TestResumeViewerRequiresExplicitSelection(t *testing.T) {
	for _, canContinue := range []bool{false, true} {
		t.Run(fmt.Sprint(canContinue), func(t *testing.T) {
			m, _ := newApprovalTestModel()
			m.width, m.height = 100, 30
			var selected string
			m.messageProcessor = func(_ context.Context, req api.MessageRequest) (api.MessageResponse, int) {
				selected = req.Content
				return api.MessageResponse{Content: "Selected run", Turn: &api.TurnStatus{Status: "completed"}}, 200
			}
			m.runViewer = func(_ context.Context, id string) (api.RunView, error) {
				if id == "" {
					return api.RunView{Options: []api.RunViewOption{{RunID: "exact-run", Title: "unfinished work", Status: "interrupted"}}}, nil
				}
				return api.RunView{RunID: id, Status: "interrupted", Content: "Not complete", CanContinue: canContinue}, nil
			}
			m.Update(m.handleResumeSelect(nil)())
			if selected != "" || m.pager == nil {
				t.Fatal("opening /resume selected execution authority")
			}
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m.Update(cmd())
			if selected != "" || m.runViewID != "exact-run" {
				t.Fatal("viewing selected execution authority")
			}
			handled, cmd := m.runViewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
			if !canContinue {
				if handled || cmd != nil || selected != "" {
					t.Fatal("pending control exposed continuation action")
				}
				return
			}
			if !handled || cmd == nil {
				t.Fatal("explicit continuation action unavailable")
			}
			m.Update(cmd())
			if selected != "/resume exact-run" || m.pager != nil {
				t.Fatalf("wrong selected target: %q", selected)
			}
		})
	}
}

// A work page opened for an exact Run never offers to go back to a chooser it
// did not come from, even when the previous viewer was left without being closed.
func TestRunViewOpenedDirectlyForgetsAnEarlierChooser(t *testing.T) {
	m := workViewModel(t, func(string) bool { return true })
	openDetail(t, m, 0)
	if !strings.Contains(footerOf(m), "Esc/b back") {
		t.Fatalf("precondition: a page opened from the chooser offers to go back: %q", footerOf(m))
	}
	m.pager = nil // the viewer was dropped without a key
	m.Update(m.handleAttach([]string{"run_b"})())
	if m.runViewID != "run_b" || strings.Contains(footerOf(m), "back") {
		t.Fatalf("a directly opened page must not offer a chooser that is gone: id=%q footer=%q", m.runViewID, footerOf(m))
	}
}

// Closing the viewer forgets the chooser, so a page opened for an exact Run
// afterwards cannot offer to return to it.
func TestRunViewClosingForgetsTheChooser(t *testing.T) {
	m := workViewModel(t, func(string) bool { return true })
	openDetail(t, m, 0)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if m.pager != nil || m.runViewChooser != nil || m.runViewFromChooser {
		t.Fatal("q left the chooser behind")
	}
	m.Update(m.loadRunView("run_b")())
	if m.pager == nil || m.runViewID != "run_b" || strings.Contains(footerOf(m), "back") {
		t.Fatalf("a page opened after closing must not offer a way back: footer=%q", footerOf(m))
	}
}

// The notice shown while another session's run is watched says what the page and
// the footer now make plain: this is a view, and the next message goes to that
// Run only after it is selected.
func TestWatchingNoticeSaysHowToSendTheRunAMessage(t *testing.T) {
	m, _ := newApprovalTestModel()
	m.runWatcher = func(context.Context, string, httpapi.StreamObserver) (api.RunObservation, error) {
		return api.RunObservation{}, nil
	}
	m.watchedRunID, m.watchedTaskTitle = "run_x", "release"
	m.attachToActiveRun()
	const want = "Watching release — view only. /resume, then c, sends your next message to it; other input starts separate work."
	if m.statusMsg != want {
		t.Fatalf("notice = %q", m.statusMsg)
	}
}
