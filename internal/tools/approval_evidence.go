package tools

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// AuthorizationEvidence is a server-selected quotation from an exact owned
// resume lineage. It informs the judge; it is never a capability or grant.
type AuthorizationEvidence struct {
	RunID         string `json:"run_id"`
	UserText      string `json:"user_text"`
	AcceptedOffer string `json:"accepted_offer,omitempty"`
}

const MaxAuthorizationEvidence = 6

// A model decision is reusable evidence about one action, never an approval
// class. User steering and environment changes invalidate it without parsing
// the user's new words into another rule taxonomy.
func triageDecisionKey(fingerprint string, scope ExecutionScope, intent RunIntentSnapshot, mode ApprovalMode) string {
	if fingerprint == "" {
		return ""
	}
	raw, err := json.Marshal(struct {
		Action, Run, Environment string
		Generation               int64
		Intent                   RunIntentSnapshot
		Mode                     ApprovalMode
		Roots                    []string
	}{fingerprint, scope.RunID, scope.EnvironmentSnapshotID, int64(scope.EnvironmentGeneration), intent, mode, scope.AllowedRoots})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("triage:exact:%x", sum[:])
}
