package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
)

const memoryConsolidationJudgeSystemPrompt = `You audit durable memory candidates for SelfMind.
Return one JSON array only. Produce exactly one decision for every cluster in the input.

Actions:
- MERGE: every selected record states the same durable fact; write one precise canonical fact.
- REINFORCE: records independently confirm the same durable user preference or convention; write one canonical fact.
- SUPERSEDE: a clearly newer fact replaces an older fact; canonical must be the current fact.
- CONFLICT: records materially contradict each other and human or later evidence is needed.
- ARCHIVE: records are temporary run details with no continuing value.
- KEEP: records are merely related, ambiguous, or describe distinct facts. KEEP is the safe default.

Rules:
- Similarity is only candidate retrieval, never proof of equivalence.
- Never merge records merely because they mention the same file, project, language, or person.
- Preserve scope. Do not infer facts absent from the records.
- A protected cluster may only be KEEP or CONFLICT.
- Use confidence >= 0.95 only when all selected records are unambiguously equivalent.
- Omit member_ids when the decision covers the whole cluster. Include it only for a strict subset.

Shape:
[{"cluster_id":"...","action":"KEEP","canonical":"","confidence":0.0,"reason":"..."}]`

type memoryJudgeAuditReport struct {
	GeneratedAt       time.Time                      `json:"generated_at"`
	SourceReport      string                         `json:"source_report"`
	EvaluatedClusters int                            `json:"evaluated_clusters"`
	Decisions         []memory.ConsolidationDecision `json:"decisions"`
	InvalidDecisions  []string                       `json:"invalid_decisions,omitempty"`
	StreamFallbacks   int                            `json:"stream_fallbacks"`
	InputTokens       int                            `json:"input_tokens"`
	OutputTokens      int                            `json:"output_tokens"`
}

// TestMemoryConsolidationJudgeLive is an opt-in, read-only model audit. It
// consumes the deterministic candidate report and writes decisions to a
// separate JSON file. It deliberately has no MemoryManager and no apply path.
func TestMemoryConsolidationJudgeLive(t *testing.T) {
	if os.Getenv("SELFMIND_MEMORY_CONSOLIDATION_LIVE") != "1" {
		t.Skip("set SELFMIND_MEMORY_CONSOLIDATION_LIVE=1 to run the read-only model judge")
	}
	inputPath := strings.TrimSpace(os.Getenv("SELFMIND_MEMORY_AUDIT_REPORT"))
	if inputPath == "" {
		t.Fatal("SELFMIND_MEMORY_AUDIT_REPORT is required")
	}
	payload, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	var candidateReport memory.ConsolidationDryRun
	if err := json.Unmarshal(payload, &candidateReport); err != nil {
		t.Fatalf("decode candidate report: %v", err)
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("load SelfMind config: %v", err)
	}
	provider := configuredAuxiliaryRoleProvider(nil, cfg, "default", llm.RoleMemoryExtract)
	if provider == nil {
		t.Fatal("models.auxiliary or models.roles.memory_extract is not configured; refusing to use the primary coding model implicitly")
	}

	limit := envPositiveInt("SELFMIND_MEMORY_JUDGE_LIMIT", 16)
	clusters := representativeMemoryClusters(candidateReport.CandidateClusters, limit)
	if len(clusters) == 0 {
		t.Fatal("candidate report contains no clusters")
	}
	report := memoryJudgeAuditReport{
		GeneratedAt:       time.Now().UTC(),
		SourceReport:      inputPath,
		EvaluatedClusters: len(clusters),
	}
	expected := make(map[string]struct{}, len(clusters))
	for _, cluster := range clusters {
		expected[cluster.ID] = struct{}{}
	}
	seenDecisions := make(map[string]struct{}, len(clusters))
	const batchSize = 4
	for start := 0; start < len(clusters); start += batchSize {
		end := start + batchSize
		if end > len(clusters) {
			end = len(clusters)
		}
		prompt, err := json.Marshal(memoryJudgeClusterPrompt(clusters[start:end]))
		if err != nil {
			t.Fatal(err)
		}
		req := llm.ChatRequest{
			SystemPrompt: memoryConsolidationJudgeSystemPrompt,
			Messages:     []llm.Message{{Role: "user", Content: string(prompt)}},
			MaxTokens:    2600,
			Options:      map[string]interface{}{"temperature": 0},
		}
		var content string
		var usedStream bool
		for attempt := 1; attempt <= 3; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			attemptContent, attemptUsage, attemptStream, attemptErr := runMemoryJudgeRequest(ctx, provider, req)
			cancel()
			report.InputTokens += attemptUsage.InputTokens
			report.OutputTokens += attemptUsage.OutputTokens
			usedStream = usedStream || attemptStream
			if attemptErr == nil {
				content, err = attemptContent, nil
				break
			}
			err = attemptErr
			t.Logf("judge batch %d attempt %d failed: %v", start/batchSize+1, attempt, attemptErr)
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * time.Second)
			}
		}
		if err != nil {
			t.Fatalf("judge batch %d: %v", start/batchSize+1, err)
		}
		if usedStream {
			report.StreamFallbacks++
		}
		decisions, err := decodeMemoryJudgeDecisions(content)
		if err != nil {
			t.Fatalf("decode judge batch %d: %v\nresponse: %s", start/batchSize+1, err, content)
		}
		for _, decision := range decisions {
			if _, ok := expected[decision.ClusterID]; !ok {
				report.InvalidDecisions = append(report.InvalidDecisions, fmt.Sprintf("unexpected cluster %s", decision.ClusterID))
				continue
			}
			if _, duplicate := seenDecisions[decision.ClusterID]; duplicate {
				report.InvalidDecisions = append(report.InvalidDecisions, fmt.Sprintf("duplicate decision for %s", decision.ClusterID))
				continue
			}
			seenDecisions[decision.ClusterID] = struct{}{}
			if err := memory.ValidateConsolidationDecision(candidateReport, decision); err != nil {
				report.InvalidDecisions = append(report.InvalidDecisions, fmt.Sprintf("%s: %v", decision.ClusterID, err))
				continue
			}
			report.Decisions = append(report.Decisions, decision)
		}
	}
	for _, cluster := range clusters {
		if _, ok := seenDecisions[cluster.ID]; !ok {
			report.InvalidDecisions = append(report.InvalidDecisions, fmt.Sprintf("missing decision for %s", cluster.ID))
		}
	}
	for _, decision := range report.Decisions {
		t.Logf("cluster=%s action=%s confidence=%.2f canonical=%q reason=%s",
			decision.ClusterID, decision.Action, decision.Confidence, decision.Canonical, decision.Reason)
	}

	outPath := strings.TrimSpace(os.Getenv("SELFMIND_MEMORY_JUDGE_OUT"))
	if outPath == "" {
		outPath = inputPath + ".judge.json"
	}
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, out, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("report=%s decisions=%d invalid=%d stream_fallbacks=%d tokens=%d+%d", outPath, len(report.Decisions), len(report.InvalidDecisions), report.StreamFallbacks, report.InputTokens, report.OutputTokens)
	if len(report.InvalidDecisions) > 0 {
		t.Fatalf("model judge produced invalid decisions: %s", strings.Join(report.InvalidDecisions, "; "))
	}
}

func runMemoryJudgeRequest(ctx context.Context, provider llm.Provider, req llm.ChatRequest) (string, llm.UsageStats, bool, error) {
	resp, err := provider.Chat(ctx, req)
	if err == nil && resp != nil && strings.TrimSpace(resp.Content) != "" {
		return resp.Content, resp.Usage, false, nil
	}
	if err != nil {
		return "", llm.UsageStats{}, false, err
	}
	var usage llm.UsageStats
	if resp != nil {
		usage = resp.Usage
	}
	stream, err := provider.StreamChat(ctx, req)
	if err != nil {
		return "", usage, true, fmt.Errorf("non-stream response was empty; stream fallback failed: %w", err)
	}
	var content strings.Builder
	for event := range stream {
		if event.Err != nil {
			return "", usage, true, event.Err
		}
		content.WriteString(event.Content)
		if event.Usage != nil {
			usage = *event.Usage
		}
	}
	if strings.TrimSpace(content.String()) == "" {
		return "", usage, true, fmt.Errorf("provider returned empty content in both non-stream and stream modes")
	}
	return content.String(), usage, true, nil
}

type memoryJudgeMember struct {
	ID         string  `json:"id"`
	Content    string  `json:"content"`
	Source     string  `json:"source,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Protected  bool    `json:"protected"`
}

type memoryJudgeCluster struct {
	ClusterID     string              `json:"cluster_id"`
	Target        string              `json:"target"`
	Scope         string              `json:"scope"`
	MinSimilarity float64             `json:"min_similarity"`
	Protected     bool                `json:"protected"`
	Members       []memoryJudgeMember `json:"members"`
}

func memoryJudgeClusterPrompt(clusters []memory.ConsolidationCluster) []memoryJudgeCluster {
	out := make([]memoryJudgeCluster, 0, len(clusters))
	for _, cluster := range clusters {
		item := memoryJudgeCluster{
			ClusterID:     cluster.ID,
			Target:        cluster.Target,
			Scope:         cluster.Scope,
			MinSimilarity: cluster.MinSimilarity,
			Protected:     cluster.Protected,
		}
		for _, fact := range cluster.Members {
			item.Members = append(item.Members, memoryJudgeMember{
				ID:         fact.ID,
				Content:    truncateMemoryJudgeText(fact.Content, 500),
				Source:     fact.Source,
				Confidence: fact.Confidence,
				Protected:  memory.IsProtectedFact(fact),
			})
		}
		out = append(out, item)
	}
	return out
}

func representativeMemoryClusters(clusters []memory.ConsolidationCluster, limit int) []memory.ConsolidationCluster {
	if limit <= 0 || limit >= len(clusters) {
		return append([]memory.ConsolidationCluster(nil), clusters...)
	}
	selected := make([]memory.ConsolidationCluster, 0, limit)
	seen := make(map[string]struct{}, limit)
	obvious := (limit + 1) / 2
	for _, cluster := range clusters {
		if len(selected) == obvious {
			break
		}
		selected = append(selected, cluster)
		seen[cluster.ID] = struct{}{}
	}
	boundary := append([]memory.ConsolidationCluster(nil), clusters...)
	sort.SliceStable(boundary, func(i, j int) bool {
		if boundary[i].MinSimilarity != boundary[j].MinSimilarity {
			return boundary[i].MinSimilarity < boundary[j].MinSimilarity
		}
		return boundary[i].ID < boundary[j].ID
	})
	for _, cluster := range boundary {
		if len(selected) == limit {
			break
		}
		if _, ok := seen[cluster.ID]; ok {
			continue
		}
		selected = append(selected, cluster)
		seen[cluster.ID] = struct{}{}
	}
	return selected
}

func decodeMemoryJudgeDecisions(raw string) ([]memory.ConsolidationDecision, error) {
	raw = strings.TrimSpace(raw)
	start, end := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("response contains no JSON array")
	}
	var decisions []memory.ConsolidationDecision
	if err := json.Unmarshal([]byte(raw[start:end+1]), &decisions); err != nil {
		return nil, err
	}
	return decisions, nil
}

func envPositiveInt(name string, fallback int) int {
	var value int
	if _, err := fmt.Sscanf(strings.TrimSpace(os.Getenv(name)), "%d", &value); err != nil || value <= 0 {
		return fallback
	}
	return value
}

func truncateMemoryJudgeText(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "..."
}
