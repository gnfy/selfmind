package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

const ExternalWatchAdapterStatusJSON = "status_json.v1"

type ExternalWatchObservationState string

const (
	ExternalWatchObservationPending   ExternalWatchObservationState = "pending"
	ExternalWatchObservationSucceeded ExternalWatchObservationState = "succeeded"
	ExternalWatchObservationFailed    ExternalWatchObservationState = "failed"
)

// ClassifyExternalWatchObservation is the registry-owned adapter seam for
// typed durable observations. Gateway scheduling consumes only the three-state
// result; provider-specific output grammar stays in this package.
func ClassifyExternalWatchObservation(adapter, output string) (ExternalWatchObservationState, error) {
	switch strings.ToLower(strings.TrimSpace(adapter)) {
	case ExternalWatchAdapterStatusJSON:
		var payload struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &payload); err != nil {
			return "", fmt.Errorf("%s observation is not valid JSON: %w", ExternalWatchAdapterStatusJSON, err)
		}
		switch strings.ToLower(strings.TrimSpace(payload.Status)) {
		case string(ExternalWatchObservationPending):
			return ExternalWatchObservationPending, nil
		case string(ExternalWatchObservationSucceeded):
			return ExternalWatchObservationSucceeded, nil
		case string(ExternalWatchObservationFailed):
			return ExternalWatchObservationFailed, nil
		default:
			return "", fmt.Errorf("%s requires status pending, succeeded, or failed", ExternalWatchAdapterStatusJSON)
		}
	default:
		return "", fmt.Errorf("unknown external watch observation adapter %q", adapter)
	}
}
