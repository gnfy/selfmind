package eval

import (
	"os"
	"path/filepath"
	"testing"
)

// seedResidueRoots builds two temp roots shaped like the real leak: a config
// home with per-case skills dirs and a data dir with per-case memory tenants,
// plus decoys that must never be touched.
func seedResidueRoots(t *testing.T) (homeRoot, dataRoot string) {
	t.Helper()
	homeRoot = t.TempDir()
	dataRoot = t.TempDir()

	mkdir := func(parts ...string) string {
		t.Helper()
		p := filepath.Join(parts...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(size int, parts ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(parts...), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Qualifying: empty skills/ subtree under the config home.
	mkdir(homeRoot, "eval-chat_basic_001-1782580513697563644", "skills")
	// Qualifying: skills tree with a skill markdown file.
	skillDir := mkdir(homeRoot, "eval-code_snippet_001-1782580519975231080", "skills", "my-skill")
	write(64, skillDir, "SKILL.md")
	// Qualifying: memory store files under the data dir.
	memDir := mkdir(dataRoot, "eval-chat_basic_001-1782580513697563644")
	write(4096, memDir, "memory.db")
	write(128, memDir, "memory.db-wal")
	// Non-matching name: never a candidate, never reported.
	mkdir(homeRoot, "eval-not-a-residue-dir")
	// Matching name but with a foreign file: must be reported and skipped.
	foreign := mkdir(dataRoot, "eval-foreign_case-1782580513697563000")
	write(10, foreign, "notes.docx")

	return homeRoot, dataRoot
}

// TestCleanDiskResidueDryRunDeletesNothing: the default is a pure report — a
// full scan with per-root sizes, zero filesystem mutation.
func TestCleanDiskResidueDryRunDeletesNothing(t *testing.T) {
	homeRoot, dataRoot := seedResidueRoots(t)

	report := CleanDiskResidue([]string{homeRoot, dataRoot}, false)
	if report.Removed != 0 {
		t.Fatalf("dry run must not remove anything, got %d", report.Removed)
	}
	if got := report.TotalDirs(); got != 3 {
		t.Fatalf("expected 3 qualifying dirs, got %d (%+v)", got, report)
	}
	if got := report.TotalSkipped(); got != 1 {
		t.Fatalf("expected 1 skipped dir, got %d (%+v)", got, report)
	}
	// 64 (SKILL.md) + 4096 (memory.db) + 128 (wal) content bytes.
	if got := report.TotalBytes(); got != 64+4096+128 {
		t.Fatalf("unexpected total bytes: %d", got)
	}
	// Nothing on disk moved, including the foreign-file dir.
	for _, p := range []string{
		filepath.Join(homeRoot, "eval-chat_basic_001-1782580513697563644", "skills"),
		filepath.Join(dataRoot, "eval-chat_basic_001-1782580513697563644", "memory.db"),
		filepath.Join(dataRoot, "eval-foreign_case-1782580513697563000", "notes.docx"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("dry run mutated the filesystem: %s (%v)", p, err)
		}
	}
}

// TestCleanDiskResidueApplyRemovesOnlyQualifying: --yes removes exactly the
// verified residue; the non-matching name and the foreign-file dir survive.
func TestCleanDiskResidueApplyRemovesOnlyQualifying(t *testing.T) {
	homeRoot, dataRoot := seedResidueRoots(t)

	report := CleanDiskResidue([]string{homeRoot, dataRoot}, true)
	if report.Removed != 3 {
		t.Fatalf("expected 3 removed dirs, got %d (%+v)", report.Removed, report)
	}
	if got := report.TotalSkipped(); got != 1 {
		t.Fatalf("expected 1 skipped dir, got %d (%+v)", got, report)
	}
	for _, gone := range []string{
		filepath.Join(homeRoot, "eval-chat_basic_001-1782580513697563644"),
		filepath.Join(homeRoot, "eval-code_snippet_001-1782580519975231080"),
		filepath.Join(dataRoot, "eval-chat_basic_001-1782580513697563644"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("qualifying residue dir should be removed: %s (err=%v)", gone, err)
		}
	}
	for _, kept := range []string{
		filepath.Join(homeRoot, "eval-not-a-residue-dir"),
		filepath.Join(dataRoot, "eval-foreign_case-1782580513697563000", "notes.docx"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("non-qualifying path must survive: %s (%v)", kept, err)
		}
	}
}

// TestRemoveEvalTenantDir: the harness self-sweep removes exactly its own
// throwaway tenant dir, and refuses non-eval tenant IDs or foreign contents.
func TestRemoveEvalTenantDir(t *testing.T) {
	base := t.TempDir()
	tenant := "eval-chat_basic_001-1782580513697563644"
	if err := os.MkdirAll(filepath.Join(base, tenant, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !RemoveEvalTenantDir(base, tenant) {
		t.Fatal("expected the eval tenant dir to be removed")
	}
	if _, err := os.Stat(filepath.Join(base, tenant)); !os.IsNotExist(err) {
		t.Fatalf("tenant dir should be gone (err=%v)", err)
	}

	// A non-eval tenant ID never qualifies, even if the dir exists.
	if err := os.MkdirAll(filepath.Join(base, "default", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if RemoveEvalTenantDir(base, "default") {
		t.Fatal("non-eval tenant must never be removed")
	}
	if _, err := os.Stat(filepath.Join(base, "default")); err != nil {
		t.Fatalf("default tenant dir must survive: %v", err)
	}

	// Foreign contents disqualify the sweep even for a matching tenant ID.
	dirty := "eval-dirty_case-1782580513697563001"
	if err := os.MkdirAll(filepath.Join(base, dirty), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, dirty, "important.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if RemoveEvalTenantDir(base, dirty) {
		t.Fatal("foreign contents must block the self-sweep")
	}
	if _, err := os.Stat(filepath.Join(base, dirty, "important.bin")); err != nil {
		t.Fatalf("foreign file must survive: %v", err)
	}
}

// TestCleanDiskResidueGuards: symlinked names, nested matches, and missing
// roots never become candidates — the scanner only ever looks at direct,
// real child directories of the given roots.
func TestCleanDiskResidueGuards(t *testing.T) {
	root := t.TempDir()
	// Nested matching dir (not a direct child of the root): ignored.
	nested := filepath.Join(root, "sub", "eval-nested_case-1782580513697563001")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// Symlink with a matching name pointing at real data: ignored.
	target := filepath.Join(t.TempDir(), "precious")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "memory.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "eval-link_case-1782580513697563002")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	report := CleanDiskResidue([]string{root, filepath.Join(root, "does-not-exist")}, true)
	if !report.Empty() || report.Removed != 0 {
		t.Fatalf("guards failed: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(target, "memory.db")); err != nil {
		t.Fatalf("symlink target must be untouched: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("nested dir must be untouched: %v", err)
	}
}
