package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
)

func (c *RunCoordinator) resolveTaskReference(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, continuation bool) (*control.Task, taskAttach, bool, error) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
		return nil, taskAttach{}, false, nil
	}
	matches, err := c.srv.Control.FindTaskReferenceMatches(ctx, identity.TenantID, identity.PersonID, req.Content, 20)
	if err != nil {
		return nil, taskAttach{}, false, fmt.Errorf("find task references: %w", err)
	}
	routable := make(map[string]control.Task)
	allCandidates := make(map[string]control.Task)
	var surfaces []string
	for _, match := range matches {
		allCandidates[match.Task.ID] = match.Task
		surfaces = append(surfaces, match.Reference.RawValue)
		if !match.Task.IsVisible() || match.Task.IsInbox() || archivedTaskStatus(match.Task.Status) {
			continue
		}
		if continuation && terminalTaskStatus(match.Task.Status) {
			continue
		}
		routable[match.Task.ID] = match.Task
	}
	candidateIDs := make([]string, 0, len(allCandidates))
	for taskID := range allCandidates {
		candidateIDs = append(candidateIDs, taskID)
	}
	sort.Strings(candidateIDs)
	candidateHints := make([]string, 0, len(candidateIDs))
	for _, taskID := range candidateIDs {
		candidateHints = append(candidateHints, taskReferenceCandidateHint(allCandidates[taskID]))
	}
	base := taskAttach{matchedSurfaceForms: surfaces, candidateTaskIDs: candidateIDs, candidateTaskHints: candidateHints}
	if len(routable) > 1 {
		routableIDs := make([]string, 0, len(routable))
		for taskID := range routable {
			routableIDs = append(routableIDs, taskID)
		}
		sort.Strings(routableIDs)
		var hints []string
		for _, taskID := range routableIDs {
			hints = append(hints, taskReferenceCandidateHint(routable[taskID]))
		}
		return nil, base, false, fmt.Errorf("task reference matches multiple resumable tasks: %s; use /resume <task_id>", strings.Join(hints, "; "))
	}
	if len(routable) == 0 {
		return nil, base, false, nil
	}
	var task control.Task
	for _, item := range routable {
		task = item
	}
	reason := taskAttachReferenceMention
	if continuation {
		reason = taskAttachReferenceContinue
	}
	attach := newTaskAttach(reason, uniqueTaskWorkKey(req.Content), false, !continuation)
	attach.matchedSurfaceForms = surfaces
	attach.candidateTaskIDs = candidateIDs
	attach.candidateTaskHints = candidateHints
	return &task, attach, true, nil
}

func taskReferenceCandidateHint(task control.Task) string {
	return fmt.Sprintf("%s [%s] %s", shortTaskID(task.ID), strings.TrimSpace(task.Status),
		textutil.Truncate(toOneLine(task.Title), 48))
}

func taskResolutionInputHash(content string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(content)))
	return hex.EncodeToString(digest[:8])
}

func (c *RunCoordinator) recordTaskResolution(ctx context.Context, identity *control.IdentityContext, run *control.Run, req api.MessageRequest, task *control.Task, attach taskAttach, finalTaskID, outcome string, analyzed bool) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || run == nil || task == nil {
		return
	}
	unmatched := []string(nil)
	if attach.workKey != "" && len(attach.matchedSurfaceForms) == 0 {
		unmatched = []string{attach.workKey}
	}
	_ = c.srv.Control.RecordTaskResolution(context.WithoutCancel(ctx), control.TaskResolutionRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID,
		InputHash: taskResolutionInputHash(req.Content), MatchedSurfaceForms: attach.matchedSurfaceForms,
		UnmatchedSalientTerms: unmatched, CandidateTaskIDs: attach.candidateTaskIDs,
		SelectedTaskID: task.ID, FinalTaskID: finalTaskID, Reason: string(attach.reason),
		Outcome: outcome, AttachPolicy: attach.resolvedPolicy(), AnalyzerEvaluated: analyzed,
	})
}

func (d *Server) applyTaskReferenceProposals(ctx context.Context, prepared *preparedPostRunAnalysis, analysis PostRunAnalysis) error {
	if d == nil || d.Control == nil || prepared == nil || prepared.identity == nil || prepared.run == nil {
		return nil
	}
	finalRun, err := d.Control.GetRun(ctx, prepared.identity.TenantID, prepared.run.ID)
	if err != nil {
		return err
	}
	if finalRun == nil || strings.TrimSpace(finalRun.TaskID) == "" {
		return fmt.Errorf("final task is unavailable for run %s", prepared.run.ID)
	}
	workspaceID := prepared.request.WorkspaceID
	for _, proposal := range analysis.TaskReferences {
		if !control.ValidTaskReference(proposal.Value, false) || tools.RedactSensitive(proposal.Value) != proposal.Value {
			continue
		}
		provenance := "analyzer"
		status := control.TaskReferenceShadow
		if proposal.Confidence >= 0.55 && control.TaskReferenceAppearsInText(prepared.request.UserInput, proposal.Value) {
			provenance = "user_text"
			status = control.TaskReferenceCandidate
		}
		_, err := d.Control.UpsertTaskReference(ctx, control.TaskReferenceWrite{
			TenantID: prepared.identity.TenantID, PersonID: prepared.identity.PersonID,
			TaskID: finalRun.TaskID, WorkspaceID: workspaceID, Class: proposal.Class, Value: proposal.Value,
			Status: status, RunID: prepared.run.ID, Provenance: provenance,
			SourceRef: "turn:" + taskResolutionInputHash(prepared.request.UserInput),
		})
		if err != nil {
			return fmt.Errorf("store task reference %q: %w", proposal.Value, err)
		}
	}
	resolutionOutcome := "accepted_unverified"
	if finalRun.TaskID != prepared.task.ID {
		resolutionOutcome = "corrected"
	}
	d.coordinator().recordTaskResolution(ctx, prepared.identity, prepared.run,
		api.MessageRequest{Content: prepared.request.UserInput}, prepared.task, prepared.attach,
		finalRun.TaskID, resolutionOutcome, true)
	return nil
}
