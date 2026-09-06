package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// TestManagedAttachmentsSurviveAContextWithoutAuthority is the case the earlier
// tests missed by asserting only that a file was saved. An async turn runs on a
// context built from Background, and a drained queue item runs long after the
// request that carried it, so neither holds the local-authority marker. Judging
// the REQUEST therefore stripped attachments that had been admitted properly and
// already lived in the person's own partition: the model was handed metadata
// with no file. Ownership is the question, and it is asked per attachment.
func TestManagedAttachmentsSurviveAContextWithoutAuthority(t *testing.T) {
	daemon, _, identity, _, _ := newApprovalTestServer(t)
	daemon.AttachmentsDir = t.TempDir()
	coord := daemon.coordinator()

	source := filepath.Join(t.TempDir(), "diagram.png")
	if err := os.WriteFile(source, []byte("PNGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Accepted by an authorized local CLI.
	admitted := coord.importAttachments(withLocalFilesystemAuthority(context.Background()),
		identity, nil, []api.MessageAttachment{{Name: "diagram.png", Path: source}})
	if len(admitted) != 1 || admitted[0].Path == "" {
		t.Fatalf("import should have admitted the file: %+v", admitted)
	}
	managed := admitted[0].Path

	// The turn that finally runs it has no marker at all.
	delivered := coord.importAttachments(context.Background(), identity, nil, admitted)
	if len(delivered) != 1 {
		t.Fatalf("attachments must not be dropped: %+v", delivered)
	}
	if delivered[0].Path != managed {
		t.Fatalf("a managed attachment must reach the model unchanged: got %q, want %q",
			delivered[0].Path, managed)
	}
	if _, err := os.Stat(delivered[0].Path); err != nil {
		t.Fatalf("the file the model is told about must exist: %v", err)
	}
}

// TestConcurrentAcceptancesDoNotOverwriteEachOther pins storage isolation. Two
// requests that both attach "diagram.png" used to write 01-diagram.png into one
// shared bucket, so the second silently replaced the contents of the first —
// while a queued row still referenced that path.
func TestConcurrentAcceptancesDoNotOverwriteEachOther(t *testing.T) {
	daemon, _, identity, _, _ := newApprovalTestServer(t)
	daemon.AttachmentsDir = t.TempDir()
	coord := daemon.coordinator()
	ctx := withLocalFilesystemAuthority(context.Background())

	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "diagram.png")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	first := coord.importAttachments(ctx, identity, nil,
		[]api.MessageAttachment{{Name: "diagram.png", Path: write("FIRST")}})
	second := coord.importAttachments(ctx, identity, nil,
		[]api.MessageAttachment{{Name: "diagram.png", Path: write("SECOND")}})

	if first[0].Path == second[0].Path {
		t.Fatalf("two acceptances shared one destination: %q", first[0].Path)
	}
	got, err := os.ReadFile(first[0].Path)
	if err != nil {
		t.Fatalf("the first acceptance no longer exists: %v", err)
	}
	if string(got) != "FIRST" {
		t.Fatalf("the first attachment was overwritten by the second: %q", got)
	}

	// A run-scoped import keeps using the run's own directory.
	run := &control.Run{ID: "run_abc"}
	scoped := coord.importAttachments(ctx, identity, run,
		[]api.MessageAttachment{{Name: "diagram.png", Path: write("SCOPED")}})
	if filepath.Base(filepath.Dir(scoped[0].Path)) != "run_abc" {
		t.Fatalf("a run-scoped import should live under the run: %q", scoped[0].Path)
	}
}
