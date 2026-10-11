package control

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ResumeSelection binds one endpoint's next agent input to an exact Run.
// Legacy person-wide pins remain display compatibility, never input authority.
type ResumeSelection struct {
	Version  int    `json:"version"`
	ThreadID string `json:"thread_id"`
	RunID    string `json:"run_id"`
}

func resumeSelectionKey(channel string) string {
	return "resume_selection.v1." + base64.RawURLEncoding.EncodeToString([]byte(strings.TrimSpace(channel)))
}

func (s *Store) PinSessionResumeSelection(ctx context.Context, tenantID, personID, channel, threadID, runID string) error {
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("resume selection requires an endpoint session")
	}
	value, err := json.Marshal(ResumeSelection{Version: 1, ThreadID: threadID, RunID: runID})
	if err != nil {
		return err
	}
	return s.pinResumeSettings(ctx, tenantID, personID, threadID, runID, map[string]string{resumeSelectionKey(channel): string(value)})
}

func decodeResumeSelection(value string) (*ResumeSelection, error) {
	if value == "" {
		return nil, nil
	}
	var selected ResumeSelection
	if err := json.Unmarshal([]byte(value), &selected); err != nil || selected.Version != 1 || selected.RunID == "" || selected.ThreadID == "" {
		return nil, fmt.Errorf("resume selection is invalid or unsupported; select the Run again")
	}
	return &selected, nil
}

func (s *Store) SessionResumeSelection(ctx context.Context, tenantID, personID, channel string) (*ResumeSelection, error) {
	if strings.TrimSpace(channel) == "" {
		return nil, nil
	}
	value, err := s.GetPersonSetting(ctx, tenantID, personID, resumeSelectionKey(channel))
	if err != nil {
		return nil, err
	}
	return decodeResumeSelection(value)
}

func (s *Store) TakeSessionResumeSelection(ctx context.Context, tenantID, personID, channel string) (*ResumeSelection, error) {
	if strings.TrimSpace(channel) == "" {
		return nil, nil
	}
	var value string
	err := s.db.QueryRowContext(ctx, `DELETE FROM person_settings
		WHERE tenant_id = ? AND person_id = ? AND key = ? RETURNING value`, normalizeTenant(tenantID), personID, resumeSelectionKey(channel)).Scan(&value)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeResumeSelection(value)
}

func clearRunResumeSelectionsTx(ctx context.Context, tx *sql.Tx, tenantID, personID, runID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM person_settings WHERE tenant_id=? AND person_id=?
		AND substr(key,1,20)='resume_selection.v1.'
		AND CASE WHEN json_valid(value) THEN json_extract(value,'$.run_id') END=?`, tenantID, personID, runID)
	return err
}

func (s *Store) ClearRunResumeSelections(ctx context.Context, tenantID, personID, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := clearRunResumeSelectionsTx(ctx, tx, normalizeTenant(tenantID), personID, runID); err != nil {
		return err
	}
	return tx.Commit()
}
