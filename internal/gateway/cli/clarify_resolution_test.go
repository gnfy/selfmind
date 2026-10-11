package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
)

func TestClarifySubmissionPreservesExactRequestAfterComposerReset(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.channel = "origin"
	model.width, model.height = 100, 30
	requests := make(chan api.MessageRequest, 1)
	model.messageProcessor = func(_ context.Context, req api.MessageRequest) (api.MessageResponse, int) {
		requests <- req
		return api.MessageResponse{Content: "Answer saved."}, 200
	}
	model.Update(MsgClarifyRequest{ID: "clarify_exact", Question: "Which format?", Channel: "origin"})
	model.editor.SetValue("json")
	var output bytes.Buffer
	program := tea.NewProgram(model, tea.WithInput(bytes.NewReader(nil)), tea.WithOutput(&output), tea.WithoutSignalHandler())
	model.program = program
	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()
	defer func() {
		program.Quit()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	program.Send(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case req := <-requests:
		if req.ClarifyID != "clarify_exact" || req.Content != "json" || req.Channel != "origin" {
			t.Fatalf("answer lost its exact request: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("answer never reached the gateway")
	}
}

func TestClarifyExpirationClosesOnlyExactPanelThroughProductionPath(t *testing.T) {
	for _, expired := range []string{"clarify_exact", "clarify_other"} {
		t.Run(expired, func(t *testing.T) {
			model := NewController("", "", nil, "").model
			model.channel = "origin"
			model.daemonRunActive = false
			model.Update(MsgClarifyRequest{ID: "clarify_exact", Question: "Which format?", Choices: []string{"json", "yaml"}, Channel: "origin"})
			var output bytes.Buffer
			program := tea.NewProgram(model, tea.WithInput(bytes.NewReader(nil)), tea.WithOutput(&output), tea.WithoutSignalHandler())
			model.program = program
			done := make(chan tea.Model, 1)
			errs := make(chan error, 1)
			go func() {
				final, err := program.Run()
				if err != nil {
					errs <- err
					return
				}
				done <- final
			}()
			model.forwardGatewayEventFrom(llm.StreamEvent{EventType: "clarify.expired", EventID: "event-expire-" + expired, Channel: "origin", Payload: map[string]interface{}{"clarify_id": expired}}, eventSourceDaemon)
			program.Quit()
			select {
			case err := <-errs:
				t.Fatal(err)
			case final := <-done:
				got := final.(*uiModel)
				if expired == "clarify_exact" {
					if got.clarifyMode || got.clarifyGateway || got.clarifyReq.ID != "" || len(got.clarifyChoices) != 0 {
						t.Fatalf("cancelled question stayed armed: mode=%t gateway=%t id=%q", got.clarifyMode, got.clarifyGateway, got.clarifyReq.ID)
					}
				} else if !got.clarifyMode || got.clarifyReq.ID != "clarify_exact" {
					t.Fatal("another question's expiry closed this panel")
				}
				if got.thinking || got.daemonRunActive {
					t.Fatal("question resolution falsely resumed execution")
				}
			case <-time.After(3 * time.Second):
				program.Kill()
				t.Fatal("resolution pipeline did not drain")
			}
		})
	}
}

func TestForeignClarifyExpiryKeepsOwnPanel(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.channel = "origin"
	model.Update(MsgClarifyRequest{ID: "clarify_other", Question: "Which target?", Channel: "other"})
	model.Update(MsgClarifyResolved{ID: "different_question"})
	if model.otherClarifyID != "clarify_other" {
		t.Fatal("unrelated resolution cleared another wait")
	}
	model.Update(MsgClarifyRequest{ID: "clarify_own", Question: "Which format?", Channel: "origin"})
	model.Update(MsgClarifyResolved{ID: "clarify_other"})
	if model.otherClarifyID != "" || !model.clarifyMode || model.clarifyReq.ID != "clarify_own" {
		t.Fatal("foreign resolution left a stale notice or affected own panel")
	}
}
