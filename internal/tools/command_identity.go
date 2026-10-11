package tools

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Program identification.
//
// The hard floor, the dangerous-operation heuristic, the observation prover,
// environment dependency analysis and proxy-removal detection all ask
// identifyCommand which program a simple command runs. They used to answer
// separately and the answers drifted: `RM -rf /`, `\rm -rf /`,
// `FOO=a/b rm -rf /` and `exec rm -rf /` passed the floor, while the shell, and
// a case-insensitive filesystem, ran rm.

const (
	// maxWrapperDepth bounds the prefix wrappers peeled off one command
	// (`sudo env nice timeout ...`). Deeper nesting is opaque, never a program.
	maxWrapperDepth = 8
	// maxScriptDepth bounds nested scripts (`sh -c "bash -c '...'"`).
	maxScriptDepth = 4
)

// programName is the program a command word runs, compared the way the shell
// and the filesystem compare it: escapes removed (`\rm` runs rm), path
// qualification stripped (`/bin/rm`), and lowercased, because a
// case-insensitive filesystem runs `RM` and `/BIN/RM` as rm. A Homebrew GNU
// name is the program it installs (`grm` is rm).
func programName(word string) string {
	word = strings.ReplaceAll(word, `\`, "")
	if word == "" {
		return ""
	}
	name := strings.ToLower(path.Base(word))
	if gnu, prefixed := strings.CutPrefix(name, "g"); prefixed {
		if _, installed := homebrewGNUPrograms[gnu]; installed {
			return gnu
		}
	}
	return name
}

// assignmentName reports the variable a NAME=value word assigns. The value
// never decides which word is the program: `PATH=/usr/bin rm` and
// `FOO="a b" rm` both run rm.
func assignmentName(word string) (string, bool) {
	eq := strings.IndexByte(word, '=')
	if eq <= 0 {
		return "", false
	}
	name := strings.TrimSuffix(word[:eq], "+")
	if open := strings.IndexByte(name, '['); open >= 0 {
		if !strings.HasSuffix(name, "]") {
			return "", false
		}
		name = name[:open]
	}
	if name == "" {
		return "", false
	}
	for i, r := range name {
		letter := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return "", false
		}
	}
	return name, true
}

// segmentProgram returns the index of the program word in a segment, skipping
// leading NAME=value assignments of any value. ok=false when the segment only
// assigns.
func segmentProgram(fields []string) (int, bool) {
	for i, field := range fields {
		if _, assignment := assignmentName(field); !assignment {
			return i, true
		}
	}
	return 0, false
}

// declarationBuiltins assign their NAME=value operands in the shell itself.
var declarationBuiltins = map[string]struct{}{
	"export": {}, "declare": {}, "typeset": {}, "readonly": {}, "local": {},
}

// codeInjectionVariables load code into the program that runs next, whatever
// that program is: the dynamic loader's libraries, and the startup files,
// modules and options of interpreters and shells, which a program may itself
// start.
var codeInjectionVariables = map[string]struct{}{
	"LD_PRELOAD": {}, "LD_LIBRARY_PATH": {}, "LD_AUDIT": {},
	"DYLD_INSERT_LIBRARIES": {}, "DYLD_LIBRARY_PATH": {}, "DYLD_FRAMEWORK_PATH": {},
	"DYLD_FALLBACK_LIBRARY_PATH": {}, "DYLD_FALLBACK_FRAMEWORK_PATH": {},
	"NODE_OPTIONS": {}, "PYTHONSTARTUP": {}, "PERL5OPT": {}, "PERL5LIB": {}, "RUBYOPT": {},
	"BASH_ENV": {}, "ZDOTDIR": {}, "GIT_EXEC_PATH": {},
}

// Two variables load code into only one kind of program: PYTHONPATH puts
// modules ahead of Python's standard library, and ENV names the startup file
// an interactive POSIX shell reads. They count when the payload runs such a
// program; `ENV=production node app.js` loads nothing.
const (
	pythonPathVariable   = "PYTHONPATH"
	shellStartupVariable = "ENV"
)

// injectionReaders reports which programs that read a conditional injection
// variable a payload runs.
type injectionReaders struct {
	python, interactiveShell bool
}

func (r injectionReaders) reads(variable string) bool {
	switch variable {
	case pythonPathVariable:
		return r.python
	case shellStartupVariable:
		return r.interactiveShell
	}
	_, injects := codeInjectionVariables[variable]
	return injects
}

// pythonProgram reports a Python interpreter: python, python3.12, pypy3 ...
func pythonProgram(name string) bool {
	return strings.HasPrefix(name, "python") || strings.HasPrefix(name, "pypy")
}

// startupFileShells read the file ENV names when they run interactively.
var startupFileShells = map[string]struct{}{
	"sh": {}, "dash": {}, "ksh": {}, "mksh": {}, "bash": {}, "zsh": {}, "ash": {},
}

// classNeutralWrappers change neither who runs a command, nor its privilege,
// nor how its words are read. A remembered class may look through them; any
// other wrapper (sudo, doas, su, env, xargs, chroot, a shell's -c, ...) leaves
// no class that bounds what runs.
var classNeutralWrappers = map[string]struct{}{
	"exec": {}, "time": {}, "command": {}, "builtin": {}, "nice": {}, "ionice": {},
	"nohup": {}, "timeout": {}, "stdbuf": {},
}

// commandLayer is one program a command passes through: a prefix wrapper on
// the way, or the program that finally runs.
type commandLayer struct {
	// Assigns holds the NAME=value words written before Word; a wrapper's path
	// option (`env -P dir`) is recorded as the PATH assignment it amounts to.
	Assigns []string
	// Word is the program word as written; empty when the command only assigns.
	Word string
	Name string // programName(Word)
	Args []string
	// Inner is the command a wrapper runs: a suffix of Args, led by the words
	// an env -S string splits into.
	Inner []string
	// index is the position of Word in the words identifyCommand was given.
	index int
}

func (l commandLayer) argv() []string {
	if l.Word == "" {
		return nil
	}
	return append([]string{l.Word}, l.Args...)
}

// commandIdentity is one simple command after leading assignments and known
// prefix wrappers are peeled off.
type commandIdentity struct {
	// Layers run from the outermost word to the program that runs; every layer
	// but the last is a prefix wrapper.
	Layers []commandLayer
	// Script is the command line the last layer runs: a shell's command
	// strings, eval's arguments, su -c, flock -c, watch or parallel. A reading
	// parses it as a nested payload.
	Script    string
	HasScript bool
	// Nested are commands the program runs from its own arguments
	// (`find -exec cmd ;`); a reading identifies each like any command.
	Nested [][]string
	// Opaque: what runs is not all visible — a wrapper's command could not be
	// extracted (`bash deploy.sh`, `sudo` alone), a shell also reads a file or
	// its input, or nesting passed its bound. A script the command also runs
	// is still read.
	Opaque bool
	// Replaced holds what a wrapper on the way replaces with its input before
	// the command runs; a program word holding one is chosen at run time.
	Replaced replacementSet
}

func (id commandIdentity) program() commandLayer {
	if len(id.Layers) == 0 {
		return commandLayer{}
	}
	return id.Layers[len(id.Layers)-1]
}

// assignments lists every NAME=value the command applies: before a program,
// as a wrapper operand (`env FOO=1`, `sudo FOO=1`), declared
// (`export FOO=1`), or formatted into a variable (`printf -v FOO value`).
func (id commandIdentity) assignments() []string {
	var out []string
	for _, layer := range id.Layers {
		out = append(out, layer.Assigns...)
	}
	program := id.program()
	if _, declares := declarationBuiltins[program.Name]; declares {
		for _, arg := range program.Args {
			if _, ok := assignmentName(arg); ok {
				out = append(out, arg)
			}
		}
	}
	if program.Name == "printf" {
		if name, value, ok := printfVariable(program.Args); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}

// printfVariable reads `printf -v NAME FORMAT` (or `-vNAME`): the variable it
// assigns and the format, which is the value when it holds no directive.
func printfVariable(args []string) (name, value string, ok bool) {
	if len(args) == 0 || !strings.HasPrefix(args[0], "-v") {
		return "", "", false
	}
	rest := args[1:]
	if name = strings.TrimPrefix(args[0], "-v"); name == "" {
		if len(rest) == 0 {
			return "", "", false
		}
		name, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 {
		value = rest[0]
	}
	return name, value, true
}

// codeInjection names the first variable the command assigns that loads code
// into a program the payload runs; which conditional readers run is decided
// by the caller from the whole payload.
func (id commandIdentity) codeInjection(readers injectionReaders) string {
	for _, assignment := range id.assignments() {
		if name, _ := assignmentName(assignment); readers.reads(name) {
			return name
		}
	}
	return ""
}

// redirectsProgramResolution reports an assignment that decides which code
// runs for a program name: PATH chooses the binary, a code-injection variable
// (PYTHONPATH whatever the program) loads code into it. Nothing named in such
// a command can stand for what runs. An interactive shell, the only reader of
// ENV, is opaque and backs no class anyway.
func (id commandIdentity) redirectsProgramResolution() bool {
	for _, assignment := range id.assignments() {
		if name, _ := assignmentName(assignment); name == "PATH" {
			return true
		}
	}
	return id.codeInjection(injectionReaders{python: true}) != ""
}

// boundsNoClass reports a command a remembered class cannot stand for: a
// wrapper that changes who runs it, its privilege or how its words are read,
// a script, or commands it runs from its own arguments.
func (id commandIdentity) boundsNoClass() bool {
	if id.HasScript || len(id.Nested) > 0 {
		return true
	}
	for _, layer := range id.Layers[:max(len(id.Layers)-1, 0)] {
		if _, neutral := classNeutralWrappers[layer.Name]; !neutral {
			return true
		}
	}
	return false
}

// identifyCommand peels leading assignments and known prefix wrappers off one
// simple command's words and names the program that runs. Assignments are
// skipped whatever their value; wrapper and program names compare through
// programName.
func identifyCommand(words []string) commandIdentity {
	var id commandIdentity
	// at holds the position of each word in the words given; an env -S
	// string's words all sit at the position of the word holding it.
	at, end := make([]int, len(words)), len(words)
	for i := range at {
		at[i] = i
	}
	for {
		i := 0
		for i < len(words) {
			if _, ok := assignmentName(words[i]); !ok {
				break
			}
			i++
		}
		layer := commandLayer{Assigns: append([]string(nil), words[:i]...), index: end}
		if i == len(words) {
			id.Layers = append(id.Layers, layer)
			return id
		}
		layer.Word, layer.Name, layer.Args, layer.index = words[i], programName(words[i]), words[i+1:], at[i]
		// A program word a wrapper on the way replaces with its input is
		// chosen at run time, like `$CMD`.
		id.Opaque = id.Opaque || id.Replaced.replaces(layer.Word)
		_, shell := shellGrammars[layer.Name]
		if shell || layer.Name == "su" || (layer.Name == "runuser" && !runuserRunsCommand(layer.Args)) {
			id.Layers = append(id.Layers, layer)
			var run shellRun
			if shell {
				run = readShell(layer.Name, layer.Args)
			} else {
				run = suShellRun(layer.Args)
			}
			id.Script, id.HasScript = run.script()
			id.Opaque = id.Opaque || run.opaque
			return id
		}
		if layer.Name == "find" {
			id.Layers = append(id.Layers, layer)
			id.Nested = findCommands(layer.Args)
			return id
		}
		if layer.Name == "eval" {
			id.Layers = append(id.Layers, layer)
			if len(layer.Args) > 0 {
				id.Script, id.HasScript = strings.Join(layer.Args, " "), true
			}
			return id
		}
		wrapper, ok := prefixWrappers[layer.Name]
		if !ok {
			id.Layers = append(id.Layers, layer)
			return id
		}
		if len(id.Layers)+1 >= maxWrapperDepth {
			id.Layers = append(id.Layers, layer)
			id.Opaque = true
			return id
		}
		unwrapped := wrapper.unwrap(layer.Args)
		layer.Assigns = append(layer.Assigns, unwrapped.assigns...)
		layer.Inner = unwrapped.inner
		id.Layers = append(id.Layers, layer)
		id.Replaced = id.Replaced.union(unwrapped.replaced)
		switch {
		case unwrapped.hasScript:
			id.Script, id.HasScript = unwrapped.script, true
			return id
		case len(unwrapped.inner) == 0:
			id.Opaque = id.Opaque || !(wrapper.bareRunsNothing || unwrapped.runsNothing)
			return id
		}
		inner := make([]int, len(unwrapped.inner))
		for k, arg := range unwrapped.innerAt {
			inner[k] = at[i+1+arg]
		}
		at, words = inner, unwrapped.inner
	}
}

// readCommand is one simple command as a reading found it.
type readCommand struct {
	id commandIdentity
	// dynamic marks the words the shell expands at run time, aligned with the
	// words identifyCommand read. The field reading cannot tell, so it leaves
	// it nil.
	dynamic []bool
	// fallback: read by fields because the shell could not parse it, or
	// recovered from another program's arguments (`find -exec`) without the
	// shell's word boundaries.
	fallback bool
	// writes are the files the shell opens for writing for this statement
	// (`> /dev/sda`), named as the shell passes them.
	writes []string
}

func (c readCommand) dynamicWord(index int) bool {
	return index < len(c.dynamic) && c.dynamic[index]
}

func (c readCommand) anyDynamic() bool {
	for _, dynamic := range c.dynamic {
		if dynamic {
			return true
		}
	}
	return false
}

// shellCommands reads payload the way the shell parses it: every simple
// command, including those inside control structures, substitutions and
// functions, with its assignments separated from its program word and the
// files its statement writes, plus the commands of every script a command
// runs. ok=false when payload is not valid shell.
func shellCommands(payload string, depth int) ([]readCommand, bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(payload), "")
	if err != nil {
		return nil, false
	}
	var commands []readCommand
	syntax.Walk(file, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		read := readCommand{writes: shellWriteTargets(stmt.Redirs, payload)}
		var words []string
		add := func(word string, isDynamic bool) {
			words = append(words, word)
			read.dynamic = append(read.dynamic, isDynamic)
		}
		switch command := stmt.Cmd.(type) {
		case *syntax.CallExpr:
			for _, assign := range command.Assigns {
				add(shellAssignWord(assign, payload))
			}
			for _, word := range command.Args {
				add(shellWord(word, payload))
			}
		case *syntax.DeclClause:
			add(command.Variant.Value, false)
			for _, assign := range command.Args {
				add(shellAssignWord(assign, payload))
			}
		}
		if len(words) > 0 || len(read.writes) > 0 {
			read.id = identifyCommand(words)
			// A program name decided at run time (`$CMD`, `$(printf rm)`,
			// `$'\x72m'`) cannot be identified, so the command is opaque.
			read.id.Opaque = read.id.Opaque || read.dynamicWord(read.id.program().index)
			commands = appendWithScript(commands, read, depth, true)
		}
		return true
	})
	return commands, true
}

// shellWriteTargets lists the files a statement's redirections open for
// writing. `>&2` duplicates a descriptor; `>&file` writes the file.
func shellWriteTargets(redirs []*syntax.Redirect, src string) []string {
	var targets []string
	for _, redir := range redirs {
		if redir == nil || redir.Word == nil {
			continue
		}
		switch redir.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.RdrInOut, syntax.DplOut, syntax.RdrClob, syntax.AppClob,
			syntax.RdrAll, syntax.RdrAllClob, syntax.AppAll, syntax.AppAllClob:
		default:
			continue
		}
		target, _ := shellWord(redir.Word, src)
		if redir.Op == syntax.DplOut && (target == "-" || strings.Trim(target, "0123456789") == "") {
			continue
		}
		targets = append(targets, target)
	}
	return targets
}

// fieldCommands is the conservative reading: separators split even inside
// quotes and quote characters are dropped, so a command embedded in a quoted
// string or in code (`os.system('rm -rf /')`) still surfaces as its own
// command. It cannot keep a quoted value in one word, so it never stands alone
// where the shell's own reading is available.
func fieldCommands(payload string, depth int) []readCommand {
	var commands []readCommand
	for _, segment := range splitTopLevelSegments(payload) {
		if fields := shellFields(segment); len(fields) > 0 {
			commands = appendWithScript(commands, readCommand{id: identifyCommand(fields), fallback: true}, depth, false)
		}
	}
	return commands
}

// appendWithScript adds a command and, when it runs a script or commands of
// its own, those commands read the same way, even when it also runs code no
// argument shows (`bash --rcfile FILE -ic 'cmd'`). A nested script the shell
// cannot parse is read by fields.
func appendWithScript(commands []readCommand, command readCommand, depth int, shell bool) []readCommand {
	deep := (command.id.HasScript || len(command.id.Nested) > 0) && depth >= maxScriptDepth
	command.id.Opaque = command.id.Opaque || deep
	commands = append(commands, command)
	if deep {
		return commands
	}
	start := len(commands)
	for _, nested := range command.id.Nested {
		id := identifyCommand(nested)
		// A program find runs from an expansion, or the found file itself
		// (`-exec {} ;`), cannot be identified.
		if word := id.program().Word; strings.ContainsAny(word, "$`") || strings.Contains(word, "{}") {
			id.Opaque = true
		}
		commands = appendWithScript(commands, readCommand{id: id, fallback: true}, depth+1, shell)
	}
	if command.id.HasScript {
		nested, ok := []readCommand(nil), false
		if shell {
			nested, ok = shellCommands(command.id.Script, depth+1)
		}
		if !ok {
			nested = fieldCommands(command.id.Script, depth+1)
		}
		commands = append(commands, nested...)
	}
	// A string a wrapper replaces with its input (`xargs -I {} sh -c '{}'`,
	// `parallel {1}`) decides every program word it reaches below it.
	if !command.id.Replaced.empty() {
		for i := start; i < len(commands); i++ {
			commands[i].id.Replaced = command.id.Replaced.union(commands[i].id.Replaced)
			commands[i].id.Opaque = commands[i].id.Opaque || commands[i].id.Replaced.replaces(commands[i].id.program().Word)
		}
	}
	return commands
}

// effectiveCommands reads payload both ways. The safety floor and the
// dangerous heuristic over-approximate, so a command either reading finds is
// classified: the shell's reading keeps quoted assignment values in one word
// and sees into control structures, while the field reading still finds a
// command quoted inside code.
func effectiveCommands(payload string) []readCommand {
	shell, _ := shellCommands(payload, 0)
	return append(shell, fieldCommands(payload, 0)...)
}

// preparationCommands reads payload as the shell parses it, and by fields only
// when it does not parse. Execution preparation must follow exactly what
// runs: a split quoted value would invent programs and lose real ones.
func preparationCommands(payload string, depth int) []readCommand {
	if commands, ok := shellCommands(payload, depth); ok {
		return commands
	}
	return fieldCommands(payload, depth)
}

// shellWord is the text a word contributes to identification: the value the
// shell passes, except that a part expanded at run time keeps its source text,
// so `"$HOME"` reads as $HOME and a double-quoted -c script keeps the quoting
// its own commands need. dynamic reports any expanded part.
func shellWord(word *syntax.Word, src string) (string, bool) {
	if value, ok := staticObservationWord(word); ok {
		return value, false
	}
	var b strings.Builder
	for _, part := range word.Parts {
		switch value := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescapeShellWord(value.Value, false))
		case *syntax.SglQuoted:
			if value.Dollar {
				b.WriteString(rawSource(value, src))
				continue
			}
			b.WriteString(value.Value)
		case *syntax.DblQuoted:
			for _, inner := range value.Parts {
				if lit, ok := inner.(*syntax.Lit); ok {
					b.WriteString(unescapeShellWord(lit.Value, true))
					continue
				}
				b.WriteString(rawSource(inner, src))
			}
		default:
			b.WriteString(rawSource(part, src))
		}
	}
	return b.String(), true
}

func rawSource(node syntax.Node, src string) string {
	start, end := int(node.Pos().Offset()), int(node.End().Offset())
	if start < 0 || end > len(src) || start > end {
		return ""
	}
	return src[start:end]
}

func shellAssignWord(assign *syntax.Assign, src string) (string, bool) {
	if assign.Naked {
		if assign.Name != nil {
			return assign.Name.Value, false
		}
		if assign.Value != nil {
			return shellWord(assign.Value, src)
		}
		return "", false
	}
	if assign.Name == nil {
		return rawSource(assign, src), true
	}
	op := "="
	if assign.Append {
		op = "+="
	}
	switch {
	case assign.Value != nil:
		value, dynamic := shellWord(assign.Value, src)
		return assign.Name.Value + op + value, dynamic
	case assign.Array != nil:
		return assign.Name.Value + op + rawSource(assign.Array, src), true
	}
	return assign.Name.Value + op, false
}

// expandCommandSegments lists the argv of each command the field reading
// finds, wrappers peeled. unparsed reports that these segments cannot stand
// for what runs, in either reading: a wrapper's command was not extracted, an
// assignment redirects which program runs, or a wrapper changes who runs the
// command, its privilege or how it is read (sudo, env, sh -c, find -exec).
// Rule derivation (remembered classes, prefix and host rules, declared
// bundles) fails closed on it.
func expandCommandSegments(cmd string, depth int) (segs [][]string, unparsed bool) {
	for _, command := range fieldCommands(cmd, depth) {
		unparsed = unparsed || command.id.Opaque || command.id.redirectsProgramResolution() || command.id.boundsNoClass()
		if argv := command.id.program().argv(); len(argv) > 0 && !command.id.HasScript {
			segs = append(segs, argv)
		}
	}
	shell, _ := shellCommands(cmd, depth)
	for _, command := range shell {
		unparsed = unparsed || command.id.Opaque || command.id.redirectsProgramResolution() || command.id.boundsNoClass()
	}
	return segs, unparsed
}

// splitTopLevelSegments splits s on shell separators (; | & newline backtick
// parens). It is deliberately NOT quote-aware: a separator inside quotes still
// splits, which is strictly more CONSERVATIVE for approval classification (a
// command hidden after an in-quote `;` still surfaces as its own segment) and,
// combined with the quote-stripping in shellFields, lets the classifier see a
// shell command embedded inside a code payload — e.g. execute_code running
// `os.system('rm -rf /')`, where the parens/quotes are just delimiters.
func splitTopLevelSegments(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ';', '|', '&', '\n', '`', '(', ')':
			return true
		}
		return false
	})
}

// shellFields strips shell quote characters and splits the segment on
// whitespace. Real shells GROUP a quoted span into one argument; splitting it
// here can surface a command quoted inside code, and a shell's reading rejoins
// a split command string. It also splits a quoted assignment value, which is
// why the shell's own reading is consulted wherever it parses.
func shellFields(seg string) []string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\'' || r == '"' {
			return -1
		}
		return r
	}, seg)
	return strings.Fields(cleaned)
}
