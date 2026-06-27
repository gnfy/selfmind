package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// liveSentinelProvider fails loudly if any method is called — it stands in for a
// real provider so a test can prove the offline guard never falls through to it.
type liveSentinelProvider struct{ t *testing.T }

func (p liveSentinelProvider) StreamChat(context.Context, ChatRequest) (<-chan StreamEvent, error) {
	p.t.Fatal("offline guard fell through to live StreamChat")
	return nil, nil
}
func (p liveSentinelProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	p.t.Fatal("offline guard fell through to live Chat")
	return nil, nil
}
func (p liveSentinelProvider) ChatCompletion(context.Context, []Message) (string, error) {
	p.t.Fatal("offline guard fell through to live ChatCompletion")
	return "", nil
}

func TestVCROfflineMissReturnsErrorNotLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SELFMIND_EVAL_VCR", "replay")
	t.Setenv("SELFMIND_EVAL_VCR_DIR", dir)
	t.Setenv("SELFMIND_EVAL_OFFLINE", "1")

	p := MaybeWrapVCR(liveSentinelProvider{t: t})
	ctx := WithVCRSession(context.Background(), "case-with-no-cassette")

	if _, err := p.StreamChat(ctx, ChatRequest{}); !errors.Is(err, ErrCassetteMiss) {
		t.Fatalf("StreamChat: want ErrCassetteMiss, got %v", err)
	}
	if _, err := p.Chat(ctx, ChatRequest{}); !errors.Is(err, ErrCassetteMiss) {
		t.Fatalf("Chat: want ErrCassetteMiss, got %v", err)
	}
	if _, err := p.ChatCompletion(ctx, nil); !errors.Is(err, ErrCassetteMiss) {
		t.Fatalf("ChatCompletion: want ErrCassetteMiss, got %v", err)
	}
}

func TestHasCassetteSession(t *testing.T) {
	dir := t.TempDir()
	if HasCassetteSession(dir, "my-case") {
		t.Fatal("expected no cassette before recording")
	}
	caseDir := filepath.Join(dir, "my-case")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "0000.json"), []byte(`{"method":"stream"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasCassetteSession(dir, "my-case") {
		t.Fatal("expected cassette to be detected after recording")
	}
}
