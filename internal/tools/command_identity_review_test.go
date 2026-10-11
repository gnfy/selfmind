package tools

import (
	"context"
	"strings"
	"testing"
)

func grantClassOf(command string) ([]string, bool) {
	return grantCommandPrefix("terminal", map[string]interface{}{"command": command})
}

// A class remembered for an ordinary `kubectl get` must not release the same
// read run as root or under any wrapper that changes who runs it or how the
// command is read. Only wrappers that change neither may be peeled for a
// class.
func TestIdentityChangingWrappersMintNoClass(t *testing.T) {
	for _, command := range []string{
		"sudo kubectl get pods",
		"sudo -u root kubectl get pods",
		"doas kubectl get pods",
		"time sudo kubectl get pods",
		"nice -n 5 sudo kubectl get pods",
		"su -c 'kubectl get pods'",
		"env FOO=1 kubectl get pods",
		"xargs kubectl get pods",
		"setsid kubectl get pods",
		"chroot / kubectl get pods",
		"flock /tmp/kube.lock kubectl get pods",
		"watch kubectl get pods",
		"sh -c 'kubectl get pods'",
		"eval kubectl get pods",
		"toybox kubectl get pods",
	} {
		if prefix, eligible := grantClassOf(command); eligible {
			t.Errorf("%q minted reusable class %v", command, prefix)
		}
	}
	// The constraint that must change the result: wrappers that change neither
	// identity nor privilege nor how the command is read keep the class.
	for _, command := range []string{
		"kubectl get pods",
		"time kubectl get pods",
		"nice -n 5 kubectl get pods",
		"timeout 30 kubectl get pods",
		"nohup kubectl get pods",
		"exec kubectl get pods",
		"command kubectl get pods",
		"stdbuf -o0 kubectl get pods",
		"ionice -c 3 kubectl get pods",
	} {
		if prefix, eligible := grantClassOf(command); !eligible || strings.Join(prefix, " ") != "kubectl get" {
			t.Errorf("%q lost its class: %v %v", command, prefix, eligible)
		}
	}
}

// The same rule seen from the host: a remembered unprivileged class releases
// its own command and nothing run as root.
func TestRememberedHostClassNeverReleasesThePrivilegedForm(t *testing.T) {
	store := newFakeGrantStore()
	scope := ExecutionScope{
		TenantID: "tenant-priv", PersonID: "person-priv", WorkspaceID: "ws-priv", WorkspaceRoot: "/work/priv",
		ApprovalMode: ApprovalOnRequest, Grants: store, StandingGrants: InteractiveStandingGrants(),
	}
	hostArgs := func(command string) map[string]interface{} {
		return map[string]interface{}{"_tool_name": "terminal", "command": command, "_effective_sandbox_mode": string(SandboxHost)}
	}
	plain := hostArgs("kubectl get pods")
	_, reason := dangerousToolCall("", "terminal", plain)
	key := approvalPatternKeyForScope("terminal", plain, reason, scope, true)
	if key == "" {
		t.Fatal("the unprivileged read must mint a class for this test to mean anything")
	}
	store.granted[store.key("person", scope.PersonID, key)] = true
	asks := 0
	scope.Approval = func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		return ToolApprovalDecision{Approved: false, ApprovalID: "apr_priv", Reason: "no"}, nil
	}
	cleanup := SetExecutionScope("person-priv", scope)
	defer cleanup()
	run := func(command string) (ran bool) {
		exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran = true; return "ok", nil })
		args := hostArgs(command)
		args["_tenant_id"] = "person-priv"
		_, _ = exec(args)
		return ran
	}
	if !run("kubectl get pods") || asks != 0 {
		t.Fatalf("the remembered class must release its own command: asks=%d", asks)
	}
	for _, command := range []string{"sudo kubectl get pods", "time sudo kubectl get pods", "doas kubectl get pods"} {
		before := asks
		if run(command) || asks != before+1 {
			t.Errorf("%q was released by the unprivileged class: asks=%d", command, asks-before)
		}
	}
}

// An escape is not part of the name a class or rule matches: the floor's ban
// on interpreters and irreversible programs holds for `\python3` and `\rm`,
// and an escaped option cannot turn a DELETE into a remembered GET.
func TestEscapedWordsCannotEvadeTheGrantFloor(t *testing.T) {
	for _, command := range []string{
		`\python3 tool.py`,
		`\rm -rf build`,
		`\curl https://api.example.test/items`,
		`gh api repos/owner/name/git/refs/heads/x -\X DELETE`,
		`\kubectl get pods`,
	} {
		if prefix, eligible := grantClassOf(command); eligible {
			t.Errorf("%q minted reusable class %v", command, prefix)
		}
		args := map[string]interface{}{"_tool_name": "terminal", "command": command, "_effective_sandbox_mode": string(SandboxIsolated)}
		_, reason := dangerousToolCall("", "terminal", args)
		if key := approvalPatternKey("terminal", args, reason); key != "" {
			t.Errorf("%q minted reusable key %q", command, key)
		}
		if rule, ok := approvalCommandPrefix("terminal", args); ok {
			t.Errorf("%q offered prefix rule %q", command, rule)
		}
	}
	// The constraint that must change the result: the bare spelling of an
	// approvable program keeps its class.
	if prefix, eligible := grantClassOf("chmod 755 script.sh"); !eligible || strings.Join(prefix, " ") != "chmod" {
		t.Fatalf("chmod lost its class: %v %v", prefix, eligible)
	}
}

// A stored key is never rewritten to its normalized form, which could match
// more than the person approved: a key the current floor would not mint is
// withdrawn.
func TestPersistedKeysAreReviewedUnderTheNormalizedFloor(t *testing.T) {
	const hostReason = "exec:" + HostEscapeApprovalReason
	const resource = "|resource=workspace:7a417b1b00a598fc:command:"
	for _, tc := range []struct {
		key  string
		keep bool
	}{
		{hostReason + resource + `\python3`, false},
		{hostReason + resource + `\kubectl get`, false},
		{hostReason + resource + `gh api get repos/o/n\x`, false},
		{"exec:invokes dangerous command: rm", false},
		{"exec:network egress command: curl", false},
		{"exec:" + codeInjectionReason + "LD_PRELOAD", false},
		{"exec:terminal|class=" + `\rm`, false},
		{"exec:terminal|class=rm", false},
		{hostReason + resource + "kubectl get", true},
		{"exec:terminal|class=kubectl get", true},
		{"exec:terminal|class=chmod", true},
		{"exec:terminal|class=ssh", true},
		// A class keyed on the reason it was asked about, or on the tool alone,
		// is not one the derivation mints and nothing redeems it.
		{"exec:invokes dangerous command: chmod", false},
		{"exec:network egress command: ssh", false},
		{"exec:terminal", false},
	} {
		if _, keep := ReviewPersistedGrantKey(tc.key); keep != tc.keep {
			t.Errorf("ReviewPersistedGrantKey(%q) keep=%v, want %v", tc.key, keep, tc.keep)
		}
	}
}

// Home and root targets are compared after the path is cleaned and trailing
// globs are collapsed, however the home directory is spelled.
func TestHardFloorComparesTargetsAfterCleaning(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"rm -r ~/",
		"rm -r $HOME/",
		"rm -rf ~/*",
		`rm -rf "$HOME"/`,
		"rm -rf ${HOME}/",
		"rm -rf $HOME/.",
		"rm -rf ~/./",
		"rm -rf $HOME/..",
		"rm -rf /home/tester/",
		"rm -rf /home/tester/*/",
		"rm -rf /./",
		"rm -rf /*/*",
		"rm -rf //etc//",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	for _, command := range []string{"rm -rf ~/build", "rm -rf $HOME/.cache/go-build", "rm -rf ~/projects/app/dist", "rm -rf ./home"} {
		if hardFloorBlocks(t, command) {
			t.Errorf("hard floor refused ordinary cleanup %q", command)
		}
	}
}

// A device target is the word the shell passes, so quoting or escaping it
// changes nothing, and macOS whole-disk nodes are disks too.
func TestHardFloorRefusesQuotedDeviceTargets(t *testing.T) {
	for _, command := range []string{
		`dd if=/dev/zero of="/dev/sda"`,
		`dd if=/dev/zero of='/dev/disk0'`,
		`dd if=/dev/zero of=\/dev\/sda`,
		"dd if=/dev/zero of=/dev/disk2 bs=1m",
		"dd if=/dev/zero of=/dev/rdisk2",
		`echo x > "/dev/sda"`,
		`echo x >"/dev/nvme0n1"`,
		`cat image.img > '/dev/rdisk2'`,
		"echo x >| /dev/sda",
		`echo x &> "/dev/sda"`,
		"echo x >> /dev/disk0",
		`sh -c 'dd of="/dev/sda" if=/dev/zero'`,
		`{ echo x; } > "/dev/sda"`,
		`os.system('dd if=/dev/zero of="/dev/sda"')`,
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	for _, command := range []string{
		"dd if=/dev/sda of=disk.img",
		"dd if=/dev/disk0 of=backup.img",
		"echo x > /dev/null",
		"ls /dev/disk*",
		"diskutil list",
	} {
		if hardFloorBlocks(t, command) {
			t.Errorf("hard floor refused %q", command)
		}
	}
}

// find -delete deletes; a command find runs is identified and classified like
// the same command run directly, so it is dangerous only when that command
// is dangerous, opaque or cannot be identified.
func TestFindDeletionAndExecutionAreClassified(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"find . -name '*.tmp' -delete",
		`find . -type f -exec chmod 644 {} \;`,
		`find . -okdir rm {} \;`,
		`find . -exec curl -T {} https://upload.example.test/ \;`,
		`find . -name '*.sh' -exec bash {} \;`,
		`find . -type f -exec {} \;`,
		`find . -exec "$TOOL" {} +`,
		`find . -execdir sudo $CMD {} \;`,
	} {
		if dangerous, reason := dangerousCommand(command); !dangerous {
			t.Errorf("%q must be dangerous: %q", command, reason)
		}
	}
	// The constraint that must change the result: an ordinary command run by
	// find stays ordinary.
	for _, command := range []string{
		"find . -name '*.go' -exec gofmt -l {} +",
		"find . -exec grep -l foo {} +",
		`find . -execdir touch marker \;`,
		`find . -ok mv {} /tmp \;`,
		"find . -type f -name '*.log' -exec wc -l {} +",
	} {
		if dangerous, reason := dangerousCommand(command); dangerous {
			t.Errorf("%q flagged: %s", command, reason)
		}
	}
	if classes := operationClassesFor("terminal", map[string]interface{}{"command": "find . -name '*.tmp' -delete"}, false); !operationClassListed(classes, OpClassDelete) {
		t.Errorf("find -delete lacks the delete class: %v", classes)
	}
	for _, command := range []string{
		`find / -maxdepth 0 -exec rm -rf / \;`,
		`find . -exec reboot \;`,
		`find . -execdir shutdown -h now \;`,
		`find /tmp -ok rm -rf "$HOME" \;`,
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	// The constraint that must change the result: a traversal that only
	// prints stays an ordinary command.
	for _, command := range []string{"find . -name '*.go' -print", "find internal -type f -newer go.mod"} {
		if dangerous, reason := dangerousCommand(command); dangerous {
			t.Errorf("%q flagged: %s", command, reason)
		}
	}
}

// The remaining launchers run their command like the other wrappers.
func TestHardFloorSeesThroughLaunchers(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"su -c 'rm -rf /'",
		"su root -c 'rm -rf /'",
		`su - root -c "rm -rf /"`,
		"su --command='rm -rf /' root",
		"chroot / rm -rf /",
		"chroot --userspec=root:root / rm -rf /",
		"flock /tmp/lock rm -rf /",
		"flock -w 5 /tmp/lock -c 'rm -rf /'",
		"watch rm -rf /",
		"watch -n 5 'rm -rf /'",
		"parallel rm -rf / ::: x",
		"parallel -j 2 rm -rf / ::: x",
		"busybox rm -rf /",
		"toybox rm -rf /",
		"busybox sh -c 'rm -rf /'",
		"BUSYBOX reboot",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	// A launcher whose command cannot be extracted is opaque, not ordinary.
	for _, command := range []string{"su root", "chroot /srv/root"} {
		if dangerous, reason := dangerousCommand(command); !dangerous || reason != "invokes an opaque wrapped command" {
			t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
		}
	}
	// The constraint that must change the result: a lock or a watch of an
	// ordinary command stays ordinary.
	for _, command := range []string{"flock -n 9", "watch -n 5 kubectl get pods", "flock /tmp/build.lock go build ./..."} {
		if dangerous, reason := dangerousCommand(command); dangerous {
			t.Errorf("%q flagged: %s", command, reason)
		}
	}
}

// Interpreter and shell startup variables load code into the program that
// reads them, so they are dangerous like loader variables and never back a
// class. PYTHONPATH only matters to Python.
func TestInterpreterInjectionVariablesAreDangerous(t *testing.T) {
	for _, variable := range []string{"NODE_OPTIONS", "PYTHONSTARTUP", "PERL5OPT", "PERL5LIB", "RUBYOPT", "BASH_ENV", "ZDOTDIR", "GIT_EXEC_PATH", "LD_AUDIT"} {
		for _, spelling := range codeInjectionSpellings {
			command := strings.ReplaceAll(spelling, "%s", variable)
			if dangerous, reason := dangerousCommand(command); !dangerous || !strings.Contains(reason, variable) {
				t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
			}
		}
		if prefix, eligible := grantClassOf(variable + "=/tmp/x kubectl get pods"); eligible {
			t.Errorf("%s minted reusable class %v", variable, prefix)
		}
	}
	for _, command := range []string{
		"PYTHONPATH=/tmp/x python3 tool.py",
		"PYTHONPATH=src python -m pytest -q",
		"export PYTHONPATH=/tmp/x; python3 tool.py",
		"PYTHONPATH=/tmp/x sh -c 'python3.12 tool.py'",
		"env PYTHONPATH=/tmp/x pypy3 tool.py",
	} {
		if dangerous, reason := dangerousCommand(command); !dangerous || !strings.Contains(reason, "PYTHONPATH") {
			t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
		}
	}
	// The constraint that must change the result: PYTHONPATH before a program
	// that is not Python changes nothing it runs, though it still backs no
	// class.
	for _, command := range []string{"PYTHONPATH=src go test ./...", "PYTHONPATH=src kubectl get pods"} {
		if dangerous, reason := dangerousCommand(command); dangerous {
			t.Errorf("%q flagged: %s", command, reason)
		}
	}
	if prefix, eligible := grantClassOf("PYTHONPATH=src kubectl get pods"); eligible {
		t.Errorf("PYTHONPATH minted reusable class %v", prefix)
	}
}

// ENV names the startup file an interactive POSIX shell reads, and nothing
// else reads it: it injects code only where such a shell runs.
func TestEnvStartupFileInjectsOnlyInteractiveShells(t *testing.T) {
	for _, command := range []string{
		"ENV=/tmp/x sh -i",
		"ENV=/tmp/x ksh -i",
		"ENV=/tmp/x bash -il",
		"ENV=/tmp/x sh",
		"ENV=/tmp/x sh -s",
		"ENV=/tmp/x dash -ic 'ls'",
		"export ENV=/tmp/x; sh -i",
	} {
		if dangerous, reason := dangerousCommand(command); !dangerous || !strings.Contains(reason, "ENV") {
			t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
		}
	}
	// The constraint that must change the result: a program that is not an
	// interactive shell never reads it.
	for _, command := range []string{
		"ENV=production node app.js",
		"ENV=staging npm start",
		"ENV=/tmp/x sh -c 'ls -la'",
		"ENV=production go test ./...",
	} {
		if dangerous, reason := dangerousCommand(command); dangerous {
			t.Errorf("%q flagged: %s", command, reason)
		}
	}
}

// Everyday commands stay ordinary: dangerous only where they load code into a
// program outside enforced containment, releasable inside it, and never a
// reusable class when they load code.
func TestEverydayCommandsStayOrdinaryAndContainable(t *testing.T) {
	corpus := []struct {
		command string
		injects bool
	}{
		{"NODE_OPTIONS=--max-old-space-size=4096 npm run build", true},
		{"PYTHONPATH=src pytest", false},
		{"ENV=production node app.js", false},
		{"find . -name '*.go' -exec gofmt -l {} +", false},
		{"CGO_ENABLED=0 go build ./...", false},
		{"NODE_ENV=production npm test", false},
		{"PYTHONPATH=. python -m pytest -q", true},
		{"find . -type f -name '*.log' -exec wc -l {} +", false},
		{"find . -exec grep -l foo {} +", false},
	}
	for _, tc := range corpus {
		if dangerous, reason := dangerousCommand(tc.command); dangerous != tc.injects {
			t.Errorf("%q outside containment: dangerous=%v reason=%q, want %v", tc.command, dangerous, reason, tc.injects)
		}
		if tc.injects {
			if prefix, eligible := grantClassOf(tc.command); eligible {
				t.Errorf("%q minted reusable class %v", tc.command, prefix)
			}
		}
	}
	if !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, false, false)
	for _, tc := range corpus {
		args := map[string]interface{}{"_tool_name": "terminal", "command": tc.command, "sandbox": string(SandboxIsolated)}
		annotateEffectiveSandboxMode(args)
		if dangerous, reason := dangerousToolCall("", "terminal", args); dangerous {
			t.Errorf("%q inside containment: dangerous with %q", tc.command, reason)
		}
		if asked, ran, _ := runContainedSmart(t, ApprovalSmart, SandboxIsolated, tc.command); asked != 0 || !ran {
			t.Errorf("contained %q must be released: asked=%d ran=%v", tc.command, asked, ran)
		}
	}
}

// A program name decided at run time cannot be identified, so it is opaque:
// it asks rather than passing as an ordinary command.
func TestRunTimeProgramNamesAreOpaque(t *testing.T) {
	for _, command := range []string{
		"$(printf rm) -rf build",
		"$CMD -rf build",
		`$'\x72m' -rf build`,
		`p=rm; "$p" -rf build`,
		`sudo "$TOOL" --version`,
		"`echo rm` -rf build",
	} {
		if dangerous, reason := dangerousCommand(command); !dangerous || reason != "invokes an opaque wrapped command" {
			t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
		}
	}
	// The constraint that must change the result: an expanded argument of a
	// known program stays ordinary.
	for _, command := range []string{`go build -o "$OUT" ./cmd/selfmind`, `echo "$HOME"`, "ls -la $(pwd)"} {
		if dangerous, reason := dangerousCommand(command); dangerous {
			t.Errorf("%q flagged: %s", command, reason)
		}
	}
	// An assignment a builtin makes counts like any other.
	for _, command := range []string{"printf -v PATH /tmp/evil; kubectl get pods", "printf -vPATH /tmp/evil; kubectl get pods"} {
		if prefix, eligible := grantClassOf(command); eligible {
			t.Errorf("%q minted reusable class %v", command, prefix)
		}
	}
	if dangerous, reason := dangerousCommand("printf -v LD_PRELOAD /tmp/x.so; ls"); !dangerous || !strings.Contains(reason, "LD_PRELOAD") {
		t.Errorf("printf -v LD_PRELOAD: dangerous=%v reason=%q", dangerous, reason)
	}
}

// Read-only proof vouches for the exact catalogued spelling. A case-sensitive
// filesystem may hold a different `LS` or `Env`; the deny side still folds
// case, as it must.
func TestObservationProofRequiresTheCataloguedSpelling(t *testing.T) {
	for _, command := range []string{
		"LS -la",
		"Ls -la",
		"GIT status",
		"Env -u HTTPS_PROXY gh pr view 19",
		"BASH -c 'ls'",
		"COMMAND ls",
		"TIMEOUT 5 ls",
		"sh -c 'LS'",
		`'\ls'`,
		// An assignment reached through an observation builtin is an
		// assignment too.
		"printf -v PATH /tmp/evil; ls",
		"printf -vPATH /tmp/evil; ls",
		"command export PATH=/tmp/evil; ls",
	} {
		if provenReadOnly(t, command) {
			t.Errorf("must NOT be provable read-only: %s", command)
		}
	}
	for _, command := range []string{"ls -la", `\ls -la`, `l\s -la`, "'ls' -la", "env -u HTTPS_PROXY gh pr view 19", "bash -c 'ls'", "printf '%s\\n' value"} {
		if !provenReadOnly(t, command) {
			t.Errorf("must stay provable read-only: %s", command)
		}
	}
	// The deny side keeps folding case.
	if dangerous, _ := dangerousCommand("RM -rf build"); !dangerous {
		t.Fatal("the dangerous heuristic must still fold case")
	}
}
