package httpapi

import (
	"context"
	"encoding/json"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

func (c *RunCoordinator) projectCommandObservations(ctx context.Context, tenant, run string, evidence []kernel.RunEvidence, out *api.VerificationOutcome) error {
	var legacyIDs []string
	for _, e := range evidence {
		if e.Kind == "command" && e.Command != nil && e.Process == nil && e.Invoked == nil && e.EffectState == "" && e.ToolCallID != "" {
			legacyIDs = append(legacyIDs, e.ToolCallID)
		}
	}
	var legacy map[string]json.RawMessage
	if len(legacyIDs) > 0 {
		var err error
		legacy, err = c.srv.Control.RunToolDispatchFacts(ctx, tenant, run, legacyIDs)
		if err != nil {
			return err
		}
	}
	for _, e := range evidence {
		if e.Kind != "command" || e.Command == nil || e.StartedAt < out.LatestMutationAt {
			continue
		}
		out.OrdinaryCommandAttempts++
		if e.Process == nil && e.Invoked == nil && e.EffectState == "" {
			var facts struct {
				Invoked     *bool                     `json:"invoked"`
				Process     *kernel.ToolProcessResult `json:"process"`
				EffectState string                    `json:"effect_state"`
			}
			if json.Unmarshal(legacy[e.ToolCallID], &facts) == nil {
				e.Invoked, e.Process, e.EffectState = facts.Invoked, facts.Process, facts.EffectState
			}
		}
		switch {
		case e.Process != nil && e.Process.Started:
			out.OrdinaryCommands++
			if e.Process.ExitCode == nil {
				out.OrdinaryCommandExitUnknown++
			} else if *e.Process.ExitCode != 0 {
				out.OrdinaryCommandFailures++
			}
		case e.Process != nil || (e.Invoked != nil && !*e.Invoked) || e.EffectState == "not_dispatched":
			out.OrdinaryCommandNotDispatched++
		default:
			out.OrdinaryCommandDispatchUnknown++
		}
	}
	return nil
}
