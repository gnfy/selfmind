package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/gateway/api"
)

type fakeBatchPostRunAnalyzer struct {
	mu          sync.Mutex
	singleCalls int
	batchCalls  int
	batchSizes  []int
}

func (f *fakeBatchPostRunAnalyzer) Analyze(context.Context, PostRunAnalysisRequest) (PostRunAnalysis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.singleCalls++
	return PostRunAnalysis{TaskDecision: "KEEP"}, nil
}

func (f *fakeBatchPostRunAnalyzer) AnalyzeBatch(_ context.Context, reqs []PostRunAnalysisRequest) (map[string]PostRunAnalysis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	f.batchSizes = append(f.batchSizes, len(reqs))
	out := make(map[string]PostRunAnalysis, len(reqs))
	for _, req := range reqs {
		out[req.RunID] = PostRunAnalysis{TaskDecision: "KEEP"}
	}
	return out, nil
}

func TestMaintenanceWorkerBatchesRunsByPersonWorkspace(t *testing.T) {
	provider := newSlowLLMProvider("completed the requested work with durable output")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	analyzer := &fakeBatchPostRunAnalyzer{}
	daemon.PostRunAnalyzer = analyzer

	for i := 0; i < 2; i++ {
		resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "cli",
			Content: strings.Repeat("Implement and verify this durable workspace improvement. ", 4),
		})
		if status != 200 || resp.Run == nil {
			t.Fatalf("turn %d failed: status=%d resp=%+v", i, status, resp)
		}
	}
	if analyzer.singleCalls != 0 || analyzer.batchCalls != 0 {
		t.Fatalf("finalization must only enqueue evidence: single=%d batch=%d", analyzer.singleCalls, analyzer.batchCalls)
	}

	// The run-count cap is itself a flush trigger; no quiet window is needed.
	daemon.PostRunMaintenance = PostRunMaintenanceOptions{
		Debounce: 5 * time.Minute, MaxWait: 15 * time.Minute, BatchMaxRuns: 2,
	}
	daemon.runMaintenancePassAt(context.Background(), time.Now())
	if analyzer.singleCalls != 0 || analyzer.batchCalls != 1 {
		t.Fatalf("expected one batch provider call: single=%d batch=%d", analyzer.singleCalls, analyzer.batchCalls)
	}
	if len(analyzer.batchSizes) != 1 || analyzer.batchSizes[0] != 2 {
		t.Fatalf("batch sizes = %v", analyzer.batchSizes)
	}
}

func TestMaintenanceWorkerWaitsForQuietWindow(t *testing.T) {
	provider := newSlowLLMProvider("completed the requested work with durable output")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	analyzer := &fakeBatchPostRunAnalyzer{}
	daemon.PostRunAnalyzer = analyzer

	resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: strings.Repeat("Implement and verify this durable workspace improvement. ", 4),
	})
	if status != 200 || resp.Run == nil {
		t.Fatalf("turn failed: status=%d resp=%+v", status, resp)
	}
	now := time.Now()
	daemon.PostRunMaintenance = PostRunMaintenanceOptions{
		Debounce: 5 * time.Minute, MaxWait: 15 * time.Minute, BatchMaxRuns: 10,
	}
	daemon.runMaintenancePassAt(context.Background(), now)
	if analyzer.batchCalls != 0 {
		t.Fatalf("fresh evidence must wait for the quiet window: calls=%d", analyzer.batchCalls)
	}
	daemon.runMaintenancePassAt(context.Background(), now.Add(6*time.Minute))
	if analyzer.batchCalls != 1 || len(analyzer.batchSizes) != 1 || analyzer.batchSizes[0] != 1 {
		t.Fatalf("quiet-window flush = calls %d sizes %v", analyzer.batchCalls, analyzer.batchSizes)
	}
}

func TestMaintenanceWorkerNeverBatchesAcrossWorkspaces(t *testing.T) {
	provider := newSlowLLMProvider("completed the requested work with durable output")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	analyzer := &fakeBatchPostRunAnalyzer{}
	daemon.PostRunAnalyzer = analyzer

	for i, cwd := range []string{t.TempDir(), t.TempDir()} {
		resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "cli", ClientCWD: cwd,
			Content: strings.Repeat("Implement and verify this durable workspace improvement. ", 4),
		})
		if status != 200 || resp.Run == nil {
			t.Fatalf("turn %d failed: status=%d resp=%+v", i, status, resp)
		}
	}
	drainPostRunMaintenance(daemon)
	if analyzer.batchCalls != 2 {
		t.Fatalf("different workspaces must use separate calls: calls=%d sizes=%v", analyzer.batchCalls, analyzer.batchSizes)
	}
	for _, size := range analyzer.batchSizes {
		if size != 1 {
			t.Fatalf("cross-workspace batch detected: sizes=%v", analyzer.batchSizes)
		}
	}
}
