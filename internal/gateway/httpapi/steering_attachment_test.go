package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// TestSteeringDeliversAttachmentsToTheModel is the assertion the earlier tests
// were missing: not "the file was saved" but "the model was told about it".
// Steering persisted attachments and handed the kernel bare text, so a person
// who attached a file to mid-turn guidance was told it was accepted while the
// model never learned the file existed.
func TestSteeringDeliversAttachmentsToTheModel(t *testing.T) {
	atts := []api.MessageAttachment{{
		Name: "diagram.png", Path: "/managed/p1/accept-x/01-diagram.png",
		MimeType: "image/png", Size: 7,
	}}

	delivered := steeringContentWithAttachments("look at the diagram", atts)
	if !strings.Contains(delivered, "look at the diagram") {
		t.Fatalf("the person's words must survive: %q", delivered)
	}
	for _, want := range []string{"attachments:", "/managed/p1/accept-x/01-diagram.png", "image/png", "diagram.png"} {
		if !strings.Contains(delivered, want) {
			t.Fatalf("guidance sent to the model is missing %q:\n%s", want, delivered)
		}
	}

	// No attachments: the guidance is untouched, so nothing is added to a
	// plain mid-turn correction.
	if got := steeringContentWithAttachments("just text", nil); got != "just text" {
		t.Fatalf("plain guidance must not be decorated: %q", got)
	}
}

// TestOpeningTurnAndSteeringDescribeAttachmentsIdentically: two renderings that
// drift are two things for the model to learn. Both go through one function.
func TestOpeningTurnAndSteeringDescribeAttachmentsIdentically(t *testing.T) {
	atts := []api.MessageAttachment{{Name: "a.txt", Path: "/managed/a.txt", Size: 3}}
	block := renderAttachmentBlock(atts)
	if block == "" {
		t.Fatal("renderer produced nothing")
	}
	if !strings.Contains(steeringContentWithAttachments("hi", atts), block) {
		t.Fatal("steering must embed the same block the opening turn renders")
	}
}

// TestDeferredSteeringKeepsItsAttachments: guidance a finished run never
// consumed becomes queued work, and must not lose the file on the way.
func TestDeferredSteeringKeepsItsAttachments(t *testing.T) {
	_, store, identity, task, _ := newApprovalTestServer(t)
	ctx := context.Background()

	run, err := store.StartRun(ctx, task, "cli", "work")
	if err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(t.TempDir(), "01-diagram.png")
	if err := os.WriteFile(managed, []byte("PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := store.AcceptSteering(ctx, control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "cli", Platform: "cli",
		Content:     "use this diagram",
		Attachments: []control.AttachmentRef{{Name: "diagram.png", Path: managed}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Attachments) != 1 {
		t.Fatalf("the durable record lost the attachment: %+v", record.Attachments)
	}
	if err := store.DeferSteering(ctx, *record); err != nil {
		t.Fatal(err)
	}
	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(queued) != 1 {
		t.Fatalf("expected one deferred row: %v len=%d", err, len(queued))
	}
	if len(queued[0].Attachments) != 1 || queued[0].Attachments[0].Path != managed {
		t.Fatalf("deferred guidance lost its attachment: %+v", queued[0].Attachments)
	}
}

// TestSteeringSendSiteCarriesAttachments guards the WIRING, not just the
// renderer. A pure-function test on the renderer stayed green while the send
// site handed the kernel bare text — which is exactly the shape of the original
// defect: the files were persisted, and delivery dropped them. Assert that
// every steering send on the path that can carry attachments actually does.
func TestSteeringSendSiteCarriesAttachments(t *testing.T) {
	root := repoRootFromTest(t)
	source, err := os.ReadFile(filepath.Join(root, "internal", "gateway", "httpapi", "handlers_steer.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)

	// The message path imports attachments; its send must include them.
	if !strings.Contains(text, "steerAttachments := ") {
		t.Fatal("the message steering path no longer imports attachments")
	}
	if !strings.Contains(text, "steeringContentWithAttachments(text, steerAttachments)") {
		t.Fatal("steering is sent to the kernel without its attachments; the person is told the file was accepted and the model never learns it exists")
	}
	// And the durable record keeps them, so an unconsumed message can defer.
	if !strings.Contains(text, "attachmentRefsFromAPI(steerAttachments)") {
		t.Fatal("the steering record no longer persists its attachments")
	}
}
