package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

// imChoiceCase seeds a pending multi-Run choice and an approval message on one
// IM chat, then sends the number of the "new work" option from that chat with
// the given edge lines.
func imChoiceCase(id, platform, channel, edge string, sentMessage bool) string {
	sent := ""
	if sentMessage {
		sent = "  sent_messages:\n    - platform: " + platform + "\n      channel: " + channel +
			"\n      message_id: msg-approval-1\n      approval_id: approval_missing\n      content: \"Approve the export script?\"\n"
	}
	return "id: " + id + "\n" +
		"title: \"IM choice and approval edge\"\n" +
		"suite: reliability\n" +
		"workspace: isolated\n" +
		"model_required: false\n" +
		"setup:\n" +
		"  task:\n    title: \"Nightly report\"\n    parked_runs:\n      - input: \"Compile the nightly report\"\n        status: waiting_user\n" +
		"  turn_choices:\n    - platform: " + platform + "\n      channel: " + channel + "\n      request: \"Also attach the error summary\"\n" +
		"      options:\n        - action: resume\n          parked_run: 1\n        - action: new\n" +
		sent +
		"turns:\n  - input: \"2\"\n    platform: " + platform + "\n    channel: " + channel + "\n" + edge +
		"expect:\n  max_duration_seconds: 30\n" +
		"assert_state:\n" +
		"  - on: runs\n    count_lte: 1\n" +
		"  - on: turn_choices\n    status: pending\n    count_gte: 1\n" +
		"  - on: queued\n    count_lte: 0\n" +
		"checks:\n  no_mojibake: true\n"
}

// A native IM reply to an approval message carries that approval edge. The
// gateway answers it as an approval reply and never lets the number answer
// the multi-Run choice pending on the same chat, so no work is queued or run.
func TestRunCaseNativeApprovalReplyNeverAnswersASeededChoice(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	for _, tc := range []struct{ platform, channel string }{
		{"weixin", "wx-chat-1"},
		// Another platform and chat keep the same invariant.
		{"telegram", "-100200300"},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			_, cfgPath := writeRunnerFixtures(t)
			edge := "    native_reply_message_id: msg-approval-1\n"
			c := loadCaseYAML(t, "im_native_approval_edge", imChoiceCase("im_native_approval_edge", tc.platform, tc.channel, edge, true))
			result, output := runLoadedCase(t, cfgPath, c)
			if result.Status != "passed" {
				data, _ := os.ReadFile(output)
				t.Fatalf("native approval edge case failed: %v\n%s", failedChecks(result), data)
			}
		})
	}
}

// Without an edge, the same number from the same chat does answer the seeded
// choice through the production multi-Run path: this proves the seed is a
// real pending multi-Run choice, and it is the constraint that must change the
// result of the case above.
func TestRunCaseBareNumberAnswersASeededMultiRunChoice(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	c := loadCaseYAML(t, "im_bare_number_choice", imChoiceCase("im_bare_number_choice", "weixin", "wx-chat-1", "", false))
	c.Expect.Contains = []string{"Saved your request as queued work"}
	result, output := runLoadedCase(t, cfgPath, c)
	if check, ok := checkByName(result, "contains:Saved your request as queued work"); !ok || !check.OK {
		data, _ := os.ReadFile(output)
		t.Fatalf("the seeded choice was not routed as a multi-Run choice: %+v\n%s", check, data)
	}
	for _, name := range []string{"state:turn_choices:pending", "state:queued"} {
		if check, ok := checkByName(result, name); !ok || check.OK {
			t.Fatalf("%s = %+v present=%v, want the answered choice to resolve and queue work", name, check, ok)
		}
	}
}

// A turn from another platform with the case user is the case person on that
// endpoint and sees the person's work; a different user on that platform is a
// stranger unless setup.accounts binds it.
func TestRunCaseEndpointTurnsActAsTheCasePerson(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full gateway harness")
	}
	_, cfgPath := writeRunnerFixtures(t)
	content := "id: endpoint_accounts\nsuite: reliability\nworkspace: isolated\nmodel_required: false\n" +
		"setup:\n  accounts:\n    - platform: weixin\n      platform_user_id: wx-second-phone\n" +
		"  task:\n    title: \"Private release audit\"\n    parked_runs:\n      - input: \"audit the release\"\n        status: interrupted\n" +
		"turns:\n" +
		"  - input: \"/status\"\n    platform: weixin\n    channel: wx-chat-1\n" +
		"  - input: \"/status\"\n    platform: weixin\n    platform_user_id: wx-second-phone\n    channel: wx-chat-2\n" +
		"    assert_state:\n      - on: runs\n        count_gte: 1\n" +
		"  - input: \"/status\"\n    platform: weixin\n    platform_user_id: wx-stranger\n    channel: wx-chat-3\n" +
		"    assert_state:\n      - on: runs\n        count_lte: 0\n" +
		"expect:\n  max_duration_seconds: 30\n  contains:\n    - \"Private release audit\"\n    - \"No active task.\"\n"
	c := loadCaseYAML(t, "endpoint_accounts", content)
	result, output := runLoadedCase(t, cfgPath, c)
	if result.Status != "passed" {
		data, _ := os.ReadFile(output)
		t.Fatalf("endpoint account case failed: %v\n%s", failedChecks(result), data)
	}
}

// A seeded sent message resolves as a native reply edge to the seeded Run, on
// its own chat only.
func TestApplyEndpointSeedsLinksSentMessagesToTheirEdges(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := loadCaseYAML(t, "sent_message_edges", "id: sent_message_edges\nworkspace: isolated\n"+
		"setup:\n  task:\n    parked_runs:\n      - input: \"first\"\n"+
		"  sent_messages:\n    - platform: weixin\n      channel: wx-chat-1\n      message_id: m-run\n      parked_run: 1\n"+
		"    - platform: weixin\n      channel: wx-chat-1\n      message_id: m-clarify\n      clarify_id: clarify_missing\n"+
		"turns:\n  - input: \"/status\"\n")
	identity, err := store.ResolveOrCreateAccount(ctx, "eval-tenant", "eval", c.caseUser(), "SelfMind Eval")
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := bindPersonAccounts(ctx, store, c, identity)
	if err != nil {
		t.Fatal(err)
	}
	var parked []string
	taskID, err := applyStateSeeds(ctx, store, nil, identity, "", t.TempDir(), "cli", c.Setup, &parked)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyEndpointSeeds(ctx, store, c, endpointSeedContext{accounts: accounts, taskID: taskID, parkedRuns: parked}); err != nil {
		t.Fatal(err)
	}
	runEdge, err := store.NativeIMReplyTarget(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-chat-1", "m-run")
	if err != nil || runEdge == nil || runEdge.RunID != parked[0] {
		t.Fatalf("run edge = %+v err=%v, want run %s", runEdge, err, parked[0])
	}
	clarifyEdge, err := store.NativeIMReplyTarget(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-chat-1", "m-clarify")
	if err != nil || clarifyEdge == nil || clarifyEdge.ClarifyID != "clarify_missing" {
		t.Fatalf("clarify edge = %+v err=%v", clarifyEdge, err)
	}
	if edge, err := store.NativeIMReplyTarget(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-chat-2", "m-run"); err == nil || edge != nil {
		t.Fatalf("a message id resolved on another chat: %+v", edge)
	}
}

// A seeded choice stores the request snapshot the gateway itself writes for a
// multi-Run choice, with every field an eval turn sends, so choosing a number
// resubmits the same request a real choice would.
func TestSeededTurnChoiceStoresTheGatewayRequest(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := loadCaseYAML(t, "seeded_choice_request", "id: seeded_choice_request\nworkspace: isolated\napproval_mode: smart\n"+
		"setup:\n  turn_choices:\n    - platform: weixin\n      channel: wx-chat-1\n      request: \"Also attach the error summary\"\n"+
		"      options:\n        - action: new\n        - action: new\n"+
		"turns:\n  - input: \"/status\"\n")
	identity, err := store.ResolveOrCreateAccount(ctx, "eval-tenant", "eval", c.caseUser(), "SelfMind Eval")
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := bindPersonAccounts(ctx, store, c, identity)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	ids, err := applyEndpointSeeds(ctx, store, c, endpointSeedContext{accounts: accounts, workspace: workspace})
	if err != nil || len(ids) != 1 {
		t.Fatalf("seeded choices = %v err=%v", ids, err)
	}
	choice, err := store.PeekPendingTurnChoice(ctx, identity.TenantID, identity.PersonID, ids[0], time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]interface{}
	if err := json.Unmarshal([]byte(choice.RequestJSON), &request); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"kind": "multi_active", "content": "Also attach the error summary", "platform": "weixin",
		"platform_user_id": c.caseUser(), "channel": "wx-chat-1", "client_cwd": workspace, "approval_mode": "smart",
	} {
		if got, _ := request[field].(string); got != want {
			t.Fatalf("seeded request %s = %q, want %q (request %s)", field, got, want, choice.RequestJSON)
		}
	}
	if choice.Channel != "wx-chat-1" || choice.AccountID != accounts[c.accountFor("weixin", "")].AccountID {
		t.Fatalf("choice asked on %q of account %q, want the weixin account on wx-chat-1", choice.Channel, choice.AccountID)
	}
}

func TestEndpointSeedValidation(t *testing.T) {
	base := "id: seeds\nworkspace: isolated\nturns:\n  - input: \"/status\"\n"
	for _, tc := range []struct {
		name, setup, want string
	}{
		{"choice account not the person's", "setup:\n  turn_choices:\n    - platform: weixin\n      platform_user_id: wx-other\n      channel: c\n      request: r\n      options:\n        - action: new\n        - action: new\n", "not the case person's"},
		{"choice option without its run", "setup:\n  turn_choices:\n    - channel: c\n      request: r\n      options:\n        - action: resume\n        - action: new\n", "needs parked_run"},
		{"choice with one option", "setup:\n  turn_choices:\n    - channel: c\n      request: r\n      options:\n        - action: new\n", "2 to 8 options"},
		{"unknown choice action", "setup:\n  turn_choices:\n    - channel: c\n      request: r\n      options:\n        - action: observe\n        - action: new\n", "steer, resume or new"},
		{"sent message without id", "setup:\n  sent_messages:\n    - channel: c\n", "message_id"},
		{"repeated sent message", "setup:\n  sent_messages:\n    - channel: c\n      message_id: m\n    - channel: c\n      message_id: m\n", "repeats message"},
		{"account without user", "setup:\n  accounts:\n    - platform: weixin\n", "platform_user_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "case.yaml")
			if err := os.WriteFile(path, []byte(base+tc.setup), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCase error = %v, want %q", err, tc.want)
			}
		})
	}
}
