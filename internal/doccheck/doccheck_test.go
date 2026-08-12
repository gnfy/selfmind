package doccheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckAcceptsGovernedCorpus(t *testing.T) {
	root := testRepo(t)
	if err := WriteIndex(root); err != nil {
		t.Fatal(err)
	}
	report := Check(root, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	if !report.OK() {
		t.Fatalf("Check() errors:\n%s", strings.Join(report.Errors, "\n"))
	}
}

func TestCheckRejectsStaleTranslationAndExpiredPlan(t *testing.T) {
	root := testRepo(t)
	manifestPath := filepath.Join(root, ManifestPath)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(data), "review_by: 2026-08-20", "review_by: 2026-08-01", 1)
	text = strings.Replace(text, "source_hash: "+mustHash(t, filepath.Join(root, "docs/source.md")), "source_hash: deadbeef", 1)
	if err := os.WriteFile(manifestPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteIndex(root); err != nil {
		t.Fatal(err)
	}
	report := Check(root, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	joined := strings.Join(report.Errors, "\n")
	for _, want := range []string{"review expired", "translation is stale"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Check() errors do not contain %q:\n%s", want, joined)
		}
	}
}

func TestCheckRejectsUnregisteredDocAndBrokenLink(t *testing.T) {
	root := testRepo(t)
	if err := os.WriteFile(filepath.Join(root, "docs/orphan.md"), []byte("[missing](nope.md)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteIndex(root); err != nil {
		t.Fatal(err)
	}
	report := Check(root, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	joined := strings.Join(report.Errors, "\n")
	for _, want := range []string{"missing from docs/manifest.yaml", "broken local link"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Check() errors do not contain %q:\n%s", want, joined)
		}
	}
}

func TestCheckRejectsMultipleActivePlansAndInvalidUTF8(t *testing.T) {
	root := testRepo(t)
	mustWrite(t, filepath.Join(root, "docs/plan-two.md"), "# Plan Two\n")
	manifestPath := filepath.Join(root, ManifestPath)
	file, err := os.OpenFile(manifestPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`  - path: docs/plan-two.md
    title: Plan Two
    class: plan
    owner: maintainers
    state: active
    language: en
    approved_by: owner
    review_by: 2026-08-20
`)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs/source.md"), []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteIndex(root); err != nil {
		t.Fatal(err)
	}
	report := Check(root, time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	joined := strings.Join(report.Errors, "\n")
	for _, want := range []string{"2 active plans", "not clean UTF-8"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Check() errors do not contain %q:\n%s", want, joined)
		}
	}
}

func testRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "# Rules\n")
	mustWrite(t, filepath.Join(root, "docs/STATUS.md"), "# Status\n")
	mustWrite(t, filepath.Join(root, "docs/source.md"), "# Source\n")
	mustWrite(t, filepath.Join(root, "docs/translation.md"), "# Translation\n")
	mustWrite(t, filepath.Join(root, "docs/plan.md"), "# Plan\n")
	hash := mustHash(t, filepath.Join(root, "docs/source.md"))
	manifest := `version: 1
documents:
  - path: docs/README.md
    title: Documentation Index
    class: guide
    owner: docs
    state: current
    language: en
  - path: docs/STATUS.md
    title: Status
    class: status
    owner: maintainers
    state: current
    language: en
  - path: docs/source.md
    title: Source
    class: reference
    owner: runtime
    state: current
    language: en
  - path: docs/translation.md
    title: Translation
    class: reference
    owner: runtime
    state: current
    language: zh-CN
    translation_of: docs/source.md
    source_hash: ` + hash + `
  - path: docs/plan.md
    title: Plan
    class: plan
    owner: maintainers
    state: active
    language: en
    approved_by: owner
    review_by: 2026-08-20
`
	mustWrite(t, filepath.Join(root, ManifestPath), manifest)
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustHash(t *testing.T, path string) string {
	t.Helper()
	hash, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
