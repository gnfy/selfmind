package tools

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// shellSingleQuote quotes s for a POSIX shell so that it arrives as one word.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellDoubleQuote quotes s inside double quotes, leaving `$` to expand the way
// a person writing `bash -c "rm -rf $HOME"` would.
func shellDoubleQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`").Replace(s) + `"`
}

// commandSpelling writes one program invocation the way a shell would still run
// that program: a different case on a case-insensitive filesystem, a path, an
// escape, a leading assignment or a known prefix wrapper.
type commandSpelling struct {
	name  string
	spell func(program, args string) string
	// proof reports whether `ls -la` written this way stays a provable
	// observation. Assignments, paths, other spellings of the name and
	// wrappers whose effect on which program runs is not fully known must not
	// keep that proof.
	proof bool
}

func titleWord(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}

var commandSpellings = []commandSpelling{
	{"bare", func(p, a string) string { return p + " " + a }, true},
	// A case-insensitive filesystem runs these as the program, so the deny
	// side folds case; a case-sensitive one may hold a different binary, so
	// proof does not.
	{"uppercase", func(p, a string) string { return strings.ToUpper(p) + " " + a }, false},
	{"title case", func(p, a string) string { return titleWord(p) + " " + a }, false},
	{"path", func(p, a string) string { return "/bin/" + p + " " + a }, false},
	{"uppercase path", func(p, a string) string { return "/BIN/" + strings.ToUpper(p) + " " + a }, false},
	{"backslash escape", func(p, a string) string { return `\` + p + " " + a }, true},
	{"quoted word", func(p, a string) string { return shellSingleQuote(p) + " " + a }, true},
	{"split quotes", func(p, a string) string { return p[:1] + "''" + p[1:] + " " + a }, true},
	{"assignment with slash", func(p, a string) string { return "FOO=a/b " + p + " " + a }, false},
	{"PATH assignment", func(p, a string) string { return "PATH=/usr/bin " + p + " " + a }, false},
	{"quoted assignment with space", func(p, a string) string { return `FOO="a b" ` + p + " " + a }, false},
	{"escaped assignment space", func(p, a string) string { return `FOO=a\ b ` + p + " " + a }, false},
	{"env assignment", func(p, a string) string { return "env FOO=a/b " + p + " " + a }, false},
	{"env option", func(p, a string) string { return "env -u FOO " + p + " " + a }, false},
	{"command", func(p, a string) string { return "command " + p + " " + a }, true},
	{"exec", func(p, a string) string { return "exec " + p + " " + a }, false},
	{"nice", func(p, a string) string { return "nice -n 5 " + p + " " + a }, false},
	{"timeout", func(p, a string) string { return "timeout 5 " + p + " " + a }, true},
	{"nohup", func(p, a string) string { return "nohup " + p + " " + a }, false},
	{"sudo", func(p, a string) string { return "sudo -u root " + p + " " + a }, false},
	{"sudo long option", func(p, a string) string { return "sudo --user root " + p + " " + a }, false},
	{"doas", func(p, a string) string { return "doas " + p + " " + a }, false},
	{"time", func(p, a string) string { return "time " + p + " " + a }, false},
	{"sh -c with assignment", func(p, a string) string {
		return "sh -c " + shellSingleQuote("FOO=a/b "+strings.ToUpper(p)+" "+a)
	}, false},
	{"bash -c", func(p, a string) string { return "bash -c " + shellDoubleQuote(p+" "+a) }, true},
	{"if body", func(p, a string) string { return "if true; then " + p + " " + a + "; fi" }, false},
}

// Every analysis that names a program must name the one the shell runs. One
// probe per analysis is written in every spelling, and each analysis must see
// through all of them the same way.
func TestEveryAnalysisIdentifiesTheProgramTheShellRuns(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, spelling := range commandSpellings {
		t.Run(spelling.name, func(t *testing.T) {
			floor := spelling.spell("rm", "-rf /")
			if blocked, _ := hardlineToolCall("", "terminal", map[string]interface{}{"command": floor}); !blocked {
				t.Errorf("hard floor missed %q", floor)
			}
			destructive := spelling.spell("rm", "-rf build")
			if dangerous, reason := dangerousToolCall("", "terminal", map[string]interface{}{"command": destructive}); !dangerous || reason != "invokes dangerous command: rm" {
				t.Errorf("dangerous classification missed %q: %v %q", destructive, dangerous, reason)
			}
			classes := operationClassesFor("terminal", map[string]interface{}{"command": destructive}, false)
			if !operationClassListed(classes, OpClassDelete) {
				t.Errorf("operation classes for %q lack delete: %v", destructive, classes)
			}
			egress := spelling.spell("curl", "-X DELETE https://api.example.test/items/1")
			if dangerous, reason := dangerousToolCall("", "terminal", map[string]interface{}{"command": egress}); !dangerous || reason != "network egress command: curl" {
				t.Errorf("egress classification missed %q: %v %q", egress, dangerous, reason)
			}
			build := spelling.spell("go", "build ./...")
			programs, opaque := execCommandProgramSet("terminal", map[string]interface{}{"command": build})
			if !stringInList("go", programs) || opaque {
				t.Errorf("environment analysis of %q: programs=%v opaque=%v", build, programs, opaque)
			}
			removal := spelling.spell("env", "-u HTTPS_PROXY gh pr view 19")
			if got := proxyRemovalVariables(removal); !reflect.DeepEqual(got, []string{"HTTPS_PROXY"}) {
				t.Errorf("proxy removal detection of %q = %v", removal, got)
			}
			observation := spelling.spell("ls", "-la")
			if got := provenReadOnly(t, observation); got != spelling.proof {
				t.Errorf("observation proof of %q = %v, want %v", observation, got, spelling.proof)
			}
		})
	}
}

func operationClassListed(classes []OperationClass, want OperationClass) bool {
	for _, class := range classes {
		if class == want {
			return true
		}
	}
	return false
}

// hardFloorCorpus is every command the floor exists to refuse.
var hardFloorCorpus = []struct{ program, args string }{
	{"rm", "-rf /"},
	{"rm", "-rf /*"},
	{"rm", "-r /home"},
	{"rm", "-rf $HOME"},
	{"rm", "-rf ~"},
	{"rm", "-rf /home/tester"},
	{"rm", "-R /etc"},
	{"rm", "--recursive --force /usr"},
	{"rm", "--no-preserve-root -rf /"},
	{"rm", "-rf /var"},
	{"rm", "-rf /boot"},
	{"mkfs", "-t ext4 /dev/sdb1"},
	{"mkfs.ext4", "/dev/sda1"},
	{"dd", "if=/dev/zero of=/dev/sda bs=1M"},
	{"dd", "if=/dev/zero of=/dev/nvme0n1"},
	{"shutdown", "-h now"},
	{"reboot", ""},
	{"halt", ""},
	{"poweroff", ""},
	{"init", "0"},
	{"init", "6"},
	{"echo", "x > /dev/sda"},
	{"rm", "-r ~/"},
	{"rm", "-rf $HOME/"},
	{"rm", "-rf ~/*"},
	{"dd", `if=/dev/zero of="/dev/sda"`},
	{"dd", "if=/dev/zero of=/dev/disk0"},
	{"echo", `x > "/dev/sda"`},
}

// The program word is respelled first; a prefix and an enclosing form are
// then applied around it.
var hardFloorProgramSpellings = []func(string) string{
	func(p string) string { return p },
	strings.ToUpper,
	titleWord,
	func(p string) string { return "/bin/" + p },
	func(p string) string { return "/BIN/" + strings.ToUpper(p) },
	func(p string) string { return `\` + p },
	shellSingleQuote,
	func(p string) string { return p[:1] + "''" + p[1:] },
}

var hardFloorPrefixes = []string{
	"",
	"FOO=a/b ",
	"PATH=/usr/bin ",
	`FOO="a b" `,
	`FOO=a\ b `,
	"env ",
	"env FOO=a/b ",
	"ENV -u FOO ",
	"/usr/bin/env PATH=/usr/bin ",
	"env --unset FOO ",
	"command ",
	`\command `,
	"exec ",
	"Exec -a name ",
	"nice -n 5 ",
	"nice --adjustment 5 ",
	"timeout 5 ",
	"timeout -k 5 10 ",
	"nohup ",
	"sudo ",
	"sudo -u root ",
	"sudo --user root ",
	"SUDO -E ",
	"doas ",
	"doas -u root ",
	"time ",
	"setsid ",
	"stdbuf -o 0 ",
	"ionice -c 3 ",
	"chroot / ",
	"flock /tmp/lock ",
	"busybox ",
	"toybox ",
}

var hardFloorEnclosures = []func(string) string{
	func(c string) string { return c },
	func(c string) string { return "sh -c " + shellSingleQuote(c) },
	func(c string) string { return "bash -c " + shellDoubleQuote(c) },
	func(c string) string { return "BASH -lc " + shellSingleQuote(c) },
	func(c string) string { return "/bin/zsh -c " + shellSingleQuote(c) },
	func(c string) string { return "if true; then " + c + "; fi" },
	func(c string) string { return "{ " + c + "; }" },
	// Spaced so nesting stays a subshell: `((...))` is arithmetic, not a command.
	func(c string) string { return "( " + c + " )" },
	func(c string) string { return "true && " + c },
	func(c string) string { return "for i in 1; do " + c + "; done" },
	func(c string) string { return "su -c " + shellSingleQuote(c) },
	func(c string) string { return "watch -n 1 " + shellSingleQuote(c) },
	func(c string) string { return "parallel " + shellSingleQuote(c) + " ::: x" },
	func(c string) string { return "flock /tmp/lock -c " + shellSingleQuote(c) },
	func(c string) string { return "eval " + shellSingleQuote(c) },
	func(c string) string { return "find . -maxdepth 0 -exec sh -c " + shellSingleQuote(c) + ` \;` },
}

func hardFloorBlocks(t *testing.T, command string) bool {
	t.Helper()
	blocked, _ := hardlineToolCall("", "terminal", map[string]interface{}{"command": command})
	return blocked
}

// The floor refuses what runs, not what it is called. Every forbidden command
// stays refused under every combination of respelling, prefix and enclosure.
func TestHardFloorBlocksEveryRespellingOfTheForbiddenCorpus(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	missed := 0
	for _, entry := range hardFloorCorpus {
		for _, program := range hardFloorProgramSpellings {
			for _, prefix := range hardFloorPrefixes {
				for _, enclose := range hardFloorEnclosures {
					command := enclose(strings.TrimSpace(prefix + program(entry.program) + " " + entry.args))
					if !hardFloorBlocks(t, command) {
						missed++
						if missed <= 40 {
							t.Errorf("hard floor missed %q", command)
						}
					}
				}
			}
		}
	}
	for _, enclose := range hardFloorEnclosures {
		if command := enclose(":(){ :|:& };:"); !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	if missed > 0 {
		t.Fatalf("hard floor missed %d respellings", missed)
	}
}

// Random compositions of wrappers and nested enclosures, from a fixed seed so
// a failure reproduces.
func TestHardFloorBlocksComposedRespellings(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	rng := rand.New(rand.NewSource(20261005))
	wrappers := hardFloorPrefixes[5:]
	assignments := hardFloorPrefixes[1:5]
	for i := 0; i < 4000; i++ {
		entry := hardFloorCorpus[rng.Intn(len(hardFloorCorpus))]
		command := hardFloorProgramSpellings[rng.Intn(len(hardFloorProgramSpellings))](entry.program) + " " + entry.args
		for n := rng.Intn(4); n > 0; n-- {
			command = wrappers[rng.Intn(len(wrappers))] + command
		}
		if rng.Intn(2) == 0 {
			command = assignments[rng.Intn(len(assignments))] + command
		}
		for n := rng.Intn(3); n > 0; n-- {
			command = hardFloorEnclosures[rng.Intn(len(hardFloorEnclosures))](strings.TrimSpace(command))
		}
		if !hardFloorBlocks(t, strings.TrimSpace(command)) {
			t.Fatalf("hard floor missed composed respelling %q", command)
		}
	}
}

// Each wrapper grammar entry is pinned by the form that needs it: a value
// option that would otherwise be read as the program, a command line carried
// as an option value, or a shell that runs -c.
func TestHardFloorSeesThroughEveryKnownWrapper(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"env -S 'rm -rf /'",
		"env --split-string='rm -rf /'",
		"env -P /usr/bin rm -rf /",
		"env -C /tmp rm -rf /",
		"sudo -D /tmp rm -rf /",
		"sudo --chdir /tmp rm -rf /",
		"doas -u root rm -rf /",
		"builtin exec rm -rf /",
		"eval rm -rf /",
		"eval 'rm -rf /'",
		"time -p rm -rf /",
		"\\time -o timing.txt rm -rf /",
		"ionice --class 3 rm -rf /",
		"stdbuf --output 0 rm -rf /",
		"xargs --max-args 1 rm -rf / < /dev/null",
		"timeout -- 5 rm -rf /",
		"ksh -c 'rm -rf /'",
		"fish -c 'rm -rf /'",
		"tcsh -fc 'rm -rf /'",
		"bash -ce 'rm -rf /'",
	} {
		if !hardFloorBlocks(t, command) {
			t.Errorf("hard floor missed %q", command)
		}
	}
	// Nesting past the bound is opaque: it asks, it is never read as the
	// wrapper alone.
	deep := strings.Repeat("nohup ", maxWrapperDepth+1) + "rm -rf build"
	if dangerous, reason := dangerousToolCall("", "terminal", map[string]interface{}{"command": deep}); !dangerous {
		t.Fatalf("nesting past the bound must stay dangerous: %q", reason)
	}
}

// The constraint that must change the result: the same respellings of
// ordinary work stay below the floor.
func TestHardFloorRespellingsKeepOrdinaryWorkApprovable(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, entry := range []struct{ program, args string }{
		{"rm", "-rf build"},
		{"rm", "/etc/hosts"},
		{"init", "restart"},
		{"dd", "if=disk.img of=copy.img"},
		{"ls", "-la /"},
		{"echo", "rm -rf /"},
	} {
		for _, program := range hardFloorProgramSpellings {
			for _, prefix := range hardFloorPrefixes {
				for _, enclose := range hardFloorEnclosures {
					command := enclose(strings.TrimSpace(prefix + program(entry.program) + " " + entry.args))
					if hardFloorBlocks(t, command) {
						t.Fatalf("hard floor refused ordinary work %q", command)
					}
				}
			}
		}
	}
}

// Ordinary development commands stay below both the floor and the dangerous
// heuristic, including the shapes the shell reading now sees into.
func TestOrdinaryCommandsStayUnflagged(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, command := range []string{
		"go build ./...",
		"go test ./internal/tools/ -run TestHardline -count=1",
		"git status --short && git log --oneline -5",
		"ls -la && cat README.md | head -20",
		"rg -n TODO internal/tools",
		"npm test",
		"make build",
		"python3 -m pytest -q",
		"docker build -t app .",
		"kubectl get pods -n default",
		"TZ=UTC date",
		"CGO_ENABLED=0 go build ./cmd/selfmind",
		`FOO="a b" go vet ./...`,
		"PATH=/usr/local/go/bin:$PATH go version",
		"LC_ALL=C sort notes.txt",
		"export GOFLAGS=-mod=mod; go build ./...",
		"set -euo pipefail; go test ./...",
		"if [ -f go.mod ]; then go build ./...; fi",
		`for f in *.go; do gofmt -l "$f"; done`,
		"exec >build.log 2>&1",
		"time go test ./...",
		"command -v jq >/dev/null && jq . data.json",
		"nohup ./server > server.log 2>&1 &",
		`echo "rm -rf / is never run here"`,
		"grep -rn LD_PRELOAD docs",
		"env GOFLAGS=-mod=mod go build ./...",
	} {
		args := map[string]interface{}{"command": command}
		if blocked, reason := hardlineToolCall("", "terminal", args); blocked {
			t.Errorf("hard floor refused %q: %s", command, reason)
		}
		if dangerous, reason := dangerousToolCall("", "terminal", args); dangerous {
			t.Errorf("dangerous heuristic flagged %q: %s", command, reason)
		}
	}
}

func TestCommandSpellingsCoverEveryProofOutcome(t *testing.T) {
	// The table above is only meaningful while it holds spellings on both sides
	// of the proof boundary.
	proven, refused := 0, 0
	for _, spelling := range commandSpellings {
		if spelling.proof {
			proven++
		} else {
			refused++
		}
	}
	if proven == 0 || refused == 0 {
		t.Fatalf("spelling table lost one side of the proof boundary: %d proven, %d refused", proven, refused)
	}
}
