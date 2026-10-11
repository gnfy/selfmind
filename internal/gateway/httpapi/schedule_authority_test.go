package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control/controltest"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/task/cron"
)

// A model-created schedule runs as an account of the person whose Run created
// it and delivers to that account's direct chat; nothing else is bound, and a
// refusal says what to do instead of picking another endpoint.
func TestScheduleAuthorityBindsOnlyTheCreatorsEndpoints(t *testing.T) {
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	person, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	im, err := store.BindAccount(ctx, person.TenantID, person.PersonID, "weixin", "wx-a", "Me on WeChat")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := store.ResolveOrCreateAccount(ctx, "default", "telegram", "900", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	authority := NewScheduleAuthority(store)
	fromCLI := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, AccountID: person.AccountID}
	fromIM := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, AccountID: im.AccountID}
	unattached := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID}
	ownIM := cron.JobEndpoint{TenantID: "default", Platform: "weixin", DeliverTo: "wx-a", Channel: "wx-a"}
	for _, tc := range []struct {
		name    string
		scope   kernel.ToolInvocationScope
		request cron.JobRequest
		want    cron.JobEndpoint
		refusal string
	}{
		{"an IM request names nothing", fromIM, cron.JobRequest{}, ownIM, ""},
		{"a CLI request names nothing", fromCLI, cron.JobRequest{}, cron.JobEndpoint{TenantID: "default", Platform: "cli", DeliverTo: "local", Channel: "cli"}, ""},
		{"own account by id alone", fromCLI, cron.JobRequest{DeliverTo: "wx-a"}, ownIM, ""},
		{"own account by platform alone", fromCLI, cron.JobRequest{Platform: "WeiXin"}, ownIM, ""},
		{"platform name as the chat", fromCLI, cron.JobRequest{Platform: "weixin", DeliverTo: "wx-a", Channel: "weixin"}, ownIM, ""},
		{"another person's account", fromCLI, cron.JobRequest{Platform: "telegram", DeliverTo: "900"}, cron.JobEndpoint{}, "telegram:900 is not one of your accounts (yours are on cli, weixin)"},
		{"another person's id alone", fromIM, cron.JobRequest{DeliverTo: "900"}, cron.JobEndpoint{}, "900 is not one of your accounts"},
		{"an unbound account", fromIM, cron.JobRequest{Platform: "telegram", DeliverTo: "901"}, cron.JobEndpoint{}, "telegram:901 is not one of your accounts"},
		{"a shared chat", fromIM, cron.JobRequest{Platform: "weixin", DeliverTo: "wx-a", Channel: "team@chatroom"}, cron.JobEndpoint{}, "team@chatroom is not your own direct chat"},
		{"a CLI chat", fromCLI, cron.JobRequest{Channel: "session-1"}, cron.JobEndpoint{}, "has no chat"},
		{"no endpoint of its own", unattached, cron.JobRequest{}, cron.JobEndpoint{}, "no endpoint of its own"},
		{"a canary that is a command", fromCLI, cron.JobRequest{Prompt: "canary:/approve"}, cron.JobEndpoint{}, "begins with a SelfMind command"},
		{"text after a command", fromCLI, cron.JobRequest{Prompt: "/approve then summarize my day"}, cron.JobEndpoint{}, "begins with a SelfMind command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			if request.Prompt == "" {
				request.Prompt = "summarize my day"
			}
			got, err := authority.Bind(ctx, tc.scope, request)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("Bind = %+v, %v; want a refusal saying %q", got, err, tc.refusal)
				}
				// A refusal goes to the model: it names platforms, never the
				// person's account ids.
				for _, id := range []string{"wx-a", "cli:local"} {
					if strings.Contains(err.Error(), id) {
						t.Fatalf("refusal %q lists the account %s", err, id)
					}
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Bind = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}

	// A job that does not run is still its person's to see and remove, with
	// the reason; a job naming an account nobody holds, with no creator on
	// record, is the command-line users' to clean up; another person's job is
	// invisible.
	for _, tc := range []struct {
		job     cron.CronJob
		owned   bool
		blocked string
	}{
		{cron.CronJob{TenantID: "default", CreatorPersonID: person.PersonID, Platform: "weixin", DeliverTo: "wx-a"}, true, ""},
		{cron.CronJob{TenantID: "default", CreatorPersonID: person.PersonID, Platform: "weixin", DeliverTo: "wx-a", Channel: "team@chatroom"}, true, "team@chatroom, which is not your own direct chat"},
		// telegram:900 is someone else here, so a row naming no account with
		// no receipt is no one's: the command line sees it.
		{cron.CronJob{TenantID: "default", Channel: "cli"}, true, "it names no account"},
		{cron.CronJob{TenantID: "default", CreatorPersonID: stranger.PersonID, Platform: "telegram", DeliverTo: "900"}, false, ""},
		{cron.CronJob{TenantID: "default", CreatorPersonID: person.PersonID, Platform: "telegram", DeliverTo: "901"}, true, "telegram:901, which is not bound to anyone here"},
		{cron.CronJob{TenantID: "other", Platform: "weixin", DeliverTo: "wx-a"}, false, ""},
	} {
		got, err := authority.Standing(ctx, fromCLI, tc.job)
		if err != nil || got.Owned != tc.owned || !strings.Contains(got.Blocked, tc.blocked) || (tc.blocked == "" && got.Blocked != "") {
			t.Fatalf("Standing(%+v) = %+v, %v; want owned=%v blocked %q", tc.job, got, err, tc.owned, tc.blocked)
		}
	}
	// A person with no command-line account does not see the unclaimed job.
	if got, err := authority.Standing(ctx, kernel.ToolInvocationScope{ControlTenantID: "default", PersonID: "person-im-only"},
		cron.CronJob{TenantID: "default", Platform: "telegram", DeliverTo: "901"}); err != nil || got.Owned {
		t.Fatalf("an IM-only person sees an unclaimed job: %+v %v", got, err)
	}
}
