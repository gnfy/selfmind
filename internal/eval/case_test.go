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

func TestLoadCaseParsesRequireCassetteAndPerTurnIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: identity_override
channel: cli
require_cassette: true
turns:
  - input: "hello"
  - input: "/status"
    channel: weixin
    platform_user_id: " eval-stranger "
`), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("LoadCase failed: %v", err)
	}
	if !c.RequireCassette {
		t.Fatalf("require_cassette should parse as true")
	}
	if got := c.Turns[0].PlatformUserID; got != "" {
		t.Fatalf("turn 0 platform_user_id = %q, want empty (case default identity)", got)
	}
	if got := c.Turns[1].PlatformUserID; got != "eval-stranger" {
		t.Fatalf("turn 1 platform_user_id = %q, want eval-stranger", got)
	}
}

func TestLoadCaseParsesStructuredCompletionExpectations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: structured_completion
turns:
  - input: "change and verify"
expect:
  status: completed
  completion_reason: completed
  resumable: false
  verification_state: passed
`), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("LoadCase failed: %v", err)
	}
	if c.Expect.CompletionReason != "completed" || c.Expect.Resumable == nil || *c.Expect.Resumable || c.Expect.VerificationState != "passed" {
		t.Fatalf("structured expectations not parsed: %+v", c.Expect)
	}
}

func TestLoadCaseRejectsSharedDataWithIsolationNeeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: shared_conflict
shared_data: true
setup:
  files:
    "a.txt": "seed"
turns:
  - input: "hello"
`), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadCase(path)
	if err == nil {
		t.Fatal("LoadCase should reject shared_data combined with setup")
	}
	if !strings.Contains(err.Error(), "shared_data") {
		t.Fatalf("error should mention shared_data, got: %v", err)
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
