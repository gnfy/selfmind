package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// toolOutputViewChunkBytes bounds one read so the tool can never re-inflate
// the context it exists to save; it matches the kernel model-surface budget.
const toolOutputViewChunkBytes = 24000

// toolOutputArtifactIDPattern accepts only the gateway-issued artifact id
// shape. It doubles as path-traversal defense: the id IS the on-disk file
// name, so nothing outside this alphabet may reach the filesystem join.
var toolOutputArtifactIDPattern = regexp.MustCompile(`^art_[A-Za-z0-9_-]{8,64}$`)

// ToolOutputViewTool reads back a spooled large tool output by reference
// (docs/execution-quality-plan.zh-CN.md W1). The gateway sink writes each
// over-budget output to <baseDir>/<personID>/<artifactID>.txt; resolution is
// pure filesystem — no control-plane round trip — and person-scoped: a run
// can only read artifacts spooled under its own person partition.
type ToolOutputViewTool struct {
	BaseTool
	baseDir string
}

func NewToolOutputViewTool(baseDir string) *ToolOutputViewTool {
	return &ToolOutputViewTool{
		BaseTool: BaseTool{
			name:        "tool_output_view",
			description: "Read a byte range of a previously truncated tool output by artifact id (from a '[SelfMind note: ... saved as artifact art_...]' truncation note). Use this to inspect omitted middle content instead of re-running the command.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"artifact_id": {
						Type:        "string",
						Description: "Artifact id from the truncation note, e.g. art_1a2b3c4d.",
					},
					"offset_bytes": {
						Type:        "integer",
						Description: "Byte offset to start reading from (default 0).",
					},
					"limit_bytes": {
						Type:        "integer",
						Description: "Bytes to read, max 24000 per call (default 24000).",
					},
				},
				Required: []string{"artifact_id"},
			},
		},
		baseDir: strings.TrimSpace(baseDir),
	}
}

func (t *ToolOutputViewTool) Execute(args map[string]interface{}) (string, error) {
	if t.baseDir == "" {
		return "", fmt.Errorf("tool output artifacts are not configured on this daemon")
	}
	artifactID, _ := args["artifact_id"].(string)
	artifactID = strings.TrimSpace(artifactID)
	if !toolOutputArtifactIDPattern.MatchString(artifactID) {
		return "", fmt.Errorf("invalid artifact id %q: expected an id like art_1a2b3c4d from a truncation note", artifactID)
	}

	personID := ""
	if scope, ok := currentExecutionScopeAny(args); ok && strings.TrimSpace(scope.PersonID) != "" {
		personID = strings.TrimSpace(scope.PersonID)
	} else if tenant, _ := args["_tenant_id"].(string); strings.TrimSpace(tenant) != "" {
		// Daemon runs partition storage by person id, which is also the
		// agent's storage tenant — the fallback covers dispatch paths that
		// carry no execution scope.
		personID = strings.TrimSpace(tenant)
	}
	if personID == "" {
		return "", fmt.Errorf("no person scope available to resolve tool output artifacts")
	}

	path := filepath.Join(t.baseDir, personID, artifactID+".txt")
	base, err := filepath.Abs(t.baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve artifact dir: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact path escapes the artifact store")
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("artifact %s not found in this person's partition (it may belong to another person, or was cleaned up)", artifactID)
	}
	total := info.Size()

	offset := intArg(args, "offset_bytes", 0)
	if offset < 0 {
		offset = 0
	}
	limit := intArg(args, "limit_bytes", toolOutputViewChunkBytes)
	if limit <= 0 || limit > toolOutputViewChunkBytes {
		limit = toolOutputViewChunkBytes
	}
	if int64(offset) >= total {
		return fmt.Sprintf("artifact %s: offset %d is beyond the end of the %d-byte output; nothing to read", artifactID, offset, total), nil
	}

	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, int64(offset))
	if err != nil && n == 0 {
		return "", fmt.Errorf("read artifact: %w", err)
	}
	end := int64(offset) + int64(n)
	header := fmt.Sprintf("artifact %s bytes %d..%d of %d", artifactID, offset, end, total)
	if end < total {
		header += fmt.Sprintf(" (call again with offset_bytes=%d for the next chunk)", end)
	}
	return header + "\n" + string(buf[:n]), nil
}
