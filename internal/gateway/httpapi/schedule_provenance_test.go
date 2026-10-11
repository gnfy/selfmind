package httpapi

// Who an older schedule row belongs to is decided once, from evidence, and
// kept: the evidence — Run receipts, the people with accounts — only ever
// disappears, and its loss must never hand a job to the installation's
// command line.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// ran reports whether firing the job ran it as the person.
func (in *installation) ranAs(t *testing.T, person *control.IdentityContext, id int64) bool {
	t.Helper()
	before := in.runs(t, person)
	in.fire(t, id)
	return in.runs(t, person) == before+1
}

func (in *installation) resetWorkHistory(t *testing.T) {
	t.Helper()
	for _, tenant := range []string{"default", "other"} {
		if _, err := in.store.ResetWorkHistory(context.Background(), tenant); err != nil {
			t.Fatal(err)
		}
	}
}

// A work-history reset removes every Run and receipt. A decision recorded
// before it stands; one made after it has only the accounts to go on, which
// the reset keeps.
func TestLegacyJobOwnershipSurvivesAWorkHistoryReset(t *testing.T) {
	t.Run("reset after the decision", func(t *testing.T) {
		in := newInstallation(t)
		stranger := in.stranger(t)
		job := in.legacyRow(t, "exfil", "", "", "")
		in.receipt(t, stranger, job)
		if line := in.listed(t, stranger, job); line == "" {
			t.Fatal("the creator does not see the job before the reset")
		}
		in.resetWorkHistory(t)
		if in.ranAs(t, in.me, job.ID) {
			t.Fatal("after a reset the stranger's job ran as the command-line person")
		}
		if line := in.listed(t, stranger, job); line == "" {
			t.Fatal("after a reset the job no longer belongs to its creator")
		}
	})
	t.Run("stranger's receipt, then a reset before any decision", func(t *testing.T) {
		in := newInstallation(t)
		stranger := in.stranger(t)
		job := in.legacyRow(t, "exfil", "", "", "")
		in.receipt(t, stranger, job)
		in.resetWorkHistory(t)
		if in.ranAs(t, in.me, job.ID) {
			t.Fatal("a job whose stranger receipt was reset away ran as the command-line person")
		}
	})
	t.Run("reset before the upgrade", func(t *testing.T) {
		in := newInstallation(t)
		in.stranger(t)
		in.resetWorkHistory(t)
		job := in.legacyRow(t, "old report", "", "", "")
		if in.ranAs(t, in.me, job.ID) {
			t.Fatal("with a stranger on record, a job with no receipt ran as the command-line person")
		}
	})
	t.Run("the decision outlives an account moving", func(t *testing.T) {
		in := newInstallation(t)
		ctx := context.Background()
		stranger := in.stranger(t)
		job := in.legacyRow(t, "old report", "", "", "")
		if line := in.listed(t, in.me, job); !strings.Contains(line, "no one here can be shown to have created it") {
			t.Fatalf("first decision: %q", line)
		}
		// The only other person's account is rebound to the command-line
		// person: no one else holds an account any more.
		if _, err := in.store.BindAccount(ctx, "default", in.me.PersonID, stranger.Platform, stranger.PlatformUserID, "Me"); err != nil {
			t.Fatal(err)
		}
		if in.ranAs(t, in.me, job.ID) {
			t.Fatal("an unclaimed job became the command line's once the evidence changed")
		}
	})
	// The variant: on an installation only its command-line person has ever
	// reached, the job is theirs before and after a reset.
	t.Run("only the command line, reset", func(t *testing.T) {
		in := newInstallation(t)
		job := in.legacyRow(t, "standup notes", "", "", "")
		in.resetWorkHistory(t)
		if !in.ranAs(t, in.me, job.ID) {
			t.Fatal("the command-line person's own job did not run after a reset")
		}
	})
}

// The older executor ran other people's jobs as the cli:local ghost, and a
// schedule's own Run may have added a job; neither is evidence of who created
// it when other people use the installation.
func TestReceiptsFromTheGhostOrAScheduleAreNoEvidence(t *testing.T) {
	t.Run("ghost receipt with a stranger present", func(t *testing.T) {
		in := newInstallation(t)
		ghost, err := in.store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "cli:local")
		if err != nil {
			t.Fatal(err)
		}
		in.stranger(t)
		job := in.legacyRow(t, "added by a job", "", "", "")
		in.receipt(t, ghost, job)
		if in.ranAs(t, in.me, job.ID) {
			t.Fatal("a cli:local receipt handed the job to the command line while strangers exist")
		}
	})
	t.Run("schedule-started receipt", func(t *testing.T) {
		in := newInstallation(t)
		in.stranger(t)
		job := in.legacyRow(t, "added by a job", "", "", "")
		in.receiptFrom(t, in.me, job, "cron")
		if in.ranAs(t, in.me, job.ID) {
			t.Fatal("a receipt from a Run a schedule started counted as the person's own act")
		}
	})
	// The variants: with no one else on record the ghost stands for the
	// command line, and the person's own turn is evidence.
	t.Run("ghost receipt alone", func(t *testing.T) {
		in := newInstallation(t)
		ghost, err := in.store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "cli:local")
		if err != nil {
			t.Fatal(err)
		}
		job := in.legacyRow(t, "added by a job", "", "", "")
		in.receipt(t, ghost, job)
		if !in.ranAs(t, in.me, job.ID) {
			t.Fatal("with no one else on record a ghost's job did not run as the command line")
		}
	})
	t.Run("own turn's receipt", func(t *testing.T) {
		in := newInstallation(t)
		in.stranger(t)
		job := in.legacyRow(t, "mine", "", "", "")
		in.receiptFrom(t, in.me, job, "")
		if !in.ranAs(t, in.me, job.ID) {
			t.Fatal("the person's own receipt did not count")
		}
	})
}

// The older tool stored every job under the default tenant, whatever tenant
// its Run was in: a creator elsewhere is still its creator, and still someone
// other than the command line.
func TestLegacyJobCreatorInAnotherTenant(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	elsewhere, err := in.store.ResolveOrCreateAccount(ctx, "other", "telegram", "77", "Carol")
	if err != nil {
		t.Fatal(err)
	}
	job := in.legacyRow(t, "carol's", "", "", "")
	in.receipt(t, elsewhere, job)
	if in.ranAs(t, in.me, job.ID) {
		t.Fatal("a job created in another tenant ran as the command-line person")
	}
	// No receipt at all: Carol's account still means someone else uses the
	// installation.
	plain := in.legacyRow(t, "no receipt", "", "", "")
	if in.ranAs(t, in.me, plain.ID) {
		t.Fatal("another tenant's person did not count as someone else")
	}
}

// The bare liveness canary's turn is fixed text, so it runs as the command
// line whoever stored it; a canary carrying a task of its own is evidence-bound.
func TestBareCanaryRunsAsTheCommandLine(t *testing.T) {
	in := newInstallation(t)
	in.stranger(t)
	bare, err := in.cronDB.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once, enabled)
		VALUES ('canary', '0 * * * *', 'canary:', 'default', 'cli', '', '', 0, 0, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	bareID, _ := bare.LastInsertId()
	if !in.ranAs(t, in.me, bareID) {
		t.Fatal("the bare canary did not run as the command line")
	}
	tasked, err := in.cronDB.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once, enabled)
		VALUES ('canary', '0 * * * *', 'canary: summarize my files', 'default', 'cli', '', '', 0, 0, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	taskedID, _ := tasked.LastInsertId()
	if in.ranAs(t, in.me, taskedID) {
		t.Fatal("a canary with a task of its own ran as the command line with no evidence")
	}
	// The daemon's own keyed canary is the installation's, whatever receipt
	// names its id.
	keyed, err := in.cronDB.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once, enabled, system_key)
		VALUES ('canary', '0 * * * *', 'canary:', 'default', 'cli', '', '', 0, 0, 1, 'canary')`)
	if err != nil {
		t.Fatal(err)
	}
	keyedID, _ := keyed.LastInsertId()
	in.receipt(t, in.stranger(t), in.job(t, keyedID))
	if !in.ranAs(t, in.me, keyedID) {
		t.Fatal("the daemon's keyed canary did not run as the command line")
	}
}

// Several people with command-line accounts leave the command line unknown:
// a job with no receipt is nobody's, and a receipt still names its creator.
func TestSeveralCommandLinePersonsLeaveTheCommandLineUnknown(t *testing.T) {
	in := newInstallation(t)
	alice, err := in.store.ResolveOrCreateAccount(context.Background(), "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	plain := in.legacyRow(t, "no receipt", "", "", "")
	mine, hers := in.runs(t, in.me), in.runs(t, alice)
	in.fire(t, plain.ID)
	if in.runs(t, in.me) != mine || in.runs(t, alice) != hers {
		t.Fatal("a job with no receipt ran as one of several command-line persons")
	}
	// Even the bare canary, which needs no evidence, has no one to run as.
	canary, err := in.cronDB.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel, platform, deliver_to, web, once, enabled)
		VALUES ('canary', '0 * * * *', 'canary:', 'default', 'cli', '', '', 0, 0, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	canaryID, _ := canary.LastInsertId()
	in.fire(t, canaryID)
	if in.runs(t, in.me) != mine || in.runs(t, alice) != hers {
		t.Fatal("the canary ran as one of several command-line persons picked at random")
	}
	receipted := in.legacyRow(t, "alice's", "", "", "")
	in.receipt(t, alice, receipted)
	if !in.ranAs(t, alice, receipted.ID) {
		t.Fatal("Alice's receipt did not make the job hers")
	}
}

// A QQ message counts as the person's direct chat only in its C2C shape.
func TestQQDirectChatNeedsTheC2CShape(t *testing.T) {
	in := newInstallation(t)
	in.webhook(t, "qq", `{"t":"C2C_MESSAGE_CREATE","channel":"group:abc","user_id":"QQME","d":{"id":"q-2","content":"hello"}}`)
	job := in.legacyRow(t, "group", "qq", "QQME", "group:abc")
	in.fire(t, job.ID)
	if got := in.deliveredOn("group:abc"); len(got) != 0 {
		t.Fatalf("a group chat was learned as a direct chat: %q", got)
	}
}

// When a job no one owns is refused, the command-line person is told once,
// in their own chat, which jobs those are and how to adopt or remove them;
// the job's prompt is never sent.
func TestUnclaimedJobsAreReportedToTheCommandLineOnce(t *testing.T) {
	in := newInstallation(t)
	in.stranger(t)
	first := in.legacyRow(t, "old report", "", "", "")
	second := in.legacyRow(t, "weekly digest", "", "", "")
	in.fire(t, first.ID)
	in.fire(t, second.ID)
	notices := in.deliveredOn("wx-me")
	if len(notices) != 1 {
		t.Fatalf("notices to the command-line person = %q", notices)
	}
	for _, want := range []string{"old report", "weekly digest", "selfmind cron adopt <id>", "remove cron job <id>"} {
		if !strings.Contains(notices[0], want) {
			t.Fatalf("notice is missing %q: %q", want, notices[0])
		}
	}
	if strings.Contains(notices[0], "summarize my day") {
		t.Fatalf("the notice carries a job's prompt: %q", notices[0])
	}
}

// Only the local command line adopts a job no one owns, after seeing what it
// does; the adopted job then runs as that person. A job someone else created
// cannot be adopted.
func TestLocalCommandLineAdoptsAnUnclaimedJob(t *testing.T) {
	in := newInstallation(t)
	stranger := in.stranger(t)
	in.daemon.Schedules = in.sched
	unclaimed := in.legacyRow(t, "old report", "", "", "")
	theirs := in.legacyRow(t, "theirs", "", "", "")
	in.receipt(t, stranger, theirs)
	adopt := func(id int64, yes bool, peer LocalPeer) (int, api.ScheduleAdoptResponse) {
		body, _ := json.Marshal(api.ScheduleAdoptRequest{Platform: "cli", PlatformUserID: "cwill", JobID: id, Adopt: yes})
		req := httptest.NewRequest(http.MethodPost, "/v1/schedules/adopt", strings.NewReader(string(body)))
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		in.daemon.Handler().ServeHTTP(rec, req.WithContext(WithLocalPeer(req.Context(), peer)))
		var resp api.ScheduleAdoptResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}
	for name, peer := range map[string]LocalPeer{
		"over tcp":            tcpPeer,
		"refused socket peer": refusedPeer,
	} {
		if status, _ := adopt(unclaimed.ID, true, peer); status != http.StatusForbidden {
			t.Fatalf("%s: status %d", name, status)
		}
	}
	if in.ranAs(t, in.me, unclaimed.ID) {
		t.Fatal("a refused adoption made the job run")
	}
	status, shown := adopt(unclaimed.ID, false, verifiedPeer)
	if status != http.StatusOK || shown.Prompt != "summarize my day" || shown.CronExpr != "0 9 * * *" || shown.Standing != "unclaimed" || shown.Adopted {
		t.Fatalf("showing the job = %d %+v", status, shown)
	}
	if status, adopted := adopt(unclaimed.ID, true, verifiedPeer); status != http.StatusOK || !adopted.Adopted {
		t.Fatalf("adoption = %d %+v", status, adopted)
	}
	if !in.ranAs(t, in.me, unclaimed.ID) {
		t.Fatal("the adopted job did not run as its adopter")
	}
	if status, _ := adopt(theirs.ID, true, verifiedPeer); status != http.StatusConflict {
		t.Fatalf("adopting another person's job: status %d", status)
	}
	explicit := in.legacyRow(t, "explicit", "weixin", "wx-me", "")
	if status, _ := adopt(explicit.ID, true, verifiedPeer); status != http.StatusConflict {
		t.Fatalf("adopting a job that names its account: status %d", status)
	}
}
