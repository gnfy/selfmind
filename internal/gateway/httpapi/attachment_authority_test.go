package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/gateway/api"
)

// TestAttachmentPathImportRequiresLocalAuthority pins who may hand the daemon a
// filesystem path. importAttachments stats and copies whatever Path a request
// carries; without an authority check, any caller holding the shared gateway
// token — the supported non-loopback deployment for webhook IM — could name an
// arbitrary readable file and have it copied into the person's attachment
// partition, which is then an allowed root the model reads from.
func TestAttachmentPathImportRequiresLocalAuthority(t *testing.T) {
	daemon, _, identity, _, _ := newApprovalTestServer(t)
	daemon.AttachmentsDir = t.TempDir()
	coord := daemon.coordinator()

	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	atts := []api.MessageAttachment{{Name: "private.txt", Path: outside}}

	// A request that has NOT proved local filesystem authority.
	remote := coord.importAttachments(context.Background(), identity, nil, atts)
	if len(remote) != 1 {
		t.Fatalf("attachments must be preserved, not dropped: %+v", remote)
	}
	if remote[0].Path != "" {
		t.Fatalf("a remote caller must not get a daemon-side path honored, got %q", remote[0].Path)
	}
	entries, _ := os.ReadDir(filepath.Join(daemon.AttachmentsDir, identity.PersonID))
	if len(entries) != 0 {
		t.Fatalf("nothing should have been imported for an unauthorized caller: %v", entries)
	}

	// The authenticated local CLI keeps working: its path is imported into the
	// person's own partition and rewritten to the managed copy.
	local := coord.importAttachments(withLocalFilesystemAuthority(context.Background()), identity, nil, atts)
	if len(local) != 1 || local[0].Path == "" {
		t.Fatalf("local CLI import should rewrite the path: %+v", local)
	}
	if local[0].Path == outside {
		t.Fatal("import must copy into the managed partition, not keep the original path")
	}
	if _, err := os.Stat(local[0].Path); err != nil {
		t.Fatalf("imported copy is not readable: %v", err)
	}
}
