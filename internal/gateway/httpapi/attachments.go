package httpapi

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

const (
	// maxImportAttachments bounds how many files one message may import.
	maxImportAttachments = 8
	// maxImportAttachmentSize bounds one imported file (20 MiB).
	maxImportAttachmentSize = 20 << 20
)

// importAttachments copies message attachments (e.g. a TUI clipboard paste
// living in the OS temp dir) into the daemon-managed, person-partitioned
// attachment store and rewrites each attachment's Path to the managed copy.
// This is the sanctioned way an out-of-workspace file enters a run: the
// person's attachment root is added to the run's ExecutionScope AllowedRoots
// (installExecutionScope), while arbitrary external locations stay unreadable.
// It also future-proofs the B topology — a client-local temp path means
// nothing to a remote daemon, but an uploaded-and-imported copy does.
//
// Degradation, never loss: an unreadable/oversized/over-count file keeps its
// original path (the agent then reports the scope error verbatim), and an
// empty AttachmentsDir disables importing entirely.
func (c *RunCoordinator) importAttachments(identity *control.IdentityContext, run *control.Run, atts []api.MessageAttachment) []api.MessageAttachment {
	if len(atts) == 0 || c == nil || c.srv == nil || c.srv.AttachmentsDir == "" || identity == nil {
		return atts
	}
	sub := "misc"
	if run != nil && run.ID != "" {
		sub = run.ID
	}
	dir := filepath.Join(c.srv.AttachmentsDir, identity.PersonID, sub)
	out := make([]api.MessageAttachment, 0, len(atts))
	imported := 0
	for _, att := range atts {
		if att.Path == "" || imported >= maxImportAttachments {
			out = append(out, att)
			continue
		}
		info, err := os.Stat(att.Path)
		if err != nil || info.IsDir() || info.Size() > maxImportAttachmentSize {
			out = append(out, att)
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			out = append(out, att)
			continue
		}
		dst := filepath.Join(dir, fmt.Sprintf("%02d-%s", imported+1, sanitizeAttachmentName(att.Name, att.Path)))
		if err := copyAttachmentFile(att.Path, dst); err != nil {
			out = append(out, att)
			continue
		}
		att.Path = dst
		if att.Size == 0 {
			att.Size = info.Size()
		}
		imported++
		out = append(out, att)
	}
	return out
}

// personAttachmentsRoot is the scope root covering the person's imported
// attachments. Added to AllowedRoots only for requests that carry attachments,
// and only ever the requester's own partition.
func (c *RunCoordinator) personAttachmentsRoot(identity *control.IdentityContext) string {
	if c == nil || c.srv == nil || c.srv.AttachmentsDir == "" || identity == nil {
		return ""
	}
	return filepath.Join(c.srv.AttachmentsDir, identity.PersonID)
}

// sanitizeAttachmentName reduces a client-supplied name to a safe flat file
// name (no separators, no shell-hostile runes); falls back to the path base.
func sanitizeAttachmentName(name, path string) string {
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(path)
	}
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := strings.Trim(b.String(), ".")
	if s == "" {
		s = "attachment"
	}
	return s
}

func copyAttachmentFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
