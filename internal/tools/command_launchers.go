package tools

import (
	"regexp"
	"slices"
	"strings"
)

// Launcher grammars.
//
// A launcher runs a command named by its own arguments: a prefix wrapper
// (`timeout 5 cmd`, `sudo -iu root cmd`), find's -exec family, su -c. Which
// word it runs is decided by the launcher's own parser, so identification reads
// each launcher with that parser's rules: getopt's short-option bundles,
// getopt_long's and Getopt::Long's abbreviations, operands, terminators and
// replacement strings. Reading a word as an option value where a variant would
// run it can only name a command that a stricter variant refuses to start.

// findCommandPrimaries run a command. -exec and -execdir end at `;` or at a
// `+` after `{}`; -ok and -okdir end only at `;` (see findCommands).
var findCommandPrimaries = map[string]struct{}{
	"-exec": {}, "-execdir": {}, "-ok": {}, "-okdir": {},
}

// prefixWrapper is a program that runs the command named by its own
// arguments, after options of its own. Its options are read the way getopt,
// getopt_long and Perl's Getopt::Long read them (wrapperOptions).
type prefixWrapper struct {
	// valueFlags take a value: the rest of a short-option bundle, the text
	// after `=`, or the next word (`sudo -u root`, `sudo -iu root`,
	// `nice --adjustment 5`).
	valueFlags map[string]struct{}
	// optionalFlags take a value only when it is attached (`xargs -i{}`,
	// `--replace=X`), or, under Getopt::Long, also the next word when it does
	// not begin with `-` (optionalTakesNext).
	optionalFlags     map[string]struct{}
	optionalTakesNext bool
	// longFlags are the long options that take no value. With the value-taking
	// ones they are every long option, which getopt_long and Getopt::Long let a
	// caller abbreviate to any unique prefix (`timeout --kill 5`).
	longFlags map[string]struct{}
	// duration: a DURATION precedes the command (`timeout 5 cmd`).
	duration bool
	// operands precede the command (`chroot ROOT cmd`, `flock FILE cmd`).
	operands int
	// scriptFlags take a whole command line as their value
	// (`flock FILE -c 'cmd'`).
	scriptFlags map[string]struct{}
	// splitFlags split their value into words read in place of the option,
	// as the wrapper's own arguments (`env -S '-i cmd'`; splitEnvString).
	splitFlags map[string]struct{}
	// pathFlags replace the search path the command is resolved through
	// (`env -P dir`); they are recorded as the PATH assignment they amount to.
	pathFlags map[string]struct{}
	// joinsScript: the command words are joined and run by a shell (`watch`,
	// `parallel`).
	joinsScript bool
	// terminators end the command words (`parallel cmd ::: args`). Those in
	// argumentTerminators introduce literal arguments, which are the command
	// lines themselves when no command precedes them (`parallel ::: 'cmd'`).
	terminators         map[string]struct{}
	argumentTerminators map[string]struct{}
	// separatorFlags rename the terminators for one invocation
	// (`parallel --arg-sep ,,`): each maps to the default it replaces.
	separatorFlags map[string]string
	// replaceFlags name a string the wrapper replaces with its input in the
	// command it runs (`xargs -I {}`); an optional one without a value names
	// `{}`. replacesBraces: every GNU parallel replacement string is one
	// (`{}`, `{1}`, `{=expr=}`). A program word holding one is chosen at run
	// time.
	replaceFlags   map[string]struct{}
	replacesBraces bool
	// targetFlags make the wrapper act on existing processes, or report
	// state, instead of running a command (`ionice -p 1234 5678`,
	// `setpriv --dump`).
	targetFlags map[string]struct{}
	// assignFlags set a variable for the command (`arch -e NAME=value`); each
	// is recorded as the assignment it makes.
	assignFlags map[string]struct{}
	// wholeWords: options are whole words, never bundles (macOS arch reads
	// `-arm64e` as one option).
	wholeWords bool
	// ownOptionsRunNothing: any option of its own means the wrapper reports or
	// installs instead of running a command (`busybox --install`).
	ownOptionsRunNothing bool
	// bareRunsNothing: written without a command it changes only the shell
	// or a lock (`exec >build.log 2>&1`, `flock -n 9`), so nothing is hidden
	// from analysis.
	bareRunsNothing bool
}

func wordSet(words ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(words))
	for _, word := range words {
		set[word] = struct{}{}
	}
	return set
}

// prefixWrappers is data: a wrapper joins it with its option grammar, never as
// a branch in a classifier. Each grammar is the union of the documented ones a
// host may run: the BSD tools macOS ships (env, xargs, nice, stdbuf, time,
// chroot, sudo) and GNU coreutils, findutils, util-linux, procps and GNU
// parallel.
var prefixWrappers = map[string]prefixWrapper{
	"env": {
		valueFlags:    wordSet("-u", "--unset", "-C", "--chdir", "-S", "--split-string", "-P", "-a", "--argv0"),
		optionalFlags: wordSet("--block-signal", "--default-signal", "--ignore-signal"),
		longFlags:     wordSet("--ignore-environment", "--null", "--debug", "--list-signal-handling", "--help", "--version"),
		splitFlags:    wordSet("-S", "--split-string"),
		pathFlags:     wordSet("-P"),
	},
	"sudo": {
		valueFlags: wordSet("-u", "--user", "-g", "--group", "-p", "--prompt", "-C", "--close-from",
			"-r", "--role", "-t", "--type", "-T", "--command-timeout", "-U", "--other-user", "-h", "--host",
			"-D", "--chdir", "-R", "--chroot", "-a", "--auth-type", "-c", "--login-class"),
		optionalFlags: wordSet("--preserve-env"),
		longFlags: wordSet("--askpass", "--background", "--bell", "--edit", "--set-home", "--help", "--login",
			"--remove-timestamp", "--reset-timestamp", "--list", "--no-update", "--non-interactive",
			"--preserve-groups", "--stdin", "--shell", "--version", "--validate"),
	},
	"doas": {valueFlags: wordSet("-u", "-C", "-a")},
	"xargs": {
		valueFlags: wordSet("-n", "-I", "-J", "-R", "-S", "-P", "-L", "-s", "-d", "-E", "-a", "--max-args",
			"--max-procs", "--max-chars", "--delimiter", "--arg-file", "--process-slot-var"),
		optionalFlags: wordSet("-i", "-l", "-e", "--replace", "--max-lines", "--eof"),
		longFlags: wordSet("--null", "--interactive", "--no-run-if-empty", "--verbose", "--exit", "--open-tty",
			"--show-limits", "--help", "--version"),
		replaceFlags: wordSet("-I", "-J", "-i", "--replace"),
	},
	"nohup": {longFlags: wordSet("--help", "--version")},
	"timeout": {
		valueFlags: wordSet("-s", "--signal", "-k", "--kill-after"),
		longFlags:  wordSet("--foreground", "--preserve-status", "--verbose", "--help", "--version"),
		duration:   true,
	},
	"nice": {valueFlags: wordSet("-n", "--adjustment"), longFlags: wordSet("--help", "--version")},
	"ionice": {
		valueFlags: wordSet("-c", "--class", "-n", "--classdata", "-p", "--pid", "-P", "--pgid",
			"-u", "--uid"),
		longFlags:   wordSet("--ignore", "--help", "--version"),
		targetFlags: wordSet("-p", "--pid", "-P", "--pgid", "-u", "--uid"),
	},
	"setsid": {longFlags: wordSet("--ctty", "--fork", "--wait", "--help", "--version")},
	"stdbuf": {
		valueFlags: wordSet("-i", "--input", "-o", "--output", "-e", "--error"),
		longFlags:  wordSet("--help", "--version"),
	},
	"command": {},
	"builtin": {bareRunsNothing: true},
	"exec":    {valueFlags: wordSet("-a"), bareRunsNothing: true},
	"time": {
		valueFlags:      wordSet("-o", "--output", "-f", "--format"),
		longFlags:       wordSet("--append", "--portability", "--quiet", "--verbose", "--help", "--version"),
		bareRunsNothing: true,
	},
	"chroot": {
		valueFlags: wordSet("--userspec", "--groups", "-u", "-g", "-G"),
		longFlags:  wordSet("--skip-chdir", "--help", "--version"),
		operands:   1,
	},
	"flock": {
		valueFlags: wordSet("-w", "--wait", "--timeout", "-E", "--conflict-exit-code"),
		longFlags: wordSet("--shared", "--exclusive", "--unlock", "--nonblock", "--nonblocking", "--nb", "--close", "--no-fork",
			"--verbose", "--fcntl", "--help", "--version"),
		operands:        1,
		scriptFlags:     wordSet("-c", "--command"),
		bareRunsNothing: true,
	},
	"watch": {
		valueFlags:    wordSet("-n", "--interval", "-q", "--equexit", "-s", "--shotsdir"),
		optionalFlags: wordSet("-d", "--differences"),
		longFlags: wordSet("--beep", "--color", "--no-color", "--errexit", "--chgexit", "--precise",
			"--no-rerun", "--no-title", "--no-wrap", "--exec", "--help", "--version"),
		joinsScript: true,
	},
	"parallel": parallelGrammar,
	// GNU parallel's sem is parallel --semaphore; without a command it only
	// waits.
	"sem": func() prefixWrapper { sem := parallelGrammar; sem.bareRunsNothing = true; return sem }(),
	// polkit's pkexec reads its own options; without a program it runs the
	// target user's shell.
	"pkexec": {
		valueFlags: wordSet("-u", "--user"),
		longFlags:  wordSet("--disable-internal-agent", "--keep-cwd", "--help", "--version"),
	},
	// util-linux taskset runs its command under a CPU mask; -p sets an
	// existing process's instead.
	"taskset": {
		longFlags:   wordSet("--all-tasks", "--cpu-list", "--pid", "--help", "--version"),
		targetFlags: wordSet("-p", "--pid"),
		operands:    1,
	},
	// util-linux nsenter; without a program it runs a shell.
	"nsenter": {
		valueFlags: wordSet("-t", "--target", "-S", "--setuid", "-G", "--setgid", "-W", "--wdns", "-N", "--net-socket"),
		optionalFlags: wordSet("-m", "--mount", "-u", "--uts", "-i", "--ipc", "-n", "--net", "-p", "--pid",
			"-C", "--cgroup", "-U", "--user", "-T", "--time", "-r", "--root", "-w", "--wd"),
		longFlags: wordSet("--all", "--user-parent", "--preserve-credentials", "--keep-caps", "--env",
			"--no-fork", "--join-cgroup", "--follow-context", "--help", "--version"),
	},
	// util-linux runuser -u runs its command directly; without -u runuser
	// reads like su (identifyCommand).
	"runuser": {
		valueFlags: wordSet("-u", "--user", "-c", "--command", "--session-command", "-g", "--group", "-G",
			"--supp-group", "-s", "--shell", "-w", "--whitelist-environment"),
		longFlags: wordSet("--fast", "--login", "--preserve-environment", "--pty", "--help", "--version"),
	},
	// util-linux setpriv; --dump reports and runs nothing.
	"setpriv": {
		valueFlags: wordSet("--ambient-caps", "--inh-caps", "--bounding-set", "--groups", "--rgid", "--egid",
			"--regid", "--ruid", "--euid", "--reuid", "--securebits", "--pdeathsig", "--selinux-label",
			"--apparmor-profile", "--landlock-access", "--landlock-rule", "--seccomp-filter"),
		longFlags: wordSet("--no-new-privs", "--keep-groups", "--clear-groups", "--init-groups", "--reset-env",
			"--dump", "--help", "--version"),
		targetFlags: wordSet("-d", "--dump"),
	},
	// macOS arch: -arch NAME or -NAME selects an architecture, -e sets and
	// -d removes a variable, -c clears the environment. Alone, and as GNU
	// arch, it prints the machine's architecture.
	"arch": {
		valueFlags:      wordSet("-arch", "-d", "-e"),
		assignFlags:     wordSet("-e"),
		wholeWords:      true,
		bareRunsNothing: true,
	},
	// macOS caffeinate; without a utility it only holds its assertions.
	"caffeinate": {valueFlags: wordSet("-t", "-w"), bareRunsNothing: true},
	"busybox": {
		longFlags:            wordSet("--install", "--list", "--list-full", "--help"),
		valueFlags:           wordSet("--show"),
		ownOptionsRunNothing: true,
		bareRunsNothing:      true,
	},
	"toybox": {
		longFlags:            wordSet("--long", "--help", "--version"),
		ownOptionsRunNothing: true,
		bareRunsNothing:      true,
	},
}

// replacementSet is what a wrapper replaces with its input: literal strings
// (`xargs -I {}`) and, for GNU parallel, every replacement string.
type replacementSet struct {
	words  []string
	braces bool
}

// parallelReplacement matches GNU parallel's replacement strings ({}, {.},
// {/}, {#}, {1}, {2.}, {=perl expression=}, ...); a brace group (`{ cmd; }`)
// is not one.
var parallelReplacement = regexp.MustCompile(`\{[^\s{}]*\}|\{=.*=\}`)

func (r replacementSet) empty() bool { return !r.braces && len(r.words) == 0 }

func (r replacementSet) replaces(word string) bool {
	if r.braces && parallelReplacement.MatchString(word) {
		return true
	}
	for _, replacement := range r.words {
		if replacement != "" && strings.Contains(word, replacement) {
			return true
		}
	}
	return false
}

func (r replacementSet) union(other replacementSet) replacementSet {
	if other.empty() {
		return r
	}
	return replacementSet{words: append(append([]string(nil), r.words...), other.words...), braces: r.braces || other.braces}
}

// findCommands lists the commands find runs. A command ends at `;`, which BSD
// find also accepts as the first character of a longer word, and the field
// reading's leftover escaping backslash. -exec and -execdir also end at a `+`
// that follows a word holding `{}` — GNU's rule, a superset of BSD's exact
// `{}` — while anywhere else `+` is an argument (`-exec xargs -E + cmd ;`).
// -ok and -okdir end only at `;`.
func findCommands(args []string) [][]string {
	var commands [][]string
	for i := 0; i < len(args); i++ {
		if _, runs := findCommandPrimaries[args[i]]; !runs {
			continue
		}
		start := i + 1
		i = findCommandEnd(args, i)
		if command := args[start:min(i, len(args))]; len(command) > 0 {
			commands = append(commands, command)
		}
	}
	return commands
}

// findCommandEnd returns the index of the word that ends the command the
// primary at args[primary] runs, or len(args).
func findCommandEnd(args []string, primary int) int {
	plusEnds := args[primary] == "-exec" || args[primary] == "-execdir"
	start := primary + 1
	for i := start; i < len(args); i++ {
		word := args[i]
		if strings.HasPrefix(word, ";") || word == `\` {
			return i
		}
		if plusEnds && word == "+" && i > start && strings.Contains(args[i-1], "{}") {
			return i
		}
	}
	return len(args)
}

// suOptions is util-linux su's grammar. getopt permutes, so its options may
// follow the user; macOS su hands everything after the user to the login
// shell, whose own -c then runs the command.
var suOptions = prefixWrapper{
	valueFlags: wordSet("-c", "--command", "--session-command", "-g", "--group", "-G", "--supp-group",
		"-s", "--shell", "-w", "--whitelist-environment"),
	scriptFlags: wordSet("-c", "--command", "--session-command"),
	longFlags:   wordSet("--fast", "--login", "--preserve-environment", "--pty", "--help", "--version"),
}

// suShellRun reads what su runs. Its operands name a user, never a command,
// and it runs the target's login shell, which may be any shell: util-linux su
// hands it `-c COMMAND` and the words after it, read with the shell's grammar
// (`su root -c -- cmd`) and also joined as one command, since the field
// reading splits a quoted command into several words. Without a -c of its
// own, su hands the words after the login to that shell (macOS su).
func suShellRun(args []string) shellRun {
	for words := args; len(words) > 0; {
		reading := suOptions.wrapperOptions(words)
		for _, option := range reading.options {
			if _, script := suOptions.scriptFlags[option.name]; script && option.hasValue {
				shell := append([]string{"-c", option.value}, reading.rest...)
				run := readShellGrammars(loginShellGrammars, shell)
				if joined := strings.Join(shell[1:], " "); !slices.Contains(run.commands, joined) {
					run.commands = append(run.commands, joined)
				}
				return run
			}
		}
		if len(reading.rest) == 0 {
			break
		}
		words = reading.rest[1:]
	}
	login := suOptions.wrapperOptions(args).rest
	if len(login) == 0 {
		return shellRun{opaque: true}
	}
	return readShellGrammars(loginShellGrammars, login[1:])
}

// runuserRunsCommand reports runuser's -u form, which runs its command
// directly; without -u runuser reads like su.
func runuserRunsCommand(args []string) bool {
	for _, option := range prefixWrappers["runuser"].wrapperOptions(args).options {
		if option.name == "-u" || option.name == "--user" {
			return true
		}
	}
	return false
}

type unwrappedCommand struct {
	inner []string
	// innerAt holds, for each word of inner, the index of the wrapper
	// argument it came from.
	innerAt   []int
	script    string
	hasScript bool
	assigns   []string
	// runsNothing: the wrapper's own options make it run no command
	// (`ionice -p 1234`, `busybox --install`), so nothing is hidden.
	runsNothing bool
	// replaced is what the wrapper replaces with its input in the command.
	replaced replacementSet
}

// wrapperOption is one option a wrapper read, under its canonical spelling.
type wrapperOption struct {
	name     string
	value    string
	hasValue bool
}

// optionReading is what a wrapper's option parser read from its arguments.
type optionReading struct {
	options []wrapperOption
	// rest are the words after the options; at holds, for each, the index of
	// the argument it came from.
	rest []string
	at   []int
	// complete is false when an option's value is missing or a split string
	// is refused, so the wrapper runs nothing.
	complete bool
	// substituted are the ${NAME} references a split string replaces with the
	// variable's value when the command runs.
	substituted []string
}

// maxSplitStrings bounds the split strings one invocation reads
// (`env -S '-S ...'`); beyond it the wrapper is read as running nothing it
// can show.
const maxSplitStrings = 8

// wrapperOptions reads a wrapper's own options the way getopt, getopt_long and
// Getopt::Long do, and returns the words after them. Options end at `--` or at
// the first word that is not one. A bundle of short options (`-iu`) ends at the
// first one that takes a value, which takes the rest of the word or the next
// word; a long option takes `=value` or the next word, and may be abbreviated
// to any unique prefix (`--kill` for `--kill-after`). A script option ends the
// options: what follows belongs to the command line it starts. A split option
// is replaced by the words its value splits into, which are read on as
// options, assignments and the command, each at the argument holding it.
func (w prefixWrapper) wrapperOptions(args []string) optionReading {
	var r optionReading
	at := make([]int, len(args))
	for i := range at {
		at[i] = i
	}
	for i, splits := 0, 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			r.rest, r.at, r.complete = args[i+1:], at[i+1:], true
			return r
		}
		if !strings.HasPrefix(arg, "-") {
			r.rest, r.at, r.complete = args[i:], at[i:], true
			return r
		}
		before := len(r.options)
		if arg == "-" {
			// A lone dash: env's -i, su's -l.
			r.options = append(r.options, wrapperOption{name: "-"})
			continue
		}
		next := func() (string, bool) {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		if strings.HasPrefix(arg, "--") {
			name, value, attached := strings.Cut(arg, "=")
			name = w.longOption(name)
			option := wrapperOption{name: name, value: value, hasValue: attached}
			if !attached {
				_, takesValue := w.valueFlags[name]
				_, optional := w.optionalFlags[name]
				switch {
				case takesValue:
					if option.value, option.hasValue = next(); !option.hasValue {
						return r
					}
				case optional && w.optionalTakesNext && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
					option.value, option.hasValue = next()
				}
			}
			r.options = append(r.options, option)
		} else if w.wholeWords {
			option := wrapperOption{name: arg}
			if _, takesValue := w.valueFlags[arg]; takesValue {
				if option.value, option.hasValue = next(); !option.hasValue {
					return r
				}
			}
			r.options = append(r.options, option)
		} else {
			for j := 1; j < len(arg); j++ {
				name, attached := "-"+arg[j:j+1], arg[j+1:]
				_, takesValue := w.valueFlags[name]
				_, optional := w.optionalFlags[name]
				_, script := w.scriptFlags[name]
				_, path := w.pathFlags[name]
				option := wrapperOption{name: name}
				switch {
				case takesValue && attached != "":
					option.value, option.hasValue = attached, true
				case takesValue:
					if option.value, option.hasValue = next(); !option.hasValue {
						return r
					}
				case optional && attached != "":
					option.value, option.hasValue = attached, true
				case optional && w.optionalTakesNext && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
					option.value, option.hasValue = next()
				case (script || path) && attached != "":
					// A command line or search path written onto the option
					// (`-P/dir`, `flock -c'cmd'`).
					option.value, option.hasValue = attached, true
				default:
					r.options = append(r.options, option)
					continue
				}
				r.options = append(r.options, option)
				break
			}
		}
		if len(r.options) == before || !r.options[len(r.options)-1].hasValue {
			continue
		}
		last := r.options[len(r.options)-1]
		if _, script := w.scriptFlags[last.name]; script {
			r.rest, r.at, r.complete = args[i+1:], at[i+1:], true
			return r
		}
		if _, split := w.splitFlags[last.name]; split {
			words, substituted, ok := splitEnvString(last.value)
			if !ok || splits == maxSplitStrings {
				return r
			}
			splits++
			r.substituted = append(r.substituted, substituted...)
			from := make([]int, len(words), len(words)+len(at)-i-1)
			for k := range from {
				from[k] = at[i]
			}
			args, at, i = append(words, args[i+1:]...), append(from, at[i+1:]...), -1
		}
	}
	r.complete = true
	return r
}

// longOption resolves a long option to its full spelling: an exact name, else
// the one known option it is a prefix of. An ambiguous or unknown name is
// returned unchanged and read as an option without a value.
func (w prefixWrapper) longOption(name string) string {
	if w.knownOption(name) || len(name) < 3 {
		return name
	}
	match := ""
	for _, set := range []map[string]struct{}{w.valueFlags, w.optionalFlags, w.longFlags, w.scriptFlags, w.pathFlags} {
		for option := range set {
			if !strings.HasPrefix(option, "--") || !strings.HasPrefix(option, name) || option == match {
				continue
			}
			if match != "" {
				return name
			}
			match = option
		}
	}
	if match == "" {
		return name
	}
	return match
}

func (w prefixWrapper) knownOption(name string) bool {
	for _, set := range []map[string]struct{}{w.valueFlags, w.optionalFlags, w.longFlags, w.scriptFlags, w.pathFlags} {
		if _, ok := set[name]; ok {
			return true
		}
	}
	return false
}

// unwrap skips a wrapper's own options and operands and returns the command
// it runs.
func (w prefixWrapper) unwrap(args []string) unwrappedCommand {
	var out unwrappedCommand
	reading := w.wrapperOptions(args)
	invocation := w.newInvocation()
	// A split string's ${NAME} is replaced with the variable's value when the
	// command runs.
	invocation.replaced.words = append(invocation.replaced.words, reading.substituted...)
	for _, option := range reading.options {
		if _, script := w.scriptFlags[option.name]; script && option.hasValue {
			out.script = strings.Join(append([]string{option.value}, reading.rest...), " ")
			out.hasScript = true
			return out
		}
		if _, path := w.pathFlags[option.name]; path && option.hasValue {
			out.assigns = append(out.assigns, "PATH="+option.value)
		}
		if _, assigns := w.assignFlags[option.name]; assigns && option.hasValue {
			if _, ok := assignmentName(option.value); ok {
				out.assigns = append(out.assigns, option.value)
			}
		}
		// A lone dash is no option of a multi-call binary's own; the words
		// after it are still read.
		if _, target := w.targetFlags[option.name]; target || (w.ownOptionsRunNothing && option.name != "-") {
			out.runsNothing = true
		}
		invocation.read(w, option)
	}
	if !reading.complete || out.runsNothing {
		return out
	}
	return w.command(reading.rest, reading.at, out, invocation)
}

// wrapperInvocation is what one invocation's options changed about how the
// wrapper reads its command: the strings it replaces with input, and renamed
// terminators.
type wrapperInvocation struct {
	replaced    replacementSet
	terminators map[string]struct{}
	arguments   map[string]struct{}
}

func (w prefixWrapper) newInvocation() wrapperInvocation {
	invocation := wrapperInvocation{
		replaced:    replacementSet{braces: w.replacesBraces},
		terminators: map[string]struct{}{},
		arguments:   map[string]struct{}{},
	}
	for word := range w.terminators {
		invocation.terminators[word] = struct{}{}
	}
	for word := range w.argumentTerminators {
		invocation.arguments[word] = struct{}{}
	}
	return invocation
}

func (v *wrapperInvocation) read(w prefixWrapper, option wrapperOption) {
	if _, replaces := w.replaceFlags[option.name]; replaces {
		replacement := option.value
		if !option.hasValue || replacement == "" {
			replacement = "{}"
		}
		v.replaced.words = append(v.replaced.words, replacement)
	}
	if replaced, renames := w.separatorFlags[option.name]; renames && option.hasValue && option.value != "" {
		_, arguments := v.arguments[replaced]
		for _, word := range []string{replaced, replaced + "+"} {
			delete(v.terminators, word)
			delete(v.arguments, word)
		}
		for _, word := range []string{option.value, option.value + "+"} {
			v.terminators[word] = struct{}{}
			if arguments {
				v.arguments[word] = struct{}{}
			}
		}
	}
}

// maxArgumentCommandLines bounds the command lines read from a wrapper's
// arguments; beyond it the wrapper is opaque.
const maxArgumentCommandLines = 64

// command reads what follows the wrapper's options: its operands, then the
// command, which a script flag after the operands (`flock FILE -c 'cmd'`), a
// terminator (`parallel cmd ::: args`) or a joining wrapper turns into a
// script. Without a command before its first terminator, a joining wrapper
// runs its literal arguments as command lines, one per combination of its
// input sources.
func (w prefixWrapper) command(rest []string, at []int, out unwrappedCommand, invocation wrapperInvocation) unwrappedCommand {
	if len(rest) < w.operands {
		return out
	}
	rest, at = rest[w.operands:], at[w.operands:]
	if w.operands > 0 && len(rest) > 0 {
		if _, script := w.scriptFlags[rest[0]]; script {
			out.script, out.hasScript = strings.Join(rest[1:], " "), len(rest) > 1
			return out
		}
	}
	if w.duration && len(rest) > 0 && isDurationToken(rest[0]) {
		rest, at = rest[1:], at[1:]
	}
	template, sources := rest, []inputSource(nil)
	for i, word := range rest {
		if _, ends := invocation.terminators[word]; ends {
			template, sources = rest[:i], splitSources(rest[i:], invocation)
			break
		}
	}
	out.replaced = invocation.replaced
	if !w.joinsScript {
		out.inner, out.innerAt = template, at[:len(template)]
		return out
	}
	if len(template) == 0 {
		lines, ok := argumentCommandLines(sources)
		out.script, out.hasScript = strings.Join(lines, "\n"), ok && len(lines) > 0
		return out
	}
	// The template as written is read too: a replacement string in its
	// program word stays opaque, and a composed line the shell cannot parse
	// (`{ cmd; } arg`) still shows its commands.
	lines := []string{strings.Join(template, " ")}
	if composed, ok := templateCommandLines(template, sources, invocation.replaced); ok {
		lines = append(lines, composed...)
	}
	out.script, out.hasScript = strings.Join(lines, "\n"), true
	return out
}

// isDurationToken reports whether tok is a DURATION GNU timeout reads before
// its command: a non-negative number C's strtod accepts (5, 1.5, .5, 1e3,
// 0x10, +5, inf) followed by at most one of the suffixes s, m, h and d. It is
// used only to skip timeout's leading argument; a command name never has this
// shape, and timeout refuses to run anything after an invalid DURATION.
func isDurationToken(tok string) bool {
	if strtodNumber(tok) {
		return true
	}
	if n := len(tok); n > 1 && strings.IndexByte("smhd", tok[n-1]) >= 0 {
		return strtodNumber(tok[:n-1])
	}
	return false
}

// strtodNumber reports whether all of s is a non-negative number strtod reads:
// decimal or hexadecimal digits with an optional fraction and exponent, or
// inf/infinity. NaN is left out because timeout rejects it.
func strtodNumber(s string) bool {
	s = strings.TrimPrefix(s, "+")
	switch strings.ToLower(s) {
	case "inf", "infinity":
		return true
	}
	digits, exponentMarks := "0123456789", "eE"
	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s, digits, exponentMarks = s[2:], "0123456789abcdefABCDEF", "pP"
	}
	mantissa := s
	if i := strings.IndexAny(s, exponentMarks); i >= 0 {
		mantissa = s[:i]
		exponent := s[i+1:]
		if strings.HasPrefix(exponent, "+") || strings.HasPrefix(exponent, "-") {
			exponent = exponent[1:]
		}
		if exponent == "" || strings.Trim(exponent, "0123456789") != "" {
			return false
		}
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	return whole+fraction != "" && strings.Trim(whole, digits) == "" && strings.Trim(fraction, digits) == ""
}
