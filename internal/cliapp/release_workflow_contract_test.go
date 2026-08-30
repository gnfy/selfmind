package cliapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflowContract struct {
	Permissions map[string]string         `yaml:"permissions"`
	Jobs        map[string]workflowJobDef `yaml:"jobs"`
}

type workflowJobDef struct {
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []struct {
		Name string `yaml:"name"`
		Run  string `yaml:"run"`
	} `yaml:"steps"`
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

func TestCIWorkflowRunsCompleteOfflineCorpus(t *testing.T) {
	workflow := loadWorkflowContract(t, "ci.yml")
	linux, ok := workflow.Jobs["gate"]
	if !ok {
		t.Fatal("CI workflow has no Linux gate job")
	}
	if run := workflowRunText(linux); !strings.Contains(run, "selfcheck --profile local-full --skip-go") {
		t.Fatalf("Linux CI must run the complete offline release corpus; run steps:\n%s", run)
	}
}

func TestReleaseWorkflowPublishesOnlyAfterPackageSmoke(t *testing.T) {
	workflow := loadWorkflowContract(t, "release.yml")
	if got := workflow.Permissions["contents"]; got != "read" {
		t.Fatalf("release workflow default contents permission = %q, want read", got)
	}

	testJob := workflow.Jobs["test"]
	if run := workflowRunText(testJob); !strings.Contains(run, "selfcheck --profile local-full --skip-go") {
		t.Fatalf("release test job must run the complete offline corpus; run steps:\n%s", run)
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
	for _, need := range []string{"npm", "npm-smoke-macos"} {
		if !containsNeed(publishJob.Needs, need) {
			t.Fatalf("publish-npm must depend on %s; needs=%v", need, publishJob.Needs)
		}
	}
	if got := publishJob.Permissions["id-token"]; got != "write" {
		t.Fatalf("publish-npm id-token permission = %q, want write", got)
	}

	run := workflowRunText(publishJob)
	platformPublish := strings.Index(run, `packages[@]:0:4`)
	launcherPublish := strings.Index(run, `packages[4]`)
	if platformPublish < 0 || launcherPublish < 0 || platformPublish >= launcherPublish {
		t.Fatalf("publish-npm must publish platform packages before the launcher")
	}

	githubRelease := workflow.Jobs["publish"]
	if !containsNeed(githubRelease.Needs, "publish-npm") {
		t.Fatalf("GitHub release must wait for npm publication; needs=%v", githubRelease.Needs)
	}
}

func TestReleaseWorkflowRejectsTagAndArtifactDrift(t *testing.T) {
	workflow := loadWorkflowContract(t, "release.yml")
	sourceRun := workflowRunText(workflow.Jobs["source"])
	for _, required := range []string{"merge-base --is-ancestor", `remote_tag`, `not workflow commit`} {
		if !strings.Contains(sourceRun, required) {
			t.Fatalf("release source guard is missing %q", required)
		}
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
		`run_selfmind status`, `run_selfmind tasks`, `${data_dir}/control.db`,
		"No active task.", "No open tasks.",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("installed gateway smoke is missing %q", required)
		}
	}
	if !strings.Contains(script, `HOME="${smoke_home}"`) || !strings.Contains(script, `data_dir: \"${data_dir}\"`) {
		t.Fatal("installed gateway smoke must isolate both home and durable data")
	}
}
