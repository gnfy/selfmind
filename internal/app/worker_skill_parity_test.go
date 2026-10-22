package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
)

// /v1/dispatch always runs on the primary agent, yet a Skill it installs must
// reach the Runs on every worker, and disabling the Skill must take it out of
// every worker again. Each Run's worker is checked through the lease hook,
// which sees the worker's Skill tools as the Run takes it.
func TestSkillChangeThroughDispatchReachesRunsOnEveryWorker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	// Outside a Run the daemon's working directory is a Skill root; keep the
	// checkout's own Skills out of this fixture.
	t.Chdir(t.TempDir())
	h := newParityHarness(t, 3, "", answerEveryRequest)
	server := &httpapi.Server{
		Control: h.store, Gateway: h.deps.Gateway, DefaultTenantID: control.DefaultTenantID,
		ApprovalWait: 30 * time.Second, ApprovalWaitUnattended: 30 * time.Second,
	}
	runs := recordRunSkillTools(h)
	const installed, created = "skill:release-checklist", "skill:dispatch-made"

	dispatchManagement(t, server, "skill_catalog", map[string]interface{}{"action": "install", "source": "official/release-checklist"})
	dispatchManagement(t, server, "skill_manage", map[string]interface{}{
		"action": "create", "name": "dispatch-made", "description": "Created through management dispatch", "content": "Follow the declared procedure.",
	})
	for worker, skills := range runs.onEveryWorker(t, server, "after-install") {
		if !skills[installed] || !skills[created] {
			t.Fatalf("a Run on worker %d lacks the dispatched Skills %s and %s: %v", worker+1, installed, created, skills)
		}
	}
	requireIdenticalWorkerCatalogs(t, h)

	dispatchManagement(t, server, "skill_manage", map[string]interface{}{"action": "disable", "name": "release-checklist"})
	for worker, skills := range runs.onEveryWorker(t, server, "after-disable") {
		if skills[installed] || !skills[created] {
			t.Fatalf("a Run on worker %d after disabling: want only %s of %s and %s, got %v", worker+1, created, installed, created, skills)
		}
	}
	requireIdenticalWorkerCatalogs(t, h)

	dispatchManagement(t, server, "skill_manage", map[string]interface{}{"action": "enable", "name": "release-checklist"})
	for worker, skills := range runs.onEveryWorker(t, server, "after-enable") {
		if !skills[installed] {
			t.Fatalf("a Run on worker %d does not see re-enabled %s: %v", worker+1, installed, skills)
		}
	}
	requireIdenticalWorkerCatalogs(t, h)
}

// runSkillTools records, for each Run, the worker it checked out and the
// Skill tools that worker held at that moment.
type runSkillTools struct {
	h      *parityHarness
	mu     sync.Mutex
	leases []runSkillLease
}

type runSkillLease struct {
	worker int
	skills map[string]bool
}

func recordRunSkillTools(h *parityHarness) *runSkillTools {
	record := &runSkillTools{h: h}
	h.deps.Gateway.SetWorkerLeaseHook(func(_ context.Context, worker int) (func(), error) {
		skills := map[string]bool{}
		for _, name := range h.deps.Workers[worker].Dispatcher.ListTools() {
			if strings.HasPrefix(name, "skill:") {
				skills[name] = true
			}
		}
		record.mu.Lock()
		record.leases = append(record.leases, runSkillLease{worker: worker, skills: skills})
		record.mu.Unlock()
		return nil, nil
	})
	return record
}

// onEveryWorker runs sequential Runs until each worker has served one, and
// returns the Skill tools each worker held for its Run.
func (r *runSkillTools) onEveryWorker(t *testing.T, server *httpapi.Server, label string) map[int]map[string]bool {
	t.Helper()
	r.mu.Lock()
	start := len(r.leases)
	r.mu.Unlock()
	workers := len(r.h.deps.Workers)
	seen := map[int]map[string]bool{}
	for attempt := 0; attempt < 2*workers && len(seen) < workers; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		resp, code := server.ProcessMessage(ctx, api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: fmt.Sprintf("skill-parity-%s-%d", label, attempt),
			Content: "Reply with ok.", ApprovalMode: "full-auto",
		})
		cancel()
		if code != http.StatusOK {
			t.Fatalf("Run %s/%d: code=%d content=%q error=%q", label, attempt, code, resp.Content, resp.Error)
		}
		r.mu.Lock()
		for _, lease := range r.leases[start:] {
			seen[lease.worker] = lease.skills
		}
		start = len(r.leases)
		r.mu.Unlock()
	}
	if len(seen) < workers {
		t.Fatalf("Runs %s reached workers %v, want all %d", label, sortedWorkers(seen), workers)
	}
	return seen
}

func sortedWorkers(seen map[int]map[string]bool) []int {
	workers := make([]int, 0, len(seen))
	for worker := range seen {
		workers = append(workers, worker)
	}
	sort.Ints(workers)
	return workers
}

func requireIdenticalWorkerCatalogs(t *testing.T, h *parityHarness) {
	t.Helper()
	primary := workerCatalog(h.deps.Workers[0].Dispatcher)
	for index, worker := range h.deps.Workers[1:] {
		if diff := describeCatalogDifference(primary, workerCatalog(worker.Dispatcher)); diff != "" {
			t.Fatalf("worker %d catalogue differs from the primary: %s", index+2, diff)
		}
	}
}

// dispatchManagement runs one management tool the way a thin client does:
// through the daemon's /v1/dispatch endpoint.
func dispatchManagement(t *testing.T, server *httpapi.Server, tool string, args map[string]interface{}) string {
	t.Helper()
	body, err := json.Marshal(api.DispatchRequest{Tool: tool, Platform: "cli", PlatformUserID: "local", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	// The person's own CLI, as the daemon's local control socket verifies it.
	request := httptest.NewRequest(http.MethodPost, "/v1/dispatch", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(httpapi.WithLocalPeer(request.Context(), httpapi.LocalPeer{Socket: true, Verified: true}))
	server.Handler().ServeHTTP(recorder, request)
	var response api.DispatchResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusOK || response.Error != "" {
		t.Fatalf("dispatch %s %v: status=%d response=%+v decode=%v", tool, args["action"], recorder.Code, response, err)
	}
	return response.Result
}
