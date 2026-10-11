package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ProviderWaitRetryCounts counts the unbroken automatic wait sequence before
// this Run. The durable pre-request checkpoint advances only after an agent
// iteration, so an iteration above zero starts a new sequence. A user-created
// continuation has no matching provider queue binding and starts one as well.
func (s *Store) ProviderWaitRetryCounts(ctx context.Context, tenantID, personID, runID string) (capacity, rate int, err error) {
	for hop := 0; hop < resumeChainMaxHops && runID != ""; hop++ {
		run, readErr := s.GetRun(ctx, tenantID, runID)
		if readErr != nil {
			return 0, 0, readErr
		}
		if run == nil || run.PersonID != personID {
			return 0, 0, fmt.Errorf("provider wait retry owner is unavailable")
		}
		checkpoint, readErr := s.IncompleteLoopCheckpointForRun(ctx, tenantID, runID)
		if readErr != nil {
			return 0, 0, readErr
		}
		if checkpoint != nil && checkpoint.Detail == "provider_request" && checkpoint.Iteration > 0 {
			break
		}
		parent := run.ResumesRunID
		if parent == "" {
			break
		}
		q, readErr := s.GetQueuedByIdempotencyKey(ctx, tenantID, "provider-wait:"+parent)
		if readErr != nil {
			return 0, 0, readErr
		}
		if q == nil || q.PersonID != personID || q.TaskID != run.TaskID || q.ReplyToRunID != parent || q.RunID != runID {
			break
		}
		var payload []byte
		readErr = s.db.QueryRowContext(ctx, `SELECT payload_json FROM task_events WHERE run_id=? AND idempotency_key=?`, parent, "run:"+parent+":terminal").Scan(&payload)
		if readErr == sql.ErrNoRows {
			break
		}
		if readErr != nil {
			return 0, 0, readErr
		}
		var terminal struct {
			ProviderWait struct {
				Reason string `json:"reason"`
			} `json:"provider_wait"`
		}
		if err := json.Unmarshal(payload, &terminal); err != nil {
			return 0, 0, fmt.Errorf("read provider wait result: %w", err)
		}
		switch terminal.ProviderWait.Reason {
		case "capacity":
			capacity++
		case "rate_limit":
			rate++
		default:
			return capacity, rate, nil
		}
		runID = parent
	}
	return capacity, rate, nil
}
