package eval

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// expectationFieldNames collects the yaml keys a case's expect block accepts.
func expectationFieldNames() map[string]bool {
	names := map[string]bool{}
	t := reflect.TypeOf(Expectations{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		names[strings.TrimSpace(strings.Split(tag, ",")[0])] = true
	}
	return names
}

// TestCaseExpectationKeysAreRecognized guards against an assertion that cannot
// fail. yaml.Unmarshal ignores unknown keys silently, so a plausible-looking but
// wrong name (not_contains instead of must_not_contain) reads as a passing case
// while asserting nothing at all.
func TestCaseExpectationKeysAreRecognized(t *testing.T) {
	root := evalRepoRoot(t)
	known := expectationFieldNames()
	casesDir := filepath.Join(root, "evalcases")

	err := filepath.Walk(casesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var raw struct {
			Expect map[string]interface{} `yaml:"expect"`
		}
		if yaml.Unmarshal(data, &raw) != nil {
			// Malformed YAML is the corpus loader's concern, not this test's.
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for key := range raw.Expect {
			if !known[key] {
				t.Errorf("%s: expect key %q is not a recognized expectation; it is silently ignored and asserts nothing", rel, key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func evalRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root")
		}
		dir = parent
	}
}
