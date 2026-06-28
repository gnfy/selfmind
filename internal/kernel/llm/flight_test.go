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
func (fakeStreamProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) { return &ChatResponse{}, nil }
func (fakeStreamProvider) ChatCompletion(context.Context, []Message) (string, error)  { return "", nil }

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
