package kernel

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"selfmind/internal/kernel/llm"
)

var flightTurnCounter atomic.Int64

// beginFlightRecording tags the turn context with a VCR session (so the
// flight-recorder-wrapped provider records this turn's model calls into its own
// directory) and returns a finalize func that writes the turn's metadata. It is
// a no-op (returns nil) when the flight recorder is off or the channel is an
// internal/background one — only real user-facing turns are recorded so they can
// be promoted into eval cases via `selfmind eval capture`.
func (a *Agent) beginFlightRecording(ctx *context.Context, tenantID, channel, prompt string) func(string, error) {
	if ctx == nil || llm.EvalVCRActive() || !llm.FlightEnabled() || isInternalChannel(channel) {
		return nil
	}
	turnID := newFlightTurnID()
	*ctx = llm.WithVCRSession(*ctx, turnID)
	return func(output string, _ error) {
		_ = llm.WriteFlightMeta(llm.FlightMeta{
			TurnID:    turnID,
			TenantID:  tenantID,
			Channel:   channel,
			Prompt:    prompt,
			Output:    output,
			CreatedAt: time.Now().Format(time.RFC3339),
		})
	}
}

// isInternalChannel excludes delegation/background/system turns from flight
// recording (channels like "cli:background_review" carry a ':').
func isInternalChannel(channel string) bool {
	switch channel {
	case "", "system", "delegation":
		return true
	}
	return strings.Contains(channel, ":")
}

func newFlightTurnID() string {
	return fmt.Sprintf("flight-%d-%d", time.Now().UnixNano(), flightTurnCounter.Add(1))
}
