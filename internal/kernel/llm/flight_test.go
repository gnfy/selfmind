package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeStreamProvider emits one event so the recorder has something to save.
type fakeStreamProvider struct{}

func (fakeStreamProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Content: "hi", FinishReason: "stop"}
	close(ch)
	return ch, nil
}
func (fakeStreamProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (fakeStreamProvider) ChatCompletion(context.Context, []Message) (string, error) { return "", nil }

func TestFlightRecorderWritesCassette(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SELFMIND_FLIGHT_RECORDER", "1")
	t.Setenv("SELFMIND_FLIGHT_DIR", dir)
	// eval VCR must be unset so the flight branch is taken.
	t.Setenv("SELFMIND_EVAL_VCR", "")

	p := MaybeWrapVCR(fakeStreamProvider{})
	if _, ok := p.(*vcrProvider); !ok {
		t.Fatalf("flight mode should wrap the provider, got %T", p)
	}
	ctx := WithVCRSession(context.Background(), "flight-test-1")
	out, err := p.StreamChat(ctx, ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range out { // drain so the recorder flushes
	}
	if _, err := os.Stat(filepath.Join(dir, "flight-test-1", "0000.json")); err != nil {
		t.Fatalf("expected a recorded cassette: %v", err)
	}
}

func TestWriteFlightMetaSecuresExistingRecordings(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SELFMIND_FLIGHT_DIR", root)

	oldDir := filepath.Join(root, "flight-old")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(oldDir, "0000.json")
	if err := os.WriteFile(oldFile, []byte(`{"method":"stream"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(oldFile, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteFlightMeta(FlightMeta{TurnID: "flight-new"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(oldDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("old flight dir mode = %v, want 0700", info.Mode().Perm())
	}
	info, err = os.Stat(oldFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("old cassette mode = %v, want 0600", info.Mode().Perm())
	}
}
