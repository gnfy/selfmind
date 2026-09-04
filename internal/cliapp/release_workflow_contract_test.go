package cliapp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflowContract struct {
	On          workflowTriggers          `yaml:"on"`
	Concurrency map[string]any            `yaml:"concurrency"`
	Permissions map[string]string         `yaml:"permissions"`
	Jobs        map[string]workflowJobDef `yaml:"jobs"`
}

type workflowTriggers struct {
	Push struct {
		Branches []string `yaml:"branches"`
	} `yaml:"push"`
	PullRequest struct {
		Branches []string `yaml:"branches"`
	} `yaml:"pull_request"`
}

type workflowJobDef struct {
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStepDef `yaml:"steps"`
}

type workflowStepDef struct {
	Name string `yaml:"name"`
	If   string `yaml:"if"`
	Run  string `yaml:"run"`
	Uses string `yaml:"uses"`
}

func loadWorkflowContract(t *testing.T, name string) workflowContract {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var workflow workflowContract
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return workflow
}

func workflowRunText(job workflowJobDef) string {
	var runs []string
	for _, step := range job.Steps {
		runs = append(runs, step.Run)
	}
	return strings.Join(runs, "\n")
}

func workflowStep(t *testing.T, job workflowJobDef, name string) workflowStepDef {
	t.Helper()
	for _, step := range job.Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("workflow job %q has no step %q", job.Name, name)
	return workflowStepDef{}
}

func containsNeed(needs any, want string) bool {
	switch values := needs.(type) {
	case string:
		return values == want
	case []any:
		for _, need := range values {
			if text, ok := need.(string); ok && text == want {
				return true
			}
		}
	}
	return false
}

func TestCIWorkflowSeparatesFastPRFromCompleteParallelMainGate(t *testing.T) {
	workflow := loadWorkflowContract(t, "ci.yml")
	if got := strings.Join(workflow.On.Push.Branches, ","); got != "main" {
		t.Fatalf("CI push branches = %q, want main only; develop pushes duplicate an open PR run", got)
	}
	if got := strings.Join(workflow.On.PullRequest.Branches, ","); got != "main" {
		t.Fatalf("CI pull-request branches = %q, want main", got)
	}
	for _, name := range []string{"core", "race", "eval", "npm-linux", "macos"} {
		if _, ok := workflow.Jobs[name]; !ok {
			t.Fatalf("CI workflow has no %s job", name)
		}
	}
	coreRun := workflowRunText(workflow.Jobs["core"])
	for _, required := range []string{"docs check", "go vet ./...", "go build ./...", "go test ./..."} {
		if !strings.Contains(coreRun, required) {
			t.Fatalf("Linux core job is missing %q; run steps:\n%s", required, coreRun)
		}
	}
	races := workflow.Jobs["race"]
	if !strings.Contains(races.If, "event_name != 'pull_request'") || !strings.Contains(workflowRunText(races), "go test -race") {
		t.Fatalf("race gate must run on main/manual but not PR: if=%q", races.If)
	}
	evalJob := workflow.Jobs["eval"]
	fast := workflowStep(t, evalJob, "Fast provider-offline PR corpus")
	full := workflowStep(t, evalJob, "Complete provider-offline main corpus")
	if !strings.Contains(fast.If, "event_name == 'pull_request'") || !strings.Contains(fast.Run, "--profile local-fast --skip-go") {
		t.Fatalf("PR eval is not the fast profile: if=%q run=%q", fast.If, fast.Run)
	}
	if !strings.Contains(full.If, "event_name != 'pull_request'") || !strings.Contains(full.Run, "--profile local-full --skip-go") {
		t.Fatalf("main eval is not the complete profile: if=%q run=%q", full.If, full.Run)
	}
	for job, platform := range map[string]string{"npm-linux": "linux-x64", "macos": "darwin-arm64"} {
		run := workflowRunText(workflow.Jobs[job])
		if !strings.Contains(run, "smoke-npm-packages.sh") {
			t.Fatalf("%s must smoke the packed npm distribution", job)
		}
		// CI smokes only the platform it installs; building the other three
		// cross-compiled binaries was pure waste (release.yml still stages
		// and verifies all four).
		if !strings.Contains(run, "smoke-npm-packages.sh 0.0.0-ci "+platform) {
			t.Fatalf("%s must smoke exactly its own platform %s; run steps:\n%s", job, platform, run)
		}
	}
}

// A cancelled main push run leaves that SHA without the successful exact-SHA
// CI evidence release.yml's source gate requires, so superseded-run
// cancellation must stay scoped to pull requests.
func TestCIWorkflowCancelsOnlyPullRequestRuns(t *testing.T) {
	workflow := loadWorkflowContract(t, "ci.yml")
	cancel := fmt.Sprint(workflow.Concurrency["cancel-in-progress"])
	if !strings.Contains(cancel, "github.event_name == 'pull_request'") {
		t.Fatalf("ci concurrency cancel-in-progress = %q, want it scoped to pull_request events", cancel)
	}
}

// Documentation-only changes skip the platform, package, and race jobs while
// the core job (docs contract, vet, build, full test) and the eval corpus
// keep running, so every main SHA still carries build/test/corpus evidence.
// The classifier must fail open: doubt counts as a code change.
func TestCIWorkflowSkipsPlatformJobsForDocsOnlyChanges(t *testing.T) {
	workflow := loadWorkflowContract(t, "ci.yml")
	changes, ok := workflow.Jobs["changes"]
	if !ok {
		t.Fatal("CI workflow has no changes classification job")
	}
	if run := workflowRunText(changes); !strings.Contains(run, `echo "code=true"`) {
		t.Fatalf("changes classifier must default (fail open) to code=true; run steps:\n%s", run)
	}
	for _, name := range []string{"npm-linux", "macos", "race"} {
		job := workflow.Jobs[name]
		if !containsNeed(job.Needs, "changes") {
			t.Fatalf("%s must depend on the changes classifier; needs=%v", name, job.Needs)
		}
		if !strings.Contains(job.If, "needs.changes.outputs.code == 'true'") {
			t.Fatalf("%s must skip documentation-only changes; if=%q", name, job.If)
		}
	}
	for _, name := range []string{"core", "eval"} {
		if got := workflow.Jobs[name].If; got != "" {
			t.Fatalf("%s must run unconditionally; if=%q", name, got)
		}
	}
}

func TestWorkflowsUseNode24ActionMajors(t *testing.T) {
	want := map[string]string{
		"actions/checkout":          "v5",
		"actions/setup-go":          "v6",
		"actions/setup-node":        "v5",
		"actions/upload-artifact":   "v6",
		"actions/download-artifact": "v7",
	}
	for _, workflowName := range []string{"ci.yml", "release.yml"} {
		workflow := loadWorkflowContract(t, workflowName)
		for jobName, job := range workflow.Jobs {
			for _, step := range job.Steps {
				for action, major := range want {
					if strings.HasPrefix(step.Uses, action+"@") && step.Uses != action+"@"+major {
						t.Errorf("%s job %s step %q uses %s; want %s@%s to avoid the retired Node 20 runtime",
							workflowName, jobName, step.Name, step.Uses, action, major)
					}
				}
			}
		}
	}
}

func TestReleaseWorkflowPublishesOnlyAfterPackageSmoke(t *testing.T) {
	workflow := loadWorkflowContract(t, "release.yml")
	if got := workflow.Permissions["contents"]; got != "read" {
		t.Fatalf("release workflow default contents permission = %q, want read", got)
	}

	testJob := workflow.Jobs["test"]
	if !containsNeed(testJob.Needs, "source") {
		t.Fatalf("stable release gate must wait for source verification; needs=%v", testJob.Needs)
	}
	if run := workflowRunText(testJob); !strings.Contains(run, "selfcheck --profile local-full --skip-go") || !strings.Contains(run, "reuses the successful exact-SHA main CI gate") {
		t.Fatalf("stable releases must rerun the full gate while prereleases reuse main CI; run steps:\n%s", run)
	}

	// The race gate runs as its own matrixed job beside the stable gate (the
	// serial in-job form spent 8-11 minutes alone); binaries must still wait
	// for BOTH, and prereleases skip the race steps the same way the stable
	// gate skips its own.
	raceJob, ok := workflow.Jobs["race"]
	if !ok {
		t.Fatal("release workflow has no stable race gate job")
	}
	if !containsNeed(raceJob.Needs, "source") {
		t.Fatalf("stable race gate must wait for source verification; needs=%v", raceJob.Needs)
	}
	if run := workflowRunText(raceJob); !strings.Contains(run, "go test -race") || !strings.Contains(run, "reuses the successful exact-SHA main CI gate") {
		t.Fatalf("stable race gate must run race tests and skip for prereleases; run steps:\n%s", run)
	}
	raceStep := workflowStep(t, raceJob, "Race-sensitive runtime tests")
	if !strings.Contains(raceStep.If, "prerelease != 'true'") {
		t.Fatalf("race tests must be gated to stable releases; if=%q", raceStep.If)
	}
	buildJob := workflow.Jobs["build"]
	for _, need := range []string{"test", "race"} {
		if !containsNeed(buildJob.Needs, need) {
			t.Fatalf("release binaries must wait for the %s gate; needs=%v", need, buildJob.Needs)
		}
	}

	packageJob := workflow.Jobs["npm"]
	packageRun := workflowRunText(packageJob)
	if strings.Contains(packageRun, "npm publish") {
		t.Fatal("npm packaging job must not publish before native package smoke")
	}
	if !strings.Contains(packageRun, "smoke-installed-gateway.sh") {
		t.Fatal("Linux package smoke must exercise the installed gateway lifecycle")
	}
	macSmokeRun := workflowRunText(workflow.Jobs["npm-smoke-macos"])
	if !strings.Contains(macSmokeRun, "smoke-installed-gateway.sh") {
		t.Fatal("macOS package smoke must exercise the installed gateway lifecycle")
	}

	publishJob, ok := workflow.Jobs["publish-npm"]
	if !ok {
		t.Fatal("release workflow has no publish-npm job")
	}
	for _, need := range []string{"npm", "npm-smoke-macos", "finalize-tag"} {
		if !containsNeed(publishJob.Needs, need) {
			t.Fatalf("publish-npm must depend on %s; needs=%v", need, publishJob.Needs)
		}
	}
	if got := publishJob.Permissions["id-token"]; got != "write" {
		t.Fatalf("publish-npm id-token permission = %q, want write", got)
	}

	run := workflowRunText(publishJob)
	if !strings.Contains(run, `archive_path="./npm-packs/${archive}"`) || !strings.Contains(run, `test -f "${archive_path}"`) {
		t.Fatal("publish-npm must fail explicitly when a downloaded tarball is missing")
	}
	publishCalls := strings.Count(run, "npm publish ")
	localPublishCalls := strings.Count(run, `npm publish "./npm-packs/`)
	if publishCalls == 0 || localPublishCalls != publishCalls {
		t.Fatalf("every npm publish input must be an explicit local tarball path; publish calls=%d explicit local calls=%d", publishCalls, localPublishCalls)
	}
	platformPublish := strings.Index(run, `packages[@]:0:4`)
	launcherPublish := strings.Index(run, `packages[4]`)
	if platformPublish < 0 || launcherPublish < 0 || platformPublish >= launcherPublish {
		t.Fatalf("publish-npm must publish platform packages before the launcher")
	}

	githubRelease := workflow.Jobs["publish"]
	for _, need := range []string{"finalize-tag", "publish-npm"} {
		if !containsNeed(githubRelease.Needs, need) {
			t.Fatalf("GitHub release must wait for %s; needs=%v", need, githubRelease.Needs)
		}
	}
}

func TestReleaseWorkflowVerifiesMainCIAndFinalizesTagAfterSmoke(t *testing.T) {
	workflow := loadWorkflowContract(t, "release.yml")
	sourceRun := workflowRunText(workflow.Jobs["source"])
	for _, required := range []string{
		"merge-base --is-ancestor", "actions/workflows/ci.yml/runs", "event=push", "status=success",
		"will be created after package verification", "not workflow commit",
	} {
		if !strings.Contains(sourceRun, required) {
			t.Fatalf("release source guard is missing %q", required)
		}
	}
	if got := workflow.Jobs["source"].Permissions["actions"]; got != "read" {
		t.Fatalf("release source actions permission = %q, want read", got)
	}

	finalize := workflow.Jobs["finalize-tag"]
	for _, need := range []string{"source", "npm", "npm-smoke-macos"} {
		if !containsNeed(finalize.Needs, need) {
			t.Fatalf("tag finalization must wait for %s; needs=%v", need, finalize.Needs)
		}
	}
	if got := finalize.Permissions["contents"]; got != "write" {
		t.Fatalf("tag finalization contents permission = %q, want write", got)
	}
	finalizeRun := workflowRunText(finalize)
	if !strings.Contains(finalizeRun, "scripts/finalize-release-tag.sh") {
		t.Fatalf("tag finalization must use the tested script; run steps:\n%s", finalizeRun)
	}

	publishRun := workflowRunText(workflow.Jobs["publish-npm"])
	for _, required := range []string{"partially published", "dist.integrity", "sha512-"} {
		if !strings.Contains(publishRun, required) {
			t.Fatalf("npm publication guard is missing %q", required)
		}
	}

	buildRun := workflowRunText(workflow.Jobs["build"])
	if strings.Contains(buildRun, "packaging/linux/install.sh") || strings.Contains(buildRun, "selfmind.service") {
		t.Fatal("release tarballs must not bundle the obsolete root/system service installer")
	}
}

func TestFinalizeReleaseTagScriptCreatesIdempotentlyAndRejectsDrift(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "finalize-release-tag.sh"))
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	work := filepath.Join(t.TempDir(), "work")
	runTestCommand(t, "", "git", "init", "--bare", remote)
	runTestCommand(t, "", "git", "init", work)
	runTestCommand(t, work, "git", "config", "user.name", "SelfMind Release Test")
	runTestCommand(t, work, "git", "config", "user.email", "release-test@selfmind.invalid")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestCommand(t, work, "git", "add", "README.md")
	runTestCommand(t, work, "git", "commit", "-m", "first")
	runTestCommand(t, work, "git", "branch", "-M", "main")
	runTestCommand(t, work, "git", "remote", "add", "origin", remote)
	runTestCommand(t, work, "git", "push", "-u", "origin", "main")
	firstSHA := strings.TrimSpace(runTestCommand(t, work, "git", "rev-parse", "HEAD"))

	const tag = "v0.1.0-beta.test"
	output, err := runReleaseTagScript(script, work, tag, firstSHA)
	if err != nil || !strings.Contains(output, "created "+tag) {
		t.Fatalf("create tag: err=%v output=%s", err, output)
	}
	output, err = runReleaseTagScript(script, work, tag, firstSHA)
	if err != nil || !strings.Contains(output, "verified existing "+tag) {
		t.Fatalf("idempotent tag verification: err=%v output=%s", err, output)
	}

	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestCommand(t, work, "git", "add", "README.md")
	runTestCommand(t, work, "git", "commit", "-m", "second")
	secondSHA := strings.TrimSpace(runTestCommand(t, work, "git", "rev-parse", "HEAD"))
	output, err = runReleaseTagScript(script, work, tag, secondSHA)
	if err == nil || !strings.Contains(output, "not verified commit") {
		t.Fatalf("tag drift must fail: err=%v output=%s", err, output)
	}
	remoteSHA := strings.TrimSpace(runTestCommand(t, "", "git", "--git-dir", remote, "rev-parse", "refs/tags/"+tag))
	if remoteSHA != firstSHA {
		t.Fatalf("drift check changed remote tag to %s, want %s", remoteSHA, firstSHA)
	}
}

func runReleaseTagScript(script, work, tag, sha string) (string, error) {
	cmd := exec.Command("bash", script)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "TAG="+tag, "GITHUB_SHA="+sha)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func runTestCommand(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func TestInstalledGatewaySmokeCoversCoreLifecycle(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "smoke-installed-gateway.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	script := string(data)
	for _, required := range []string{
		"mktemp -d", "trap cleanup", "gateway start", "gateway restart --drain",
		"gateway stop", `assert_state "${stopped_status}" "stopped"`,
		`run_selfmind status`, `run_selfmind resume`, `${data_dir}/control.db`,
		"No active task.", "Nothing needs attention.",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("installed gateway smoke is missing %q", required)
		}
	}
	// This contract pinned the script's TEXT, so it stayed green when the
	// command it names was retired and only CI caught the break. Assert the
	// commands the script drives are still in the CLI's own surface.
	root := requireFile(t, filepath.Join("..", "..", "internal", "cliapp", "root.go"))
	for _, invoked := range []string{"status", "resume", "gateway"} {
		if !strings.Contains(root, `"selfmind `+invoked) {
			t.Fatalf("smoke drives %q, which no longer appears in the CLI usage surface", invoked)
		}
	}
	if !strings.Contains(script, `HOME="${smoke_home}"`) || !strings.Contains(script, `data_dir: \"${data_dir}\"`) {
		t.Fatal("installed gateway smoke must isolate both home and durable data")
	}
}

func requireFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
