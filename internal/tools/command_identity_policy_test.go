package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/envprofiles"
)

func dangerousCommand(command string) (bool, string) {
	return dangerousToolCall("", "terminal", map[string]interface{}{"command": command})
}

// The dangerous heuristic names the program the shell runs, whatever case,
// path or leading assignment it is written with.
func TestDangerousClassificationNamesTheProgramThatRuns(t *testing.T) {
	for _, tc := range []struct{ command, reason string }{
		{"RM -rf build", "invokes dangerous command: rm"},
		{"Rm -rf build", "invokes dangerous command: rm"},
		{"/BIN/RM -rf build", "invokes dangerous command: rm"},
		{`\rm -rf build`, "invokes dangerous command: rm"},
		{"PATH=/usr/bin rm -rf build", "invokes dangerous command: rm"},
		{`FOO="a b" rm -rf build`, "invokes dangerous command: rm"},
		{"exec kill -9 4242", "invokes dangerous command: kill"},
		{"time CHMOD 777 build", "invokes dangerous command: chmod"},
		{"if true; then rm -rf build; fi", "invokes dangerous command: rm"},
		{"CURL -X DELETE https://api.example.test/items/1", "network egress command: curl"},
		{"/usr/bin/Curl -X DELETE https://api.example.test/items/1", "network egress command: curl"},
		{"KUBECONFIG=/tmp/kube WGET https://api.example.test/x", "network egress command: wget"},
	} {
		dangerous, reason := dangerousCommand(tc.command)
		if !dangerous || reason != tc.reason {
			t.Errorf("%q: dangerous=%v reason=%q, want %q", tc.command, dangerous, reason, tc.reason)
		}
	}
	// An assignment that loads code into the program is dangerous on its own.
	if dangerous, reason := dangerousCommand("LD_PRELOAD=/tmp/x.so curl https://api.example.test"); !dangerous || reason == "" {
		t.Fatalf("LD_PRELOAD curl must be dangerous: %v %q", dangerous, reason)
	}
}

var codeInjectionSpellings = []string{
	"%s=/tmp/x.so ls -la",
	`%s="/tmp/a b.so" ls -la`,
	"FOO=1 %s=/tmp/x.so ls -la",
	"env %s=/tmp/x.so ls -la",
	"sudo %s=/tmp/x.so ls -la",
	"sh -c '%s=/tmp/x.so ls -la'",
	"export %s=/tmp/x.so; ls -la",
	"%s=/tmp/x.so; export %s; ls -la",
}

// A loader variable maps arbitrary code into whatever program runs next, so
// outside enforced containment the program cannot vouch for the call: it is
// dangerous with any program, and it never becomes a remembered class.
func TestCodeInjectionAssignmentIsDangerousWhateverTheProgram(t *testing.T) {
	for _, variable := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH"} {
		for _, spelling := range codeInjectionSpellings {
			command := strings.ReplaceAll(spelling, "%s", variable)
			dangerous, reason := dangerousCommand(command)
			if !dangerous || !strings.Contains(reason, variable) {
				t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
			}
			if class := DescribeGrantClass("terminal", reason, map[string]interface{}{"command": command}); class != "" {
				t.Errorf("%q minted a reusable class %q", command, class)
			}
		}
	}
	// Names are case-sensitive and only the loader's own variables count.
	for _, command := range []string{
		"ld_preload=/tmp/x.so ls -la",
		"MY_LD_PRELOAD=/tmp/x.so ls -la",
		"echo LD_PRELOAD=/tmp/x.so",
		"grep -rn DYLD_INSERT_LIBRARIES docs",
	} {
		if dangerous, reason := dangerousCommand(command); dangerous {
			t.Errorf("%q flagged: %s", command, reason)
		}
	}
}

// runContainedSmart runs one terminal call through the approval funnel in
// smart mode under the given effective sandbox, refusing every ask.
func runContainedSmart(t *testing.T, mode ApprovalMode, sandbox SandboxMode, command string) (asked int, ran bool, req ToolApprovalRequest) {
	t.Helper()
	person := "person-injection-" + string(mode) + "-" + string(sandbox) + "-" + strings.ReplaceAll(command, " ", "_")
	cleanup := SetExecutionScope(person, ExecutionScope{
		TenantID: "tenant-injection", PersonID: person, TaskID: "task-injection",
		WorkspaceRoot: "/workspace/app", ApprovalMode: mode, Grants: newFakeGrantStore(),
		Judge: &fakeJudge{reply: "ESCALATE"},
		Approval: func(ctx context.Context, request ToolApprovalRequest) (ToolApprovalDecision, error) {
			asked++
			req = request
			return ToolApprovalDecision{Approved: false, ApprovalID: "apr_injection", Reason: "no"}, nil
		},
	})
	defer cleanup()
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	})
	_, _ = exec(map[string]interface{}{
		"_tenant_id": person, "_tool_name": "terminal",
		"command": command, "sandbox": string(sandbox),
	})
	return asked, ran, req
}

// Injected code runs inside the same enforced sandbox as the program it is
// injected into, so it does not widen the blast radius that lets smart mode
// release a contained call. Outside that containment the injection is
// dangerous and approval follows the mode. Neither ever remembers a class.
func TestCodeInjectionAssignmentIsReleasedOnlyByEnforcedContainment(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, false, false)
	for _, command := range []string{"LD_PRELOAD=/tmp/x.so ls -la", "env DYLD_INSERT_LIBRARIES=/tmp/x.dylib ls -la", "NODE_OPTIONS=--require=/tmp/x.js node build.js"} {
		if asked, ran, _ := runContainedSmart(t, ApprovalSmart, SandboxIsolated, command); asked != 0 || !ran {
			t.Errorf("contained %q must be released: asked=%d ran=%v", command, asked, ran)
		}
		asked, ran, req := runContainedSmart(t, ApprovalSmart, SandboxHost, command)
		if asked != 1 || ran {
			t.Errorf("host %q escaped approval: asked=%d ran=%v", command, asked, ran)
		}
		if req.GrantClass != "" || req.RunGrantClass != "" || len(req.RuleCandidates) != 0 {
			t.Errorf("host %q offered reusable authority: class=%q run=%q rules=%v", command, req.GrantClass, req.RunGrantClass, req.RuleCandidates)
		}
		if !strings.Contains(req.Reason, "injects code through environment variable") {
			t.Errorf("host %q lost its injection reason: %q", command, req.Reason)
		}
		// Containment is a smart-mode release; on-request still asks.
		if asked, ran, _ := runContainedSmart(t, ApprovalOnRequest, SandboxIsolated, command); asked != 1 || ran {
			t.Errorf("on-request %q must ask: asked=%d ran=%v", command, asked, ran)
		}
		if prefix, eligible := grantClassOf(command); eligible {
			t.Errorf("%q minted reusable class %v", command, prefix)
		}
	}
}

// Read-only proof follows the program the shell will run and the arguments
// it will receive. An assignment can redirect which binary runs, and an escape
// can hide a flag from the catalog.
func TestObservationProofFollowsTheProgramThatRuns(t *testing.T) {
	for _, command := range []string{
		"PATH=/tmp/evil ls",
		"PATH=/tmp/evil:$PATH ls",
		`PATH="/tmp/evil" ls -la`,
		"env PATH=/tmp/evil ls",
		"env -P /tmp/evil ls",
		"env -u HTTPS_PROXY PATH=/tmp/evil ls",
		"timeout 5 env PATH=/tmp/evil ls",
		"command env PATH=/tmp/evil ls",
		"sh -c 'PATH=/tmp/evil ls'",
		"PATH=/tmp/evil; ls",
		"export PATH=/tmp/evil; ls",
		"LD_PRELOAD=/tmp/x.so ls",
		"LD_LIBRARY_PATH=/tmp ls",
		"DYLD_INSERT_LIBRARIES=/tmp/x.dylib ls",
		"DYLD_LIBRARY_PATH=/tmp ls",
		"exec ls",
		"nice ls",
		`yq \-\i '.a = 1' config.yaml`,
		`sort -\o out.txt in.txt`,
		`gh api -\X DELETE repos/owner/name/git/refs/heads/x`,
		`\rm -f notes.txt`,
	} {
		if provenReadOnly(t, command) {
			t.Errorf("must NOT be provable read-only: %s", command)
		}
	}
	// The constraint that must change the result: the same programs, spelled
	// so the shell still runs exactly them, stay provable.
	for _, command := range []string{
		"ls -la",
		`\ls -la`,
		"command ls",
		"timeout 5 ls",
		"sh -c 'ls -la'",
		`yq '.a' config.yaml`,
		"sort in.txt",
		`gh api repos/owner/name/git/refs/heads/\x`,
		`cat notes\ 2026.md`,
	} {
		if !provenReadOnly(t, command) {
			t.Errorf("must stay provable read-only: %s", command)
		}
	}
}

func programSet(t *testing.T, command string) ([]string, bool) {
	t.Helper()
	return execCommandProgramSet("terminal", map[string]interface{}{"command": command})
}

// Environment analysis prepares the program the shell runs. An assignment
// (any value, including a path or spaces) and a known wrapper change neither
// the program nor its decidability.
func TestEnvironmentAnalysisResolvesTheProgramBehindAssignmentsAndWrappers(t *testing.T) {
	for _, tc := range []struct{ command, program string }{
		{"PATH=/x go build ./...", "go"},
		{"PATH=/usr/local/go/bin:$PATH go build", "go"},
		{`FOO="a b" go build`, "go"},
		{"LD_PRELOAD=/x.so go build", "go"},
		{"env PATH=/x go build", "go"},
		{"sh -c 'PATH=/x GO build'", "go"},
		{`\go build`, "go"},
		{"GO build", "go"},
		{"exec go build", "go"},
		{"nice -n 5 go build", "go"},
		{"KUBECONFIG=/x kubectl get pods", "kubectl"},
		{`GIT_SSH_COMMAND="ssh -i key" git push`, "git"},
		{`FOO="a b" aws sts get-caller-identity`, "aws"},
	} {
		programs, opaque := programSet(t, tc.command)
		if opaque {
			t.Errorf("%q became undecidable", tc.command)
		}
		if !stringInList(tc.program, programs) {
			t.Errorf("%q lost its program %s: %v", tc.command, tc.program, programs)
		}
	}
	if profiles := envprofiles.Match([]string{"go"}); len(profiles) == 0 {
		t.Fatal("go must match a profile for this test to mean anything")
	}
	programs, _ := programSet(t, "PATH=/x go build ./...")
	if len(envprofiles.Match(programs)) == 0 {
		t.Fatalf("PATH=/x go build lost the go profile: %v", programs)
	}
	// The constraint that must change the result: an executed script stays
	// undecidable under the same assignment.
	for _, command := range []string{"PATH=/x ./build.sh", `FOO="a b" ./deploy.sh`, `MSG="hello world" python3 tool.py`} {
		if _, opaque := programSet(t, command); !opaque {
			t.Errorf("%q must stay undecidable", command)
		}
	}
}

// A quoted value containing whitespace must not move the program word: each
// command prepares exactly like the bare command it runs.
func TestQuotedAssignmentPreparesLikeTheBareCommand(t *testing.T) {
	for _, pair := range [][2]string{
		{`FOO="a b" aws sts get-caller-identity`, "aws sts get-caller-identity"},
		{`MSG="hello world" python3 tool.py`, "python3 tool.py"},
		{`FOO='a b' ./deploy.sh`, "./deploy.sh"},
		{`GIT_SSH_COMMAND="ssh -i key" git push`, "git push"},
		{`FOO=a\ b go build ./...`, "go build ./..."},
		{"KUBECONFIG=/tmp/kube kubectl get pods", "kubectl get pods"},
	} {
		gotPrograms, gotOpaque := programSet(t, pair[0])
		wantPrograms, wantOpaque := programSet(t, pair[1])
		if !reflect.DeepEqual(gotPrograms, wantPrograms) || gotOpaque != wantOpaque {
			t.Errorf("%q read as programs=%v opaque=%v, but %q is programs=%v opaque=%v",
				pair[0], gotPrograms, gotOpaque, pair[1], wantPrograms, wantOpaque)
		}
	}

	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[default]\nregion=us-east-1\n[profile session]\nsso_session=work\n"
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	tenant, workspace := profileExecScope(t, home, "", executionenv.TrustTrusted)
	prepare := func(command string) map[string]interface{} {
		t.Helper()
		material := execMaterialForArgs(map[string]interface{}{"_tenant_id": tenant, "_tool_name": "terminal", "command": command}, workspace)
		if material.ProfileError != nil {
			t.Fatalf("%q: %v", command, material.ProfileError)
		}
		return preparationView(material)
	}
	// The comparisons below only mean something while the bare forms receive
	// the state a misread program would lose.
	if profiles := prepare("aws sts get-caller-identity")["profiles"]; !reflect.DeepEqual(profiles, []string{"aws"}) {
		t.Fatalf("bare aws must prepare its profile: %v", profiles)
	}
	if inventory, _ := prepare("python3 tool.py")["inventory"].([]string); len(inventory) == 0 {
		t.Fatal("an executed script must receive the host inventory")
	}
	for _, pair := range [][2]string{
		{`FOO="a b" aws sts get-caller-identity`, "aws sts get-caller-identity"},
		{`MSG="hello world" python3 tool.py`, "python3 tool.py"},
		{`FOO='a b' ./deploy.sh`, "./deploy.sh"},
	} {
		if got, want := prepare(pair[0]), prepare(pair[1]); !reflect.DeepEqual(got, want) {
			t.Errorf("%q prepared differently from %q:\n%+v\n%+v", pair[0], pair[1], got, want)
		}
	}
	// An inline selector inside a -c script selects state like the same
	// selector at top level.
	topLevel := prepare("AWS_PROFILE=session aws sts get-caller-identity")
	if overlays, _ := topLevel["overlays"].([]SandboxOverlayMount); len(overlays) == 0 {
		t.Fatal("the selected profile must need writable state for this comparison to mean anything")
	}
	for _, command := range []string{
		"bash -c 'AWS_PROFILE=session aws sts get-caller-identity'",
		"sh -c 'export AWS_PROFILE=session; aws sts get-caller-identity'",
		"env -S 'AWS_PROFILE=session aws sts get-caller-identity'",
	} {
		if got := prepare(command); !reflect.DeepEqual(got["overlays"], topLevel["overlays"]) || !reflect.DeepEqual(got["profiles"], topLevel["profiles"]) {
			t.Errorf("%q prepared differently from the top-level selector:\n%+v\n%+v", command, got, topLevel)
		}
	}
}

// Overrides are read wherever the shell applies them, including inside the
// scripts a command runs.
func TestEnvironmentOverridesAreReadInsideScripts(t *testing.T) {
	for _, tc := range []struct{ command, variable string }{
		{"bash -c 'AWS_PROFILE=session aws sts get-caller-identity'", "AWS_PROFILE"},
		{"sh -c 'env -u AWS_PROFILE aws sts get-caller-identity'", "AWS_PROFILE"},
		{"bash -c \"export AWS_CONFIG_FILE=/other/config; aws sts get-caller-identity\"", "AWS_CONFIG_FILE"},
		{"sudo env AWS_PROFILE=session aws sts get-caller-identity", "AWS_PROFILE"},
		{"env -uAWS_PROFILE aws sts get-caller-identity", "AWS_PROFILE"},
		{"env -S'AWS_PROFILE=session aws sts get-caller-identity'", "AWS_PROFILE"},
		{"env --split-string='AWS_CONFIG_FILE=/other aws sts get-caller-identity'", "AWS_CONFIG_FILE"},
		{"bash -c 'env $CONFIG aws sts get-caller-identity'", "*"},
	} {
		if !execEnvironmentOverrides(tc.command)[tc.variable] {
			t.Errorf("lost %s override in %s", tc.variable, tc.command)
		}
	}
	if overrides := execEnvironmentOverrides("bash -c 'env -u HTTPS_PROXY aws sts get-caller-identity'"); overrides["*"] || overrides["AWS_PROFILE"] {
		t.Fatalf("proxy edit inside a script widened authentication dependencies: %v", overrides)
	}
}

// Detection unwraps like the floor and treats an empty assignment as the
// removal it is.
func TestProxyRemovalDetectionUnwrapsLikeTheFloor(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"exec env -u HTTPS_PROXY gh pr view 19", []string{"HTTPS_PROXY"}},
		{"nohup env -u HTTPS_PROXY gh pr view 19", []string{"HTTPS_PROXY"}},
		{"sudo -E env --unset HTTP_PROXY curl https://api.example.test", []string{"HTTP_PROXY"}},
		{"HTTPS_PROXY= gh pr view 19", []string{"HTTPS_PROXY"}},
		{`https_proxy="" gh pr view 19`, []string{"https_proxy"}},
		{"env ALL_PROXY= gh pr view 19", []string{"ALL_PROXY"}},
		{"export HTTPS_PROXY=; gh pr view 19", []string{"HTTPS_PROXY"}},
		{"env -u HTTPS_PROXY gh pr view \"$PR\"", []string{"HTTPS_PROXY"}},
		{"env -u PATH -u HTTPS_PROXY gh pr view 19", []string{"HTTPS_PROXY"}},
		// Replacing or keeping a route is not a removal.
		{"HTTPS_PROXY=http://proxy.example.test:8080 gh pr view 19", nil},
		{"FOO= gh pr view 19", nil},
		{"env -u PATH gh pr view 19", nil},
	} {
		if got := proxyRemovalVariables(tc.command); !reflect.DeepEqual(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
			t.Errorf("%q removals=%v, want %v", tc.command, got, tc.want)
		}
	}
}

// A payload whose program resolution is redirected cannot back a reusable
// class: the class would name a program that may not be the one that runs.
func TestRedirectedProgramResolutionMintsNoReusableClass(t *testing.T) {
	for _, command := range []string{
		"PATH=/tmp/evil kubectl get pods",
		"env PATH=/tmp/evil kubectl get pods",
		"env -P /tmp/evil kubectl get pods",
		"export PATH=/tmp/evil; kubectl get pods",
		"PATH=/tmp/evil; kubectl get pods",
		"LD_PRELOAD=/tmp/x.so kubectl get pods",
	} {
		if prefix, eligible := grantCommandPrefix("terminal", map[string]interface{}{"command": command}); eligible {
			t.Errorf("%q minted reusable class %v", command, prefix)
		}
	}
	// Any other assignment changes what the program reads, so it leaves no
	// class either unless the run's environment already holds it
	// (TestAssignmentTheSnapshotVouchesForKeepsTheClass).
	for _, command := range []string{"FOO=a/b kubectl get pods", "TZ=Asia/Shanghai kubectl get pods"} {
		if prefix, eligible := grantCommandPrefix("terminal", map[string]interface{}{"command": command}); eligible {
			t.Errorf("%q minted reusable class %v", command, prefix)
		}
	}
	// The constraint that must change the result: the bare command keeps its
	// own class.
	if prefix, eligible := grantCommandPrefix("terminal", map[string]interface{}{"command": "kubectl get pods"}); !eligible || strings.Join(prefix, " ") != "kubectl get" {
		t.Errorf("kubectl get pods lost its class: %v %v", prefix, eligible)
	}
}
