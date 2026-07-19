package httpapi

import (
	"context"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

// controlToolLedger is the control-backed implementation of the kernel's
// per-run tool ledger (Loop Engineering P0-B). A durable claim is a
// correctness boundary for state-changing tools; storage failures are
// returned to the kernel instead of being swallowed. The safety property holds
// as long as the
// dispatch row lands before execution — a lost OUTCOME write merely leaves an
// entry that recovery conservatively treats as uncertain, which is the safe
// direction.
type controlToolLedger struct {
	store    *control.Store
	tenantID string
}

func (c *RunCoordinator) newToolLedger(identity *control.IdentityContext) kernel.ToolLedger {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
		return nil
	}
	return &controlToolLedger{store: c.srv.Control, tenantID: identity.TenantID}
}

func (l *controlToolLedger) ClaimDispatch(ctx context.Context, entry kernel.ToolLedgerEntry) (kernel.ToolDispatchDecision, error) {
	if l == nil || l.store == nil {
		return kernel.ToolDispatchDecision{}, fmt.Errorf("tool ledger is unavailable")
	}
	claim, err := l.store.ClaimToolDispatch(ctx, l.tenantID, control.ToolLedgerEntry{
		RunID:      entry.RunID,
		ToolCallID: entry.ToolCallID,
		ToolName:   entry.ToolName,
		ArgsHash:   entry.ArgsHash,
		RetryClass: string(entry.RetryClass),
	})
	if err != nil {
		log.Warn("tool ledger dispatch record failed", "run_id", entry.RunID, "tool", entry.ToolName, "error", err)
		return kernel.ToolDispatchDecision{}, err
	}
	return kernel.ToolDispatchDecision{Execute: claim.Execute, Status: claim.Status}, nil
}

func (l *controlToolLedger) RecordOutcome(ctx context.Context, runID, toolCallID string, ok bool) error {
	if l == nil || l.store == nil {
		return fmt.Errorf("tool ledger is unavailable")
	}
	if err := l.store.RecordToolOutcome(ctx, l.tenantID, runID, toolCallID, ok); err != nil {
		log.Warn("tool ledger outcome record failed", "run_id", runID, "error", err)
		return err
	}
	return nil
}
