package httpapi

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
)

// The tool events stored in control.db carry the call's arguments and a
// preview of its result. Both are masked before they are stored: credentials
// recognized by shape, by any spelling of their name, behind any JSON
// escaping, and registered secrets that overlap; and masking happens before
// the preview is cut, so a cut cannot leave the start of a secret no longer
// recognizable as one.
func TestStoredToolEventsAreRedactedBeforeTheyAreCut(t *testing.T) {
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Stored", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "run command")
	if err != nil {
		t.Fatal(err)
	}
	tools.RegisterSensitiveValue("stored-r10-alpha-beta")
	tools.RegisterSensitiveValue("beta.gamma-r10-tail")
	tools.RegisterSensitiveValue("stored-r10-7d102e-opaque-value")
	tools.RegisterSensitiveValue(`wpr"stored+r10-secret`)
	const token = "ghp_R7k2Qx9LmN4pT8vW1yZ3aB6cD0eF5gH2jK7s"
	// esc starts a JSON \uXXXX escape the way other producers write them.
	const esc = "\\u"
	basic := base64.StdEncoding.EncodeToString([]byte("stored-user:stored-pass-r10"))
	server := &Server{Control: store, DefaultTenantID: "default"}
	record := func(event llm.StreamEvent) {
		server.coordinator().recordStreamEvent(ctx, "cli", task, run, event)
	}
	record(llm.StreamEvent{EventType: "tool.started", ToolName: "terminal", ToolCallID: "c1",
		ToolArgs: `{"command":"curl -H 'Authorization: Basic ` + basic + `' https://example.test && mysql --password v7d102e-stored-flag"}`})
	record(llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", ToolCallID: "c1",
		ToolResult: `{"p` + esc + `0061ssword":"v7d102e-stored-obfuscated","note":"wpr` + esc + `0022stored` + esc + `002Br10-secret","ids":"stored-r10-alpha-beta.gamma-r10-tail"}`})
	record(llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", ToolCallID: "c2",
		ToolResult: strings.Repeat("x", 990) + " " + token})
	record(llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", ToolCallID: "c3",
		ToolResult: strings.Repeat("y", 990) + " stored-r10-7d102e-opaque-value"})

	events, err := store.ListTaskEvents(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, event := range events {
		if event.Type != "tool.started" && event.Type != "tool.completed" {
			continue
		}
		stored++
		for _, hidden := range []string{basic, "v7d102e-stored-flag", "v7d102e-stored-obfuscated", "r10-secret",
			"alpha", "gamma", "R7k2Q", "stored-r1"} {
			if strings.Contains(string(event.Payload), hidden) {
				t.Errorf("a stored %s event leaked %q: %s", event.Type, hidden, event.Payload)
			}
		}
	}
	if stored != 4 {
		t.Fatalf("stored %d tool events, want 4", stored)
	}
}

// The formats added to redaction reach the events stored in control.db the
// same way: a private key's body, a URL's password, a cookie, a command-line
// password and a value spelled with terminal color codes.
func TestStoredToolEventsMaskPrivateKeysAndCommonFormats(t *testing.T) {
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Formats", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "run command")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, DefaultTenantID: "default"}
	record := func(event llm.StreamEvent) {
		server.coordinator().recordStreamEvent(ctx, "cli", task, run, event)
	}
	record(llm.StreamEvent{EventType: "tool.started", ToolName: "terminal", ToolCallID: "f1",
		ToolArgs: `{"command":"curl -u deploy:r14curlpw7d102e https://example.test && mysql -u root -pr14mysql7d102e db && git clone https://bot:r14urlpw7d102e@example.test/x.git"}`})
	record(llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", ToolCallID: "f1",
		ToolResult: "id_key:\n-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("r14StoredKeyBodyLine0123456789abcdefghijklmnopqrstuvwxyz\n", 30) + "-----END OPENSSH PRIVATE KEY-----"})
	record(llm.StreamEvent{EventType: "tool.completed", ToolName: "terminal", ToolCallID: "f2",
		ToolResult: "< Set-Cookie: session=r14cookie7d102e; Path=/\n\x1b[1mpassword\x1b[0m: \x1b[32mr14color7d102e\x1b[0m"})
	events, err := store.ListTaskEvents(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, event := range events {
		if event.Type != "tool.started" && event.Type != "tool.completed" {
			continue
		}
		stored++
		for _, hidden := range []string{"r14curlpw", "r14mysql", "r14urlpw", "r14StoredKeyBody", "r14cookie", "r14color"} {
			if strings.Contains(string(event.Payload), hidden) {
				t.Errorf("a stored %s event leaked %q: %s", event.Type, hidden, event.Payload)
			}
		}
	}
	if stored != 3 {
		t.Fatalf("stored %d tool events, want 3", stored)
	}
}
