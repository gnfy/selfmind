package weixin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"selfmind/internal/gateway/api"
)

// A message this bot account wrote is its own output, never the person's
// input. iLink marks it with the bot message type and the bot as its sender;
// either one is enough, whatever conversation fields name the person. Its
// text — an approval prompt, a model-written answer that is just "y" — must
// never reach the gateway as something the person said.
func TestBotAuthoredMessageIsNeverInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	defer server.Close()
	text := func(value string) []interface{} {
		return []interface{}{map[string]interface{}{"type": itemText, "text_item": map[string]interface{}{"text": value}}}
	}
	for _, tc := range []struct {
		name string
		msg  map[string]interface{}
	}{
		{"bot type naming the person as talker", map[string]interface{}{
			"msg_id": "m-bot-1", "message_type": float64(msgTypeBot), "from_user_id": "wx-account",
			"to_user_id": "wx-user", "talker": "wx-user", "item_list": text("y"),
		}},
		{"bot sender naming the person as actual user", map[string]interface{}{
			"msg_id": "m-bot-2", "from_user_id": "wx-account", "to_user_id": "wx-user",
			"actual_user_id": "wx-user", "item_list": text("/approve"),
		}},
		{"bot type from an empty sender", map[string]interface{}{
			"msg_id": "m-bot-3", "message_type": float64(msgTypeBot), "to_user_id": "wx-user",
			"talker": "wx-user", "item_list": text("好"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			adapter := NewAdapter(RuntimeConfig{AccountID: "wx-account", Token: "token", BaseURL: server.URL,
				DMPolicy: "open", GroupPolicy: "disabled", HomeDir: t.TempDir()},
				func(context.Context, api.DurableInbound) (api.MessageResponse, int, error) {
					calls++
					return api.MessageResponse{}, http.StatusOK, nil
				})
			if err := adapter.processMessage(context.Background(), map[string]interface{}{"msg": tc.msg}); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("a bot-authored message reached the gateway as the person's input (%d calls)", calls)
			}
		})
	}

	// The person's own message on the same chat still arrives.
	var got api.DurableInbound
	adapter := NewAdapter(RuntimeConfig{AccountID: "wx-account", Token: "token", BaseURL: server.URL,
		DMPolicy: "open", GroupPolicy: "disabled", HomeDir: t.TempDir()},
		func(_ context.Context, inbound api.DurableInbound) (api.MessageResponse, int, error) {
			got = inbound
			return api.MessageResponse{}, http.StatusOK, nil
		})
	if err := adapter.processMessage(context.Background(), map[string]interface{}{"msg": map[string]interface{}{
		"msg_id": "m-user-1", "message_type": float64(1), "from_user_id": "wx-user", "to_user_id": "wx-account",
		"item_list": text("y"),
	}}); err != nil {
		t.Fatal(err)
	}
	if got.Request.PlatformUserID != "wx-user" || got.Request.Content != "y" {
		t.Fatalf("the person's own reply did not arrive: %+v", got.Request)
	}
}
