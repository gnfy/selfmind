package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRankSkillsBM25FUsesRareTermsAndFieldWeights(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		skills []SkillInfo
		want   string
	}{
		{
			name:  "rare metadata terms beat a common product family",
			query: "AWS CodeBuild SSM SecureString KMS",
			skills: []SkillInfo{
				{Name: "aws-codebuild-release", Description: "Execute an AWS CodeBuild release."},
				{Name: "aws-codebuild-release-prep", Description: "Prepare an AWS CodeBuild release record."},
				{Name: "aws-codebuild-ssm-parameter", Description: "Use SSM SecureString parameters in AWS CodeBuild with KMS decryption."},
			},
			want: "aws-codebuild-ssm-parameter",
		},
		{
			name:  "name field beats repeated description text",
			query: "verification",
			skills: []SkillInfo{
				{Name: "release-verification", Description: "Confirm the final state."},
				{Name: "release-helper", Description: "Verification verification verification helper."},
			},
			want: "release-verification",
		},
		{
			name:  "short focused description beats long incidental match",
			query: "retentionpolicy",
			skills: []SkillInfo{
				{Name: "focused-check", Description: "retentionpolicy"},
				{Name: "broad-check", Description: "retentionpolicy " + strings.Repeat("general workflow metadata details ", 30)},
			},
			want: "focused-check",
		},
		{
			name:  "canonical name mention wins",
			query: "Use gcp-release-prep for this request, then report the release status",
			skills: []SkillInfo{
				{Name: "gcp-release-prep", Description: "Prepare a release."},
				{Name: "release-status-reporter", Description: "Report GCP release status and release history."},
			},
			want: "gcp-release-prep",
		},
		{
			name:  "canonical name survives stop-word filtering",
			query: "use using",
			skills: []SkillInfo{
				{Name: "using", Description: "Generic workflow."},
				{Name: "generic", Description: "Use a generic workflow."},
			},
			want: "using",
		},
		{
			name:  "CJK bigrams match metadata",
			query: "请检查发布元数据并告诉我版本",
			skills: []SkillInfo{
				{Name: "release-inspection", Description: "检查发布元数据并核对版本信息"},
				{Name: "frontend-colors", Description: "调整前端颜色样式"},
			},
			want: "release-inspection",
		},
		{
			name:  "empty description falls back to name terms",
			query: "cloudarmor review",
			skills: []SkillInfo{
				{Name: "cloudarmor-review"},
				{Name: "release-review", Description: "Review a release."},
			},
			want: "cloudarmor-review",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rankSkillsBM25F(tt.query, tt.skills, 3)
			if len(got) == 0 || got[0].Name != tt.want {
				t.Fatalf("rankSkillsBM25F(%q)=%+v, want first %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestRankSkillsBM25FUsesWorkspaceOnlyAsTieBreak(t *testing.T) {
	skills := []SkillInfo{
		{Name: "a-user-check", Description: "distinctterm", Scope: SkillScopeUser},
		{Name: "z-workspace-check", Description: "distinctterm", Scope: SkillScopeWorkspace},
	}
	got := rankSkillsBM25F("distinctterm", skills, 2)
	if len(got) != 2 || got[0].Name != "z-workspace-check" {
		t.Fatalf("workspace tie-break ranking=%+v", got)
	}
	if unrelated := rankSkillsBM25F("unrelated", skills, 2); len(unrelated) != 0 {
		t.Fatalf("scope must not create a lexical match: %+v", unrelated)
	}
}

func TestRankSkillsBM25FRejectsSingleCJKBigramCollision(t *testing.T) {
	skills := []SkillInfo{
		{Name: "frontend-colors", Description: "整理前端颜色样式并输出对照表"},
		{Name: "db-backup", Description: "备份生产数据库并校验完整性"},
	}
	query := "请帮我把今天的会议纪要整理成周报发给团队"
	if got := rankSkillsBM25F(query, skills, 3); len(got) != 0 {
		t.Fatalf("single CJK bigram collision returned unrelated candidates: %+v", got)
	}
}

func TestRankSkillsBM25FIsBoundedAndDeterministic(t *testing.T) {
	var skills []SkillInfo
	for i := 0; i < 20; i++ {
		skills = append(skills, SkillInfo{Name: fmt.Sprintf("common-%02d", i), Description: "common metadata"})
	}
	first := rankSkillsBM25F("common", skills, 3)
	second := rankSkillsBM25F("common", skills, 3)
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("bounded results: first=%d second=%d", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Fatalf("non-deterministic rank at %d: %q != %q", i, first[i].Name, second[i].Name)
		}
	}
}

func TestSearchSkillsForTenantUsesBoundedMetadataRanking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tool := NewSkillManageTool()
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("common-flow-%02d", i)
		if _, err := tool.Execute(directSkillMutationArgs(map[string]interface{}{
			"action": "create", "name": name, "description": "Common workflow metadata", "content": "body",
		})); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if _, err := tool.Execute(directSkillMutationArgs(map[string]interface{}{
		"action": "create", "name": "ssm-parameter-flow",
		"description": "Use CodeBuild SSM SecureString with KMS decryption", "content": "body",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(directSkillMutationArgs(map[string]interface{}{
		"action": "create", "name": "body-only-flow",
		"description": "Generic metadata", "content": "ultravioletneedle",
	})); err != nil {
		t.Fatal(err)
	}

	results, err := SearchSkillsForTenant("default", "CodeBuild SSM SecureString KMS")
	if err != nil || len(results) == 0 || results[0].Name != "ssm-parameter-flow" {
		t.Fatalf("ranked search=%+v err=%v", results, err)
	}
	payload, err := SkillsListJSONForTenant("default", "CodeBuild SSM SecureString KMS", false)
	if err != nil {
		t.Fatalf("ranked JSON search: %v", err)
	}
	var decoded struct {
		Count        int  `json:"count"`
		TotalMatches int  `json:"total_matches"`
		Truncated    bool `json:"truncated"`
		Skills       []struct {
			Name string `json:"name"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode ranked JSON search: %v", err)
	}
	if len(decoded.Skills) == 0 || decoded.Skills[0].Name != "ssm-parameter-flow" {
		t.Fatalf("JSON search lost relevance order: %+v", decoded.Skills)
	}
	results, err = SearchSkillsForTenant("default", "common")
	if err != nil || len(results) != maxSkillSearchResults {
		t.Fatalf("bounded search count=%d err=%v", len(results), err)
	}
	payload, err = SkillsListJSONForTenant("default", "common", false)
	if err != nil || json.Unmarshal([]byte(payload), &decoded) != nil {
		t.Fatalf("decode truncated JSON search: %v payload=%s", err, payload)
	}
	if decoded.Count != maxSkillSearchResults || decoded.TotalMatches != 12 || !decoded.Truncated {
		t.Fatalf("truncation metadata=%+v", decoded)
	}
	results, err = SearchSkillsForTenant("default", "ultravioletneedle")
	if err != nil || len(results) != 0 {
		t.Fatalf("search must remain metadata-only: %+v err=%v", results, err)
	}
	if _, err := tool.Execute(directSkillMutationArgs(map[string]interface{}{
		"action": "disable", "name": "ssm-parameter-flow",
	})); err != nil {
		t.Fatal(err)
	}
	candidates, err := RankSkillCandidatesForTenant("default", "CodeBuild SSM SecureString KMS", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.Name == "ssm-parameter-flow" {
			t.Fatalf("disabled Skill leaked into runtime candidates: %+v", candidates)
		}
	}
}

func BenchmarkRankSkillsBM25F(b *testing.B) {
	for _, size := range []int{78, 500, 1000} {
		skills := make([]SkillInfo, 0, size)
		for i := 0; i < size; i++ {
			skills = append(skills, SkillInfo{
				Name:        fmt.Sprintf("gcp-release-workflow-%04d", i),
				Description: fmt.Sprintf("Verify Cloud Build release %04d with GitOps ArgoCD Kubernetes and environment metadata", i),
			})
		}
		b.Run(fmt.Sprintf("skills_%d", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				rankSkillsBM25F("verify GCP Cloud Build release GitOps ArgoCD duplicate production", skills, 3)
			}
		})
	}
}
