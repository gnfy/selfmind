package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/textutil"
)

// ContinuityAction is gateway-owned control metadata. Natural-language work
// selection is expressed by Main through the broker tools; only explicit
// controls and atomically claimed choices set these values at ingress.
type ContinuityAction string

const (
	ContinuityNew     ContinuityAction = "new"
	ContinuitySteer   ContinuityAction = "steer"
	ContinuityResume  ContinuityAction = "resume"
	ContinuityObserve ContinuityAction = "observe"
)

// ContinuityCandidate is a bounded, read-only status card. Every string is
// quoted data, never an instruction; raw transcripts, tool output, and
// credentials are not part of this contract.
type ContinuityCandidate struct {
	RunID          string   `json:"run_id"`
	TaskID         string   `json:"task_id"`
	Title          string   `json:"title"`
	TaskStatus     string   `json:"task_status"`
	RunStatus      string   `json:"run_status"`
	Channel        string   `json:"channel,omitempty"`
	Workspace      string   `json:"workspace,omitempty"`
	InputSummary   string   `json:"input_summary,omitempty"`
	HandoffSummary string   `json:"handoff_summary,omitempty"`
	NextSteps      []string `json:"next_steps,omitempty"`
	Risks          []string `json:"risks,omitempty"`
	CurrentStep    string   `json:"current_step,omitempty"`
	LastActivity   string   `json:"last_activity,omitempty"`
	Age            string   `json:"age,omitempty"`
	Active         bool     `json:"active"`
	Resumable      bool     `json:"resumable"`
	Evidence       []string `json:"evidence,omitempty"`

	score     int
	startedAt time.Time
}

func isStandaloneContinueControl(input string) bool {
	clean := strings.ToLower(strings.Trim(strings.TrimSpace(input), " \t\r\n.!?,;:。！？；：，"))
	switch clean {
	case "continue", "resume", "go on", "keep going", "继续", "接着", "继续吧", "接着来":
		return true
	default:
		return false
	}
}

func continuityRunResumable(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "interrupted", "waiting_user", "verification_partial", "blocked":
		return true
	default:
		return false
	}
}

func (d *Server) continuityCandidateForRun(ctx context.Context, identity *control.IdentityContext, run control.Run, active *activeRun, score int, evidence []string) (ContinuityCandidate, bool) {
	if d == nil || d.Control == nil || identity == nil || run.PersonID != identity.PersonID {
		return ContinuityCandidate{}, false
	}
	task, err := d.Control.GetTask(ctx, identity.TenantID, run.TaskID)
	if err != nil || task == nil || task.PersonID != identity.PersonID {
		return ContinuityCandidate{}, false
	}
	card := ContinuityCandidate{
		RunID: run.ID, TaskID: run.TaskID, Title: textutil.Truncate(toOneLine(task.Title), 96),
		TaskStatus: task.Status, RunStatus: run.Status, Channel: run.Channel,
		InputSummary: textutil.Truncate(toOneLine(run.InputSummary), 180),
		Age:          relativeAge(run.StartedAt), Active: active != nil && active.RunID == run.ID,
		Resumable: continuityRunResumable(run.Status), score: score, startedAt: run.StartedAt,
		Evidence: append([]string(nil), evidence...),
	}
	sort.Strings(card.Evidence)
	if workspaceID := firstNonEmpty(run.WorkspaceID, task.WorkspaceID); workspaceID != "" {
		if workspace, _ := d.Control.GetWorkspace(ctx, identity.TenantID, workspaceID); workspace != nil && workspace.OwnerPersonID == identity.PersonID {
			card.Workspace = strings.TrimSpace(workspace.Name)
			if card.Workspace == "" {
				card.Workspace = filepath.Base(workspace.LocalPath)
			}
		}
	}
	if handoff, _ := d.Control.RunHandoff(ctx, identity.TenantID, identity.PersonID, run.ID); handoff != nil {
		card.HandoffSummary = textutil.Truncate(toOneLine(handoff.Summary), 220)
		card.NextSteps = boundedOneLineStrings(handoff.NextSteps, 3, 120)
		card.Risks = boundedOneLineStrings(handoff.Risks, 2, 120)
	}
	if plan := d.latestPlanForRun(ctx, identity.TenantID, identity.PersonID, run.TaskID, run.ID); len(plan) > 0 {
		for _, step := range plan {
			if step.Status == "in_progress" {
				card.CurrentStep = textutil.Truncate(toOneLine(step.Step), 140)
				break
			}
		}
	}
	card.LastActivity = d.latestActivityForRun(ctx, identity, run)
	return card, true
}

func (d *Server) exactContinuityCandidate(ctx context.Context, identity *control.IdentityContext, runID string) (ContinuityCandidate, bool) {
	if d == nil || d.Control == nil || identity == nil || strings.TrimSpace(runID) == "" {
		return ContinuityCandidate{}, false
	}
	run, err := d.Control.GetRun(ctx, identity.TenantID, strings.TrimSpace(runID))
	if err != nil || run == nil || run.PersonID != identity.PersonID {
		return ContinuityCandidate{}, false
	}
	return d.continuityCandidateForRun(ctx, identity, *run, d.coordinator().currentActive(identity.PersonID), 0, []string{"explicit_choice"})
}

func boundedOneLineStrings(values []string, limit, maxChars int) []string {
	out := make([]string, 0, min(len(values), limit))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, textutil.Truncate(toOneLine(value), maxChars))
		if len(out) == limit {
			break
		}
	}
	return out
}

func (d *Server) latestActivityForRun(ctx context.Context, identity *control.IdentityContext, run control.Run) string {
	if d == nil || d.Control == nil || identity == nil {
		return ""
	}
	events, err := d.Control.ListRunEvents(ctx, identity.TenantID, identity.PersonID, run.TaskID, run.ID, 30)
	if err != nil {
		return ""
	}
	for _, event := range events {
		var payload struct {
			Tool    string `json:"tool"`
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		switch event.Type {
		case "tool.started":
			if payload.Tool != "" {
				return "running " + payload.Tool
			}
		case "tool.completed":
			if payload.Tool != "" && payload.Error != "" {
				return textutil.Truncate(payload.Tool+" failed: "+toOneLine(payload.Error), 140)
			}
			if payload.Tool != "" {
				return payload.Tool + " finished"
			}
		case "agent.thinking", "agent.step":
			if strings.TrimSpace(payload.Message) != "" {
				return textutil.Truncate(toOneLine(payload.Message), 140)
			}
		}
	}
	return ""
}

func (d *Server) continuityProgressResponse(ctx context.Context, identity *control.IdentityContext, candidate ContinuityCandidate) api.MessageResponse {
	content := continuityProgressContent(candidate)
	task, _ := d.Control.GetTask(ctx, identity.TenantID, candidate.TaskID)
	run, _ := d.Control.GetRun(ctx, identity.TenantID, candidate.RunID)
	return api.MessageResponse{Identity: identity, Task: task, Run: run, Content: content,
		Turn: messageTurn("completed", candidate.TaskStatus, "idle", candidate.TaskID, candidate.RunID, content)}
}

func continuityProgressContent(candidate ContinuityCandidate) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s · %s\n", candidate.Title, candidate.RunStatus)
	if candidate.CurrentStep != "" {
		fmt.Fprintf(&sb, "Current step: %s\n", candidate.CurrentStep)
	}
	if candidate.LastActivity != "" {
		fmt.Fprintf(&sb, "Latest activity: %s\n", candidate.LastActivity)
	}
	if candidate.HandoffSummary != "" {
		fmt.Fprintf(&sb, "Latest result: %s\n", candidate.HandoffSummary)
	}
	if len(candidate.NextSteps) > 0 {
		sb.WriteString("Next: " + strings.Join(candidate.NextSteps, "; ") + "\n")
	}
	fmt.Fprintf(&sb, "Last recorded: %s · run %s", candidate.Age, shortRunID(candidate.RunID))
	return strings.TrimSpace(sb.String())
}
