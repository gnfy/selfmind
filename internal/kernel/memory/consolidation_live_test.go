package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestMemoryConsolidationLiveDryRun is an opt-in, read-only audit over an
// existing memory.db. It never constructs MemoryManager and opens SQLite with
// mode=ro, so schema setup and mutations are impossible. The generated JSON is
// the input to the separate model-judge experiment.
func TestMemoryConsolidationLiveDryRun(t *testing.T) {
	dbPath := os.Getenv("SELFMIND_MEMORY_AUDIT_DB")
	if dbPath == "" {
		t.Skip("set SELFMIND_MEMORY_AUDIT_DB to run the read-only live audit")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("open memory database read-only: %v", err)
	}

	facts, err := readAuditFacts(db)
	if err != nil {
		t.Fatal(err)
	}
	threshold := envFloat("SELFMIND_MEMORY_AUDIT_SIMILARITY", 0.42)
	archiveDays := envInt("SELFMIND_MEMORY_AUDIT_ARCHIVE_DAYS", 180)
	report := BuildConsolidationDryRun(facts, ConsolidationDryRunConfig{
		CandidateSimilarity: threshold,
		MaxClusterSize:      12,
		ArchiveAfter:        time.Duration(archiveDays) * 24 * time.Hour,
	}, time.Now())

	clustered := 0
	for _, cluster := range report.CandidateClusters {
		clustered += len(cluster.Members)
	}
	t.Logf("facts=%d protected=%d clusters=%d clustered_facts=%d singletons=%d age_candidates=%d threshold=%.2f",
		report.TotalFacts, report.ProtectedFacts, len(report.CandidateClusters), clustered,
		report.SingletonFacts, len(report.ArchiveCandidates), threshold)
	for i, cluster := range report.CandidateClusters {
		if i == 8 {
			break
		}
		t.Logf("cluster=%s target=%s scope=%s members=%d protected=%t similarity=%.2f..%.2f",
			cluster.ID, cluster.Target, cluster.Scope, len(cluster.Members), cluster.Protected, cluster.MinSimilarity, cluster.MaxSimilarity)
		for j, fact := range cluster.Members {
			if j == 3 {
				break
			}
			t.Logf("  [%s] %s", shortAuditRef(fact.ID), fact.Content)
		}
	}

	if out := os.Getenv("SELFMIND_MEMORY_AUDIT_OUT"); out != "" {
		payload, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, payload, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("report=%s", out)
	}
}

func readAuditFacts(db *sql.DB) ([]Fact, error) {
	rows, err := db.Query(`SELECT id, target, content, created_at,
		COALESCE(source,''), COALESCE(scope,''), COALESCE(confidence,0),
		COALESCE(created_from_run,''), last_verified_at
		FROM facts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []Fact
	for rows.Next() {
		var fact Fact
		var created interface{}
		var verified sql.NullInt64
		if err := rows.Scan(&fact.ID, &fact.Target, &fact.Content, &created, &fact.Source, &fact.Scope, &fact.Confidence, &fact.CreatedFromRun, &verified); err != nil {
			return nil, err
		}
		fact.CreatedAt = parseAuditTime(created)
		if verified.Valid && verified.Int64 > 0 {
			fact.LastVerifiedAt = time.Unix(verified.Int64, 0).UTC()
		}
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}

func parseAuditTime(value interface{}) time.Time {
	switch typed := value.(type) {
	case time.Time:
		return typed
	case int64:
		return time.Unix(typed, 0).UTC()
	case float64:
		return time.Unix(int64(typed), 0).UTC()
	case []byte:
		value = string(typed)
	}
	text := fmt.Sprint(value)
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05 -0700 MST",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func envFloat(name string, fallback float64) float64 {
	value, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}

func shortAuditRef(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
