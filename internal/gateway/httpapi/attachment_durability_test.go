package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
)

// TestQueuedWorkKeepsItsAttachments pins what "accepted" means. A request that
// parks used to persist its text and drop its files: the person was told the
// work was accepted, and the model never saw the image. The files must be
// imported into the person's partition BEFORE the row is written, and restored
// when it drains.
func TestQueuedWorkKeepsItsAttachments(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	daemon.AttachmentsDir = t.TempDir()
	ctx := withLocalFilesystemAuthority(context.Background())

	unready := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(unready, []byte("models:\n  primary:\n    provider: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon.ModelChanges = &modelchange.Service{ConfigPath: unready}

	source := filepath.Join(t.TempDir(), "diagram.png")
	if err := os.WriteFile(source, []byte("PNGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	resp := daemon.enqueueUntilModelReady(ctx, identity, api.MessageRequest{
		Platform: "cli", Channel: "cli", Content: "look at this",
		Attachments: []api.MessageAttachment{{Name: "diagram.png", Path: source, MimeType: "image/png"}},
	})
	if !resp.Accepted {
		t.Fatalf("work should be accepted into the queue: %+v", resp)
	}

	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("expected one queued row, got %d", len(queued))
	}
	if len(queued[0].Attachments) != 1 {
		t.Fatalf("queued work lost its attachment: %+v", queued[0].Attachments)
	}
	ref := queued[0].Attachments[0]
	if ref.Name != "diagram.png" || ref.MimeType != "image/png" {
		t.Fatalf("attachment metadata not preserved: %+v", ref)
	}
	if ref.Path == source {
		t.Fatal("the durable row must reference the managed copy, not the caller's path")
	}
	if _, err := os.Stat(ref.Path); err != nil {
		t.Fatalf("the managed copy must survive for the drain: %v", err)
	}

	// And the drain hands them back to the turn.
	restored := attachmentsFromRefs(queued[0].Attachments)
	if len(restored) != 1 || restored[0].Path != ref.Path || restored[0].Name != "diagram.png" {
		t.Fatalf("restored attachments do not match what was queued: %+v", restored)
	}
}

// A row written before the column existed decodes as no attachments, which is
// what it had — historical rows stay capability-inert.
func TestQueueRowsWithoutAttachmentsStayReadable(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	_ = daemon
	ctx := context.Background()

	queued, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli", Platform: "cli", Content: "no files here",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued.Attachments) != 0 {
		t.Fatalf("a row with no attachments must decode as none: %+v", queued.Attachments)
	}
	rows, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list failed: %v rows=%d", err, len(rows))
	}
	if len(rows[0].Attachments) != 0 {
		t.Fatalf("re-read must also decode as none: %+v", rows[0].Attachments)
	}
}
