package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

func TestIncomingActiveRequiresSourceOrExactReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  api.MessageRequest
		want bool
	}{
		{"own source", api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-a"}, true},
		{"other CLI", api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-b"}, false},
		{"other platform same channel", api.MessageRequest{Platform: "weixin", PlatformUserID: "local", Channel: "session-a"}, false},
		{"other account same channel", api.MessageRequest{Platform: "cli", PlatformUserID: "other", Channel: "session-a"}, false},
		{"IM ordinary", api.MessageRequest{Platform: "weixin", PlatformUserID: "wx", Channel: "chat"}, false},
		{"cross endpoint reply", api.MessageRequest{Platform: "weixin", PlatformUserID: "wx", Channel: "chat", ReplyToRunID: "run-a"}, true},
		{"stale reply", api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-a", ReplyToRunID: "run-missing"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coord := (&Server{}).coordinator()
			a := &activeRun{PersonID: "person", Platform: "cli", PlatformUserID: "local", Channel: "session-a", RunID: "run-a"}
			coord.beginActive("person", a)
			got := coord.activeForIncoming("person", tc.req)
			if (got != nil) != tc.want {
				t.Fatalf("source lookup = %+v, want selected=%v", got, tc.want)
			}
		})
	}
}

func TestSingleForeignActiveDoesNotOwnOrdinaryInput(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "exact reply"}[exact], func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx", "IM"); err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "CLI work", Channel: "session-a"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "session-a", "CLI work")
			if err != nil {
				t.Fatal(err)
			}
			d := &Server{Control: store, DefaultTenantID: "default"}
			active := &activeRun{TenantID: identity.TenantID, PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "session-a",
				TaskID: task.ID, RunID: run.ID, StartedAt: time.Now(), Steer: make(chan kernel.SteeringInput, 1)}
			if !d.coordinator().beginActive(identity.PersonID, active) {
				t.Fatal("cannot register Run")
			}
			defer d.coordinator().endActiveRun(identity.PersonID, active)
			req := api.MessageRequest{Platform: "weixin", PlatformUserID: "wx", Channel: "chat", Content: "please check the remaining work"}
			if exact {
				req.ReplyToRunID = run.ID
			}
			resp, status := d.ProcessMessage(ctx, req)
			if status != 200 || resp.Turn == nil {
				t.Fatalf("response=%+v status=%d", resp, status)
			}
			if exact {
				if resp.Turn.RunID != run.ID || resp.Turn.QueueID != "" || len(active.Steer) != 1 {
					t.Fatalf("exact edge lost target: %+v", resp)
				}
			} else if resp.Turn.QueueID == "" || len(active.Steer) != 0 {
				t.Fatalf("ordinary foreign input steered another session: %+v", resp)
			}
		})
	}
}

func TestBareStopUsesSourceInsteadOfPersonCardinality(t *testing.T) {
	for _, tc := range []struct {
		name, platform, source    string
		second, exact, wantCancel bool
	}{
		{"foreign sole CLI", "cli", "session-b", false, false, false},
		{"multiple active needs exact", "cli", "session-a", true, false, false},
		{"exact cross session", "cli", "session-b", false, true, true},
		{"own CLI", "cli", "session-a", false, false, true},
		{"IM sole cross endpoint", "weixin", "chat", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			d := &Server{Control: store, DefaultTenantID: "default"}
			d.coordinator().activeLimit = 2
			cancelled, otherCancelled := false, false
			a := &activeRun{PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "session-a", RunID: "run-a", Cancel: func() { cancelled = true }}
			if !d.coordinator().beginActive(identity.PersonID, a) {
				t.Fatal("cannot register A")
			}
			defer d.coordinator().endActiveRun(identity.PersonID, a)
			if tc.second {
				b := &activeRun{PersonID: identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "session-b", RunID: "run-b", Cancel: func() { otherCancelled = true }}
				if !d.coordinator().beginActive(identity.PersonID, b) {
					t.Fatal("cannot register B")
				}
				defer d.coordinator().endActiveRun(identity.PersonID, b)
			}
			req := api.MessageRequest{Platform: tc.platform, PlatformUserID: "local", Channel: tc.source, Content: "/stop"}
			if tc.exact {
				req.ReplyToRunID = a.RunID
			}
			handled, reply, _, err := d.tryHandleControlCommand(ctx, identity, req)
			if err != nil || !handled || cancelled != tc.wantCancel || otherCancelled {
				t.Fatalf("cancelled=%v other=%v reply=%q err=%v", cancelled, otherCancelled, reply, err)
			}
			if tc.wantCancel && !strings.Contains(reply, "Stopping run run-a") {
				t.Fatalf("wrong receipt=%q", reply)
			}
		})
	}
}

func TestParkedProviderStopKeepsCLISelectionBoundary(t *testing.T) {
	for _, target := range []string{"other CLI bare", "own CLI bare", "other CLI exact", "other CLI reply", "other CLI ordinal", "IM sole"} {
		t.Run(target, func(t *testing.T) {
			s := newProviderWaitSequence(t)
			ctx := context.Background()
			if err := s.park(t, "rate_limit", 0); err != nil {
				t.Fatal(err)
			}
			req := api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-b", Content: "/stop"}
			switch target {
			case "own CLI bare":
				req.Channel = s.run.Channel
			case "other CLI exact":
				req.Content += " " + s.run.ID
			case "other CLI reply":
				req.ReplyToRunID = s.run.ID
			case "other CLI ordinal":
				s.daemon.taskLists.rememberAttention(s.owner, req.Channel, []control.AttentionItem{{Thread: control.Thread{ID: s.task.ID}, RunID: s.run.ID}}, time.Now())
				req.Content += " 1"
			case "IM sole":
				if _, err := s.store.BindAccount(ctx, s.owner.TenantID, s.owner.PersonID, "weixin", "wx", "IM"); err != nil {
					t.Fatal(err)
				}
				req.Platform, req.PlatformUserID, req.Channel = "weixin", "wx", "chat"
			}
			resp, status := s.daemon.ProcessMessage(ctx, req)
			if status != 200 || resp.Error != "" {
				t.Fatalf("stop response=%+v status=%d", resp, status)
			}
			run, _ := s.store.GetRun(ctx, s.owner.TenantID, s.run.ID)
			q, _ := s.store.GetQueuedByIdempotencyKey(ctx, s.owner.TenantID, "provider-wait:"+s.run.ID)
			if target == "other CLI bare" {
				if run.Status != "waiting_external" || q.Status != control.QueueStatusQueued {
					t.Fatalf("unselected other session cancelled parked work: run=%+v q=%+v receipt=%q", run, q, resp.Content)
				}
			} else if run.Status != "cancelled" || q.Status != control.QueueStatusCancelled {
				t.Fatalf("selected work did not cancel atomically: run=%+v q=%+v receipt=%q", run, q, resp.Content)
			}
		})
	}
}
