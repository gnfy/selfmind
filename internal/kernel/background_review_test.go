package kernel

// Unit coverage for the background-review claim parser: it must intercept only
// clear "skill created/updated/patched: <name>" claims and pass everything
// else through untouched (the end-to-end verification path is covered by
// internal/tools/background_review_integration_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type quotaReviewProvider struct{ calls int }

func (p *quotaReviewProvider) quotaError() error {
	return &llm.ProviderError{Provider: "kimi-coding", Class: llm.ProviderErrorQuota, StatusCode: 403, Message: "usage limit reached"}
}

func (p *quotaReviewProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	p.calls++
	return "", p.quotaError()
}

func (p *quotaReviewProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	return nil, p.quotaError()
}

func (p *quotaReviewProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls++
	return nil, p.quotaError()
}

func TestExtractSkillChangeClaims(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want []string
	}{
		{
			name: "created claim",
			resp: "skill created: review-flow",
			want: []string{"review-flow"},
		},
		{
			name: "updated claim mid-sentence",
			resp: "Done. skill updated: go-project-analysis.",
			want: []string{"go-project-analysis"},
		},
		{
			name: "patched claim case-insensitive with full-width colon",
			resp: "Skill Patched： deploy-checklist",
			want: []string{"deploy-checklist"},
		},
		{
			name: "backtick-quoted name",
			resp: "skill created: `pr-review` for future runs",
			want: []string{"pr-review"},
		},
		{
			name: "multiple claims deduplicated",
			resp: "skill created: alpha, skill updated: beta, skill updated: alpha",
			want: []string{"alpha", "beta"},
		},
		{
			name: "nothing to save passes through",
			resp: "Nothing to save.",
			want: nil,
		},
		{
			name: "prose about skills without a claim passes through",
			resp: "I reviewed the existing skills and the closest skill already covers this workflow, so no skill was updated.",
			want: nil,
		},
		{
			name: "memory claim is not intercepted",
			resp: "memory saved: user prefers table-driven tests",
			want: nil,
		},
		{
			name: "placeholder from prompt example does not match",
			resp: "for example: skill updated: <name>",
			want: nil,
		},
		{
			name: "verb without colon does not match",
			resp: "the skill updated its own references directory",
			want: nil,
		},
		{
			name: "empty response",
			resp: "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractSkillChangeClaims(tc.resp)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("extractSkillChangeClaims(%q) = %v, want %v", tc.resp, got, tc.want)
			}
		})
	}
}

// fakeClaimBackend simulates the restricted review backend for verification:
// skill_view succeeds only for names present in the existing set.
type fakeClaimBackend struct {
	existing map[string]bool
	calls    []string
}

func (b *fakeClaimBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	b.calls = append(b.calls, name)
	if name != "skill_view" {
		return "", fmt.Errorf("unexpected tool %s", name)
	}
	skill, _ := args["name"].(string)
	if b.existing[skill] {
		return `{"success": true}`, nil
	}
	return "", fmt.Errorf("skill not found: %s", skill)
}

func (b *fakeClaimBackend) GetToolDefinitions() []map[string]interface{} { return nil }

type fakeReviewMetadataBackend struct {
	fakeClaimBackend
}

func (b *fakeReviewMetadataBackend) ToolExecutionMetadata(name string, _ map[string]interface{}) ToolExecutionMetadata {
	return ToolExecutionMetadata{Origin: "builtin", Category: "review", ReadOnly: name == "skill_view"}
}

func TestRestrictedReviewBackendForwardsTrustedMetadata(t *testing.T) {
	inner := &fakeReviewMetadataBackend{}
	backend := &restrictedReviewBackend{inner: inner, allowed: map[string]bool{"skill_view": true}}
	metadata := backend.ToolExecutionMetadata("skill_view", map[string]interface{}{"name": "example"})
	if metadata.Origin != "builtin" || metadata.Category != "review" || !metadata.ReadOnly {
		t.Fatalf("forwarded metadata=%+v", metadata)
	}
	if denied := backend.ToolExecutionMetadata("terminal", nil); denied.Origin != "" || denied.Category != "" ||
		denied.RiskLevel != "" || denied.ReadOnly || len(denied.OperationClasses) != 0 {
		t.Fatalf("disallowed tool metadata leaked through wrapper: %+v", denied)
	}
}

func TestUnverifiedSkillClaims(t *testing.T) {
	backend := &fakeClaimBackend{existing: map[string]bool{"real-skill": true}}

	// Honest claim: verification passes, nothing flagged.
	if got := unverifiedSkillClaims(backend, "default", "skill created: real-skill"); got != nil {
		t.Fatalf("honest claim flagged as unverified: %v", got)
	}

	// Hallucinated claim: flagged with the claimed name.
	got := unverifiedSkillClaims(backend, "default", "skill updated: ghost-skill")
	if !reflect.DeepEqual(got, []string{"ghost-skill"}) {
		t.Fatalf("hallucinated claim not flagged: %v", got)
	}

	// Non-claim text: no verification dispatches at all.
	backend.calls = nil
	if got := unverifiedSkillClaims(backend, "default", "Nothing to save."); got != nil || len(backend.calls) != 0 {
		t.Fatalf("non-claim text triggered verification: flagged=%v calls=%v", got, backend.calls)
	}
}

func TestDurableBackgroundReviewPropagatesQuotaError(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	model := &quotaReviewProvider{}
	engine := NewBackgroundReviewEngine(memory.NewMemoryManager(provider), &fakeClaimBackend{}, model,
		EvolutionConfig{Enabled: true}, 1, 1)
	payload, err := json.Marshal(ReviewJobPayload{
		Channel: "cli", ReviewMemory: true,
		Messages: []ReviewMessage{{Role: "user", Content: "remember this workflow"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.RunReviewFromPayload(context.Background(), "tenant", string(payload))
	if err == nil || !llm.IsQuotaError(err) {
		t.Fatalf("review error = %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 non-retryable request", model.calls)
	}
}
