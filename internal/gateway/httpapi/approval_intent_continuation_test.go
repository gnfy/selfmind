package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

func TestContinuationRetainsAcceptedOfferAsEvidenceNotAuthority(t *testing.T) {
	d, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	parent, err := store.StartRun(ctx, task, "cli", "Proceed")
	if err != nil {
		t.Fatal(err)
	}
	intent := runIntentSnapshot(api.MessageRequest{Content: "Proceed"}, task, parent, nil)
	intent.PriorAssistantOffer = "Inspect the requested service and repair the configuration in this workspace."
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: parent.ID, Type: "run.started", Payload: mustJSON(map[string]interface{}{"approval_intent": persistedApprovalIntent{Version: 2, Snapshot: intent}})}); err != nil {
		t.Fatal(err)
	}
	child := &control.Run{ID: parent.ID, ResumesRunID: parent.ID, WorkspaceID: parent.WorkspaceID}
	system := runIntentSnapshot(api.MessageRequest{Origin: runOriginWatch, Content: "Continue; the watcher succeeded."}, task, child, nil)
	got := d.coordinator().continuationApprovalIntent(ctx, identity, child, system)
	if got.UserAuthored() || len(got.ExplicitAllow) != 0 {
		t.Fatal("system continuation acquired authority")
	}
	if len(got.AuthorizationEvidence) != 1 || got.AuthorizationEvidence[0].RunID != parent.ID || got.AuthorizationEvidence[0].UserText != "Proceed" || !strings.Contains(got.AuthorizationEvidence[0].AcceptedOffer, "repair the configuration") {
		t.Fatalf("lost accepted offer: %+v", got.AuthorizationEvidence)
	}
	stranger := *identity
	stranger.PersonID = "another-person"
	if other := d.coordinator().continuationApprovalIntent(ctx, &stranger, child, system); len(other.AuthorizationEvidence) != 0 {
		t.Fatal("cross-person evidence leak")
	}
	child.WorkspaceID = "different-workspace"
	if other := d.coordinator().continuationApprovalIntent(ctx, identity, child, system); len(other.AuthorizationEvidence) != 0 {
		t.Fatal("cross-workspace evidence widened authorization")
	}
}

func TestContinuationEvidenceSurvivesAnotherSystemResume(t *testing.T) {
	d, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	parent, err := store.StartRun(ctx, task, "cli", "system resume")
	if err != nil {
		t.Fatal(err)
	}
	prior := tools.RunIntentSnapshot{Source: "system:watch", AuthorizationEvidence: []tools.AuthorizationEvidence{{RunID: "original", UserText: "Inspect receipt.txt"}}, AddedRequirements: []string{"Only inspect; leave the file unchanged."}}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: parent.ID, Type: "run.started", Payload: mustJSON(map[string]interface{}{"approval_intent": persistedApprovalIntent{Version: 2, Snapshot: prior}})}); err != nil {
		t.Fatal(err)
	}
	child := &control.Run{ID: parent.ID, ResumesRunID: parent.ID, WorkspaceID: parent.WorkspaceID}
	got := d.coordinator().continuationApprovalIntent(ctx, identity, child, tools.RunIntentSnapshot{Source: "system:recovery"})
	if len(got.AuthorizationEvidence) != 1 || got.AuthorizationEvidence[0].UserText != "Inspect receipt.txt" || len(got.AddedRequirements) != 1 {
		t.Fatalf("evidence lost: %+v", got)
	}
	if len(prior.AuthorizationEvidence) != 1 {
		t.Fatal("mutated frozen parent")
	}
}

func TestContinuationSeparatesSystemGuidanceFromDurableUserProhibitions(t *testing.T) {
	d, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	parent, err := store.StartRun(ctx, task, "cli", "prepare receipt")
	if err != nil {
		t.Fatal(err)
	}
	intent := runIntentSnapshot(api.MessageRequest{Content: "Prepare receipt.txt. Do not modify config.yaml."}, task, parent, nil)
	// Historical v1 evidence retains its frozen restriction; new runs no longer parse one.
	intent.ExplicitDeny = []string{"do not"}
	intent.DenyScopes = []tools.DenyScope{{Marker: "do not", Clause: "do not modify config.yaml", Classes: []tools.OperationClass{tools.OpClassWrite}, Targets: []string{"config.yaml"}, Resolved: true}}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: parent.ID, Type: "run.started", Payload: mustJSON(map[string]interface{}{"approval_intent": persistedApprovalIntent{Version: 1, Snapshot: intent}})}); err != nil {
		t.Fatal(err)
	}
	child := &control.Run{ID: parent.ID, ResumesRunID: parent.ID}
	system := runIntentSnapshot(api.MessageRequest{Origin: runOriginWatch, Content: "Do not weaken criteria. Never invent evidence."}, task, child, nil)
	if system.HasExplicitDeny() {
		t.Fatal("system guidance became a user prohibition")
	}
	restored := d.coordinator().continuationApprovalIntent(ctx, identity, child, system)
	if restored.UserAuthored() || len(restored.ExplicitAllow) > 0 {
		t.Fatal("continuation gained user authorization")
	}
	if restored.DenyBlocks([]tools.OperationClass{tools.OpClassWrite}, []string{"receipt.txt"}) {
		t.Fatal("unrelated write blocked")
	}
	if !restored.DenyBlocks([]tools.OperationClass{tools.OpClassWrite}, []string{"config.yaml"}) {
		t.Fatal("user's protected file lost on resume")
	}
	if _, err := store.AcceptSteering(ctx, control.SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: parent.ID, Channel: "cli", Content: "Do not access the network.", ContentHash: "network-deny"}); err != nil {
		t.Fatal(err)
	}
	restored = d.coordinator().continuationApprovalIntent(ctx, identity, child, system)
	if len(restored.AddedRequirements) != 1 || restored.AddedRequirements[0] != "Do not access the network." {
		t.Fatal("late user narrowing lost")
	}
	denies, scopes := len(restored.ExplicitDeny), len(restored.DenyScopes)
	for n := 0; n < 10; n++ {
		restored = d.coordinator().intentWithAddedRequirements(ctx, identity, restored, parent.ID)
	}
	if len(restored.ExplicitDeny) != denies || len(restored.DenyScopes) != scopes {
		t.Fatal("re-reading the same steering grew the policy context")
	}
	stranger := *identity
	stranger.PersonID = "other-person"
	restored = d.coordinator().continuationApprovalIntent(ctx, &stranger, child, system)
	if !restored.DenyBlocks([]tools.OperationClass{tools.OpClassWrite}, []string{"receipt.txt"}) {
		t.Fatal("missing/cross-person evidence failed open")
	}
}

func TestLegacyContinuationWithoutRecordedIntentRequiresConfirmation(t *testing.T) {
	d, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	parent, err := store.StartRun(ctx, task, "cli", "legacy request")
	if err != nil {
		t.Fatal(err)
	}
	child := &control.Run{ResumesRunID: parent.ID}
	snapshot := d.coordinator().continuationApprovalIntent(ctx, identity, child, runIntentSnapshot(api.MessageRequest{Origin: runOriginWatch, Content: "Continue"}, task, child, nil))
	if !snapshot.HasExplicitDeny() {
		t.Fatal("legacy source was assumed to have no constraints")
	}
}

func TestApprovalRequirementsKeepRepeatedCorrectionInOrder(t *testing.T) {
	d, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	run, err := store.StartRun(ctx, task, "cli", "inspect")
	if err != nil {
		t.Fatal(err)
	}
	base := tools.RunIntentSnapshot{Source: "direct", RawUserText: "Inspect"}
	for i, text := range []string{"Keep it a draft", "Publish after verification", "Keep it a draft"} {
		if _, err := store.AcceptSteering(ctx, control.SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, Content: text, ContentHash: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		base = d.coordinator().intentWithAddedRequirements(ctx, identity, base, run.ID)
	}
	for i := 0; i < 3; i++ {
		base = d.coordinator().intentWithAddedRequirements(ctx, identity, base, run.ID)
	}
	if len(base.AddedRequirements) != 3 || base.AddedRequirements[2] != "Keep it a draft" {
		t.Fatalf("correction chronology lost: %+v", base.AddedRequirements)
	}
}

func TestWatcherEvidenceIncludesParentClaimedAfterRunStart(t *testing.T) {
	d, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	original, err := store.StartRun(ctx, task, "cli", "original task")
	if err != nil {
		t.Fatal(err)
	}
	originalIntent := runIntentSnapshot(api.MessageRequest{Content: "Prepare a receipt; leave config.yaml unchanged."}, task, original, nil)
	if _, err = store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: original.ID, Type: "run.started", Payload: mustJSON(map[string]interface{}{"approval_intent": persistedApprovalIntent{Version: 3, Snapshot: originalIntent}})}); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishRun(ctx, identity.TenantID, original.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	sourceTask, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Proceed", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.StartRun(ctx, sourceTask, "cli", "Proceed")
	if err != nil {
		t.Fatal(err)
	}
	frozen := runIntentSnapshot(api.MessageRequest{Content: "Proceed"}, sourceTask, source, nil)
	if _, err = store.AppendEvent(ctx, control.Event{TaskID: sourceTask.ID, RunID: source.ID, Type: "run.started", Payload: mustJSON(map[string]interface{}{"approval_intent": persistedApprovalIntent{Version: 3, Snapshot: frozen}})}); err != nil {
		t.Fatal(err)
	}
	source, err = store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, source.ID, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	child := &control.Run{ID: source.ID, ResumesRunID: source.ID}
	got := d.coordinator().continuationApprovalIntent(ctx, identity, child, tools.RunIntentSnapshot{Source: "system:watch", ModelAuthorization: true})
	if got.AuthorizationEvidenceIncomplete || len(got.AuthorizationEvidence) != 2 || got.AuthorizationEvidence[0].RunID != original.ID || got.AuthorizationEvidence[1].UserText != "Proceed" {
		t.Fatalf("post-start parent claim lost its authorization history: %+v", got)
	}
}
