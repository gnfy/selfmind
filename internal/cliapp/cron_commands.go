package cliapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"selfmind/internal/gateway/api"
)

const cronUsage = "usage: selfmind cron adopt <job_id> [--yes]"

func (a *App) runCronCommandIfRequested() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "cron" {
		return false, 0
	}
	if len(a.args) >= 3 && a.args[2] == "adopt" {
		return true, a.cronAdopt(a.args[3:])
	}
	fmt.Fprintln(a.stderr, cronUsage)
	return true, 2
}

// cronAdopt shows a schedule no one can be shown to own — what it runs and
// when — and, with --yes, records that it is the local person's. It reaches
// the daemon only through the local-control endpoint, so neither a model nor
// a daemon turn can adopt a job.
func (a *App) cronAdopt(args []string) int {
	yes, id := false, int64(0)
	for _, arg := range args {
		switch {
		case arg == "--yes" || arg == "-yes":
			yes = true
		case id == 0:
			parsed, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || parsed <= 0 {
				fmt.Fprintln(a.stderr, cronUsage)
				return 2
			}
			id = parsed
		default:
			fmt.Fprintln(a.stderr, cronUsage)
			return 2
		}
	}
	if id == 0 {
		fmt.Fprintln(a.stderr, cronUsage)
		return 2
	}
	a.ensureLocalGateway()
	resp, err := a.postScheduleAdopt(id, yes)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	state := "off"
	if resp.Enabled {
		state = "on"
	}
	fmt.Fprintf(a.stdout, "Schedule %d %q (%s)\nWhen: %s\nPrompt: %s\n", resp.JobID, resp.Name, state, resp.CronExpr, resp.Prompt)
	switch {
	case resp.Adopted:
		fmt.Fprintf(a.stdout, "Schedule %d is now yours and runs as you. It stays %s until you change it: selfmind send \"turn on cron job %d\"\n",
			resp.JobID, state, resp.JobID)
	case resp.Standing == "yours":
		fmt.Fprintf(a.stdout, "Schedule %d is already yours.\n", resp.JobID)
	default:
		fmt.Fprintf(a.stdout, "No one here can be shown to have created it. If it is yours, run: selfmind cron adopt %d --yes\n", resp.JobID)
	}
	return 0
}

func (a *App) postScheduleAdopt(id int64, adopt bool) (api.ScheduleAdoptResponse, error) {
	body, _ := json.Marshal(api.ScheduleAdoptRequest{
		TenantID:       os.Getenv("SELF_TENANT_ID"),
		Platform:       "cli",
		PlatformUserID: platformUserID(),
		JobID:          id,
		Adopt:          adopt,
	})
	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, a.gatewayURL()+"/v1/schedules/adopt", bytes.NewReader(body))
	if err != nil {
		return api.ScheduleAdoptResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(req)
	httpResp, err := a.gatewayHTTPClient().Do(req)
	if err != nil {
		return api.ScheduleAdoptResponse{}, err
	}
	defer httpResp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if httpResp.StatusCode >= 400 {
		return api.ScheduleAdoptResponse{}, fmt.Errorf("%s", a.gatewayErrorLine(httpResp.Status, data))
	}
	var out api.ScheduleAdoptResponse
	if err := json.Unmarshal(bytes.TrimSpace(data), &out); err != nil {
		return api.ScheduleAdoptResponse{}, fmt.Errorf("adopt: %s", strings.TrimSpace(err.Error()))
	}
	return out, nil
}
