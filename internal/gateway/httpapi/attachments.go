package httpapi

import (
	"context"
	"fmt"
	"github.com/google/uuid"
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
func (c *RunCoordinator) importAttachments(ctx context.Context, identity *control.IdentityContext, run *control.Run, atts []api.MessageAttachment) []api.MessageAttachment {
	if len(atts) == 0 || c == nil || c.srv == nil || c.srv.AttachmentsDir == "" || identity == nil {
		return atts
	}
	// A path is either something the daemon ALREADY OWNS, or a claim about its
	// filesystem that the caller must be entitled to make.
	//
	// Only a request that proved local filesystem authority may make the claim:
	// otherwise any caller holding the shared gateway token — the supported
	// non-loopback deployment for webhook IM — could name an arbitrary readable
	// file and have it copied into the person's partition, which is then an
	// allowed root the model reads from. Platform is self-declared and cannot
	// gate this; the local control token, compared in constant time against a
	// loopback peer, is what establishes that authority. Remote callers are not
	// blocked from attaching — they upload the bytes.
	//
	// Ownership is checked per attachment rather than per request because the
	// two questions are asked at different times. An async turn runs on a fresh
	// context and a drained queue item runs long after acceptance, so neither
	// carries the marker; judging the request would strip files that were
	// admitted properly and already live in the person's own partition.
	owned := c.personAttachmentsRoot(identity)
	authorized := hasLocalFilesystemAuthority(ctx)
	// One directory per acceptance. A shared bucket let two requests that both
	// attached "diagram.png" write 01-diagram.png to the same place, and the
	// second silently replaced the contents of the first — which a queued row
	// still referenced by path.
	sub := ""
	if run != nil {
		sub = strings.TrimSpace(run.ID)
	}
	if sub == "" {
		sub = "accept-" + uuid.NewString()
	}
	dir := filepath.Join(c.srv.AttachmentsDir, identity.PersonID, sub)
	out := make([]api.MessageAttachment, 0, len(atts))
	imported := 0
	for _, att := range atts {
		if att.Path == "" || imported >= maxImportAttachments {
			out = append(out, att)
			continue
		}
		if pathWithin(owned, att.Path) {
			// Already imported, on an earlier turn or before it was queued.
			// Re-copying would duplicate bytes and rename the file out from
			// under a durable reference that points at it.
			out = append(out, att)
			continue
		}
		if !authorized {
			att.Path = ""
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

// pathWithin reports whether path lives inside root. Both are cleaned and
// compared as absolute paths, so ".." cannot walk out of the partition and a
// sibling directory sharing a name prefix is not mistaken for a child.
func pathWithin(root, path string) bool {
	root = strings.TrimSpace(root)
	path = strings.TrimSpace(path)
	if root == "" || path == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// attachmentRefsFromAPI converts admitted attachments into the durable shape.
// Only managed copies reach here: importAttachments has already decided what a
// caller-supplied path is allowed to mean.
func attachmentRefsFromAPI(atts []api.MessageAttachment) []control.AttachmentRef {
	if len(atts) == 0 {
		return nil
	}
	refs := make([]control.AttachmentRef, 0, len(atts))
	for _, att := range atts {
		refs = append(refs, control.AttachmentRef{
			Kind: att.Kind, Path: att.Path, MimeType: att.MimeType,
			Name: att.Name, Size: att.Size,
		})
	}
	return refs
}

// attachmentsFromRefs restores durable attachments for a turn that resumes.
func attachmentsFromRefs(refs []control.AttachmentRef) []api.MessageAttachment {
	if len(refs) == 0 {
		return nil
	}
	atts := make([]api.MessageAttachment, 0, len(refs))
	for _, ref := range refs {
		atts = append(atts, api.MessageAttachment{
			Kind: ref.Kind, Path: ref.Path, MimeType: ref.MimeType,
			Name: ref.Name, Size: ref.Size,
		})
	}
	return atts
}

// renderAttachmentBlock is the ONE description the model gets of attached
// files. Steering used to hand the kernel bare text, so guidance that carried a
// file was persisted with it and delivered without it: the model was never told
// the file existed. Both the opening turn and mid-turn guidance render through
// here so they cannot drift into describing the same thing two ways.
func renderAttachmentBlock(attachments []api.MessageAttachment) string {
	if len(attachments) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("attachments:\n")
	for i, att := range attachments {
		fmt.Fprintf(&sb, "- index: %d\n", i+1)
		if att.Kind != "" {
			fmt.Fprintf(&sb, "  kind: %s\n", att.Kind)
		}
		if att.Path != "" {
			fmt.Fprintf(&sb, "  path: %s\n", att.Path)
		}
		if att.MimeType != "" {
			fmt.Fprintf(&sb, "  mime_type: %s\n", att.MimeType)
		}
		if att.Name != "" {
			fmt.Fprintf(&sb, "  name: %s\n", att.Name)
		}
		if att.Size > 0 {
			fmt.Fprintf(&sb, "  size: %d\n", att.Size)
		}
	}
	sb.WriteString("When useful, inspect attachment paths with local tools before answering.\n")
	return sb.String()
}

// steeringContentWithAttachments is what the kernel folds into the conversation.
// The durable record keeps the person's words and their hash unchanged, so
// dedup still works on what they actually typed.
func steeringContentWithAttachments(text string, attachments []api.MessageAttachment) string {
	block := renderAttachmentBlock(attachments)
	if block == "" {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return block
	}
	return text + "\n\n" + block
}
