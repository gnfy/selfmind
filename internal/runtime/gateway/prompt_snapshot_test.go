package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/promptassets"
)

func TestRecordPromptSnapshotLoadedStoresMetadataOnly(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := filepath.Join(t.TempDir(), "prompts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte("## Persona\n\nPRIVATE-STATIC-CONTENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := promptassets.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	recordPromptSnapshotLoaded(store, "instance-test", snapshot, app.PromptSnapshotStatus{
		Source:       app.PromptSourceActive,
		ActiveRoot:   snapshot.Root(),
		SnapshotHash: snapshot.Hash(),
	})
	event, err := store.LatestGatewayRuntimeEvent(context.Background(), "prompt.snapshot.loaded")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		SnapshotHash     string `json:"snapshot_hash"`
		Source           string `json:"source"`
		Degraded         bool   `json:"degraded"`
		BuildFingerprint string `json:"build_fingerprint"`
		Files            []struct {
			ID string `json:"id"`
		} `json:"files"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SnapshotHash != snapshot.Hash() || payload.Source != app.PromptSourceActive || payload.Degraded || payload.BuildFingerprint == "" || len(payload.Files) != len(promptassets.Catalog()) {
		t.Fatalf("payload = %s", event.Payload)
	}
	if string(event.Payload) == "" || json.Valid(event.Payload) == false {
		t.Fatalf("invalid payload: %s", event.Payload)
	}
	if strings.Contains(string(event.Payload), "PRIVATE-STATIC-CONTENT") {
		t.Fatalf("prompt content leaked into runtime event: %s", event.Payload)
	}
}

func TestRecordPromptSnapshotLoadedPersistsDegradedFinding(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot := promptassets.Empty(filepath.Join(t.TempDir(), "prompts"))
	recordPromptSnapshotLoaded(store, "instance-degraded", snapshot, app.PromptSnapshotStatus{
		Source:          app.PromptSourceBuiltIn,
		ActiveRoot:      snapshot.Root(),
		SnapshotHash:    snapshot.Hash(),
		ActiveErrorKind: "invalid_content",
		ActiveError:     "invalid prompt agent: unknown section",
	})
	event, err := store.GatewayRuntimeEventForInstance(context.Background(), "instance-degraded", "prompt.workspace.degraded")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Source    string `json:"source"`
		Degraded  bool   `json:"degraded"`
		ErrorKind string `json:"active_error_kind"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Source != app.PromptSourceBuiltIn || !payload.Degraded || payload.ErrorKind != "invalid_content" {
		t.Fatalf("payload = %s", event.Payload)
	}
}
