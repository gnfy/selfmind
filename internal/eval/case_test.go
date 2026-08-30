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

func TestLoadCaseRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: misspelled_contract
turns:
  - input: "continue"
checks:
  require_same_task: true
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadCase(path)
	if err == nil {
		t.Fatal("LoadCase should reject an unknown checks field")
	}
	if !strings.Contains(err.Error(), "require_same_task") || !strings.Contains(err.Error(), "field") {
		t.Fatalf("error should identify the unknown field, got %v", err)
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

func TestLoadCaseDefaultsToModelRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: default_model
turns:
  - input: "hello"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("LoadCase failed: %v", err)
	}
	if !c.RequiresModel() {
		t.Fatal("model_required must default to true")
	}
}

func TestLoadCaseParsesProviderlessContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: providerless
model_required: false
turns:
  - input: "/status"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("LoadCase failed: %v", err)
	}
	if c.RequiresModel() {
		t.Fatal("model_required: false was not preserved")
	}
}

func TestLoadCaseRejectsProviderlessCassetteRequirement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: contradictory
model_required: false
require_cassette: true
turns:
  - input: "/status"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), "model_required") {
		t.Fatalf("expected model/cassette contract error, got %v", err)
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

func TestLoadCaseParsesGatewayRejectionExpectations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: gateway_rejection
turns:
  - input: "reject this"
expect:
  http_status: 400
  require_no_task: true
  require_no_run: true
checks:
  no_provider_stack_dump: true
`), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("LoadCase failed: %v", err)
	}
	if c.Expect.HTTPStatus != 400 || !c.Expect.RequireNoTask || !c.Expect.RequireNoRun {
		t.Fatalf("gateway rejection expectations not parsed: %+v", c.Expect)
	}
	if c.Checks.NoEmptyResponse {
		t.Fatalf("an expected gateway rejection must not require assistant output: %+v", c.Checks)
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

func TestLoadCaseParsesCIAndCommandRequirements(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: ci_case
ci:
  required: true
  reason: cross_platform
  platforms: [linux, macos]
requires:
  commands: [node, npm, node]
turns:
  - input: "hello"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCase(path)
	if err != nil {
		t.Fatalf("LoadCase failed: %v", err)
	}
	if !c.RequiredOnCI("linux") || !c.RequiredOnCI("darwin") || c.RequiredOnCI("windows") {
		t.Fatalf("unexpected CI platforms: %+v", c.CI)
	}
	if got := strings.Join(c.Requires.Commands, ","); got != "node,npm" {
		t.Fatalf("commands = %q, want node,npm", got)
	}
}

func TestLoadCaseRejectsIncompleteCIMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: bad_ci
ci:
  required: true
  platforms: [linux]
turns:
  - input: "hello"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), "ci.reason") {
		t.Fatalf("expected actionable ci.reason error, got %v", err)
	}
}

func TestLoadCaseRejectsCommandPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(path, []byte(`
id: bad_command
requires:
  commands: [/usr/bin/node]
turns:
  - input: "hello"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCase(path); err == nil || !strings.Contains(err.Error(), "command name") {
		t.Fatalf("expected command-name validation error, got %v", err)
	}
}
