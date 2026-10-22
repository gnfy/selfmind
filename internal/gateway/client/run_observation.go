package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/gateway/api"
)

func observedRunEvent(event api.RunEvent) (api.RunObservation, error) {
	var payload struct {
		Outcome     api.RunOutcome `json:"outcome"`
		FinalAnswer string         `json:"final_answer"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return api.RunObservation{RunID: event.RunID}, fmt.Errorf("Run outcome could not be decoded: %w", err)
	}
	status := strings.TrimSpace(payload.Outcome.Status)
	if status == "" {
		return api.RunObservation{RunID: event.RunID}, fmt.Errorf("Run outcome has no recorded state")
	}
	return api.RunObservation{RunID: event.RunID, Status: status, Summary: payload.Outcome.Summary, FinalAnswer: payload.FinalAnswer}, nil
}

// Absence from the active registry means observation must use durable state.
// The view follows only committed exact continuation edges, so a parent that
// parked while its child started is still observed as work in progress.
func (c *Client) observedRunState(ctx context.Context, runID string) (api.RunObservation, error) {
	if runID == "" {
		return api.RunObservation{}, nil // legacy unscoped observer has no outcome
	}
	view, err := c.ViewRun(ctx, runID)
	if err != nil {
		return api.RunObservation{RunID: runID}, err
	}
	if view.RunID == "" || view.Status == "" {
		return api.RunObservation{RunID: runID}, fmt.Errorf("Run state was not available")
	}
	return api.RunObservation{RunID: view.RunID, Status: view.Status, Summary: view.Summary}, nil
}
