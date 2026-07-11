package memory

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ConsolidationDryRunConfig controls candidate retrieval for the offline
// memory self-organization experiment. Retrieval is deliberately permissive:
// it only presents plausible groups to an LLM judge. It never decides that two
// memories are equivalent and never mutates storage.
type ConsolidationDryRunConfig struct {
	CandidateSimilarity float64
	MaxClusterSize      int
	ArchiveAfter        time.Duration
}

func (c ConsolidationDryRunConfig) normalized() ConsolidationDryRunConfig {
	if c.CandidateSimilarity <= 0 || c.CandidateSimilarity > 1 {
		c.CandidateSimilarity = 0.42
	}
	if c.MaxClusterSize <= 0 {
		c.MaxClusterSize = 12
	}
	return c
}

// ConsolidationCluster is an LLM-review candidate, not a merge decision.
// Target and scope are homogeneous by construction. A protected cluster may
// only be kept or flagged as a conflict by ValidateConsolidationDecision.
type ConsolidationCluster struct {
	ID            string  `json:"id"`
	Target        string  `json:"target"`
	Scope         string  `json:"scope"`
	Members       []Fact  `json:"members"`
	MinSimilarity float64 `json:"min_similarity"`
	MaxSimilarity float64 `json:"max_similarity"`
	Protected     bool    `json:"protected"`
}

type ConsolidationArchiveCandidate struct {
	Fact Fact          `json:"fact"`
	Age  time.Duration `json:"age"`
}

// ConsolidationDryRun is safe to compute over live data: it is a read-only
// inventory of candidate groups and age candidates. Archive candidates are
// informational until the production model has last-access metadata.
type ConsolidationDryRun struct {
	TotalFacts        int                             `json:"total_facts"`
	ProtectedFacts    int                             `json:"protected_facts"`
	SingletonFacts    int                             `json:"singleton_facts"`
	CandidateClusters []ConsolidationCluster          `json:"candidate_clusters"`
	ArchiveCandidates []ConsolidationArchiveCandidate `json:"archive_candidates"`
}

// ConsolidationDecision is the future LLM judge contract. During the dry-run
// phase decisions are validated and reported only; applying them is
// intentionally out of scope.
type ConsolidationDecision struct {
	ClusterID  string   `json:"cluster_id"`
	Action     string   `json:"action"` // KEEP, MERGE, REINFORCE, SUPERSEDE, CONFLICT, ARCHIVE
	Canonical  string   `json:"canonical,omitempty"`
	Confidence float64  `json:"confidence"`
	Reason     string   `json:"reason,omitempty"`
	MemberIDs  []string `json:"member_ids,omitempty"`
}

func IsProtectedFact(f Fact) bool {
	return strings.EqualFold(strings.TrimSpace(f.Target), "pinned") ||
		strings.EqualFold(strings.TrimSpace(f.Source), SourceUser)
}

// BuildConsolidationDryRun generates same-target, same-scope candidate groups.
// The quadratic scan is acceptable for an opt-in offline audit and keeps the
// production write path untouched while the algorithm is being evaluated.
func BuildConsolidationDryRun(facts []Fact, cfg ConsolidationDryRunConfig, now time.Time) ConsolidationDryRun {
	cfg = cfg.normalized()
	report := ConsolidationDryRun{TotalFacts: len(facts)}
	if len(facts) == 0 {
		return report
	}
	if now.IsZero() {
		now = time.Now()
	}

	partitions := make(map[string][]int)
	for i, fact := range facts {
		if IsProtectedFact(fact) {
			report.ProtectedFacts++
		}
		if cfg.ArchiveAfter > 0 && !IsProtectedFact(fact) {
			stamp := fact.LastVerifiedAt
			if stamp.IsZero() {
				stamp = fact.CreatedAt
			}
			if !stamp.IsZero() && now.Sub(stamp) >= cfg.ArchiveAfter {
				report.ArchiveCandidates = append(report.ArchiveCandidates, ConsolidationArchiveCandidate{Fact: fact, Age: now.Sub(stamp)})
			}
		}
		partition := strings.ToLower(strings.TrimSpace(fact.Target)) + "\x00" + normalizedConsolidationScope(fact)
		partitions[partition] = append(partitions[partition], i)
	}

	partitionKeys := make([]string, 0, len(partitions))
	for key := range partitions {
		partitionKeys = append(partitionKeys, key)
	}
	sort.Strings(partitionKeys)
	for _, partition := range partitionKeys {
		indexes := partitions[partition]
		sort.SliceStable(indexes, func(i, j int) bool {
			left, right := factConsolidationRank(facts[indexes[i]]), factConsolidationRank(facts[indexes[j]])
			if left != right {
				return left > right
			}
			return facts[indexes[i]].ID < facts[indexes[j]].ID
		})
		used := make(map[int]bool, len(indexes))
		for _, seed := range indexes {
			if used[seed] {
				continue
			}
			used[seed] = true
			chunk := []int{seed}
			type scoredCandidate struct {
				index int
				score float64
			}
			var candidates []scoredCandidate
			for _, candidate := range indexes {
				if used[candidate] || candidate == seed {
					continue
				}
				score := ConsolidationSimilarity(facts[seed].Content, facts[candidate].Content)
				if score >= cfg.CandidateSimilarity {
					candidates = append(candidates, scoredCandidate{index: candidate, score: score})
				}
			}
			sort.SliceStable(candidates, func(i, j int) bool {
				if candidates[i].score != candidates[j].score {
					return candidates[i].score > candidates[j].score
				}
				return facts[candidates[i].index].ID < facts[candidates[j].index].ID
			})
			for _, candidate := range candidates {
				if len(chunk) == cfg.MaxClusterSize {
					break
				}
				acceptable := true
				for _, member := range chunk {
					if ConsolidationSimilarity(facts[member].Content, facts[candidate.index].Content) < cfg.CandidateSimilarity {
						acceptable = false
						break
					}
				}
				if acceptable {
					chunk = append(chunk, candidate.index)
					used[candidate.index] = true
				}
			}
			if len(chunk) < 2 {
				report.SingletonFacts++
				continue
			}
			cluster := ConsolidationCluster{
				Target: facts[chunk[0]].Target,
				Scope:  normalizedConsolidationScope(facts[chunk[0]]),
			}
			for _, idx := range chunk {
				cluster.Members = append(cluster.Members, facts[idx])
				cluster.Protected = cluster.Protected || IsProtectedFact(facts[idx])
			}
			cluster.MinSimilarity, cluster.MaxSimilarity = clusterSimilarityRange(cluster.Members)
			cluster.ID = consolidationClusterID(cluster)
			report.CandidateClusters = append(report.CandidateClusters, cluster)
		}
	}

	sort.SliceStable(report.CandidateClusters, func(i, j int) bool {
		if len(report.CandidateClusters[i].Members) != len(report.CandidateClusters[j].Members) {
			return len(report.CandidateClusters[i].Members) > len(report.CandidateClusters[j].Members)
		}
		return report.CandidateClusters[i].ID < report.CandidateClusters[j].ID
	})
	sort.SliceStable(report.ArchiveCandidates, func(i, j int) bool {
		return report.ArchiveCandidates[i].Age > report.ArchiveCandidates[j].Age
	})
	return report
}

func ValidateConsolidationDecision(report ConsolidationDryRun, decision ConsolidationDecision) error {
	var cluster *ConsolidationCluster
	for i := range report.CandidateClusters {
		if report.CandidateClusters[i].ID == strings.TrimSpace(decision.ClusterID) {
			cluster = &report.CandidateClusters[i]
			break
		}
	}
	if cluster == nil {
		return fmt.Errorf("unknown consolidation cluster %q", decision.ClusterID)
	}
	action := strings.ToUpper(strings.TrimSpace(decision.Action))
	switch action {
	case "KEEP", "MERGE", "REINFORCE", "SUPERSEDE", "CONFLICT", "ARCHIVE":
	default:
		return fmt.Errorf("unsupported consolidation action %q", decision.Action)
	}
	if decision.Confidence < 0 || decision.Confidence > 1 {
		return fmt.Errorf("consolidation confidence must be between 0 and 1")
	}
	if cluster.Protected && action != "KEEP" && action != "CONFLICT" {
		return fmt.Errorf("cluster %s contains user-confirmed or pinned memory and cannot be changed automatically", cluster.ID)
	}
	if (action == "MERGE" || action == "REINFORCE" || action == "SUPERSEDE") && strings.TrimSpace(decision.Canonical) == "" {
		return fmt.Errorf("action %s requires canonical memory text", action)
	}
	allowed := make(map[string]struct{}, len(cluster.Members))
	for _, fact := range cluster.Members {
		allowed[fact.ID] = struct{}{}
	}
	for _, id := range decision.MemberIDs {
		if _, ok := allowed[id]; !ok {
			return fmt.Errorf("memory %s does not belong to cluster %s", id, cluster.ID)
		}
	}
	return nil
}

func normalizedConsolidationScope(f Fact) string {
	scope := strings.ToLower(strings.TrimSpace(f.Scope))
	if scope == "" {
		if strings.EqualFold(strings.TrimSpace(f.Target), "user") || strings.EqualFold(strings.TrimSpace(f.Target), "pinned") {
			return "global"
		}
		return "legacy"
	}
	return scope
}

func ConsolidationSimilarity(a, b string) float64 {
	a, b = normalizeConsolidationText(a), normalizeConsolidationText(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	shorter, longer := a, b
	if len([]rune(shorter)) > len([]rune(longer)) {
		shorter, longer = longer, shorter
	}
	if len([]rune(shorter)) >= 16 && strings.Contains(longer, shorter) {
		return 0.94
	}
	at, bt := consolidationTerms(a), consolidationTerms(b)
	word := overlapScore(at, bt)
	if consolidationContainsCJK(a) || consolidationContainsCJK(b) {
		grams := overlapScore(consolidationNGrams(a, 2), consolidationNGrams(b, 2))
		if grams > word {
			word = grams
		}
	}
	return word
}

func normalizeConsolidationText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"the user ", "user is ", "user has ", "user's ", "user wants ", "user prefers ", "用户希望", "用户偏好", "用户"} {
		value = strings.TrimPrefix(value, prefix)
	}
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '/' || r == '.' || r == '_' || r == '-')
	}), " ")
}

func consolidationTerms(value string) map[string]struct{} {
	stop := map[string]struct{}{
		"about": {}, "and": {}, "codebase": {}, "current": {}, "for": {}, "from": {}, "game": {}, "has": {}, "is": {}, "of": {}, "on": {}, "or": {}, "prefers": {}, "project": {}, "repo": {}, "repository": {}, "should": {}, "the": {}, "this": {}, "to": {}, "user": {}, "wants": {}, "with": {}, "workspace": {},
	}
	out := make(map[string]struct{})
	for _, token := range strings.Fields(value) {
		if _, skip := stop[token]; skip || len([]rune(token)) < 2 {
			continue
		}
		out[token] = struct{}{}
	}
	return out
}

func consolidationNGrams(value string, n int) map[string]struct{} {
	runes := []rune(strings.ReplaceAll(value, " ", ""))
	out := make(map[string]struct{})
	if len(runes) < n {
		return out
	}
	for i := 0; i+n <= len(runes); i++ {
		out[string(runes[i:i+n])] = struct{}{}
	}
	return out
}

func overlapScore(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if _, ok := b[token]; ok {
			intersection++
		}
	}
	if intersection == 0 {
		return 0
	}
	if intersection < 2 {
		return 0
	}
	union := len(a) + len(b) - intersection
	jaccard := float64(intersection) / float64(union)
	minSize := len(a)
	if len(b) < minSize {
		minSize = len(b)
	}
	overlap := float64(intersection) / float64(minSize)
	if weighted := overlap * 0.82; weighted > jaccard {
		return weighted
	}
	return jaccard
}

func consolidationContainsCJK(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func factConsolidationRank(f Fact) float64 {
	score := f.Confidence * 100
	if IsProtectedFact(f) {
		score += 100
	}
	if !f.LastVerifiedAt.IsZero() {
		score += 10
	}
	return score
}

func clusterSimilarityRange(facts []Fact) (float64, float64) {
	minScore := 1.0
	maxScore := 0.0
	for i := range facts {
		for j := i + 1; j < len(facts); j++ {
			score := ConsolidationSimilarity(facts[i].Content, facts[j].Content)
			if score < minScore {
				minScore = score
			}
			if score > maxScore {
				maxScore = score
			}
		}
	}
	if len(facts) < 2 {
		return 0, 0
	}
	return minScore, maxScore
}

func consolidationClusterID(cluster ConsolidationCluster) string {
	ids := make([]string, 0, len(cluster.Members))
	for _, fact := range cluster.Members {
		id := strings.TrimSpace(fact.ID)
		if id == "" {
			id = normalizeConsolidationText(fact.Content)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	digest := sha1.Sum([]byte(strings.Join([]string{cluster.Target, cluster.Scope, strings.Join(ids, "|")}, "\x00")))
	return "mc_" + hex.EncodeToString(digest[:])[:10]
}
