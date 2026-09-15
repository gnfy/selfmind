package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
)

func (h *runtimeHarness) waitForExternalWatchContinuation(ctx context.Context, parent api.MessageResponse, approvalMode string) (*control.Run, api.RunOutcome, llm.UsageStats, error) {
	if parent.Run == nil || parent.Identity == nil || parent.Task == nil || parent.Outcome == nil || parent.Outcome.Status != "waiting_external" {
		return nil, api.RunOutcome{}, llm.UsageStats{}, fmt.Errorf("external-watch eval requires a real waiting_external parent")
	}
	// Preserve the case's foreground approval mode for the isolated identity.
	// Set its durable preference so queued work resolves policy through
	// the production path too. This never writes the operator's data directory.
	if err := h.controlStore.SetPersonSetting(ctx, h.tenantID, parent.Identity.PersonID, "approval_mode", firstNonEmpty(approvalMode, "full-auto")); err != nil {
		return nil, api.RunOutcome{}, llm.UsageStats{}, err
	}
	stop := h.server.StartExternalWatchWorker(ctx)
	defer stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, api.RunOutcome{}, llm.UsageStats{}, fmt.Errorf("external-watch continuation did not settle: %w", ctx.Err())
		case <-tick.C:
			runs, err := h.controlStore.ListTaskRuns(ctx, h.tenantID, parent.Task.ID, 50)
			if err != nil {
				return nil, api.RunOutcome{}, llm.UsageStats{}, err
			}
			var child *control.Run
			for i := range runs {
				if runs[i].ResumesRunID == parent.Run.ID {
					if child != nil {
						return nil, api.RunOutcome{}, llm.UsageStats{}, fmt.Errorf("watcher produced duplicate continuations")
					}
					child = &runs[i]
				}
			}
			if child == nil || child.FinishedAt == nil {
				continue
			}
			// A durable Run can finish before its async owner returns. Wait for
			// that owner to leave before inspecting evidence or closing storage.
			// Delivery is deliberately absent in this case; its queue may remain
			// started until delivery reconciliation and is not proof of a retry.
			if h.server.ActiveRunCount() != 0 {
				continue
			}
			events, err := h.controlStore.ListTaskEvents(ctx, parent.Task.ID, 200)
			if err != nil {
				return nil, api.RunOutcome{}, llm.UsageStats{}, err
			}
			var usage llm.UsageStats
			for _, event := range events {
				if event.RunID != child.ID || event.Type != "provider.call.usage" {
					continue
				}
				var call llm.UsageStats
				if err := json.Unmarshal(event.Payload, &call); err != nil {
					return nil, api.RunOutcome{}, llm.UsageStats{}, err
				}
				usage.InputTokens += call.InputTokens
				usage.OutputTokens += call.OutputTokens
			}
			for _, event := range events {
				if event.RunID != child.ID || event.Type != "run.finished" {
					continue
				}
				var payload struct {
					Outcome api.RunOutcome `json:"outcome"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return nil, api.RunOutcome{}, llm.UsageStats{}, err
				}
				if payload.Outcome.Status == "" {
					return nil, api.RunOutcome{}, llm.UsageStats{}, fmt.Errorf("continuation lacks a structured outcome")
				}
				return child, payload.Outcome, usage, nil
			}
		}
	}
}
