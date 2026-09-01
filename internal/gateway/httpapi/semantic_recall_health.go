package httpapi

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

const (
	semanticRecallDegradedNotice  = "Background degraded: semantic_recall is retrying; other maintenance remains available."
	semanticRecallRecoveredNotice = "Background recovered: semantic_recall is available again."
)

type SemanticExpansionAttempt interface {
	ExpandWithError(context.Context, string) (string, error)
}

// SemanticRecallHealthOptions wires the one role that participates in the
// foreground recall selector but must fail open to deterministic lexical
// search. Probe and Record keep provider mechanics and durable model state in
// their owning packages.
type SemanticRecallHealthOptions struct {
	Expander SemanticExpansionAttempt
	Initial  modelchange.RouteReadiness
	Probe    func(context.Context) modelchange.ProbeResult
	Record   func(modelchange.ProbeResult) error

	RetryInitial time.Duration
	RetryMax     time.Duration
	// RouteLabel is safe provider/model metadata shown only in transition
	// notices so a person can verify which configured role actually failed.
	RouteLabel string
}

// SemanticRecallHealth gates expansion, retries transient failures with
// bounded backoff, and persists recovery. It never substitutes auxiliary for
// an explicit semantic_recall route.
type SemanticRecallHealth struct {
	expander SemanticExpansionAttempt
	probe    func(context.Context) modelchange.ProbeResult
	record   func(modelchange.ProbeResult) error

	mu               sync.Mutex
	ready            bool
	fatal            bool
	degradedNotified bool
	retryInitial     time.Duration
	retryMax         time.Duration
	retryDelay       time.Duration
	retryScheduled   bool
	retryCh          chan time.Duration
	notify           func(string, bool)
	routeLabel       string
}

func NewSemanticRecallHealth(opts SemanticRecallHealthOptions) *SemanticRecallHealth {
	if opts.Expander == nil {
		return nil
	}
	initial := opts.RetryInitial
	if initial <= 0 {
		initial = 30 * time.Second
	}
	maximum := opts.RetryMax
	if maximum < initial {
		maximum = 15 * time.Minute
	}
	return &SemanticRecallHealth{
		expander: opts.Expander, probe: opts.Probe, record: opts.Record,
		ready: opts.Initial.Ready, fatal: !opts.Initial.Ready && opts.Initial.FailureClass == modelchange.FailureModel,
		retryInitial: initial, retryMax: maximum, retryDelay: initial,
		retryCh: make(chan time.Duration, 1), routeLabel: strings.TrimSpace(opts.RouteLabel),
	}
}

func (h *SemanticRecallHealth) SetNotifier(notify func(string, bool)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.notify = notify
	h.mu.Unlock()
}

// Expand implements RecallQueryExpander. An unhealthy role returns the raw
// query immediately; a live failure transitions the role before doing the same.
func (h *SemanticRecallHealth) Expand(ctx context.Context, query string) string {
	if h == nil || h.expander == nil {
		return query
	}
	h.mu.Lock()
	ready := h.ready
	h.mu.Unlock()
	if !ready {
		return query
	}
	expanded, err := h.expander.ExpandWithError(ctx, query)
	if err != nil {
		h.markFailure(semanticRecallProbeResult(err))
		return query
	}
	return expanded
}

func (h *SemanticRecallHealth) Ready() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ready
}

// Start begins the bounded recovery loop. Fatal configuration/auth/model
// failures remain parked for human repair; transient failures retry normally.
func (h *SemanticRecallHealth) Start(ctx context.Context) func() {
	if h == nil {
		return func() {}
	}
	workerCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	var notify func(string, bool)
	emitDegraded := false
	if !h.ready {
		notify, emitDegraded = h.claimDegradedNoticeLocked()
		if !h.fatal {
			h.scheduleRetryLocked(h.retryDelay)
		}
	}
	h.mu.Unlock()
	if emitDegraded && notify != nil {
		notify(h.notice(semanticRecallDegradedNotice), false)
	}
	go h.run(workerCtx)
	return cancel
}

func (h *SemanticRecallHealth) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case delay := <-h.retryCh:
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
			h.mu.Lock()
			h.retryScheduled = false
			ready, fatal := h.ready, h.fatal
			h.mu.Unlock()
			if ready || fatal || h.probe == nil {
				continue
			}
			result := h.probe(ctx)
			result.Route = modelchange.RouteSemanticRecall
			if result.OK {
				h.markSuccess(result)
			} else {
				h.markFailure(result)
			}
		}
	}
}

func (h *SemanticRecallHealth) markSuccess(result modelchange.ProbeResult) {
	result.Route = modelchange.RouteSemanticRecall
	result.OK = true
	if h.record != nil {
		if err := h.record(result); err != nil {
			log.Warn("semantic_recall: persist recovery failed", "error", err)
			return
		}
	}
	h.mu.Lock()
	wasReady := h.ready
	h.ready = true
	h.fatal = false
	h.retryDelay = h.retryInitial
	notify := h.notify
	shouldNotify := !wasReady && h.degradedNotified
	if shouldNotify {
		h.degradedNotified = false
	}
	h.mu.Unlock()
	if shouldNotify && notify != nil {
		notify(h.notice(semanticRecallRecoveredNotice), true)
	}
}

func (h *SemanticRecallHealth) markFailure(result modelchange.ProbeResult) {
	result.Route = modelchange.RouteSemanticRecall
	result.OK = false
	if strings.TrimSpace(result.Error) == "" {
		result.Error = "semantic_recall probe failed"
	}
	if result.FailureClass == "" {
		result.FailureClass = modelchange.FailureInfrastructure
	}
	if h.record != nil {
		if err := h.record(result); err != nil {
			log.Warn("semantic_recall: persist degraded state failed", "error", err)
		}
	}
	h.mu.Lock()
	wasReady := h.ready
	h.ready = false
	h.fatal = result.FailureClass == modelchange.FailureModel
	var notify func(string, bool)
	emitDegraded := false
	// One live transient failure immediately disables semantic expansion and
	// starts recovery, but does not flash a degraded/recovered pair in the UI.
	// Notify only if the retry also fails. A degraded state loaded at startup is
	// still announced immediately by Start.
	if !wasReady || h.fatal {
		notify, emitDegraded = h.claimDegradedNoticeLocked()
	}
	if !h.fatal {
		delay := h.retryDelay
		h.retryDelay *= 2
		if h.retryDelay > h.retryMax {
			h.retryDelay = h.retryMax
		}
		h.scheduleRetryLocked(delay)
	}
	h.mu.Unlock()
	if emitDegraded && notify != nil {
		notify(h.notice(semanticRecallDegradedNotice), false)
	}
}

func (h *SemanticRecallHealth) notice(base string) string {
	if h == nil || strings.TrimSpace(h.routeLabel) == "" {
		return base
	}
	return strings.Replace(base, "semantic_recall", "semantic_recall ("+h.routeLabel+")", 1)
}

func (h *SemanticRecallHealth) claimDegradedNoticeLocked() (func(string, bool), bool) {
	if h.degradedNotified {
		return nil, false
	}
	h.degradedNotified = true
	return h.notify, true
}

func (h *SemanticRecallHealth) scheduleRetryLocked(delay time.Duration) {
	if h.retryScheduled || h.ready || h.fatal {
		return
	}
	h.retryScheduled = true
	select {
	case h.retryCh <- delay:
	default:
		h.retryScheduled = false
	}
}

func semanticRecallProbeResult(err error) modelchange.ProbeResult {
	result := modelchange.ProbeResult{
		Route: modelchange.RouteSemanticRecall,
		Error: strings.TrimSpace(tools.RedactSensitive(err.Error())),
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result.FailureClass = modelchange.FailureInfrastructure
		return result
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		result.FailureClass = modelchange.FailureInfrastructure
		return result
	}
	if info, ok := llm.ProviderErrorInfo(err); ok {
		switch info.Class {
		case llm.ProviderErrorAuth, llm.ProviderErrorInvalidRequest:
			result.FailureClass = modelchange.FailureModel
		default:
			result.FailureClass = modelchange.FailureInfrastructure
		}
		return result
	}
	if llm.IsRetryableError(err) {
		result.FailureClass = modelchange.FailureInfrastructure
	} else {
		result.FailureClass = modelchange.FailureModel
	}
	return result
}
