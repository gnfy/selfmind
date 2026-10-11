package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/task/cron"
)

func allMarks(t *testing.T, store *control.Store, people ...*control.IdentityContext) []control.MCPReadOnlyMark {
	t.Helper()
	var out []control.MCPReadOnlyMark
	for _, person := range people {
		marks, err := store.ListMCPReadOnlyMarks(context.Background(), person.TenantID, person.PersonID)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, marks...)
	}
	return out
}

// Only the person's own typed words change a marking. A scheduled prompt is
// text the daemon replays later; it can hold the command (the model can store
// one with the cron tool), and it can even name another person's endpoint.
// When it fires it must mark and unmark nothing, in any spelling.
func TestCronFiredToolsCommandChangesNoMarking(t *testing.T) {
	server, store, _ := newToolsCommandServer(t)
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "telegram", "900", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	executor := NewCronExecutor(server, nil)
	for _, job := range []cron.CronJob{
		{TenantID: control.DefaultTenantID, Platform: "telegram", DeliverTo: "900", Prompt: "/tools readonly github/get_issue on"},
		{TenantID: control.DefaultTenantID, Platform: "telegram", DeliverTo: "900", Prompt: "/TOOLS ReadOnly github/get_issue ON"},
		{TenantID: control.DefaultTenantID, Platform: "cli", DeliverTo: "local", Channel: "session-cron", Prompt: "/tools readonly github/get_issue on"},
	} {
		if err := executor.RunCronJob(ctx, job); err != nil {
			t.Fatalf("cron job %q: %v", job.Prompt, err)
		}
		if marks := allMarks(t, store, owner, stranger); len(marks) != 0 {
			t.Fatalf("cron prompt %q (%s/%s) recorded markings: %+v", job.Prompt, job.Platform, job.DeliverTo, marks)
		}
	}

	// A marking the owner made at their terminal is not withdrawn by a
	// scheduled "off" either.
	sendCommand(t, server, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-1",
		Content: "/tools readonly github/get_issue on"})
	if err := executor.RunCronJob(ctx, cron.CronJob{TenantID: control.DefaultTenantID, Platform: "cli", DeliverTo: "local",
		Channel: "session-cron", Prompt: "/tools readonly github/get_issue off"}); err != nil {
		t.Fatal(err)
	}
	if marks := allMarks(t, store, owner); len(marks) != 1 {
		t.Fatalf("a scheduled off withdrew the owner's marking: %+v", marks)
	}
}

// The same refusal for every daemon-originated turn, however its origin is
// carried: an explicit origin, a watcher finalization, or the turn tag. The
// gateway refuses daemon text that begins with a command before any control
// runs, so the command's own origin check is a second line behind it.
func TestDaemonOriginatedToolsCommandIsRefused(t *testing.T) {
	server, store, _ := newToolsCommandServer(t)
	owner, err := store.ResolveOrCreateAccount(context.Background(), control.DefaultTenantID, "cli", "local", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	base := api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-1",
		Content: "/tools readonly github/get_issue on"}
	type turn struct {
		ctx context.Context
		req api.MessageRequest
	}
	cases := map[string]turn{}
	for _, origin := range []string{runOriginCron, runOriginWatch, runOriginApproval, runOriginRecovery, runOriginResource, runOriginProviderWait} {
		req := base
		req.Origin = origin
		cases["origin "+origin] = turn{context.Background(), req}
	}
	watch := base
	watch.WatchID = "watch_1"
	cases["watcher finalization"] = turn{context.Background(), watch}
	cases["turn source tag"] = turn{kernel.WithTurnSource(context.Background(), "cron"), base}
	for name, tc := range cases {
		ctx, cancel := context.WithTimeout(tc.ctx, 30*time.Second)
		response, code := server.ProcessMessage(ctx, tc.req)
		cancel()
		if code != http.StatusOK || !strings.Contains(response.Content, "commands run only from your own messages") {
			t.Errorf("%s: code=%d reply=%q", name, code, response.Content)
		}
	}
	if marks := allMarks(t, store, owner); len(marks) != 0 {
		t.Fatalf("a daemon-originated turn recorded markings: %+v", marks)
	}
	// Daemon text runs no command at all, a listing included.
	listing := base
	listing.Origin, listing.Content = runOriginCron, "/tools readonly"
	if response, _ := server.ProcessMessage(context.Background(), listing); !strings.Contains(response.Content, "commands run only from your own messages") {
		t.Fatalf("daemon-originated listing = %q", response.Content)
	}
	// The person's own turn still marks.
	if reply := sendCommand(t, server, base); !strings.Contains(reply, "Marked github/get_issue read-only for you") {
		t.Fatalf("the person's own turn = %q", reply)
	}
}
