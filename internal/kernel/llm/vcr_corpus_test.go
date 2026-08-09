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
	// "/tmp/" belongs here even though the workspace placeholder covers the
	// eval scratch directory: a recording session's HOME can itself live under
	// /tmp (mine did), and nothing else would have caught a path leaking from
	// there into a committed cassette.
	markers := []string{"/home/", "/Users/", "/mnt/", "/root/", "/tmp/", "c:\\", "C:\\"}
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

// The corpus must hold no recorded provider failure at all. Nine of them (every
// `completion` call in the corpus) sat here for weeks: each replayed a flaky
// session's timeout forever, so the call it stood for was never verified again
// while five require_cassette cases still reported green. They were re-recorded
// live on 2026-08-10; save() warns loudly when a new one is written, and this
// test is what keeps the count at zero.
func TestCorpusRecordsNoProviderFailures(t *testing.T) {
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
		t.Errorf("%s records a provider failure (%q) that will replay forever; re-record the case instead of committing the failure",
			key, c.Error)
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
