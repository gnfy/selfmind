package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const orphanPartitionQuarantineDir = ".orphaned-person-partitions"

type PersonPartitionCleanupReport struct {
	Root          string   `json:"root"`
	ControlDB     string   `json:"control_db,omitempty"`
	Applied       bool     `json:"applied"`
	Inconclusive  bool     `json:"inconclusive,omitempty"`
	KnownPersons  int      `json:"known_persons"`
	Candidates    int      `json:"candidates"`
	Protected     int      `json:"protected"`
	Skipped       int      `json:"skipped"`
	Quarantined   int      `json:"quarantined"`
	QuarantineDir string   `json:"quarantine_dir,omitempty"`
	Partitions    []string `json:"partitions,omitempty"`
}

// CleanupOrphanPersonPartitions finds filesystem person partitions that do
// not exist in control.db. Apply mode moves them into a timestamped quarantine
// under the same SelfMind root, so recovery is a rename rather than a restore
// from backup. Known persons and symlinks are never moved.
func CleanupOrphanPersonPartitions(root string, knownPersonIDs []string, apply bool) (PersonPartitionCleanupReport, error) {
	root = filepath.Clean(os.ExpandEnv(strings.TrimSpace(root)))
	report := PersonPartitionCleanupReport{Root: root, Applied: apply}
	if err := validatePersonPartitionCleanupRoot(root); err != nil {
		return report, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return report, nil
		}
		return report, err
	}
	known := make(map[string]struct{}, len(knownPersonIDs))
	for _, id := range knownPersonIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			known[id] = struct{}{}
		}
	}
	report.KnownPersons = len(known)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "person_") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			report.Skipped++
			continue
		}
		if _, ok := known[entry.Name()]; ok {
			report.Protected++
			continue
		}
		report.Partitions = append(report.Partitions, entry.Name())
	}
	sort.Strings(report.Partitions)
	report.Candidates = len(report.Partitions)
	if report.KnownPersons == 0 && report.Candidates > 0 {
		report.Inconclusive = true
		if apply {
			return report, fmt.Errorf("refusing to apply cleanup: control.db contains no known persons while %d person partition(s) exist under %s", report.Candidates, root)
		}
	}
	if !apply || report.Candidates == 0 {
		return report, nil
	}
	quarantineDir := filepath.Join(root, orphanPartitionQuarantineDir, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.MkdirAll(quarantineDir, 0o700); err != nil {
		return report, err
	}
	report.QuarantineDir = quarantineDir
	for _, name := range report.Partitions {
		source := filepath.Join(root, name)
		target := filepath.Join(quarantineDir, name)
		if _, err := os.Lstat(target); err == nil {
			return report, fmt.Errorf("quarantine target already exists: %s", target)
		} else if !os.IsNotExist(err) {
			return report, err
		}
		if err := os.Rename(source, target); err != nil {
			return report, fmt.Errorf("quarantine %s: %w", name, err)
		}
		report.Quarantined++
	}
	return report, nil
}

func validatePersonPartitionCleanupRoot(root string) error {
	if strings.TrimSpace(root) == "" || root == "." {
		return fmt.Errorf("cleanup root must be an explicit SelfMind data root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if abs == filepath.VolumeName(abs)+string(os.PathSeparator) {
		return fmt.Errorf("refusing to scan filesystem root %s", abs)
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == abs {
		return fmt.Errorf("refusing to scan the home directory directly; use its .selfmind child")
	}
	return nil
}

func FormatPersonPartitionCleanupReport(report PersonPartitionCleanupReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Orphan person partition cleanup (%s)\n", map[bool]string{true: "apply", false: "dry-run"}[report.Applied])
	fmt.Fprintf(&b, "Root: %s\n", report.Root)
	if report.ControlDB != "" {
		fmt.Fprintf(&b, "Control DB: %s\n", report.ControlDB)
	}
	if report.Inconclusive {
		b.WriteString("Status: inconclusive (control.db has no known persons; apply is blocked)\n")
	} else {
		b.WriteString("Status: verified against control.db\n")
	}
	fmt.Fprintf(&b, "Known persons: %d\nCandidates: %d\nProtected: %d\nSkipped: %d\nQuarantined: %d\n",
		report.KnownPersons, report.Candidates, report.Protected, report.Skipped, report.Quarantined)
	const previewLimit = 20
	for i, name := range report.Partitions {
		if i == previewLimit {
			fmt.Fprintf(&b, "- ... and %d more\n", len(report.Partitions)-previewLimit)
			break
		}
		fmt.Fprintf(&b, "- %s\n", name)
	}
	if report.QuarantineDir != "" {
		fmt.Fprintf(&b, "Quarantine: %s\n", report.QuarantineDir)
	}
	if report.Inconclusive {
		b.WriteString("Action: verify storage.data_dir and evolution.skills_dir; no partition can be quarantined from this result.\n")
	} else if !report.Applied && report.Candidates > 0 {
		b.WriteString("Dry-run only; stop the gateway, review this list, then re-run with --apply.\n")
	}
	return b.String()
}
