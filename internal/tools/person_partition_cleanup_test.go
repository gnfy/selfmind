package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupOrphanPersonPartitionsDryRunAndQuarantine(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".selfmind")
	known := "person_known"
	orphan := "person_orphan"
	for _, name := range []string{known, orphan} {
		if err := os.MkdirAll(filepath.Join(root, name, "learning"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, orphan, "learning", "evidence.json"), []byte("recoverable"), 0o600); err != nil {
		t.Fatal(err)
	}

	dry, err := CleanupOrphanPersonPartitions(root, []string{known}, false)
	if err != nil || dry.Candidates != 1 || dry.Protected != 1 || dry.Quarantined != 0 {
		t.Fatalf("dry=%+v err=%v", dry, err)
	}
	if _, err := os.Stat(filepath.Join(root, orphan)); err != nil {
		t.Fatalf("dry-run moved orphan: %v", err)
	}

	applied, err := CleanupOrphanPersonPartitions(root, []string{known}, true)
	if err != nil || applied.Quarantined != 1 || applied.QuarantineDir == "" {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	if _, err := os.Stat(filepath.Join(root, orphan)); !os.IsNotExist(err) {
		t.Fatalf("orphan remains at source: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(applied.QuarantineDir, orphan, "learning", "evidence.json"))
	if err != nil || string(data) != "recoverable" {
		t.Fatalf("quarantined evidence=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, known)); err != nil {
		t.Fatalf("known person moved: %v", err)
	}
}

func TestCleanupOrphanPersonPartitionsSkipsSymlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".selfmind")
	outside := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "person_link")); err != nil {
		t.Fatal(err)
	}
	report, err := CleanupOrphanPersonPartitions(root, nil, true)
	if err != nil || report.Candidates != 0 || report.Skipped != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "person_link")); err != nil {
		t.Fatalf("symlink removed: %v", err)
	}
}

func TestCleanupOrphanPersonPartitionsRejectsEmptyAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".selfmind")
	partition := filepath.Join(root, "person_real", "skills")
	if err := os.MkdirAll(partition, 0o700); err != nil {
		t.Fatal(err)
	}

	dry, err := CleanupOrphanPersonPartitions(root, nil, false)
	if err != nil || !dry.Inconclusive || dry.Candidates != 1 {
		t.Fatalf("dry=%+v err=%v", dry, err)
	}
	applied, err := CleanupOrphanPersonPartitions(root, nil, true)
	if err == nil || !applied.Inconclusive || applied.Quarantined != 0 {
		t.Fatalf("apply=%+v err=%v", applied, err)
	}
	if _, statErr := os.Stat(partition); statErr != nil {
		t.Fatalf("partition moved despite empty authority: %v", statErr)
	}
}
