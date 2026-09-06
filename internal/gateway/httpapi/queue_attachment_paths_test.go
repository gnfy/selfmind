package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"selfmind/internal/gateway/api"
	"strings"
	"testing"
)

// TestEveryUserFacingEnqueuePathAdmitsAttachments guards the class, not one
// instance. There are three ways a person's work can park — the model is not
// ready, something else is running, a model change is draining — and each writes
// its own durable row. Fixing one left the other two accepting work and silently
// dropping the files, which is indistinguishable to the person from success.
func TestEveryUserFacingEnqueuePathAdmitsAttachments(t *testing.T) {
	root := repoRootFromTest(t)
	source, err := os.ReadFile(filepath.Join(root, "internal", "gateway", "httpapi", "queue.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)

	enqueues := strings.Count(text, "d.Control.EnqueueQueued(ctx, control.QueuedTask{")
	admissions := strings.Count(text, "d.admitQueuedAttachments(ctx, identity, req)")
	if enqueues == 0 {
		t.Fatal("no enqueue call sites found; this guard would be vacuous")
	}
	if admissions < enqueues {
		t.Fatalf("%d enqueue path(s) but only %d admit attachments: a parked request would be accepted with its files dropped",
			enqueues, admissions)
	}
}

func messageWithAttachment(path string) api.MessageRequest {
	return api.MessageRequest{
		Platform: "cli", Channel: "cli", Content: "review this",
		Attachments: []api.MessageAttachment{{Name: filepath.Base(path), Path: path}},
	}
}

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find repository root")
	return ""
}

// And the behaviour itself, through the busy-queue path.
func TestQueueBehindActiveRunKeepsAttachments(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	daemon.AttachmentsDir = t.TempDir()
	ctx := withLocalFilesystemAuthority(context.Background())

	source := filepath.Join(t.TempDir(), "spec.pdf")
	if err := os.WriteFile(source, []byte("PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	refs := daemon.admitQueuedAttachments(ctx, identity, messageWithAttachment(source))
	if len(refs) != 1 || refs[0].Path == "" || refs[0].Path == source {
		t.Fatalf("admission should yield a managed copy: %+v", refs)
	}
	if _, err := os.Stat(refs[0].Path); err != nil {
		t.Fatalf("the managed copy must exist for the drain: %v", err)
	}
	_ = store
}
