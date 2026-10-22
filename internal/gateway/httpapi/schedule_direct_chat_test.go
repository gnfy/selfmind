package httpapi

// On Feishu, Lark, and QQ a person's direct chat with SelfMind has its own id,
// not the person's user id: the Feishu parser records the union id ("on_") as
// the user and the direct chat is "oc_"; a QQ C2C chat is "c2c:<openid>". The
// direct chat is learned from a message the platform marks as sent there, and
// a schedule for that account accepts it and delivers there. A chat the
// platform marks as a group is never learned as anyone's direct chat.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/task/cron"
)

func (in *installation) webhook(t *testing.T, platform, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	in.daemon.handleIMWebhook(rec, httptest.NewRequest(http.MethodPost, "/v1/im/"+platform, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s webhook: %d %s", platform, rec.Code, rec.Body.String())
	}
}

func feishuMessage(eventID, chatID, chatType string) string {
	return `{"header":{"event_id":"` + eventID + `"},"event":{"sender":{"sender_id":{"union_id":"on_me","open_id":"ou_me"}},` +
		`"message":{"message_id":"m-` + eventID + `","chat_id":"` + chatID + `","chat_type":"` + chatType + `","content":"{\"text\":\"hello\"}"}}}`
}

func (in *installation) deliveredOn(channel string) []string {
	var out []string
	for _, msg := range in.sender.snapshot() {
		if msg.Channel == channel {
			out = append(out, msg.Content)
		}
	}
	return out
}

func TestFeishuScheduleDeliversToTheLearnedDirectChat(t *testing.T) {
	in := newInstallation(t)
	ctx := context.Background()
	in.webhook(t, "feishu", feishuMessage("ev-1", "oc_direct", "p2p"))
	// A group message from the same person teaches nothing.
	in.webhook(t, "feishu", feishuMessage("ev-2", "oc_group", "group"))
	person, err := in.store.ResolveAccount(ctx, "default", "feishu", "on_me")
	if err != nil || person == nil {
		t.Fatalf("feishu account: %+v %v", person, err)
	}

	// The cron tool in that chat accepts the direct chat and stores the user;
	// the group is refused.
	scope := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, AccountID: person.AccountID}
	authority := NewScheduleAuthority(in.store)
	if endpoint, err := authority.Bind(ctx, scope, cron.JobRequest{Channel: "oc_direct", Prompt: "summarize my day"}); err != nil ||
		endpoint.DeliverTo != "on_me" {
		t.Fatalf("Bind in the direct chat = %+v, %v", endpoint, err)
	}
	if _, err := authority.Bind(ctx, scope, cron.JobRequest{Channel: "oc_group", Prompt: "summarize my day"}); err == nil {
		t.Fatal("Bind accepted a group chat")
	}

	// A job the older tool stored with the real direct chat runs and
	// delivers there.
	direct := in.legacyRow(t, "daily", "feishu", "on_me", "oc_direct")
	in.receipt(t, person, direct)
	before := len(in.deliveredOn("oc_direct"))
	in.fire(t, direct.ID)
	if got := in.deliveredOn("oc_direct"); len(got) != before+1 || !strings.Contains(got[len(got)-1], "Scheduled turn finished.") {
		t.Fatalf("the direct-chat job delivered %q", got)
	}
	// One with no chat is delivered to the direct chat, not the user id.
	plain := in.legacyRow(t, "plain", "feishu", "on_me", "")
	in.receipt(t, person, plain)
	in.fire(t, plain.ID)
	if got := in.deliveredOn("oc_direct"); len(got) != before+2 {
		t.Fatalf("a job with no chat was not delivered to the direct chat: %q", got)
	}
	// The variant: the group is refused, turned off, and the person is told
	// in their direct chat; nothing reaches the group.
	group := in.legacyRow(t, "team digest", "feishu", "on_me", "oc_group")
	in.receipt(t, person, group)
	in.fire(t, group.ID)
	if in.job(t, group.ID).Enabled || len(in.deliveredOn("oc_group")) != 0 {
		t.Fatalf("the group job stayed on or reached the group: %q", in.deliveredOn("oc_group"))
	}
	if got := in.deliveredOn("oc_direct"); len(got) != before+3 || !strings.Contains(got[len(got)-1], "oc_group") {
		t.Fatalf("the refusal notice did not reach the direct chat: %q", got)
	}
}

func TestQQScheduleDeliversToTheC2CChat(t *testing.T) {
	in := newInstallation(t)
	in.webhook(t, "qq", `{"t":"C2C_MESSAGE_CREATE","d":{"id":"q-1","content":"hello","author":{"user_openid":"QQME"}}}`)
	person, err := in.store.ResolveAccount(context.Background(), "default", "qq", "QQME")
	if err != nil || person == nil {
		t.Fatalf("QQ account: %+v %v", person, err)
	}
	job := in.legacyRow(t, "daily", "qq", "QQME", "c2c:QQME")
	in.receipt(t, person, job)
	in.fire(t, job.ID)
	if got := in.deliveredOn("c2c:QQME"); len(got) != 1 || !strings.Contains(got[0], "Scheduled turn finished.") {
		t.Fatalf("the C2C job delivered %q", got)
	}
	// A guild channel is not the person's direct chat.
	guild := in.legacyRow(t, "guild", "qq", "QQME", "channel:42")
	in.receipt(t, person, guild)
	in.fire(t, guild.ID)
	if in.job(t, guild.ID).Enabled || len(in.deliveredOn("channel:42")) != 0 {
		t.Fatal("a guild-channel job ran or stayed on")
	}
}
