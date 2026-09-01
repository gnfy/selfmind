package httpapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/modelchange"
)

type semanticHealthExpander struct {
	mu     sync.Mutex
	err    error
	calls  int
	result string
}

func (e *semanticHealthExpander) ExpandWithError(_ context.Context, query string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.err != nil {
		return query, e.err
	}
	if e.result != "" {
		return e.result, nil
	}
	return query, nil
}

func TestSemanticRecallTransientFailureRecoversWithBoundedRetry(t *testing.T) {
	expander := &semanticHealthExpander{result: "deployment release rollout"}
	var probes atomic.Int32
	var recordedMu sync.Mutex
	var recorded []modelchange.ProbeResult
	h := NewSemanticRecallHealth(SemanticRecallHealthOptions{
		Expander: expander,
		Initial: modelchange.RouteReadiness{
			Ready: false, FailureClass: modelchange.FailureInfrastructure, Reason: "context deadline exceeded",
		},
		Probe: func(context.Context) modelchange.ProbeResult {
			attempt := probes.Add(1)
			if attempt == 1 {
				return modelchange.ProbeResult{Error: "rate limited", FailureClass: modelchange.FailureInfrastructure}
			}
			return modelchange.ProbeResult{OK: true}
		},
		Record: func(result modelchange.ProbeResult) error {
			recordedMu.Lock()
			recorded = append(recorded, result)
			recordedMu.Unlock()
			return nil
		},
		RetryInitial: time.Millisecond,
		RetryMax:     4 * time.Millisecond,
	})
	notices := make(chan string, 4)
	h.SetNotifier(func(message string, _ bool) { notices <- message })
	stop := h.Start(context.Background())
	defer stop()

	if got := <-notices; got != semanticRecallDegradedNotice {
		t.Fatalf("degraded notice = %q", got)
	}
	deadline := time.Now().Add(time.Second)
	for !h.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !h.Ready() || probes.Load() != 2 {
		t.Fatalf("ready=%v probes=%d", h.Ready(), probes.Load())
	}
	if got := <-notices; got != semanticRecallRecoveredNotice {
		t.Fatalf("recovery notice = %q", got)
	}
	if got := h.Expand(context.Background(), "deployment"); got != "deployment release rollout" {
		t.Fatalf("expanded = %q", got)
	}
	recordedMu.Lock()
	defer recordedMu.Unlock()
	if len(recorded) != 2 || recorded[0].OK || !recorded[1].OK {
		t.Fatalf("recorded transitions = %+v", recorded)
	}
}

func TestSemanticRecallFatalFailureWaitsForHumanRepair(t *testing.T) {
	var probes atomic.Int32
	h := NewSemanticRecallHealth(SemanticRecallHealthOptions{
		Expander: &semanticHealthExpander{},
		Initial: modelchange.RouteReadiness{
			Ready: false, FailureClass: modelchange.FailureModel, Reason: "authentication failed",
		},
		Probe: func(context.Context) modelchange.ProbeResult {
			probes.Add(1)
			return modelchange.ProbeResult{OK: true}
		},
		RetryInitial: time.Millisecond,
		RetryMax:     2 * time.Millisecond,
	})
	stop := h.Start(context.Background())
	defer stop()
	time.Sleep(15 * time.Millisecond)
	if probes.Load() != 0 {
		t.Fatalf("fatal route was retried %d time(s)", probes.Load())
	}
	if got := h.Expand(context.Background(), "deployment"); got != "deployment" {
		t.Fatalf("fatal route did not degrade to lexical query: %q", got)
	}
}

func TestSemanticRecallLiveTimeoutStopsPerTurnCalls(t *testing.T) {
	expander := &semanticHealthExpander{err: context.DeadlineExceeded}
	h := NewSemanticRecallHealth(SemanticRecallHealthOptions{
		Expander: expander,
		Initial:  modelchange.RouteReadiness{Ready: true},
		Record: func(result modelchange.ProbeResult) error {
			if result.FailureClass != modelchange.FailureInfrastructure {
				return errors.New("timeout was not classified as infrastructure")
			}
			return nil
		},
	})
	if got := h.Expand(context.Background(), "deployment"); got != "deployment" {
		t.Fatalf("failed expansion = %q", got)
	}
	if h.Ready() {
		t.Fatal("live timeout left semantic_recall ready")
	}
	if got := h.Expand(context.Background(), "second query"); got != "second query" {
		t.Fatalf("degraded expansion = %q", got)
	}
	expander.mu.Lock()
	defer expander.mu.Unlock()
	if expander.calls != 1 {
		t.Fatalf("degraded role made %d per-turn calls, want 1", expander.calls)
	}
}

func TestSemanticRecallLiveFailureNoticeIsDebouncedAndNamesRoute(t *testing.T) {
	expander := &semanticHealthExpander{err: context.DeadlineExceeded}
	notices := make(chan string, 2)
	h := NewSemanticRecallHealth(SemanticRecallHealthOptions{
		Expander: expander, Initial: modelchange.RouteReadiness{Ready: true},
		RouteLabel: "deepseek/deepseek-v4-flash",
	})
	h.SetNotifier(func(message string, _ bool) { notices <- message })
	if got := h.Expand(context.Background(), "release"); got != "release" {
		t.Fatalf("expanded=%q", got)
	}
	select {
	case notice := <-notices:
		t.Fatalf("first transient failure should be quiet, got %q", notice)
	default:
	}
	h.markFailure(modelchange.ProbeResult{FailureClass: modelchange.FailureInfrastructure, Error: "retry timeout"})
	select {
	case notice := <-notices:
		if !strings.Contains(notice, "semantic_recall (deepseek/deepseek-v4-flash)") {
			t.Fatalf("notice=%q", notice)
		}
	case <-time.After(time.Second):
		t.Fatal("second failure did not emit degraded notice")
	}
}
