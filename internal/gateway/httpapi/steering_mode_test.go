package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

// Through the whole message path — where the gateway reads the person's mode at
// ingress for a new admission — guidance that steers a live Run still records
// no mode but one the request itself named.
func TestSteeringThroughTheMessagePathRecordsOnlyAModeTheRequestNamed(t *testing.T) {
	for _, tc := range []struct {
		name, requested, want string
	}{
		{"no mode named", "", ""},
		{"explicit mode named", "read-only", "read-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			s.setModeAt(t, "full-auto", time.Now().Unix()-3600)
			task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "release", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.store.StartRun(ctx, task, "cli", "publish the release notes")
			if err != nil {
				t.Fatal(err)
			}
			active := &activeRun{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, TaskID: task.ID, RunID: run.ID,
				Channel: "session-steer", Platform: "cli", PlatformUserID: "local", Summary: "release", StartedAt: time.Now(),
				Steer: make(chan kernel.SteeringInput, 4)}
			if !s.daemon.coordinator().beginActive(s.identity.PersonID, active) {
				t.Fatal("failed to register the active Run")
			}
			reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
				Channel: "session-steer", Content: "also tidy the changelog", ApprovalMode: tc.requested})
			if code != http.StatusOK || !reply.Accepted {
				t.Fatalf("the guidance was not steered into the live Run: %d %+v", code, reply)
			}
			rows, err := s.store.ListUnconsumedSteering(ctx, s.identity.TenantID, run.ID, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("expected the one accepted guidance row: %+v %v", rows, err)
			}
			if rows[0].ApprovalMode != tc.want {
				t.Fatalf("the guidance recorded mode %q, want %q", rows[0].ApprovalMode, tc.want)
			}
		})
	}
}

// Guidance a finished Run never consumed becomes queued work, admitted when it
// drains. It carries only the mode its own request named: a Run's handle holds
// the person's mode as it was when that Run began, and copying that onto the
// guidance would run it under the old, looser mode after the person tightened
// theirs.
func TestDeferredSteeringRunsUnderTheModeInForceAtItsDrainNotTheRunsOld(t *testing.T) {
	for _, tc := range []struct {
		name       string
		steer      func(t *testing.T, s admissionProbeServer, active *activeRun)
		wantJudged bool
	}{
		{
			// The thin-client endpoint (the TUI) names no mode.
			name: "runs-steer-endpoint",
			steer: func(t *testing.T, s admissionProbeServer, active *activeRun) {
				body, _ := json.Marshal(api.RunSteerRequest{Channel: "cli", RunID: active.RunID, Text: "also tidy the changelog"})
				rec := httptest.NewRecorder()
				// The thin client is the person's own CLI, verified on the local
				// control socket.
				s.daemon.handleRunSteer(rec, asLocalCLI(httptest.NewRequest(http.MethodPost, "/v1/runs/steer", bytes.NewReader(body))))
				if rec.Code != http.StatusOK {
					t.Fatalf("steer = %d %s", rec.Code, rec.Body.String())
				}
			},
			wantJudged: true,
		},
		{
			// The shared message path, with no mode of its own.
			name: "message-path-without-a-mode",
			steer: func(t *testing.T, s admissionProbeServer, active *activeRun) {
				if _, ok := s.daemon.steerActiveRun(context.Background(), s.identity, active,
					api.MessageRequest{Channel: "cli", Content: "also tidy the changelog"}); !ok {
					t.Fatal("steerActiveRun refused")
				}
			},
			wantJudged: true,
		},
		{
			// The constraint that must change the result: a mode the request
			// itself names is the person's explicit choice for this guidance.
			name: "message-path-with-an-explicit-mode",
			steer: func(t *testing.T, s admissionProbeServer, active *activeRun) {
				if _, ok := s.daemon.steerActiveRun(context.Background(), s.identity, active,
					api.MessageRequest{Channel: "cli", Content: "also tidy the changelog", ApprovalMode: "full-auto"}); !ok {
					t.Fatal("steerActiveRun refused")
				}
			},
			wantJudged: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			s.setModeAt(t, "full-auto", time.Now().Unix()-3600)
			task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "release", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.store.StartRun(ctx, task, "cli", "publish the release notes")
			if err != nil {
				t.Fatal(err)
			}
			// The Run began under the person's full-auto, which its handle holds.
			active := &activeRun{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, TaskID: task.ID, RunID: run.ID,
				Channel: "cli", Platform: "cli", PlatformUserID: "local", ApprovalMode: "full-auto",
				Summary: "release", StartedAt: time.Now(), Steer: make(chan kernel.SteeringInput, 4)}
			coord := s.daemon.coordinator()
			if !coord.beginActive(s.identity.PersonID, active) {
				t.Fatal("failed to register the active Run")
			}
			tc.steer(t, s, active)

			// The person tightens their mode while the Run is still going, and
			// the Run ends without consuming the guidance.
			s.setModeAt(t, "smart", time.Now().Unix()+30)
			if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			coord.endActive(s.identity.PersonID)
			coord.deferUnconsumedSteering(s.identity, active)
			rows, err := s.store.ListQueued(ctx, s.identity.TenantID, s.identity.PersonID, control.QueueStatusQueued)
			if err != nil || len(rows) != 1 {
				t.Fatalf("expected one deferred row: %+v %v", rows, err)
			}
			row := rows[0]
			coord.drainQueue(s.identity)
			waitUntil(t, 5*time.Second, func() bool {
				q, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
				return q != nil && q.Status == control.QueueStatusDone && q.RunID != ""
			}, "the deferred guidance did not run")
			waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(s.identity.PersonID) == 0 }, "the drained Run did not finish")
			if probe := s.provider.probe(t, 1); probe.judged != tc.wantJudged {
				t.Fatalf("deferred guidance ran with judged=%t, want %t (row mode %q)", probe.judged, tc.wantJudged, row.ApprovalMode)
			}
		})
	}
}

func TestSteerEndpointKeepsExplicitRequestMode(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.store.StartRun(ctx, task, "cli", "publish release notes")
	if err != nil {
		t.Fatal(err)
	}
	active := &activeRun{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, TaskID: task.ID, RunID: run.ID, Channel: "cli", Platform: "cli", PlatformUserID: "local", StartedAt: time.Now(), Steer: make(chan kernel.SteeringInput, 4)}
	if !s.daemon.coordinator().beginActive(s.identity.PersonID, active) {
		t.Fatal("register active Run")
	}
	body, _ := json.Marshal(map[string]string{"channel": "cli", "run_id": run.ID, "text": "also tidy notes", "approval_mode": "read-only"})
	recorder := httptest.NewRecorder()
	s.daemon.handleRunSteer(recorder, asLocalCLI(httptest.NewRequest(http.MethodPost, "/v1/runs/steer", bytes.NewReader(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("steer=%d %s", recorder.Code, recorder.Body.String())
	}
	rows, err := s.store.ListUnconsumedSteering(ctx, s.identity.TenantID, run.ID, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("guidance %+v %v", rows, err)
	}
	if rows[0].ApprovalMode != "read-only" {
		t.Fatalf("explicit steer mode lost: stored=%q want=read-only", rows[0].ApprovalMode)
	}
}
