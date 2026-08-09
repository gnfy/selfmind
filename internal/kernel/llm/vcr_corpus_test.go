package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const vcrCorpusRoot = "../../../.vcr"

func vcrCorpusFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(vcrCorpusRoot, "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no cassettes checked out")
	}
	sort.Strings(files)
	return files
}

// A cassette that carries the recording machine's absolute paths only replays
// on that machine. Five of them were recorded before WithVCRWorkspace existed
// and made an eval case pass locally while failing in CI for weeks, because
// every replayed tool call pointed at a directory the runner did not have.
func TestCassettesCarryNoMachineAbsolutePaths(t *testing.T) {
	// Anything rooted at a real user's home or mount point. The portable form
	// is the {{SELFMIND_VCR_WORKSPACE}} placeholder that save() writes.
	markers := []string{"/home/", "/Users/", "/mnt/", "/root/", "c:\\", "C:\\"}
	for _, file := range vcrCorpusFiles(t) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, marker := range markers {
			if strings.Contains(text, marker) {
				t.Errorf("%s contains the recording machine's path %q; re-record it, or migrate the literal workspace prefix to %s",
					file, marker, vcrWorkspacePlaceholder)
			}
		}
	}
}

// knownErrorCassettes is a ratchet, not an allowlist to grow. Each entry
// replays a provider failure recorded during one flaky session, so the call it
// stands for is never verified — every `completion` cassette in the corpus is
// one of these, which is why nothing exercises that path today. save() now
// refuses to write new ones; clearing these needs a live re-record.
var knownErrorCassettes = map[string]struct{}{
	"continuity_resume/0000.json":                  {},
	"continuity_task_attach/0000.json":             {},
	"continuity_task_attach/0002.json":             {},
	"memory_pinned_recall/0000.json":               {},
	"memory_pinned_recall/0003.json":               {},
	"recall_cross_task/0000.json":                  {},
	"recall_cross_task/0002.json":                  {},
	"reliability_create_and_verify/0000.json":      {},
	"reliability_external_watch_handoff/0000.json": {},
}

func TestRecordedProviderFailuresDoNotGrow(t *testing.T) {
	for _, file := range vcrCorpusFiles(t) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var c cassette
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if strings.TrimSpace(c.Error) == "" {
			continue
		}
		key := filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(file), filepath.ToSlash(vcrCorpusRoot)+"/"))
		if _, known := knownErrorCassettes[key]; !known {
			t.Errorf("%s records a provider failure (%q) that will replay forever; re-record the case instead of committing the failure",
				key, c.Error)
		}
	}
}

// An empty cassette directory reads as "this case has a recording" to anyone
// scanning the corpus, while replay finds nothing at ordinal 0000.
func TestNoEmptyCassetteDirectories(t *testing.T) {
	entries, err := os.ReadDir(vcrCorpusRoot)
	if err != nil {
		t.Skip("no cassette corpus checked out")
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(vcrCorpusRoot, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Errorf(".vcr/%s is empty; delete it or record the case", entry.Name())
		}
	}
}
