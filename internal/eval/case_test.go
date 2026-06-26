package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCaseAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: sample
turns:
  - input: "hello"
`), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("LoadCase failed: %v", err)
	}
	if c.Channel != "cli" {
		t.Fatalf("channel = %q, want cli", c.Channel)
	}
	if !c.Checks.NoMojibake || !c.Checks.NoEmptyResponse {
		t.Fatalf("default checks not applied: %+v", c.Checks)
	}
}

func TestLoadCaseAllowsPerTurnChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: channels
channel: cli
turns:
  - input: "hello"
    channel: " weixin "
`), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("LoadCase failed: %v", err)
	}
	if got := c.Turns[0].Channel; got != "weixin" {
		t.Fatalf("turn channel = %q, want weixin", got)
	}
}

func TestLoadCaseRejectsMojibakeFixtureText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: bad_fixture
turns:
  - input: "鍒嗘瀽涓€涓嬪綋鍓?selfmind 椤圭洰"
`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadCase(path)
	if err == nil {
		t.Fatalf("LoadCase should reject mojibake fixture text")
	}
	if !strings.Contains(err.Error(), "mojibake") || !strings.Contains(err.Error(), "turns[0].input") {
		t.Fatalf("error should identify mojibake field, got: %v", err)
	}
}
