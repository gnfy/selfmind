package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

// Per-run spool caps: a run that keeps producing huge outputs stops spooling
// (later truncations degrade to the plain head/tail note) instead of filling
// the disk. Oldest artifacts are kept — the earliest evidence of a turn is
// usually what a continuation needs.
const (
	maxToolArtifactsPerRun     = 32
	maxToolArtifactBytesPerRun = 64 << 20 // 64 MiB
)

// toolArtifactSink spools one run's over-budget tool outputs under
// <ToolOutputDir>/<personID>/<artifactID>.txt and records durable metadata as
// a control-plane task artifact. The file name IS the artifact id, so the
// read-back tool (tools.tool_output_view) resolves person-scoped paths with
// no DB round trip. Safe for concurrent use: read-only tool batches run in
// parallel.
type toolArtifactSink struct {
	dir      string // person-scoped spool dir
	store    *control.Store
	tenantID string
	taskID   string
	runID    string
	tool     string

	mu         sync.Mutex
	saved      int
	savedBytes int
}

// newToolArtifactSink returns nil when spooling is not configured; kernel
// treats a nil sink as "degrade to the plain truncation note".
func (c *RunCoordinator) newToolArtifactSink(identity *control.IdentityContext, task *control.Task, run *control.Run) kernel.ToolArtifactSink {
	d := c.srv
	if d.ToolOutputDir == "" || identity == nil || task == nil || run == nil {
		return nil
	}
	return &toolArtifactSink{
		dir:      filepath.Join(d.ToolOutputDir, identity.PersonID),
		store:    d.Control,
		tenantID: identity.TenantID,
		taskID:   task.ID,
		runID:    run.ID,
	}
}

func (s *toolArtifactSink) SaveToolOutput(ctx context.Context, toolName, content string) (kernel.ToolArtifactRef, error) {
	if len(content) == 0 {
		return kernel.ToolArtifactRef{}, fmt.Errorf("empty tool output")
	}
	s.mu.Lock()
	if s.saved >= maxToolArtifactsPerRun || s.savedBytes+len(content) > maxToolArtifactBytesPerRun {
		s.mu.Unlock()
		return kernel.ToolArtifactRef{}, fmt.Errorf("per-run tool artifact budget exhausted")
	}
	s.saved++
	s.savedBytes += len(content)
	s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return kernel.ToolArtifactRef{}, err
	}
	id := "art_" + uuid.NewString()
	path := filepath.Join(s.dir, id+".txt")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return kernel.ToolArtifactRef{}, err
	}

	sum := sha256.Sum256([]byte(content))
	metadata, _ := json.Marshal(map[string]interface{}{
		"tool":   toolName,
		"bytes":  len(content),
		"sha256": hex.EncodeToString(sum[:]),
	})
	// The row is durable metadata (resume listings, retention); the file is
	// the read path. A row failure keeps the artifact readable — log, don't
	// fail the tool call.
	if s.store != nil {
		if _, err := s.store.SaveArtifact(ctx, control.Artifact{
			ID:       id,
			TaskID:   s.taskID,
			RunID:    s.runID,
			Kind:     "tool_output",
			Name:     toolName,
			URI:      path,
			MimeType: "text/plain",
			Metadata: metadata,
		}); err != nil {
			log.Warn("tool artifact row write failed", "artifact", id, "error", err)
		}
	}
	return kernel.ToolArtifactRef{ID: id, Bytes: len(content)}, nil
}
