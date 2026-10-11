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
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pager != nil || m.activePlanJSON != originalPlan {
		t.Fatal("closing view did not restore own UI")
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
