package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

// TestReleaseCorpusHasCompleteEvidence keeps evalcases/ honest as a release
// gate. Draft scenarios belong outside evalcases; every model-backed case here
// must land with replay evidence, while deterministic control-plane cases must
// explicitly opt out of the model and must not carry stale cassettes.
func TestReleaseCorpusHasCompleteEvidence(t *testing.T) {
	evalRoot := filepath.Clean("../../evalcases")
	vcrRoot := filepath.Clean("../../.vcr")
	files, err := ListCaseFiles(evalRoot)
	if err != nil {
		t.Fatal(err)
	}
	caseIDs := make(map[string]string, len(files))
	for _, file := range files {
		c, err := LoadCase(file)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if previous, exists := caseIDs[c.ID]; exists {
			t.Errorf("duplicate eval id %q in %s and %s", c.ID, previous, file)
		} else {
			caseIDs[c.ID] = file
		}
		if strings.TrimSpace(c.Suite) == "" {
			t.Errorf("%s: release cases must declare suite ownership", file)
		}
		hasCassette := llm.HasCassetteSession(vcrRoot, c.ID)
		switch {
		case c.RequiresModel() && !hasCassette:
			t.Errorf("%s: model-backed release case has no .vcr/%s cassette; record it or remove the case from evalcases", file, c.ID)
		case !c.RequiresModel() && hasCassette:
			t.Errorf("%s: model_required:false case has stale .vcr/%s data; remove the cassette", file, c.ID)
		}
	}

	entries, err := os.ReadDir(vcrRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, exists := caseIDs[entry.Name()]; !exists {
			t.Errorf("orphan cassette directory .vcr/%s has no release case", entry.Name())
		}
	}
}
