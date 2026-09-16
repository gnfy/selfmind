package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

type persistedApprovalIntent struct {
	Version  int                     `json:"version"`
	Snapshot tools.RunIntentSnapshot `json:"snapshot"`
}

// System text is never a person's prohibition or authorization. A continuation
// carries attributed human evidence, not a grant, from its exact parent.
// Historical/missing evidence asks the human instead of inferring permission.
func (c *RunCoordinator) continuationApprovalIntent(ctx context.Context, identity *control.IdentityContext, run *control.Run, snapshot tools.RunIntentSnapshot) tools.RunIntentSnapshot {
	return c.continuationApprovalIntentAtDepth(ctx, identity, run, snapshot, 0)
}

func (c *RunCoordinator) continuationApprovalIntentAtDepth(ctx context.Context, identity *control.IdentityContext, run *control.Run, snapshot tools.RunIntentSnapshot, depth int) tools.RunIntentSnapshot {
	if depth >= tools.MaxAuthorizationEvidence {
		return unavailableContinuationIntent(snapshot)
	}
	if run == nil || run.ResumesRunID == "" {
		return snapshot
	}
	parentRun, parentErr := c.srv.Control.GetRun(ctx, identity.TenantID, run.ResumesRunID)
	if parentErr != nil || parentRun == nil || parentRun.PersonID != identity.PersonID || parentRun.WorkspaceID != run.WorkspaceID || !slices.Equal(parentRun.ExecutionRoots, run.ExecutionRoots) {
		return unavailableContinuationIntent(snapshot)
	}
	var evidence persistedApprovalIntent
	raw, err := c.srv.Control.RunApprovalIntent(ctx, identity.TenantID, identity.PersonID, run.ResumesRunID)
	if err != nil || len(raw) == 0 || json.Unmarshal(raw, &evidence) != nil || (evidence.Version < 1 || evidence.Version > 3) || evidence.Snapshot.Source == "" {
		return unavailableContinuationIntent(snapshot)
	}
	parent := evidence.Snapshot
	// A direct claim happens after run.started. Follow the actual owned edge
	// when its frozen evidence predates that claim; never infer ancestry from
	// the assistant's prose or a nearby Thread.
	if evidence.Version == 3 && parentRun.ResumesRunID != "" && parent.AuthorizationParentRunID != parentRun.ResumesRunID {
		parent = c.continuationApprovalIntentAtDepth(ctx, identity, parentRun, parent, depth+1)
	}
	snapshot.AuthorizationParentRunID = parentRun.ID
	snapshot = appendUniqueProhibitions(snapshot, parent.ExplicitDeny, parent.DenyScopes)
	snapshot.AuthorizationEvidence = append([]tools.AuthorizationEvidence(nil), parent.AuthorizationEvidence...)
	snapshot.AuthorizationEvidenceIncomplete = snapshot.AuthorizationEvidenceIncomplete || parent.AuthorizationEvidenceIncomplete
	snapshot.AddedRequirements = append([]string(nil), parent.AddedRequirements...)
	snapshot.AddedRequirementIDs = append([]string(nil), parent.AddedRequirementIDs...)
	if parent.UserAuthored() && strings.TrimSpace(parent.RawUserText) != "" {
		quotation := tools.AuthorizationEvidence{RunID: parentRun.ID, UserText: parent.RawUserText, AcceptedOffer: parent.PriorAssistantOffer}
		if len(snapshot.AuthorizationEvidence) < tools.MaxAuthorizationEvidence {
			snapshot.AuthorizationEvidence = append(snapshot.AuthorizationEvidence, quotation)
		} else {
			snapshot.AuthorizationEvidenceIncomplete = true
		}
	}
	return c.intentWithAddedRequirements(ctx, identity, snapshot, run.ID)
}

func unavailableContinuationIntent(snapshot tools.RunIntentSnapshot) tools.RunIntentSnapshot {
	snapshot.AuthorizationEvidenceIncomplete = true
	if !snapshot.UserAuthored() {
		snapshot.ExplicitDeny = append(snapshot.ExplicitDeny, "Prior user constraints unavailable; confirm the action before execution.")
	}
	return snapshot
}

// The same steering evidence may be read after several resumes. Preserve its
// restrictions once, so repeated recovery cannot grow policy context forever.
func appendUniqueProhibitions(snapshot tools.RunIntentSnapshot, deny []string, scopes []tools.DenyScope) tools.RunIntentSnapshot {
	seenDeny := make(map[string]bool, len(snapshot.ExplicitDeny))
	for _, value := range snapshot.ExplicitDeny {
		seenDeny[value] = true
	}
	for _, value := range deny {
		if !seenDeny[value] {
			snapshot.ExplicitDeny = append(snapshot.ExplicitDeny, value)
			seenDeny[value] = true
		}
	}
	seenScope := make(map[string]bool, len(snapshot.DenyScopes))
	for _, scope := range snapshot.DenyScopes {
		raw, _ := json.Marshal(scope)
		seenScope[string(raw)] = true
	}
	for _, scope := range scopes {
		raw, _ := json.Marshal(scope)
		if !seenScope[string(raw)] {
			snapshot.DenyScopes = append(snapshot.DenyScopes, scope)
			seenScope[string(raw)] = true
		}
	}
	return snapshot
}
