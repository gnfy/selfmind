package tools

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const DefaultSkillMigrationGrace = 30 * 24 * time.Hour

type SkillMigrationItem struct {
	Partition string `json:"partition"`
	Name      string `json:"name"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
}

type SkillMigrationReport struct {
	Root            string               `json:"root"`
	Target          string               `json:"target"`
	Applied         bool                 `json:"applied"`
	Migrated        int                  `json:"migrated"`
	Deduped         int                  `json:"deduped"`
	Conflicts       int                  `json:"conflicts"`
	Partitions      int                  `json:"partitions"`
	EmptyPartitions int                  `json:"empty_partitions"`
	Items           []SkillMigrationItem `json:"items"`
}

// MigratePersonSkillsToControl moves agent-created skill assets out of person
// partitions. The control copy wins conflicts; identical content is deduped;
// conflicting person copies remain available through the read-only legacy
// fallback. Memory learning history in the same learning/ directory is never
// moved.
func MigratePersonSkillsToControl(root, controlTenant string, apply bool, grace time.Duration) (SkillMigrationReport, error) {
	root = filepath.Clean(os.ExpandEnv(strings.TrimSpace(root)))
	controlTenant = fallbackTenant(controlTenant)
	if grace <= 0 {
		grace = DefaultSkillMigrationGrace
	}
	report := SkillMigrationReport{Root: root, Target: controlTenant, Applied: apply}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return report, nil
		}
		return report, err
	}
	targetDir := SkillsDirForTenant(root, controlTenant)
	targetUsage, err := loadSkillUsageForDir(targetDir)
	if err != nil {
		return report, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "person_") {
			continue
		}
		sourceDir := SkillsDirForTenant(root, entry.Name())
		skills, err := migrationSkillEntries(sourceDir)
		if err != nil {
			return report, err
		}
		if len(skills) == 0 {
			if info, statErr := os.Stat(sourceDir); statErr == nil && info.IsDir() {
				report.EmptyPartitions++
				if apply {
					if err := removeEmptySkillPartition(sourceDir); err != nil && !os.IsNotExist(err) {
						return report, err
					}
				}
			}
			continue
		}
		report.Partitions++
		sourceUsage, _ := loadSkillUsageForDir(sourceDir)
		for _, source := range skills {
			name := strings.TrimSuffix(filepath.Base(source), ".md")
			target := filepath.Join(targetDir, filepath.Base(source))
			sourceHash, hashErr := migrationSkillHash(source)
			if hashErr != nil {
				return report, hashErr
			}
			action := "migrate"
			collisions := existingSkillInstallCollisions(targetDir, name)
			if len(collisions) > 1 {
				report.Conflicts++
				report.Items = append(report.Items, SkillMigrationItem{Partition: entry.Name(), Name: name, Action: "conflict", Reason: "control tenant has multiple legacy formats"})
				continue
			}
			if len(collisions) == 1 {
				target = collisions[0].Path
				targetHash, targetErr := migrationSkillHash(target)
				if targetErr != nil {
					return report, targetErr
				}
				if targetHash != sourceHash {
					report.Conflicts++
					report.Items = append(report.Items, SkillMigrationItem{Partition: entry.Name(), Name: name, Action: "conflict", Reason: "control tenant has different content"})
					continue
				}
				action = "dedupe"
			}
			report.Items = append(report.Items, SkillMigrationItem{Partition: entry.Name(), Name: name, Action: action})
			if action == "dedupe" {
				report.Deduped++
			} else {
				report.Migrated++
			}
			if !apply {
				continue
			}
			if action == "migrate" {
				if err := copySkillAsset(source, target); err != nil {
					return report, err
				}
			}
			rec := sourceUsage[name]
			if rec.Name == "" {
				rec = SkillUsageRecord{Name: name, Source: SkillSourceAgentCreated, State: SkillStateActive, CreatedAt: nowRFC3339()}
			}
			rec.MigratedFrom = entry.Name()
			rec.GovernanceNotBefore = time.Now().UTC().Add(grace).Format(time.RFC3339)
			if existing := targetUsage[name]; existing.Name == "" {
				targetUsage[name] = rec
			}
			if err := migrateSkillLearningAudit(root, entry.Name(), controlTenant, name); err != nil {
				return report, err
			}
			if err := os.RemoveAll(source); err != nil {
				return report, err
			}
			delete(sourceUsage, name)
		}
		if apply {
			if err := saveSkillUsageForDir(sourceDir, sourceUsage); err != nil {
				return report, err
			}
			_ = removeEmptySkillPartition(sourceDir)
		}
	}
	if apply && (report.Migrated > 0 || report.Deduped > 0) {
		if err := saveSkillUsageForDir(targetDir, targetUsage); err != nil {
			return report, err
		}
	}
	sort.Slice(report.Items, func(i, j int) bool {
		if report.Items[i].Partition == report.Items[j].Partition {
			return report.Items[i].Name < report.Items[j].Name
		}
		return report.Items[i].Partition < report.Items[j].Partition
	})
	return report, nil
}

func migrationSkillEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".md") {
			out = append(out, filepath.Join(dir, entry.Name()))
		}
	}
	return out, nil
}

func migrationSkillHash(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		hash, _, err := hashSkillDirectory(path)
		return hash, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func copySkillAsset(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		return atomicWriteFile(target, string(data))
	}
	tmp := target + ".migrating"
	_ = os.RemoveAll(tmp)
	if err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(source, path)
		dest := filepath.Join(tmp, rel)
		if info.IsDir() {
			return os.MkdirAll(dest, info.Mode().Perm())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		inErr := in.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if inErr != nil {
			return inErr
		}
		return closeErr
	}); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func migrateSkillLearningAudit(root, personID, controlTenant, skillName string) error {
	path := filepath.Join(root, personID, "learning", "learning-log.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	existing, _ := learningChangeIDs(controlTenant)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var change LearningChange
		if json.Unmarshal(scanner.Bytes(), &change) != nil || change.Kind != "skill" || kernelSkillName(change.Target) != kernelSkillName(skillName) || existing[change.ID] {
			continue
		}
		change.TenantID = controlTenant
		if err := appendLearningChange(change); err != nil {
			return err
		}
		existing[change.ID] = true
	}
	return scanner.Err()
}

func learningChangeIDs(tenantID string) (map[string]bool, error) {
	ids := map[string]bool{}
	dir, err := learningDir(tenantID)
	if err != nil {
		return ids, err
	}
	f, err := os.Open(filepath.Join(dir, "learning-log.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return ids, nil
		}
		return ids, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var change LearningChange
		if json.Unmarshal(scanner.Bytes(), &change) == nil && change.ID != "" {
			ids[change.ID] = true
		}
	}
	return ids, scanner.Err()
}

func kernelSkillName(name string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(name, "_", "-")))
}

func removeEmptySkillPartition(dir string) error {
	entries, err := migrationSkillEntries(dir)
	if err != nil || len(entries) > 0 {
		return err
	}
	_ = os.Remove(usageFilePath(dir))
	return os.Remove(dir)
}

func FormatSkillMigrationReport(report SkillMigrationReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Skill partition migration (%s)\n", map[bool]string{true: "apply", false: "dry-run"}[report.Applied])
	fmt.Fprintf(&b, "Root: %s\nTarget: %s\nPartitions: %d\nEmpty partitions: %d\nMigrate: %d\nDeduplicate: %d\nConflicts: %d\n",
		report.Root, report.Target, report.Partitions, report.EmptyPartitions, report.Migrated, report.Deduped, report.Conflicts)
	for _, item := range report.Items {
		fmt.Fprintf(&b, "- %s/%s: %s", item.Partition, item.Name, item.Action)
		if item.Reason != "" {
			fmt.Fprintf(&b, " (%s)", item.Reason)
		}
		b.WriteByte('\n')
	}
	if !report.Applied {
		b.WriteString("Dry-run only; re-run with --apply after reviewing conflicts.\n")
	}
	return b.String()
}
