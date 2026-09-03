package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/textutil"
)

// parentRunResolution is the read-only, pre-run answer to "which prior run
// does this turn continue?". It is resolved BEFORE the child run exists and
// threaded to every context channel (selector, resume block, loop checkpoint)
// so all three agree on one parent — or on the absence of one. Zero candidates
// means a root turn; more than one means ambiguity that stays visible instead
// of being guessed away.
type parentRunResolution struct {
	candidates []control.Run
}

func (r parentRunResolution) exact() *control.Run {
	if len(r.candidates) != 1 {
		return nil
	}
	run := r.candidates[0]
	return &run
}

func (r parentRunResolution) ambiguous() bool {
	return len(r.candidates) > 1
}

// resolveParentRun lists the task's unclaimed resumable runs. Read-only; the
// claim itself happens atomically inside child-run creation
// (StartRunOptions.ParentRunID).
func (c *RunCoordinator) resolveParentRun(ctx context.Context, identity *control.IdentityContext, task *control.Task) (parentRunResolution, error) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil {
		return parentRunResolution{}, fmt.Errorf("parent run resolver is unavailable")
	}
	runs, err := c.srv.Control.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, task.ID, 5)
	if err != nil {
		return parentRunResolution{}, fmt.Errorf("list unresolved parent runs: %w", err)
	}
	return parentRunResolution{candidates: runs}, nil
}

// resolveExplicitParent narrows resolution to one platform-named run: reply
// and approval bindings are exact, so the task's other unresolved runs are
// irrelevant. A named run that is terminal or already claimed fails closed;
// it never degrades to a root turn on the bounded task card.
//
// The one exception is a Run parked on a daemon watcher (waiting_external):
// it is outside the resumable set, yet its watcher finalization must continue
// it as an exact child. Once every watcher it registered has concluded and no
// continuation claimed it, it is the exact parent (validateParentClaimTx
// admits the same case). Once its finalization has claimed it, a binding that
// still names it points at finished system work rather than a person's open
// question, so it continues the Thread's current resolution instead of
// failing closed.
func (c *RunCoordinator) resolveExplicitParent(ctx context.Context, identity *control.IdentityContext, task *control.Task, runID string) (parentRunResolution, error) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || strings.TrimSpace(runID) == "" {
		return parentRunResolution{}, fmt.Errorf("explicit parent run is unavailable")
	}
	runs, err := c.srv.Control.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, task.ID, 20)
	if err != nil {
		return parentRunResolution{}, fmt.Errorf("list explicit parent run: %w", err)
	}
	for _, run := range runs {
		if run.ID == runID {
			return parentRunResolution{candidates: []control.Run{run}}, nil
		}
	}
	run, err := c.srv.Control.GetRun(ctx, identity.TenantID, runID)
	if err != nil {
		return parentRunResolution{}, fmt.Errorf("load explicit parent run: %w", err)
	}
	if run != nil && run.PersonID == identity.PersonID && run.TaskID == task.ID && strings.EqualFold(run.Status, "waiting_external") {
		claimed, err := c.watcherRunClaimed(ctx, identity, task, run.ID)
		if err != nil {
			return parentRunResolution{}, err
		}
		if claimed {
			return c.resolveParentRun(ctx, identity, task)
		}
		live, err := c.watcherRunHasLiveWatch(ctx, identity, run.ID)
		if err != nil {
			return parentRunResolution{}, err
		}
		if !live {
			return parentRunResolution{candidates: []control.Run{*run}}, nil
		}
	}
	return parentRunResolution{}, fmt.Errorf("continuation parent %s is no longer resumable", shortRunID(runID))
}

// watcherRunClaimed reports whether a child Run of this task already continues
// runID on the forward edge validateParentClaimTx enforces. The claim itself
// stays atomic inside run creation; this is the read-only pre-check.
func (c *RunCoordinator) watcherRunClaimed(ctx context.Context, identity *control.IdentityContext, task *control.Task, runID string) (bool, error) {
	children, err := c.srv.Control.ListTaskRuns(ctx, identity.TenantID, task.ID, 50)
	if err != nil {
		return false, fmt.Errorf("list watcher continuations: %w", err)
	}
	for _, child := range children {
		if child.ParentRunID == runID {
			return true, nil
		}
	}
	return false, nil
}

// watcherRunHasLiveWatch reports whether any pending or running watcher still
// belongs to runID; while one does, nothing may claim the Run away from it.
func (c *RunCoordinator) watcherRunHasLiveWatch(ctx context.Context, identity *control.IdentityContext, runID string) (bool, error) {
	active, err := c.srv.Control.ListExternalWatchesForPerson(ctx, identity.TenantID, identity.PersonID, control.ExternalWatchListActive, 50, 0)
	if err != nil {
		return false, fmt.Errorf("list live watchers: %w", err)
	}
	for _, watch := range active {
		if watch.RunID == runID {
			return true, nil
		}
	}
	return false, nil
}

// parentCandidatesResponse answers an ambiguous continuation deterministically:
// the model is never started, and the person sees the concrete waiting runs so
// the next message can pick one. Platform reply metadata may bind one directly;
// `/resume <run_id>` is the universal precise fallback.
func (c *RunCoordinator) parentCandidatesResponse(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, task *control.Task, candidates []control.Run) api.MessageResponse {
	var sb strings.Builder
	fmt.Fprintf(&sb, "I found several possible continuations under %s. Which one did you mean?\n",
		textutil.Truncate(task.Title, 60))
	options := make([]control.TurnChoiceOption, 0, 4)
	for i, run := range candidates {
		if i >= 3 {
			break
		}
		summary := textutil.Truncate(oneLine(run.InputSummary), 80)
		if summary == "" {
			summary = "(no input summary)"
		}
		key := fmt.Sprintf("%d", i+1)
		label := fmt.Sprintf("%s · %s · %s", shortRunID(run.ID), run.Status, summary)
		options = append(options, control.TurnChoiceOption{Key: key, Label: label, Action: "resume", TaskID: task.ID, RunID: run.ID})
		fmt.Fprintf(&sb, "%s. %s\n", key, label)
	}
	newKey := fmt.Sprintf("%d", len(options)+1)
	options = append(options, control.TurnChoiceOption{Key: newKey, Label: "This is new work", Action: "new"})
	fmt.Fprintf(&sb, "%s. This is new work\n", newKey)
	choice, choiceErr := c.srv.createTurnChoice(ctx, identity, req, options)
	if choiceErr == nil {
		fmt.Fprintf(&sb, "Reply with a number, or use /choose %s <number> from another endpoint.", choice.ID)
	} else {
		sb.WriteString("Use /resume <run_id> to choose precisely.")
	}
	content := sb.String()
	if c != nil && c.srv != nil && c.srv.Control != nil {
		ids := make([]string, 0, len(candidates))
		for _, run := range candidates {
			ids = append(ids, run.ID)
		}
		_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
			TaskID:     task.ID,
			Type:       "continuation.candidates",
			Visibility: "task",
			Channel:    task.LastChannel,
			Payload: mustJSON(map[string]interface{}{
				"count":     len(candidates),
				"run_ids":   ids,
				"choice_id": choiceID(choice),
			}),
		})
	}
	return api.MessageResponse{
		Identity: identity,
		Task:     task,
		Content:  content,
		Turn:     messageTurn("waiting_user", task.Status, "idle", task.ID, "", content),
		Choice:   choice,
	}
}

// shortRunID mirrors shortTaskID's compact `run_xxxxxxxx` reference form.
// crossTaskCandidatesResponse renders the §5.3 step-7 disambiguation for a
// person-typed continuation whose cue matched pending runs under SEVERAL
// tasks. Deterministic and model-free, like the task-scoped variant.
func (c *RunCoordinator) crossTaskCandidatesResponse(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, candidates []control.Run) api.MessageResponse {
	var sb strings.Builder
	sb.WriteString("I found several possible continuations. Which one did you mean?\n")
	options := make([]control.TurnChoiceOption, 0, 4)
	for i, run := range candidates {
		if i >= 3 {
			break
		}
		title := ""
		if c != nil && c.srv != nil && c.srv.Control != nil {
			if task, err := c.srv.Control.GetTask(ctx, identity.TenantID, run.TaskID); err == nil && task != nil {
				title = textutil.Truncate(toOneLine(task.Title), 48)
			}
		}
		summary := textutil.Truncate(oneLine(run.InputSummary), 72)
		if summary == "" {
			summary = "(no input summary)"
		}
		key := fmt.Sprintf("%d", i+1)
		label := fmt.Sprintf("%s · %s · %s · %s", title, run.Status, relativeAge(run.StartedAt), summary)
		options = append(options, control.TurnChoiceOption{Key: key, Label: label, Action: "resume", TaskID: run.TaskID, RunID: run.ID})
		fmt.Fprintf(&sb, "%s. %s\n", key, label)
	}
	newKey := fmt.Sprintf("%d", len(options)+1)
	options = append(options, control.TurnChoiceOption{Key: newKey, Label: "This is new work", Action: "new"})
	fmt.Fprintf(&sb, "%s. This is new work\n", newKey)
	choice, choiceErr := c.srv.createTurnChoice(ctx, identity, req, options)
	if choiceErr == nil {
		fmt.Fprintf(&sb, "Reply with a number, or use /choose %s <number> from another endpoint.", choice.ID)
	} else {
		sb.WriteString("Use /resume <run_id> to choose precisely.")
	}
	content := sb.String()
	if c != nil && c.srv != nil && c.srv.Control != nil && len(candidates) > 0 {
		ids := make([]string, 0, len(candidates))
		for _, run := range candidates {
			ids = append(ids, run.ID)
		}
		_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
			TaskID:     candidates[0].TaskID,
			Type:       "continuation.candidates",
			Visibility: "task",
			Channel:    candidates[0].Channel,
			Payload: mustJSON(map[string]interface{}{
				"count":       len(candidates),
				"run_ids":     ids,
				"cross_task":  true,
				"person_wide": true,
				"choice_id":   choiceID(choice),
			}),
		})
	}
	return api.MessageResponse{
		Identity: identity,
		Content:  content,
		Turn:     messageTurn("waiting_user", "", "idle", "", "", content),
		Choice:   choice,
	}
}

func choiceID(choice *api.TurnChoice) string {
	if choice == nil {
		return ""
	}
	return choice.ID
}

func shortRunID(id string) string {
	const prefix = "run_"
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix)+8 {
		return id[:len(prefix)+8]
	}
	return id
}

func (d *Server) resolveUnresolvedRunReference(ctx context.Context, identity *control.IdentityContext, ref string) (*control.Run, string, error) {
	ref = strings.TrimSpace(ref)
	if d == nil || d.Control == nil || identity == nil || ref == "" {
		return nil, "", nil
	}
	runs, err := d.Control.ListExplicitlyResumableRunsForPerson(ctx, identity.TenantID, identity.PersonID, 200)
	if err != nil {
		return nil, "", err
	}
	var matches []control.Run
	for _, run := range runs {
		if run.ID == ref || shortRunID(run.ID) == ref || strings.HasPrefix(run.ID, ref) {
			matches = append(matches, run)
		}
	}
	switch len(matches) {
	case 0:
		return nil, "Run not found or no longer resumable. Use /tasks and /task <task_id> runs to inspect waiting work.", nil
	case 1:
		return &matches[0], "", nil
	default:
		return nil, "That run reference is ambiguous; copy the longer run id from /task <task_id> runs.", nil
	}
}

func relativeAge(at time.Time) string {
	if at.IsZero() {
		return "unknown age"
	}
	d := time.Since(at)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
