package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/task/cron"
)

// Settling an external effect is the person's statement that they know its
// result. A scheduled prompt holding /effects settle, fired as the person, is
// stored text: it settles nothing and gets the scheduled-command notice. The
// person's own message settles.
func TestScheduledTextNeverSettlesAnExternalEffect(t *testing.T) {
	f := newScheduledTextFixture(t)
	ctx := context.Background()
	run, err := f.store.StartRunWithOptions(ctx, f.task, "cli", "publish", control.StartRunOptions{MaxActiveRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := f.store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: f.person.TenantID,
		PersonID: f.person.PersonID, RunID: run.ID, EffectID: "publish", TargetKeys: []string{control.UnknownExternalTarget}})
	if err != nil || !decision.Granted {
		t.Fatalf("claim: %+v %v", decision, err)
	}
	claimID := decision.Claims[0].ID
	if err := f.store.RecordExternalEffectDispatch(ctx, control.ExternalEffectDispatch{TenantID: f.person.TenantID,
		PersonID: f.person.PersonID, RunID: run.ID, EffectID: "publish", ToolCallID: "publish", ToolName: "terminal",
		Summary: "npm publish --tag next"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkExternalEffectPossible(ctx, f.person.TenantID, run.ID, "publish"); err != nil {
		t.Fatal(err)
	}
	exit := 1
	if err := f.store.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: f.person.TenantID,
		PersonID: f.person.PersonID, RunID: run.ID, EffectID: "publish", ToolCallID: "publish", Outcome: "exit 1", ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}
	settled := func() bool {
		t.Helper()
		claims, err := f.store.ExternalEffectClaimsForEffect(ctx, f.person.TenantID, f.person.PersonID, run.ID, "publish")
		if err != nil || len(claims) != 1 {
			t.Fatalf("claims: %+v %v", claims, err)
		}
		return claims[0].State == control.ExternalClaimObserved
	}
	for _, prompt := range []string{"/effects settle " + claimID, "/EFFECTS settle " + claimID + " it is live", "  /effects settle " + claimID} {
		for _, job := range []func(string) cron.CronJob{cliJob, imJob} {
			f.fire(t, job(prompt))
			if settled() {
				t.Fatalf("scheduled %q settled the effect", prompt)
			}
		}
	}
	if notices := f.deliveredTo("weixin", "wx-me"); len(notices) == 0 || !strings.Contains(notices[0], "commands run only from your own messages") {
		t.Fatalf("notices = %q", notices)
	}
	resp, status := f.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "/effects settle " + claimID + " checked the registry"})
	if status != 200 || !strings.Contains(resp.Content, "Settled external effect "+claimID) || !settled() {
		t.Fatalf("the person's own settle: %d %q", status, resp.Content)
	}
}
