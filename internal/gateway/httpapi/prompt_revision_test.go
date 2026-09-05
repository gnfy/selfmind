package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/promptassets"
)

func TestPostRunReplayPayloadPinsPromptSnapshot(t *testing.T) {
	raw, err := buildPostRunMaintenancePayload(
		&control.IdentityContext{TenantID: "tenant", PersonID: "person"},
		&control.Task{ID: "task"},
		&control.Run{ID: "run"},
		"workspace", "input", api.RunOutcome{}, taskAttach{}, "prompt-hash",
	)
	if err != nil {
		t.Fatal(err)
	}
	var payload postRunJobPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PromptSnapshotHash != "prompt-hash" {
		t.Fatalf("prompt hash = %q", payload.PromptSnapshotHash)
	}
}

func TestBlockPromptRevisionJobPersistsAfterWorkerContextCancellation(t *testing.T) {
	store := controltest.NewStore(t)
	const tenantID = "tenant"
	const key = "skillreview_missing_revision"
	if _, err := store.EnqueueMaintenanceJob(context.Background(), tenantID, key, SkillReviewJobVersion, `{}`); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMaintenanceJob(context.Background(), tenantID, key, SkillReviewJobVersion); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	_, revisionErr := promptassets.LoadRevision(t.TempDir(), strings.Repeat("a", 64))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := &Server{Control: store}
	if !server.blockPromptRevisionJob(ctx, tenantID, key, SkillReviewJobVersion, revisionErr) {
		t.Fatal("cancelled worker context did not persist prompt revision block")
	}
	job, err := store.GetMaintenanceJob(context.Background(), tenantID, key, SkillReviewJobVersion)
	if err != nil || job == nil || job.Status != control.MaintenanceJobBlockedPromptRevision {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if server.blockPromptRevisionJob(context.Background(), tenantID, "missing", SkillReviewJobVersion, revisionErr) {
		t.Fatal("zero-row transition reported as handled")
	}
}
