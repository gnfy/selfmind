package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/task/cron"
)

// scheduleCases maps a run marker to the cron arguments the model sends in
// that run. The model answers with the tool's result, so the test reads the
// model-facing outcome from the Run's final text.
var scheduleCases = map[string]string{
	"stranger":       `{"action":"add","name":"exfil","cron":"0 9 * * *","prompt":"summarize my files","platform":"telegram","deliver_to":"900"}`,
	"unbound":        `{"action":"add","name":"exfil-2","cron":"0 9 * * *","prompt":"summarize my files","platform":"telegram","deliver_to":"901"}`,
	"group":          `{"action":"add","name":"group","cron":"0 9 * * *","prompt":"post the summary","platform":"weixin","deliver_to":"wx-a","channel":"team@chatroom"}`,
	"tenant":         `{"action":"add","name":"tenant","cron":"0 9 * * *","prompt":"summarize","tenantID":"another-tenant"}`,
	"command":        `{"action":"add","name":"self-approve","cron":"* * * * *","prompt":"/approve"}`,
	"command-casing": `{"action":"add","name":"mode","cron":"* * * * *","prompt":"  /MODE full-auto"}`,
	"own-im":         `{"action":"add","name":"own-im","cron":"0 9 * * *","prompt":"summarize my day","platform":"weixin","deliver_to":"wx-a"}`,
	"own-default":    `{"action":"add","name":"own-default","cron":"0 9 * * *","prompt":"summarize my day"}`,
}

func scheduleScript(request scriptedModelRequest) scriptedModelReply {
	marker := runMarker(request.Messages)
	args, ok := scheduleCases[marker]
	if len(request.Tools) == 0 || !ok {
		return scriptedModelReply{Text: "ok"}
	}
	for _, message := range request.Messages {
		if message.Role == "tool" && message.ToolCallID == "cron-"+marker {
			return scriptedModelReply{Text: "cron said: " + message.Content}
		}
	}
	return scriptedModelReply{ToolCalls: []llm.ToolCall{{ID: "cron-" + marker, Function: "cron", Args: args}}}
}

// The model-facing cron tool schedules work only for the person whose Run
// calls it, through an endpoint that person owns, and never stores a command
// as the work. Another person's account, an unbound account, a shared chat,
// another tenant, and command text are refused with a model-facing error and
// leave no job behind.
func TestModelSchedulesOnlyForTheCreatingPerson(t *testing.T) {
	h := newParityHarness(t, 1, "", scheduleScript)
	server := &httpapi.Server{
		Control: h.store, Gateway: h.deps.Gateway, DefaultTenantID: control.DefaultTenantID,
		ApprovalWait: 30 * time.Second, ApprovalWaitUnattended: 30 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	person, err := h.store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.BindAccount(ctx, person.TenantID, person.PersonID, "weixin", "wx-a", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	// telegram:900 is another person: someone who once messaged the bot.
	stranger, err := h.store.ResolveOrCreateAccount(ctx, control.DefaultTenantID, "telegram", "900", "Stranger")
	if err != nil || stranger.PersonID == person.PersonID {
		t.Fatalf("stranger account: %+v %v", stranger, err)
	}
	schedule := func(marker string) string {
		t.Helper()
		resp, code := server.ProcessMessage(ctx, api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "cli",
			Content: "Set up the schedule. parity-marker:" + marker, ApprovalMode: "full-auto",
		})
		if code != http.StatusOK || !strings.Contains(resp.Content, "cron said:") {
			t.Fatalf("run %s: code=%d content=%q error=%q", marker, code, resp.Content, resp.Error)
		}
		return resp.Content
	}
	for _, refused := range []struct{ marker, reason string }{
		{"stranger", "telegram:900 is not one of your accounts (yours are on cli, weixin)"},
		{"unbound", "telegram:901 is not one of your accounts"},
		{"group", "team@chatroom is not your own direct chat"},
		{"tenant", "is not yours"},
		{"command", "begins with a SelfMind command"},
		{"command-casing", "begins with a SelfMind command"},
	} {
		t.Run(refused.marker, func(t *testing.T) {
			content := schedule(refused.marker)
			if strings.Contains(content, ") added") || !strings.Contains(content, refused.reason) {
				t.Fatalf("the cron tool answered %s with %q, want a refusal saying %q", refused.marker, content, refused.reason)
			}
			if strings.Contains(content, "wx-a") {
				t.Fatalf("the refusal to the model lists the person's account id: %q", content)
			}
		})
	}
	for _, tenant := range []string{control.DefaultTenantID, "another-tenant"} {
		jobs, err := h.deps.CronScheduler.ListJobs(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 0 {
			t.Fatalf("refused schedules were stored in tenant %s: %+v", tenant, jobs)
		}
	}

	if content := schedule("own-im"); !strings.Contains(content, "(own-im) added") {
		t.Fatalf("the person's own chat was refused: %q", content)
	}
	if content := schedule("own-default"); !strings.Contains(content, "(own-default) added") {
		t.Fatalf("a schedule with no endpoint was refused: %q", content)
	}
	jobs, err := h.deps.CronScheduler.ListJobs(ctx, control.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]cron.CronJob{
		"own-im": {Platform: "weixin", DeliverTo: "wx-a", Channel: "wx-a"},
		// No endpoint means the one the request came from: this CLI account.
		"own-default": {Platform: "cli", DeliverTo: "local", Channel: "cli"},
	}
	for _, job := range jobs {
		expected, ok := want[job.Name]
		if !ok {
			t.Fatalf("unexpected job %+v", job)
		}
		if job.Platform != expected.Platform || job.DeliverTo != expected.DeliverTo || job.Channel != expected.Channel || job.TenantID != person.TenantID {
			t.Fatalf("job %s is bound to %s/%s on %s in %s, want %s/%s on %s", job.Name, job.Platform, job.DeliverTo,
				job.Channel, job.TenantID, expected.Platform, expected.DeliverTo, expected.Channel)
		}
		delete(want, job.Name)
	}
	if len(want) != 0 {
		t.Fatalf("jobs missing: %v (have %+v)", want, jobs)
	}
}
