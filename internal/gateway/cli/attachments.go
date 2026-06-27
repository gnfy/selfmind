package cli

import (
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/gateway/api"
)

// imageExtensions are recognized when a user pastes/drags an image path into the
// composer. Terminals deliver dragged files as paths (and SSH users can type a
// path), so path detection is the cross-environment way to "paste an image".
var imageExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

// imageAttachmentsFromInput scans submitted CLI input for tokens that are
// existing local image files and returns them as attachments. The backend
// renders attachment paths into the turn and the agent inspects them with the
// vision tool, so no inline multimodal plumbing is required. The original text
// is left intact (the path is still useful context). Returns nil when none are
// found, so non-image turns are unaffected.
func imageAttachmentsFromInput(input, cwd string) []api.MessageAttachment {
	seen := map[string]bool{}
	var out []api.MessageAttachment
	// Consider both each whitespace token and the whole trimmed line (a dragged
	// path may contain spaces and arrive as the entire input).
	candidates := append(tokenizeForPaths(input), strings.TrimSpace(input))
	for _, raw := range candidates {
		path, mime, ok := resolveImagePath(raw, cwd)
		if !ok || seen[path] {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		seen[path] = true
		out = append(out, api.MessageAttachment{
			Kind:     "image",
			Path:     path,
			MimeType: mime,
			Name:     filepath.Base(path),
			Size:     info.Size(),
		})
	}
	return out
}

func tokenizeForPaths(input string) []string {
	return strings.FieldsFunc(input, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' })
}

// resolveImagePath normalizes a token (quotes, file:// scheme, ~ and relative
// paths) and reports the absolute path + mime type if it looks like an image.
func resolveImagePath(raw, cwd string) (string, string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, `"'`)
	s = strings.TrimPrefix(s, "file://")
	if s == "" {
		return "", "", false
	}
	mime, ok := imageExtensions[strings.ToLower(filepath.Ext(s))]
	if !ok {
		return "", "", false
	}
	if strings.HasPrefix(s, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			s = filepath.Join(home, s[2:])
		}
	}
	if !filepath.IsAbs(s) && cwd != "" {
		s = filepath.Join(cwd, s)
	}
	return filepath.Clean(s), mime, true
}
