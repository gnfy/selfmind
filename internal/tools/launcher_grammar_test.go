package tools

import (
	"sort"
	"strings"
	"testing"
)

// launcherCase is one invocation written from a launcher's documented grammar:
// the program it runs ("" when it runs none), the words the grammar gives some
// other role (an option value, a lock file, a process id), and whether the
// program it runs is chosen at run time.
type launcherCase struct {
	command string
	runs    string
	not     []string
	opaque  bool
}

// identifiedPrograms is every program either reading names for a payload.
func identifiedPrograms(command string) map[string]bool {
	out := map[string]bool{}
	for _, read := range effectiveCommands(command) {
		if name := read.id.program().Name; name != "" {
			out[name] = true
		}
	}
	return out
}

func anyOpaqueRead(command string) bool {
	for _, read := range effectiveCommands(command) {
		if read.id.Opaque {
			return true
		}
	}
	return false
}

func checkLauncherCases(t *testing.T, cases []launcherCase) {
	t.Helper()
	for _, tc := range cases {
		programs := identifiedPrograms(tc.command)
		named := make([]string, 0, len(programs))
		for name := range programs {
			named = append(named, name)
		}
		sort.Strings(named)
		if tc.runs != "" && !programs[tc.runs] {
			t.Errorf("%q runs %q by its grammar, identified %v", tc.command, tc.runs, named)
		}
		for _, word := range tc.not {
			if programs[strings.ToLower(word)] {
				t.Errorf("%q: %q is not a program by the grammar, identified %v", tc.command, word, named)
			}
		}
		if opaque := anyOpaqueRead(tc.command); opaque != tc.opaque {
			t.Errorf("%q: opaque=%v, want %v (identified %v)", tc.command, opaque, tc.opaque, named)
		}
	}
}

// find(1) on macOS and GNU findutils: -exec and -execdir end at `;`, or at `+`
// only right after a word holding `{}`; -ok and -okdir end only at `;`. BSD
// find also ends a command at any word that begins with `;`.
func TestFindExecGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: `find . -exec kill {} \;`, runs: "kill"},
		{command: `find . -exec kill {} +`, runs: "kill"},
		{command: `find . -execdir kill {} +`, runs: "kill"},
		{command: `find . -ok kill {} \;`, runs: "kill"},
		{command: `find . -okdir kill {} \;`, runs: "kill"},
		{command: `find . -name a -o \( -name b -a ! -name c \) -exec kill {} \;`, runs: "kill"},
		// A `+` not after `{}` is an argument: xargs takes it as its EOF word
		// and runs kill.
		{command: `find . -exec xargs -E + kill \;`, runs: "kill"},
		// GNU ends at a `+` after any word holding `{}`, so kill runs second.
		{command: `find . -exec echo x{} + -exec kill \;`, runs: "kill"},
		// BSD ends at a word beginning with `;`, so kill runs second.
		{command: `find . -exec echo ';x' -exec kill \;`, runs: "kill"},
		// -ok never ends at `+`: `-exec kill` is an argument of echo.
		{command: `find . -ok echo {} + -exec kill \;`, runs: "echo", not: []string{"kill"}},
		{command: `find . -exec {} \;`, opaque: true},
		{command: `find . -execdir x{}y \;`, opaque: true},
	})
	for _, command := range []string{
		"find . -name '*.tmp' -o -delete",
		"find . ! -name keep -delete",
		`find . \( -name a -o -name b \) -delete`,
		"find -L . -type f -delete -o -print",
	} {
		if dangerous, reason := dangerousCommand(command); !dangerous || reason != findDeleteReason {
			t.Errorf("%q: dangerous=%v reason=%q", command, dangerous, reason)
		}
	}
}

// xargs(1) on macOS (-J, -R and -S take values) and GNU findutils (optional
// -i, -l and -e values are attached only; long options abbreviate). With a
// replacement string the program word may be replaced at run time.
func TestXargsGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "xargs kill", runs: "kill"},
		{command: "xargs -0 kill", runs: "kill"},
		{command: "xargs -n 1 kill", runs: "kill", not: []string{"1"}},
		{command: "xargs -n1 kill", runs: "kill"},
		{command: "xargs -0n 1 kill", runs: "kill", not: []string{"1"}},
		{command: "xargs -tI {} kill {}", runs: "kill", not: []string{"{}"}},
		{command: "xargs -J % cp % /dest", runs: "cp", not: []string{"%"}},
		{command: "xargs -I % -R 2 kill %", runs: "kill", not: []string{"2"}},
		{command: "xargs -I % -S 512 kill %", runs: "kill", not: []string{"512"}},
		{command: "xargs -E stop kill", runs: "kill", not: []string{"stop"}},
		{command: "xargs -L 2 -P 4 -s 4096 kill", runs: "kill", not: []string{"2", "4", "4096"}},
		{command: "xargs --max-args 1 --max-procs 4 kill", runs: "kill", not: []string{"1", "4"}},
		{command: "xargs --max-args=1 kill", runs: "kill"},
		{command: "xargs --max-a 1 kill", runs: "kill", not: []string{"1"}},
		{command: "xargs -a list.txt -d , kill", runs: "kill", not: []string{"list.txt", ","}},
		{command: "xargs --arg-file list.txt --delimiter , kill", runs: "kill", not: []string{"list.txt"}},
		{command: "xargs --process-slot-var SLOT kill", runs: "kill", not: []string{"SLOT"}},
		{command: "xargs -i kill {}", runs: "kill"},
		{command: "xargs -l kill", runs: "kill"},
		{command: "xargs -e kill", runs: "kill"},
		{command: "xargs --replace kill {}", runs: "kill"},
		{command: "xargs --max-lines kill", runs: "kill"},
		{command: "xargs -- kill", runs: "kill"},
		{command: "xargs -I {} {} -f", opaque: true},
		{command: "xargs -I {} sh -c '{} -f'", opaque: true},
		{command: "xargs -I sh sh -c 'ls'", opaque: true},
		{command: "xargs -I CMD CMD -f", opaque: true},
		{command: "xargs -i {} -f", opaque: true},
		{command: "xargs --replace=X X -f", opaque: true},
		{command: "xargs -J % % -f", opaque: true},
	})
}

// GNU coreutils timeout: options, then a DURATION that strtod reads (sign,
// exponent, hex, inf) with an optional s/m/h/d suffix, then the command.
func TestTimeoutGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "timeout 5 kill", runs: "kill", not: []string{"5"}},
		{command: "timeout 5s kill", runs: "kill"},
		{command: "timeout 1.5m kill", runs: "kill"},
		{command: "timeout .5 kill", runs: "kill"},
		{command: "timeout 2d kill", runs: "kill"},
		{command: "timeout 1e3 kill", runs: "kill", not: []string{"1e3"}},
		{command: "timeout inf kill", runs: "kill", not: []string{"inf"}},
		{command: "timeout infinity kill", runs: "kill", not: []string{"infinity"}},
		{command: "timeout +5 kill", runs: "kill", not: []string{"+5"}},
		{command: "timeout 0x10 kill", runs: "kill", not: []string{"0x10"}},
		{command: "timeout -k 2 5 kill", runs: "kill", not: []string{"2", "5"}},
		{command: "timeout -k2 5 kill", runs: "kill"},
		{command: "timeout -vk 2 5 kill", runs: "kill", not: []string{"2", "5"}},
		{command: "timeout --kill-after 2 5 kill", runs: "kill", not: []string{"2"}},
		{command: "timeout --kill-after=2 5 kill", runs: "kill"},
		{command: "timeout --kill 2 5 kill", runs: "kill", not: []string{"2", "5"}},
		{command: "timeout -s TERM 5 kill", runs: "kill", not: []string{"TERM"}},
		{command: "timeout -sTERM 5 kill", runs: "kill"},
		{command: "timeout --sig TERM 5 kill", runs: "kill", not: []string{"TERM"}},
		{command: "timeout --foreground --preserve-status -v 5 kill", runs: "kill"},
		{command: "timeout -- 5 kill", runs: "kill"},
	})
}

// nice(1) on macOS (-n increment, the deprecated -increment) and GNU nice
// (--adjustment, -N, --N and -+N).
func TestNiceGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "nice kill", runs: "kill"},
		{command: "nice -n 5 kill", runs: "kill", not: []string{"5"}},
		{command: "nice -n5 kill", runs: "kill"},
		{command: "nice -n -5 kill", runs: "kill", not: []string{"-5"}},
		{command: "nice -5 kill", runs: "kill"},
		{command: "nice --5 kill", runs: "kill"},
		{command: "nice -+5 kill", runs: "kill"},
		{command: "nice --adjustment 5 kill", runs: "kill", not: []string{"5"}},
		{command: "nice --adjustment=5 kill", runs: "kill"},
		{command: "nice --adj 5 kill", runs: "kill", not: []string{"5"}},
	})
}

// util-linux ionice: -c and -n take values; with -p, -P or -u every operand
// is an id and no command runs.
func TestIoniceGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "ionice -c 3 kill", runs: "kill", not: []string{"3"}},
		{command: "ionice -c3 kill", runs: "kill"},
		{command: "ionice -c 2 -n 7 kill", runs: "kill", not: []string{"2", "7"}},
		{command: "ionice -tc 3 kill", runs: "kill", not: []string{"3"}},
		{command: "ionice --class idle kill", runs: "kill", not: []string{"idle"}},
		// --class is a prefix of --classdata, so only --classd abbreviates.
		{command: "ionice --classd 7 -c 2 kill", runs: "kill", not: []string{"7"}},
		{command: "ionice -p 1234 5678", not: []string{"1234", "5678"}},
		{command: "ionice -c 3 -P 42 43", not: []string{"42", "43"}},
		{command: "ionice -u 1000 1001", not: []string{"1000", "1001"}},
		{command: "ionice --pid 1234 5678", not: []string{"5678"}},
	})
}

// stdbuf(1) on macOS and GNU coreutils: -i, -o and -e take a buffering mode.
func TestStdbufGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "stdbuf -o L kill", runs: "kill", not: []string{"L"}},
		{command: "stdbuf -oL kill", runs: "kill"},
		{command: "stdbuf -i0 -o0 -e0 kill", runs: "kill"},
		{command: "stdbuf --output=L kill", runs: "kill"},
		{command: "stdbuf --output L kill", runs: "kill", not: []string{"L"}},
		{command: "stdbuf --out L kill", runs: "kill", not: []string{"L"}},
	})
}

// su(1) on macOS passes everything after the login to the login shell, so
// `-c` reaches the shell; util-linux su reads -c/--command itself, attached
// or apart, anywhere on the line.
func TestSuGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "su -c kill", runs: "kill"},
		{command: "su root -c kill", runs: "kill"},
		{command: "su - root -c kill", runs: "kill"},
		{command: "su -m operator -c poweroff", runs: "poweroff", not: []string{"operator"}},
		{command: "su -lc kill root", runs: "kill"},
		{command: `su -c'kill -9 1' root`, runs: "kill"},
		{command: "su -ckill root", runs: "kill"},
		{command: "su --command=kill root", runs: "kill"},
		{command: "su --command kill root", runs: "kill"},
		{command: "su --comm kill root", runs: "kill"},
		{command: "su --session-command kill root", runs: "kill"},
		{command: "su -s /bin/sh -c kill root", runs: "kill"},
		{command: "su -- root -c kill", runs: "kill"},
		{command: "su root", opaque: true},
	})
}

// chroot(8) on macOS takes -G, -g and -u values; GNU chroot takes --userspec
// and --groups. Without a command it runs an interactive shell.
func TestChrootGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "chroot /srv kill", runs: "kill", not: []string{"srv"}},
		{command: "chroot -u root /srv kill", runs: "kill", not: []string{"root", "srv"}},
		{command: "chroot -g wheel /srv kill", runs: "kill", not: []string{"wheel", "srv"}},
		{command: "chroot -G wheel,staff /srv kill", runs: "kill", not: []string{"srv"}},
		{command: "chroot --userspec=root:root /srv kill", runs: "kill", not: []string{"srv"}},
		{command: "chroot --userspec root:root /srv kill", runs: "kill", not: []string{"srv"}},
		{command: "chroot --users root:root /srv kill", runs: "kill", not: []string{"srv"}},
		{command: "chroot --skip-chdir /srv kill", runs: "kill", not: []string{"srv"}},
		{command: "chroot /srv", opaque: true},
	})
}

// util-linux flock: options stop at the first operand, the lock; then a
// command, or -c/--command and exactly one command line; a bare number locks a
// descriptor and runs nothing.
func TestFlockGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "flock /tmp/lock kill", runs: "kill", not: []string{"lock"}},
		{command: "flock -n /tmp/lock kill", runs: "kill", not: []string{"lock"}},
		{command: "flock -w 5 /tmp/lock kill", runs: "kill", not: []string{"5", "lock"}},
		{command: "flock -w5 /tmp/lock kill", runs: "kill", not: []string{"lock"}},
		{command: "flock -nw 5 /tmp/lock kill", runs: "kill", not: []string{"5", "lock"}},
		{command: "flock --timeout 5 /tmp/lock kill", runs: "kill", not: []string{"5", "lock"}},
		{command: "flock --wai 5 /tmp/lock kill", runs: "kill", not: []string{"5", "lock"}},
		{command: "flock -E 2 /tmp/lock kill", runs: "kill", not: []string{"2", "lock"}},
		{command: "flock /tmp/lock -c kill", runs: "kill", not: []string{"lock"}},
		{command: "flock /tmp/lock --command kill", runs: "kill", not: []string{"lock"}},
		{command: "flock -s 9", not: []string{"9"}},
	})
}

// procps watch: options stop at the command, whose words are joined and run by
// a shell; -n/--interval and -q/--equexit take values.
func TestWatchGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "watch kill", runs: "kill"},
		{command: "watch -n 5 kill", runs: "kill", not: []string{"5"}},
		{command: "watch -n5 kill", runs: "kill"},
		{command: "watch -tn 5 kill", runs: "kill", not: []string{"5"}},
		{command: "watch --interval 5 kill", runs: "kill", not: []string{"5"}},
		{command: "watch --interval=5 kill", runs: "kill"},
		{command: "watch --int 5 kill", runs: "kill", not: []string{"5"}},
		{command: "watch -d -x kill", runs: "kill"},
		{command: "watch --differences=permanent kill", runs: "kill"},
		{command: "watch -q 3 kill", runs: "kill", not: []string{"3"}},
		{command: "watch -- kill", runs: "kill"},
	})
}

// GNU parallel: bundled options, then a command template ended by `:::`,
// `:::+`, `::::` or `::::+`. With no template the arguments are the command
// lines; a replacement string in the program word is decided at run time;
// --arg-sep and --arg-file-sep rename the separators.
func TestParallelGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "parallel kill ::: 1 2", runs: "kill", not: []string{"1", "2"}},
		{command: "parallel -j 4 kill ::: 1", runs: "kill", not: []string{"4"}},
		{command: "parallel -j4 kill ::: 1", runs: "kill"},
		{command: "parallel -kj 4 kill ::: 1", runs: "kill", not: []string{"4"}},
		{command: "parallel --jobs 4 kill ::: 1", runs: "kill", not: []string{"4"}},
		{command: "parallel --nice 10 kill ::: 1", runs: "kill", not: []string{"10"}},
		{command: "parallel --halt now,fail=1 kill ::: 1", runs: "kill", not: []string{"now,fail=1"}},
		{command: "parallel --ssh ssh kill ::: 1", runs: "kill", not: []string{"ssh"}},
		{command: "parallel --tmpdir /tmp/p --tag kill ::: 1", runs: "kill", not: []string{"p"}},
		{command: "parallel kill :::: args.txt", runs: "kill", not: []string{"args.txt"}},
		{command: "parallel kill :::+ 1 2", runs: "kill"},
		{command: "parallel -- kill ::: 1", runs: "kill"},
		{command: "parallel ::: kill", runs: "kill"},
		{command: "parallel ::: 'kill -9 1' ls", runs: "kill"},
		{command: "parallel --arg-sep ,, kill ,, 1", runs: "kill", not: []string{",,"}},
		{command: "parallel --arg-sep ,, ,, kill", runs: "kill"},
		{command: "parallel :::: cmds.txt", opaque: true},
		// Literal arguments are composed into the template, which is read as
		// written as well.
		{command: "parallel {} ::: kill", runs: "kill", opaque: true},
		{command: "parallel {1} {2} ::: kill ::: 1", runs: "kill", opaque: true},
		{command: "parallel -I X X ::: kill", runs: "kill", opaque: true},
		{command: "parallel sh -c {} ::: kill", runs: "kill", opaque: true},
		{command: "parallel {} :::: cmds.txt", opaque: true},
		// A brace group is shell syntax, not a replacement string.
		{command: "parallel '{ kill -9 1; }' ::: x", runs: "kill"},
		{command: "parallel echo {} ::: x", runs: "echo"},
	})
}

// busybox and toybox run the applet their first argument names; their own
// options (--install, --list, --long, ...) run no applet. ash and hush are
// busybox shells and run -c like any shell.
func TestMultiCallGrammarFidelity(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "busybox kill", runs: "kill"},
		{command: "busybox sh -c kill", runs: "kill"},
		{command: "busybox ash -c kill", runs: "kill"},
		{command: "busybox hush -c kill", runs: "kill"},
		{command: "ash -c kill", runs: "kill"},
		{command: "toybox kill", runs: "kill"},
		{command: "toybox sh -c kill", runs: "kill"},
		{command: "busybox --install -s /usr/local/bin", not: []string{"bin", "-s"}},
		{command: "busybox --list"},
		{command: "toybox --long"},
	})
}

// The same short-option rules hold for the identity-changing wrappers: an
// option bundle ends at the first option that takes a value, which takes the
// rest of the word or the next word.
func TestPrivilegeWrapperOptionBundles(t *testing.T) {
	checkLauncherCases(t, []launcherCase{
		{command: "sudo -iu root kill", runs: "kill", not: []string{"root"}},
		{command: "sudo -Eu root kill", runs: "kill", not: []string{"root"}},
		{command: "sudo --us root kill", runs: "kill", not: []string{"root"}},
		{command: "doas -nu root kill", runs: "kill", not: []string{"root"}},
		{command: "env -iu FOO kill", runs: "kill", not: []string{"FOO"}},
		{command: "env --uns FOO kill", runs: "kill", not: []string{"FOO"}},
		{command: "time -ao out.txt kill", runs: "kill", not: []string{"out.txt"}},
		{command: "exec -cla name kill", runs: "kill", not: []string{"name"}},
	})
}

// Proxy-removal detection reads env's options with the same grammar, so a
// bundled or abbreviated -u is still a removal.
func TestProxyRemovalReadsEnvOptionBundles(t *testing.T) {
	for _, command := range []string{
		"env -iu HTTPS_PROXY gh api user",
		"env -uHTTPS_PROXY gh api user",
		"env --uns HTTPS_PROXY gh api user",
		"env --unset=HTTPS_PROXY gh api user",
	} {
		if removed := proxyRemovalVariables(command); len(removed) != 1 || removed[0] != "HTTPS_PROXY" {
			t.Errorf("%q: removed %v", command, removed)
		}
	}
	if removed := proxyRemovalVariables("env -C /tmp gh api user"); len(removed) != 0 {
		t.Errorf("a -C value is not a removal: %v", removed)
	}
}

// The hard floor follows the same grammars: every spelling above that runs a
// forbidden command is refused, and a spelling that runs none is not.
func TestHardFloorFollowsLauncherGrammars(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		`find . -exec xargs -E + rm -rf / \;`,
		`find . -exec echo ';x' -exec rm -rf / \;`,
		"xargs -0n 1 rm -rf /",
		"xargs -J % rm -rf /",
		"xargs -I % -R 2 rm -rf /",
		"timeout 1e3 rm -rf /",
		"timeout inf rm -rf /",
		"timeout -vk 2 5 rm -rf /",
		"timeout --kill 2 5 rm -rf /",
		"nice --adj 5 rm -rf /",
		"ionice -tc 3 rm -rf /",
		"stdbuf --out L rm -rf /",
		`su -c'rm -rf /' root`,
		"su --comm 'rm -rf /' root",
		"chroot -u root / rm -rf /",
		"chroot --users root:root / rm -rf /",
		"flock -nw 5 /tmp/lock rm -rf /",
		"watch -tn 5 rm -rf /",
		"parallel -kj 4 rm -rf / ::: x",
		"parallel --nice 10 rm -rf / ::: x",
		"parallel ::: 'rm -rf /'",
		"parallel --arg-sep ,, rm -rf / ,, x",
		"busybox ash -c 'rm -rf /'",
		"sudo -iu root rm -rf /",
		"env -iu FOO rm -rf /",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	for _, command := range []string{
		"ionice -p 1234 rm -rf /",
		`find . -ok echo {} + -exec rm -rf / \;`,
		"busybox --install -s /usr/local/bin",
	} {
		if hardFloorBlocks(t, command) {
			t.Errorf("hard floor refused %q, which runs no forbidden command", command)
		}
	}
}
