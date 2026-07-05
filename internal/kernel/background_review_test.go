package kernel

// Unit coverage for the background-review claim parser: it must intercept only
// clear "skill created/updated/patched: <name>" claims and pass everything
// else through untouched (the end-to-end verification path is covered by
// internal/tools/background_review_integration_test.go).

import (
	"fmt"
	"reflect"
	"testing"
)

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
