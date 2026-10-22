package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/task/cron"
)

// enableAdoption gives the daemon its scheduler, as the gateway runner does.
// Call it before any adoptAs.
func (in *installation) enableAdoption() {
	in.daemon.Schedules = in.sched
}

// adoptAs sends one adoption request from the person's verified local CLI as
// the command-line account user.
func (in *installation) adoptAs(t *testing.T, user string, id int64, yes bool) (int, string, api.ScheduleAdoptResponse) {
	t.Helper()
	body, _ := json.Marshal(api.ScheduleAdoptRequest{Platform: "cli", PlatformUserID: user, JobID: id, Adopt: yes})
	req := httptest.NewRequest(http.MethodPost, "/v1/schedules/adopt", strings.NewReader(string(body)))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	in.daemon.Handler().ServeHTTP(rec, asLocalCLI(req))
	var resp api.ScheduleAdoptResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, rec.Body.String(), resp
}

// Adoption is a CLI-only control: over TCP or from a refused socket peer it is
// rejected, with or without a stolen local-control token.
func TestScheduleAdoptionRequiresTheVerifiedLocalCLI(t *testing.T) {
	in := newInstallation(t)
	in.enableAdoption()
	unclaimed := in.legacyRow(t, "old report", "", "", "")
	body, _ := json.Marshal(api.ScheduleAdoptRequest{Platform: "cli", PlatformUserID: "cwill", JobID: unclaimed.ID, Adopt: true})
	for _, peer := range []LocalPeer{tcpPeer, refusedPeer} {
		req := httptest.NewRequest(http.MethodPost, "/v1/schedules/adopt", strings.NewReader(string(body)))
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-SelfMind-Local-Control-Token", "stolen")
		req = req.WithContext(WithLocalPeer(req.Context(), peer))
		rec := httptest.NewRecorder()
		in.daemon.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("peer %+v: status %d body %q", peer, rec.Code, rec.Body.String())
		}
	}
	// The verified CLI adopts it.
	if status, body, _ := in.adoptAs(t, "cwill", unclaimed.ID, true); status != http.StatusOK {
		t.Fatalf("verified adoption failed: %d %s", status, body)
	}
}

// Another person's job is never shown, not even its prompt, and cannot be
// adopted; that includes a creator in another tenant.
func TestAnotherPersonsJobIsNeitherShownNorAdopted(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	stranger := in.stranger(t)
	elsewhere, err := in.store.ResolveOrCreateAccount(ctx, "other", "telegram", "77", "Carol")
	if err != nil {
		t.Fatal(err)
	}
	in.enableAdoption()
	theirs := in.legacyRow(t, "theirs", "", "", "")
	in.receipt(t, stranger, theirs)
	carols := in.legacyRow(t, "carol's", "", "", "")
	in.receipt(t, elsewhere, carols)
	for _, id := range []int64{theirs.ID, carols.ID} {
		for _, yes := range []bool{false, true} {
			status, body, _ := in.adoptAs(t, "cwill", id, yes)
			if status != http.StatusConflict || strings.Contains(body, "summarize my day") {
				t.Fatalf("job %d (adopt=%v): status %d body %q", id, yes, status, body)
			}
		}
	}
	// The creator in the other tenant owns the job.
	review, err := ReviewSchedules(ctx, in.store, elsewhere, []cron.CronJob{in.job(t, carols.ID)})
	if err != nil || review.Own != 1 {
		t.Fatalf("Carol's review of her job = %+v %v", review, err)
	}
	// The variant: a job no one owns is shown with its prompt.
	unclaimed := in.legacyRow(t, "old report", "", "", "")
	if status, _, shown := in.adoptAs(t, "cwill", unclaimed.ID, false); status != http.StatusOK || shown.Prompt != "summarize my day" {
		t.Fatalf("an unclaimed job: %d %+v", status, shown)
	}
}

// A request naming a command-line account nobody holds is refused, and
// creates no person: a second command-line person would leave the
// installation's command line unknown.
func TestAdoptionNeedsAnExistingCommandLineAccount(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	in.stranger(t)
	in.enableAdoption()
	job := in.legacyRow(t, "old report", "", "", "")
	before, err := in.store.PersonsWithAccounts(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := in.adoptAs(t, "mallory", job.ID, false); status != http.StatusNotFound {
		t.Fatalf("an unknown command-line account: status %d", status)
	}
	if account, err := in.store.ResolveAccount(ctx, "default", "cli", "mallory"); err != nil || account != nil {
		t.Fatalf("the request created cli:mallory: %+v %v", account, err)
	}
	after, err := in.store.PersonsWithAccounts(ctx, 100)
	if err != nil || len(after) != len(before) {
		t.Fatalf("persons %d -> %d (%v)", len(before), len(after), err)
	}
	// The variant: the existing command-line account is answered.
	if status, _, _ := in.adoptAs(t, "cwill", job.ID, false); status != http.StatusOK {
		t.Fatalf("the command-line person: status %d", status)
	}
}

// Of several adoptions racing for one job, exactly one wins.
func TestOnlyOneConcurrentAdoptionWins(t *testing.T) {
	in := newInstallation(t)
	in.stranger(t)
	in.enableAdoption()
	job := in.legacyRow(t, "old report", "", "", "")
	var mu sync.Mutex
	won := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body, resp := in.adoptAs(t, "cwill", job.ID, true)
			if status != http.StatusOK && status != http.StatusConflict {
				t.Errorf("adoption: %d %s", status, body)
			}
			if resp.Adopted {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d adoptions won, want exactly one", won)
	}
}

// Doctor reads recorded decisions and records none: a decision it made itself
// would bind the daemon to whatever evidence a read-only look happened to see.
func TestDoctorReadsDecisionsAndRecordsNone(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	stranger := in.stranger(t)
	theirs := in.legacyRow(t, "theirs", "", "", "")
	in.receipt(t, stranger, theirs)
	fresh := in.legacyRow(t, "fresh", "", "", "")

	if _, err := ReviewSchedules(ctx, in.store, in.me, []cron.CronJob{in.job(t, fresh.ID)}); err != nil {
		t.Fatal(err)
	}
	if verdict, err := in.store.LegacyScheduleVerdict(ctx, "default", legacyScheduleKey(in.job(t, fresh.ID))); err != nil || verdict != "" {
		t.Fatalf("doctor recorded %q (%v)", verdict, err)
	}

	// The daemon decides the stranger's job; a reset then removes the
	// receipt, so deciding again would call it no one's.
	if line := in.listed(t, stranger, theirs); line == "" {
		t.Fatal("the creator does not see the job")
	}
	in.resetWorkHistory(t)
	review, err := ReviewSchedules(ctx, in.store, in.me, []cron.CronJob{in.job(t, theirs.ID)})
	if err != nil || review.Others != 1 || review.Unclaimed != 0 || len(review.Stuck) != 0 {
		t.Fatalf("doctor did not read the recorded decision: %+v %v", review, err)
	}
}
