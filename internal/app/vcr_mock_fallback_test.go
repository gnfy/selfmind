package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/config"
)

// TestBuildLLMProviderMockFallbackReplaysCassettes pins the CI offline-gate
// fix: in a credential-less environment (fresh CI runner, clean HOME) the
// provider falls back to the setup-diagnostic mock — which must STILL be
// VCR-wrapped, so eval replay answers from recorded cassettes instead of the
// mock text. Before the fix the mock bypassed MaybeWrapVCR entirely and the
// offline gate failed 8 replay cases on every push while passing on any
// machine with credentials.
func TestBuildLLMProviderMockFallbackReplaysCassettes(t *testing.T) {
	// A clean HOME prevents real credential files from resolving a provider;
	// blank the env tokens modelruntime also accepts so a developer machine
	// behaves like CI here.
	t.Setenv("HOME", t.TempDir())
	for _, env := range []string{
		"CODEX_ACCESS_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_TOKEN",
		"GEMINI_OAUTH_ACCESS_TOKEN", "GOOGLE_OAUTH_ACCESS_TOKEN",
		"QWEN_ACCESS_TOKEN", "QWEN_API_KEY", "OPENAI_API_KEY",
	} {
		t.Setenv(env, "")
	}

	vcrDir := t.TempDir()
	session := "mock-fallback-replay"
	cassette := map[string]interface{}{
		"method": "stream",
		"events": []map[string]interface{}{
			{"content": "FROM CASSETTE", "finish_reason": "stop"},
		},
	}
	data, err := json.Marshal(cassette)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(vcrDir, session), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vcrDir, session, "0000.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SELFMIND_EVAL_VCR", "replay")
	t.Setenv("SELFMIND_EVAL_VCR_DIR", vcrDir)
	t.Setenv("SELFMIND_EVAL_OFFLINE", "1")
	llm.ResetVCRSession(session)

	provider := buildLLMProvider(&config.Config{})
	ctx := llm.WithVCRSession(context.Background(), session)
	stream, err := provider.StreamChat(ctx, llm.ChatRequest{})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	var out strings.Builder
	for ev := range stream {
		out.WriteString(ev.Content)
	}
	if got := out.String(); got != "FROM CASSETTE" {
		t.Fatalf("replay content = %q, want the cassette content (mock bypassed the VCR wrapper?)", got)
	}

	// Without a VCR session on the context, strict offline replay must reach
	// the inner mock (background calls fall through by design) — proving the
	// wrapper is in the path rather than the provider being the raw mock.
	plain, err := provider.StreamChat(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatalf("sessionless StreamChat: %v", err)
	}
	var fallback strings.Builder
	for ev := range plain {
		fallback.WriteString(ev.Content)
	}
	if fallback.String() == "FROM CASSETTE" {
		t.Fatal("sessionless call must not replay the cassette")
	}
}
