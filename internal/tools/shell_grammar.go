package tools

import (
	"slices"
	"strings"
)

// Shell invocation grammars.
//
// A POSIX shell's -c is a flag, not an option that takes the next word: the
// command string is the first operand after all of the shell's options, which
// may follow -c and end at `--` or a lone `-`
// (`sh -c [-abCefhimnuvx] [-o option] [+o option] [--] command_string`).
// Taking the word after -c read `sh -c -- 'rm -rf ~'` as a program named `--`.
// Each shell reads its own options its own way, so a shell is read with the
// grammar of every shell its name may stand for, and every command string any
// of them would run is classified.

// shellRun is what one grammar reads from a shell's arguments.
type shellRun struct {
	// commands are the command strings it runs, each with the words after it
	// joined on: the field reading splits a quoted string into several words,
	// and positional parameters only add words to classify.
	commands []string
	// opaque: it also runs code no argument shows — a script file, standard
	// input, or a startup file an option names.
	opaque bool
}

func (r shellRun) script() (string, bool) {
	return strings.Join(r.commands, "\n"), len(r.commands) > 0
}

// posixShellGrammar is how one POSIX shell reads its options: words beginning
// with `-` or `+` up to `--`, a lone `-`, or the first operand. Single-letter
// options cluster (`-ec`), and -c is a flag in either sign.
type posixShellGrammar struct {
	// valueLetters take a value (`-o pipefail`, `+o posix`, bash's
	// `-O extglob`, mksh's `-T tty`): the next word, or with clusterValue the
	// rest of their cluster when it has any (zsh and ksh read `-opipefail`,
	// while bash and dash read `-oe pipefail` as `-o pipefail -e`).
	valueLetters string
	clusterValue bool
	// longOptions are multi-letter options, true for those that take the next
	// word. With leadingLong (bash) they are read only before the first
	// single-letter option, spelled with one dash or two (`-norc`,
	// `--rcfile FILE`); after it bash reads `-rcfile` as -r -c -f -i -l -e.
	longOptions map[string]bool
	leadingLong bool
	// startupFiles are the long options that name a file of commands the
	// shell runs first.
	startupFiles map[string]struct{}
	// breakLetter ends the options after the word holding it (zsh's -b).
	breakLetter byte
	// plusEnds: a lone `+` ends the options as a lone `-` does (zsh).
	plusEnds bool
}

var (
	bashShellGrammar = posixShellGrammar{
		valueLetters: "oO",
		longOptions: map[string]bool{"debug": false, "debugger": false, "dump-po-strings": false,
			"dump-strings": false, "help": false, "init-file": true, "login": false, "noediting": false,
			"noprofile": false, "norc": false, "posix": false, "pretty-print": false, "protected": false,
			"rcfile": true, "restricted": false, "verbose": false, "version": false, "wordexp": false},
		leadingLong:  true,
		startupFiles: wordSet("init-file", "rcfile"),
	}
	// dash, busybox ash and hush.
	dashShellGrammar = posixShellGrammar{valueLetters: "o"}
	zshShellGrammar  = posixShellGrammar{valueLetters: "o", clusterValue: true,
		longOptions: map[string]bool{"emulate": true}, breakLetter: 'b', plusEnds: true}
	// zsh emulating sh reads sh's option letters, where -b is set -b.
	zshAsShGrammar = posixShellGrammar{valueLetters: "o", clusterValue: true,
		longOptions: map[string]bool{"emulate": true}, plusEnds: true}
	// ksh93 (-R file) and mksh (-T tty).
	kshShellGrammar = posixShellGrammar{valueLetters: "oRT", clusterValue: true}
)

// posixShellOptions is what a POSIX shell's options ask of it.
type posixShellOptions struct {
	command  bool     // -c: the first operand is the command string
	letters  string   // single-letter options turned on (`-ie` → "ie")
	operands []string // the words after the options
	startup  bool     // an option names a startup file of commands
}

func (g posixShellGrammar) read(args []string) posixShellOptions {
	var out posixShellOptions
	leading := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-" || arg == "--" || (arg == "+" && g.plusEnds):
			out.operands = args[i+1:]
			return out
		case arg == "" || (arg[0] != '-' && arg[0] != '+'):
			out.operands = args[i:]
			return out
		}
		if name, long := g.longOption(arg, leading); long {
			if g.longOptions[name] {
				if i++; i == len(args) {
					return out
				}
				_, startup := g.startupFiles[name]
				out.startup = out.startup || startup
			}
			continue
		}
		leading = false
		stop := false
		for j := 1; j < len(arg); j++ {
			letter := arg[j]
			switch {
			case letter == 'c':
				out.command = true
			case strings.IndexByte(g.valueLetters, letter) >= 0:
				if g.clusterValue && j+1 < len(arg) {
					j = len(arg)
				} else if i++; i == len(args) {
					return out
				}
			case g.breakLetter != 0 && letter == g.breakLetter:
				stop = true
			case arg[0] == '-':
				out.letters += string(letter)
			}
		}
		if stop {
			out.operands = args[i+1:]
			return out
		}
	}
	return out
}

// longOption names the multi-letter option a word spells: `--name`, or for a
// leadingLong shell before its first single-letter option also `-name`.
func (g posixShellGrammar) longOption(arg string, leading bool) (string, bool) {
	if name, ok := strings.CutPrefix(arg, "--"); ok {
		return name, true
	}
	if _, known := g.longOptions[arg[1:]]; known && g.leadingLong && leading && arg[0] == '-' {
		return arg[1:], true
	}
	return "", false
}

func (g posixShellGrammar) run(args []string) shellRun {
	options := g.read(args)
	run := shellRun{opaque: options.startup || !options.command || len(options.operands) == 0}
	if options.command && len(options.operands) > 0 {
		run.commands = []string{strings.Join(options.operands, " ")}
	}
	return run
}

// cshShellRun reads csh and tcsh: the word after the cluster holding c is the
// command whatever it begins with (`csh -fc 'cmd'`, `tcsh -cf 'cmd'`); -b
// ends the options after its word, and the first other word is a script.
func cshShellRun(args []string) shellRun {
	for i, arg := range args {
		if len(arg) < 2 || arg[0] != '-' {
			break
		}
		if strings.IndexByte(arg[1:], 'c') >= 0 {
			if i+1 < len(args) {
				return shellRun{commands: []string{strings.Join(args[i+1:], " ")}}
			}
			break
		}
		if strings.IndexByte(arg[1:], 'b') >= 0 {
			break
		}
	}
	return shellRun{opaque: true}
}

// fishShellOptions is fish's getopt_long grammar, which stops at its first
// operand.
var fishShellOptions = prefixWrapper{
	valueFlags: wordSet("-c", "--command", "-C", "--init-command", "-d", "--debug", "-o", "--debug-output",
		"-f", "--features", "-p", "--profile", "--profile-startup", "-D", "--debug-stack-frames"),
	longFlags: wordSet("--interactive", "--login", "--no-config", "--no-execute", "--private",
		"--print-rusage-self", "--print-debug-categories", "--help", "--version"),
	scriptFlags: wordSet("-c", "--command", "-C", "--init-command"),
}

// fishShellRun reads fish: every -c and -C runs its command string, and -C
// alone runs before the input or script fish then reads.
func fishShellRun(args []string) shellRun {
	var run shellRun
	command := false
	for words := args; ; {
		reading := fishShellOptions.wrapperOptions(words)
		if len(reading.options) == 0 {
			break
		}
		option := reading.options[len(reading.options)-1]
		if _, script := fishShellOptions.scriptFlags[option.name]; !script || !option.hasValue {
			break
		}
		run.commands = append(run.commands, strings.Join(append([]string{option.value}, reading.rest...), " "))
		command = command || option.name == "-c" || option.name == "--command"
		words = reading.rest
	}
	run.opaque = !command
	return run
}

// posixShellGrammars lists the POSIX grammars each name may stand for: sh is
// bash, dash, busybox ash, ksh or zsh as the host installs it.
var posixShellGrammars = map[string][]posixShellGrammar{
	"sh":   {bashShellGrammar, dashShellGrammar, zshAsShGrammar, kshShellGrammar},
	"bash": {bashShellGrammar},
	"dash": {dashShellGrammar}, "ash": {dashShellGrammar}, "hush": {dashShellGrammar},
	"zsh": {zshShellGrammar},
	"ksh": {kshShellGrammar}, "mksh": {kshShellGrammar},
}

// shellGrammars are the shells that run a command string of their own; ash
// and hush are the shells busybox carries.
var shellGrammars = func() map[string][]func([]string) shellRun {
	grammars := map[string][]func([]string) shellRun{
		"csh": {cshShellRun}, "tcsh": {cshShellRun}, "fish": {fishShellRun},
	}
	for name, posix := range posixShellGrammars {
		for _, grammar := range posix {
			grammars[name] = append(grammars[name], grammar.run)
		}
	}
	return grammars
}()

// loginShellGrammars read the words su hands a target's login shell, which
// may be any of the shells.
var loginShellGrammars = []func([]string) shellRun{
	bashShellGrammar.run, dashShellGrammar.run, zshShellGrammar.run, zshAsShGrammar.run, kshShellGrammar.run,
	cshShellRun, fishShellRun,
}

// readShell reads a shell's arguments with every grammar its name may stand
// for.
func readShell(name string, args []string) shellRun {
	return readShellGrammars(shellGrammars[name], args)
}

func readShellGrammars(grammars []func([]string) shellRun, args []string) shellRun {
	var reading shellRun
	for _, grammar := range grammars {
		run := grammar(args)
		reading.opaque = reading.opaque || run.opaque
		for _, command := range run.commands {
			if !slices.Contains(reading.commands, command) {
				reading.commands = append(reading.commands, command)
			}
		}
	}
	return reading
}

// exactShellCommand is the command string a POSIX shell runs when every
// grammar its name stands for reads one -c operand and nothing more: no
// positional parameters and no startup file.
func exactShellCommand(name string, args []string) (string, bool) {
	grammars := posixShellGrammars[name]
	command := ""
	for i, grammar := range grammars {
		options := grammar.read(args)
		if !options.command || options.startup || len(options.operands) != 1 ||
			(i > 0 && options.operands[0] != command) {
			return "", false
		}
		command = options.operands[0]
	}
	return command, strings.TrimSpace(command) != ""
}

// interactiveShell reports a POSIX shell that runs interactively or reads its
// commands from standard input under any grammar its name stands for: `-i`,
// `-s`, or neither -c nor a script operand.
func interactiveShell(program commandLayer) bool {
	if _, shell := startupFileShells[program.Name]; !shell {
		return false
	}
	for _, grammar := range posixShellGrammars[program.Name] {
		options := grammar.read(program.Args)
		if strings.ContainsAny(options.letters, "is") || (!options.command && len(options.operands) == 0) {
			return true
		}
	}
	return false
}
