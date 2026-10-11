package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/kernel/task/cron"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/textutil"
)

// A job no one can be shown to own does not run, and the installation's
// command line is the only place that may take it on: its person is told, and
// may adopt it on the local command line after seeing what it does. No model
// and no daemon turn can adopt a job.

// namesNoAccount reports whether a stored job names no account: the row an
// older release ran as the "local" command-line account.
func namesNoAccount(job cron.CronJob) bool {
	return strings.EqualFold(firstNonEmptyString(job.Platform, job.Channel, "cli"), "cli") && strings.TrimSpace(job.DeliverTo) == ""
}

// noticeUnclaimedSchedules tells the installation's command-line person, once
// per set of jobs, which jobs no one can be shown to own do not run, and how to
// adopt or remove them. It names the jobs, never their prompts.
func (d *Server) noticeUnclaimedSchedules(ctx context.Context, sched *cron.Scheduler, tenantID string) {
	if d == nil || d.Control == nil || d.Delivery == nil || sched == nil {
		return
	}
	cli, err := installationCommandLine(ctx, d.Control)
	if err != nil || cli.person == "" {
		log.Warn("cron: no command-line person to tell about unclaimed schedules", "error", err)
		return
	}
	person, err := commandLineAccount(ctx, d.Control, cli.person)
	if err != nil || person == nil {
		return
	}
	jobs, err := sched.ListJobs(ctx, tenantID)
	if err != nil {
		log.Warn("cron: unclaimed schedules could not be listed", "error", err)
		return
	}
	var lines, keys []string
	for _, job := range jobs {
		view, err := viewSchedule(ctx, d.Control, job, true)
		if err != nil {
			log.Warn("cron: schedule could not be read", "job_id", job.ID, "error", err)
			return
		}
		if view.owner != "" || view.refusal == "" {
			continue
		}
		state := "off"
		if job.Enabled {
			state = "on until it comes due"
		}
		if namesNoAccount(job) && strings.TrimSpace(job.SystemKey) == "" {
			state += ", adoptable"
		}
		lines = append(lines, fmt.Sprintf("- %d %q (%s)", job.ID, textutil.Truncate(toOneLine(job.Name), 60), state))
		keys = append(keys, firstNonEmptyString(legacyScheduleKey(job), fmt.Sprint(job.ID)))
	}
	if len(lines) == 0 {
		return
	}
	text := "Scheduled jobs that no one here can be shown to have created do not run; each is turned off when it comes due:\n" +
		strings.Join(lines, "\n") +
		"\nIf an adoptable one is yours, see what it does and adopt it on this computer: selfmind cron adopt <id>. " +
		"Otherwise remove it: selfmind send \"remove cron job <id>\"."
	if !d.coordinator().deliverToPreferredIM(ctx, person, delivery.Message{TenantID: person.TenantID, PersonID: person.PersonID,
		Content: text, LogicalKey: "schedules-unclaimed:" + strings.Join(keys, ",")}) {
		log.Warn("cron: unclaimed schedules notice not confirmed; selfmind doctor lists them", "jobs", len(lines))
	}
}

// handleScheduleAdopt (POST /v1/schedules/adopt) shows the local CLI person a
// schedule that names no account and, when asked, records that it is theirs.
// Only the authenticated local CLI reaches it.
func (d *Server) handleScheduleAdopt(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.ScheduleAdoptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if !d.localCLIRequest(r, req.Platform) {
		http.Error(w, "a schedule can only be adopted from the authenticated local CLI", http.StatusForbidden)
		return
	}
	if d.Schedules == nil || d.Control == nil {
		http.Error(w, "scheduling is not available", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	// A request never creates a person: a second command-line person would
	// leave the installation's command line unknown.
	user := fallback(req.PlatformUserID, "local")
	identity, err := d.Control.ResolveAccount(ctx, d.tenantID(req.TenantID), "cli", user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if identity == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("no command-line account cli:%s is bound here; use SelfMind from this command line first", user))
		return
	}
	job, found, err := d.storedSchedule(ctx, identity.TenantID, req.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("no schedule %d", req.JobID))
		return
	}
	if !namesNoAccount(job) || strings.TrimSpace(job.SystemKey) != "" || legacyScheduleKey(job) == "" {
		writeError(w, http.StatusConflict, fmt.Errorf("schedule %d names its account; only a schedule that names none can be adopted", job.ID))
		return
	}
	view, err := viewSchedule(ctx, d.Control, job, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	standing := scheduleStanding(view, identity)
	if standing == "someone else's" {
		// Another person's job is theirs to see: not even its prompt is shown.
		writeError(w, http.StatusConflict, fmt.Errorf("schedule %d was created by someone else; it cannot be shown or adopted", job.ID))
		return
	}
	resp := api.ScheduleAdoptResponse{JobID: job.ID, Name: job.Name, CronExpr: job.CronExpr, Prompt: job.Prompt,
		Enabled: job.Enabled, Standing: standing}
	if req.Adopt && standing == "unclaimed" {
		adopted, err := d.Control.ReplaceLegacyScheduleVerdict(ctx, job.TenantID, legacyScheduleKey(job),
			legacyVerdictUnclaimed, legacyVerdictAdopted+identity.PersonID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !adopted {
			writeError(w, http.StatusConflict, fmt.Errorf("schedule %d was claimed meanwhile; run `selfmind cron adopt %d` again to see whose it is", job.ID, job.ID))
			return
		}
		log.Info("cron: schedule adopted on the local command line", "job_id", job.ID, "person_id", identity.PersonID)
		resp.Adopted, resp.Standing = true, "yours"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Server) storedSchedule(ctx context.Context, tenantID string, id int64) (cron.CronJob, bool, error) {
	jobs, err := d.Schedules.ListJobs(ctx, tenantID)
	if err != nil {
		return cron.CronJob{}, false, err
	}
	for _, job := range jobs {
		if job.ID == id {
			return job, true, nil
		}
	}
	return cron.CronJob{}, false, nil
}

func scheduleStanding(view scheduleView, identity *control.IdentityContext) string {
	switch view.owner {
	case "":
		return "unclaimed"
	case identity.PersonID:
		return "yours"
	default:
		return "someone else's"
	}
}
