package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSkillLearningHealthMatchesReadinessAndPartitionsEvidence(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	identity, err := s.ResolveOrCreateAccount(ctx, "default", "cli", "learning", "Learning")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect metadata", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	var last *Run
	for i := 1; i <= 3; i++ {
		last, err = s.StartRun(ctx, task, "cli", "inspect release metadata")
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"tool.started", "tool.completed"} {
			if _, err := s.AppendEvent(ctx, Event{TaskID: task.ID, RunID: last.ID, Type: kind, Payload: json.RawMessage(`{"tool":"read_file"}`)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *identity, RunID: last.ID, RunStatus: "done", TaskID: task.ID, TaskStatus: "in_progress", VerificationState: "passed", Channel: "cli", Event: Event{Type: "run.finished"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MaterializeWorkflowObservations(ctx, identity.TenantID, last.ID); err != nil {
			t.Fatal(err)
		}
		h, err := s.SkillLearningHealthForWorkspace(ctx, identity.TenantID, identity.PersonID, "", 101)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := s.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, last.ID)
		if err != nil {
			t.Fatal(err)
		}
		if h.MaxIndependentSuccesses != i || h.VerifiedSuccesses != i || (len(ready) > 0) != (h.Reasons["ready"] > 0) {
			t.Fatalf("iteration %d: health=%+v ready=%d", i, h, len(ready))
		}
	}
	digests, err := s.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, last.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest := digests[0]
	if _, err := s.CreateSkillCandidateVersion(ctx, identity.TenantID, "key", "inspect-metadata", "", "# Procedure\nInspect metadata", digest.EvidenceSetHash, nil, digest); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(digest)
	if _, err := s.EnqueueMaintenanceJob(ctx, identity.TenantID, "skillcuration_test", 101, string(payload)); err != nil {
		t.Fatal(err)
	}
	h, err := s.SkillLearningHealthForWorkspace(ctx, identity.TenantID, identity.PersonID, "", 101)
	if err != nil || h.Versions["candidate"] != 1 || h.Jobs["pending"] != 1 {
		t.Fatalf("health=%+v err=%v", h, err)
	}
	for _, scope := range []struct{ tenant, person, workspace string }{
		{identity.TenantID, "stranger", ""}, {"other-tenant", identity.PersonID, ""}, {identity.TenantID, identity.PersonID, "other-workspace"},
	} {
		h, err := s.SkillLearningHealthForWorkspace(ctx, scope.tenant, scope.person, scope.workspace, 101)
		if err != nil || h.Observations != 0 || len(h.Jobs) != 0 || len(h.Versions) != 0 {
			t.Fatalf("cross-scope evidence: %+v %v", h, err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.SkillLearningHealthForWorkspace(ctx, identity.TenantID, identity.PersonID, "", 101); err == nil {
		t.Fatal("cancelled query reported an empty success")
	}
}
