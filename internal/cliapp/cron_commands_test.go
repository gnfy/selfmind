package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"selfmind/internal/gateway/api"
)

// `selfmind cron adopt` shows the schedule first and asks the daemon to
// record ownership only with --yes.
func TestCronAdoptShowsTheJobAndAdoptsOnlyWithYes(t *testing.T) {
	var mu sync.Mutex
	var requests []api.ScheduleAdoptRequest
	// `selfmind cron adopt` reaches the daemon on its local control socket, so
	// the fake daemon must answer there, like the real one.
	server := newFakeGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/schedules/adopt" {
			http.NotFound(w, r)
			return
		}
		var req api.ScheduleAdoptRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.ScheduleAdoptResponse{JobID: req.JobID, Name: "old report", CronExpr: "0 9 * * *",
			Prompt: "summarize my day", Standing: map[bool]string{false: "unclaimed", true: "yours"}[req.Adopt], Adopted: req.Adopt})
	}))
	t.Setenv("SELF_GATEWAY_URL", server.URL)
	run := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		app := &App{ctx: context.Background(), args: append([]string{"selfmind", "cron", "adopt"}, args...),
			stdout: &stdout, stderr: &stderr, gatewayEnsured: true}
		_, code := app.runCronCommandIfRequested()
		return code, stdout.String() + stderr.String()
	}
	code, out := run("7")
	if code != 0 || !strings.Contains(out, "Prompt: summarize my day") || !strings.Contains(out, "When: 0 9 * * *") ||
		!strings.Contains(out, "selfmind cron adopt 7 --yes") {
		t.Fatalf("show = %d %q", code, out)
	}
	code, out = run("7", "--yes")
	if code != 0 || !strings.Contains(out, "is now yours") {
		t.Fatalf("adopt = %d %q", code, out)
	}
	if len(requests) != 2 || requests[0].Adopt || !requests[1].Adopt || requests[1].JobID != 7 || requests[1].Platform != "cli" {
		t.Fatalf("requests = %+v", requests)
	}
	for _, bad := range [][]string{{}, {"x"}, {"7", "8"}} {
		if code, _ := run(bad...); code != 2 {
			t.Fatalf("args %q: exit %d", bad, code)
		}
	}
}
