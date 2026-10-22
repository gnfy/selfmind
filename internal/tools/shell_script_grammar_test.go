package tools

import (
	"strings"
	"testing"
)

// A POSIX shell's -c is a flag: the command string is the first operand after
// the shell's options, which may follow -c and end at `--` or `-`
// (`sh [-abCefhimnuvx] [-o option] -c [--] command_string [name [arg...]]`).
// Taking the word after -c as the script read `sh -c -- 'rm -rf ~'` as a
// program named `--`, so neither the floor nor the dangerous heuristic saw rm,
// and smart mode released it as contained.
func TestShellCommandStringFollowsTheShellsOptions(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"sh -c -- 'rm -rf ~'",
		"bash -c -e 'rm -rf /'",
		"bash -c -o pipefail 'rm -rf /'",
		"bash -c +o posix 'rm -rf /'",
		"bash -ce -- 'rm -rf /'",
		"bash -c - 'rm -rf /'",
		"bash -O extglob -c 'rm -rf /'",
		"bash -c -O extglob 'rm -rf /'",
		"bash --norc -c -- 'rm -rf /'",
		"bash --rcfile ./rc -c 'rm -rf /'",
		"zsh -c -- 'rm -rf /'",
		"dash -c -x 'rm -rf /'",
		"ash -c -- 'rm -rf /'",
		"ksh -c +x 'rm -rf /'",
		"sudo sh -c -- 'rm -rf /'",
		"fish --command='rm -rf /'",
		"fish -C 'rm -rf /'",
		"fish --init-command 'rm -rf /' -c true",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	for _, command := range []string{"sh -c -- 'chmod 777 x'", "bash -c -e 'kill 1'", "zsh -c -- 'ssh build.example.test'"} {
		if dangerous, reason := dangerousCommand(command); !dangerous {
			t.Errorf("%q must be dangerous: %q", command, reason)
		}
	}
	// The constraint that must change the result: words after the command
	// string are positional parameters, not commands.
	for _, command := range []string{"sh -c 'echo ok' -- rm -rf /", "bash -c 'ls' rm -rf /", "sh -c -- 'echo ok' rm -rf /"} {
		if hardFloorBlocks(t, command) {
			t.Errorf("hard floor refused %q, whose rm is only a positional parameter", command)
		}
	}
	// A shell with no -c runs a script file or its standard input: opaque.
	for _, command := range []string{"sh -- ./deploy.sh", "bash -e deploy.sh", "sh -o pipefail"} {
		if dangerous, reason := dangerousCommand(command); !dangerous || reason != "invokes an opaque wrapped command" {
			t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
		}
	}
}

// A contained smart-mode run released `sh -c -- 'rm -rf ~'` before the floor
// could see rm. It must now be refused before any mode applies.
func TestShellOptionsBeforeTheCommandCannotHideItFromSmartMode(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, true, false)
	for _, command := range []string{"sh -c -- 'rm -rf ~'", "bash -c -e 'rm -rf /'"} {
		asked, ran, _ := runContainedSmart(t, ApprovalSmart, SandboxIsolated, command)
		if ran || asked != 0 {
			t.Errorf("%q: ran=%v asked=%d, want a hard block", command, ran, asked)
		}
	}
}

// env -S splits its string into env's own arguments (macOS env(1), GNU env):
// options, assignments and the utility, with quoting and the \_ separator. It
// is not a shell script, so reading it as one missed `env -S '-i rm -rf /'`.
func TestEnvSplitStringIsReadAsEnvArguments(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"env -S '-i rm -rf /'",
		`env -S'rm\_-rf\_/'`,
		"env -S 'FOO=1 rm -rf /'",
		"env --split-string='rm -rf /'",
		`env -S 'rm -rf "/"'`,
		"env -S '-u PATH rm -rf /'",
		"env -S 'rm -rf' /",
		"env -S 'rm -rf ${HOME}'",
		"env -iS 'rm -rf /'",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	for _, command := range []string{`env -S'chmod\_777\_x'`, "env -S '-i kill 1'"} {
		if dangerous, reason := dangerousCommand(command); !dangerous {
			t.Errorf("%q must be dangerous: %q", command, reason)
		}
	}
	// A comment ends the string, and a substitution in the program word is
	// decided at run time.
	if hardFloorBlocks(t, "env -S '#rm -rf /' true") {
		t.Error("a commented-out command runs nothing")
	}
	if dangerous, reason := dangerousCommand("env -S '${CMD} -rf build'"); !dangerous || reason != "invokes an opaque wrapped command" {
		t.Errorf("a substituted program word must be opaque: %v %q", dangerous, reason)
	}
	// Proxy-removal detection reads the split arguments too.
	if removed := proxyRemovalVariables("env -S '-u HTTPS_PROXY' gh api user"); len(removed) != 1 || removed[0] != "HTTPS_PROXY" {
		t.Errorf("removal inside -S: %v", removed)
	}
}

// Environment analysis reads env's own options, wherever they are written: a
// cluster, or a split string.
func TestEnvIgnoreEnvironmentIsReadFromEnvsOwnOptions(t *testing.T) {
	for _, command := range []string{
		"env -S '-i aws sts get-caller-identity'",
		"env -iu HTTPS_PROXY aws sts get-caller-identity",
		"env - aws sts get-caller-identity",
	} {
		if !execEnvironmentOverrides(command)["*"] {
			t.Errorf("%q discards the environment", command)
		}
	}
	// The constraint that must change the result: removing one variable is
	// not discarding all of them.
	if overrides := execEnvironmentOverrides("env -S '-u AWS_PROFILE' aws sts get-caller-identity"); overrides["*"] || !overrides["AWS_PROFILE"] {
		t.Errorf("overrides = %v", overrides)
	}
}

// Each shell reads its options with its own grammar, and a name several
// shells answer to (sh, a login shell) with each of theirs.
func TestShellGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "bash -c -e kill", runs: "kill"},
		{command: "bash -oe pipefail -c kill", runs: "kill", not: []string{"pipefail"}},
		{command: "bash -O extglob -c kill", runs: "kill", not: []string{"extglob"}},
		{command: "bash -norc -c kill", runs: "kill"},
		// After a single-letter option bash reads -rcfile as -r -c -f -i -l -e;
		// before one it is --rcfile, whose value is a file, and bash then reads
		// its input.
		{command: "bash -e -rcfile kill", runs: "kill"},
		{command: "bash -rcfile kill", not: []string{"kill"}, opaque: true},
		// A startup file runs code no argument shows; the command string is
		// still read.
		{command: "bash --rcfile ./rc -ic kill", runs: "kill", opaque: true},
		// zsh's -O is a flag, its -o takes the rest of its cluster, and its -b
		// ends the options, after which -c is a script file.
		{command: "zsh -c -O kill", runs: "kill"},
		{command: "zsh -opipefail -c kill", runs: "kill"},
		{command: "zsh --emulate sh -c kill", runs: "kill", not: []string{"sh"}},
		{command: "zsh -b -c kill", not: []string{"kill"}, opaque: true},
		// sh may be bash or dash, whose -o takes the next word, or zsh or ksh,
		// whose -o takes the rest of its cluster.
		{command: "sh -c -opipefail kill", runs: "kill", opaque: true},
		{command: "mksh -T /dev/tty2 -c kill", runs: "kill"},
		// Words after the command string are positional parameters.
		{command: "sh -c 'echo ok' kill", runs: "echo", not: []string{"kill"}},
		// csh runs the word after the cluster holding c; -b ends its options.
		{command: "tcsh -cf kill", runs: "kill"},
		{command: "csh -b -c kill", not: []string{"kill"}, opaque: true},
		// fish runs every -c and -C.
		{command: "fish -c 'echo hi' -C kill", runs: "kill"},
		{command: "fish --comm=kill", runs: "kill"},
		// su hands `-c COMMAND` and the words after it, or the words after the
		// login, to the target's login shell.
		{command: "su root -c -- kill", runs: "kill"},
		{command: "su -m operator -c -e kill", runs: "kill"},
		{command: "su root -x -c kill", runs: "kill"},
	})
}

// env -S splits its string into env's own arguments, read on as options,
// assignments and the utility.
func TestEnvSplitStringGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "env -S 'kill -9 1'", runs: "kill"},
		{command: "env -S'-i kill'", runs: "kill"},
		{command: `env -S'kill\_-9\_1'`, runs: "kill"},
		{command: "env -S 'FOO=1' kill", runs: "kill", not: []string{"foo=1"}},
		{command: "env -S '-u HOME -C /tmp' kill", runs: "kill", not: []string{"home", "/tmp"}},
		{command: "env -S 'kill' -9 1", runs: "kill"},
		{command: "env -S '#kill' true", runs: "true", not: []string{"kill"}},
		{command: `env -S 'echo \c kill'`, runs: "echo", not: []string{"kill"}},
		{command: "env -S \"-S 'kill -9 1'\"", runs: "kill"},
		// A string env refuses runs nothing it can show. (The field reading,
		// which drops quotes, still names kill.)
		{command: `env -S '"kill'`, opaque: true},
		{command: `env -S 'a\q' kill`, opaque: true},
		{command: "env -S '$CMD'", opaque: true},
		// ${NAME} is the variable's value when env runs.
		{command: "env -S '${CMD} -9 1'", opaque: true},
		{command: "env -S 'sh -c ${SCRIPT}'", runs: "sh", opaque: true},
	})
}

// splitEnvString is env's own splitting; pin it directly.
func TestSplitEnvString(t *testing.T) {
	cases := []struct {
		in    string
		words []string
		ok    bool
	}{
		{"a b\tc", []string{"a", "b", "c"}, true},
		{`'a b' "c d"`, []string{"a b", "c d"}, true},
		{`a\_b`, []string{"a", "b"}, true},
		{`"a\_b"`, []string{"a b"}, true},
		{`'a\_b'`, []string{`a\_b`}, true},
		{`'it\'s'`, []string{"it's"}, true},
		{`a #b c`, []string{"a"}, true},
		{`a b#c`, []string{"a", "b#c"}, true},
		{`\#a`, []string{"#a"}, true},
		{`a \c b`, []string{"a"}, true},
		{`a\cb`, []string{"a"}, true},
		{`${HOME}/x`, []string{"${HOME}/x"}, true},
		{`'${HOME}'`, []string{"${HOME}"}, true},
		{`''`, []string{""}, true},
		{`"\c"`, nil, false},
		{`'a`, nil, false},
		{`a\q`, nil, false},
		{`$HOME`, nil, false},
		{`${1X}`, nil, false},
		{`a\`, nil, false},
	}
	for _, tc := range cases {
		words, _, ok := splitEnvString(tc.in)
		if ok != tc.ok || (ok && !sameWords(words, tc.words)) {
			t.Errorf("splitEnvString(%q) = %q, %v; want %q, %v", tc.in, words, ok, tc.words, tc.ok)
		}
	}
	if _, substituted, _ := splitEnvString(`rm ${A} "${B}" '${C}'`); !sameWords(substituted, []string{"${A}", "${B}"}) {
		t.Errorf("substituted = %q", substituted)
	}
}

// The observation prover reads a shell's command string with the same
// grammar. A startup file runs code the proof never saw, and a name whose
// shells disagree proves nothing.
func TestObservationProofReadsTheShellsOwnGrammar(t *testing.T) {
	for _, command := range []string{
		"bash --rcfile ./rc -ic 'git status'",
		"bash --init-file ./rc -i -c 'git status'",
		"zsh -b -c 'git status'",
		"sh -c -opipefail 'git status'",
		"sh -c 'git status' positional",
	} {
		if provenReadOnly(t, command) {
			t.Errorf("must NOT be provable read-only: %s", command)
		}
	}
	// The constraint that must change the result: options every grammar reads
	// alike leave the proof intact.
	for _, command := range []string{
		"bash -c 'git status'",
		"bash -lc 'git status'",
		"sh -c -- 'git status'",
		"bash -o pipefail -c 'git status'",
		"bash -ec 'git status'",
	} {
		if !provenReadOnly(t, command) {
			t.Errorf("must stay provable read-only: %s", command)
		}
	}
}

// ENV injects code where an interactive shell reads it; the shell's grammar
// decides whether it runs interactively.
func TestEnvStartupFileFollowsTheShellsGrammar(t *testing.T) {
	for _, command := range []string{"ENV=/tmp/x sh -o posix -i -c 'ls'", "ENV=/tmp/x bash -O extglob -i -c 'ls'"} {
		if dangerous, reason := dangerousCommand(command); !dangerous || !strings.Contains(reason, "ENV") {
			t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
		}
	}
	for _, command := range []string{"ENV=/tmp/x sh -o posix -c 'ls'", "ENV=/tmp/x bash -O extglob -c 'ls'"} {
		if dangerous, reason := dangerousCommand(command); dangerous {
			t.Errorf("%q flagged: %s", command, reason)
		}
	}
}
