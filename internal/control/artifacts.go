package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *Store) SaveArtifact(ctx context.Context, artifact Artifact) (*Artifact, error) {
	if artifact.TaskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	artifact.URI = strings.TrimSpace(artifact.URI)
	if artifact.URI == "" {
		return nil, fmt.Errorf("artifact uri is required")
	}
	artifact.Kind = normalizeName(artifact.Kind, "artifact")
	if artifact.ID == "" {
		artifact.ID = "art_" + uuid.NewString()
	}
	if len(artifact.Metadata) == 0 {
		artifact.Metadata = json.RawMessage(`{}`)
	}
	artifact.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_artifacts
		   (id, thread_id, run_id, kind, name, uri, mime_type, metadata_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.ID, artifact.TaskID, artifact.RunID, artifact.Kind, artifact.Name, artifact.URI,
		artifact.MimeType, string(artifact.Metadata), artifact.CreatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return &artifact, nil
}

func (s *Store) ListTaskArtifacts(ctx context.Context, taskID string, limit int) ([]Artifact, error) {
	if taskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, thread_id, COALESCE(run_id, ''), kind, COALESCE(name, ''),
		        uri, COALESCE(mime_type, ''), COALESCE(metadata_json, '{}'), created_at
		 FROM task_artifacts WHERE thread_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Artifact
	for rows.Next() {
		var item Artifact
		var metadata string
		var created int64
		if err := rows.Scan(&item.ID, &item.TaskID, &item.RunID, &item.Kind, &item.Name,
			&item.URI, &item.MimeType, &metadata, &created); err != nil {
			return nil, err
		}
		item.Metadata = json.RawMessage(metadata)
		item.CreatedAt = time.Unix(created, 0)
		out = append(out, item)
	}
	return out, rows.Err()
}
