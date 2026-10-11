package tools

import (
	"strings"
)

// Unlike program discovery, configuration selection needs exact argv values.
// Retain quoted literal boundaries; dynamic words leave selection unknown.
func execLiteralProgramArguments(command string, depth int) (map[string][][]string, bool) {
	arguments := map[string][][]string{}
	commands, ok := shellCommands(command, depth)
	if !ok {
		return arguments, true
	}
	unknown := false
	for _, read := range commands {
		// A nested script the shell could not parse has no literal boundaries.
		unknown = unknown || read.fallback
		program := read.id.program()
		if program.Word == "" || read.id.HasScript {
			continue
		}
		if !read.anyDynamic() {
			arguments[program.Name] = append(arguments[program.Name], program.argv())
			continue
		}
		// An expanded word leaves this invocation's selection unknown. When it
		// may be the program word, or the word a wrapper runs, the program set
		// is unknown too.
		arguments[program.Name] = append(arguments[program.Name], nil)
		unknown = unknown || read.dynamicWord(program.index) || len(read.id.Layers) > 1
	}
	return arguments, unknown
}

// Environment analysis names programs through identifyCommand, the same
// identification as the safety floor, but non-executing interpreter modes
// don't depend on programs inside a script. This must never turn an
// invocation into a proven read-only or approved call.
//
// The payload is read as the shell parses it. A leading VAR=value changes what
// the program sees, not which program runs, whatever the value: `TZ=UTC date`,
// `KUBECONFIG=/x kubectl ...` and `FOO="a b" aws ...` prepare like the bare
// command. A selector the snapshot cannot resolve (AWS_PROFILE=x aws ...)
// reaches preparation as an environment override instead.
func expandEnvironmentSegments(command string, depth int) ([][]string, bool) {
	var segments [][]string
	opaque := false
	for _, read := range preparationCommands(command, depth) {
		argv := read.id.program().argv()
		if read.id.Opaque && !nonExecutingProgram(argv) {
			opaque = true
		}
		// A command that only assigns runs nothing; one that runs a script is
		// represented by the script's own commands.
		if len(argv) == 0 || read.id.HasScript {
			continue
		}
		segments = append(segments, argv)
	}
	return segments, opaque
}

func nonExecutingProgram(fields []string) bool {
	if len(fields) < 2 {
		return false
	}
	base := programName(fields[0])
	_, shell := shellGrammars[base]
	if !shell && !scriptCarryingPrograms[base] {
		return false
	}
	if len(fields) == 2 && fields[1] == "--version" {
		return true
	}
	// Keep ambiguous/combined switches opaque. This describes dependencies only,
	// not whether startup hooks or the interpreter itself can have effects.
	if shell && fields[1] == "-n" {
		return len(fields) == 2 || (len(fields) == 3 && !strings.HasPrefix(fields[2], "-"))
	}
	return false
}

// Shell-local configuration overrides cannot be resolved from the immutable
// process snapshot. Preserve their state requirements rather than assuming the
// snapshot's default authentication; proxy-only edits don't widen dependencies.
// Overrides count wherever the shell applies them, including inside the
// scripts a command runs.
func execEnvironmentOverrides(command string) map[string]bool {
	overrides := map[string]bool{}
	commands, ok := shellCommands(command, 0)
	if !ok {
		overrides["*"] = true
		return overrides
	}
	for _, read := range commands {
		if read.fallback {
			overrides["*"] = true
			continue
		}
		for _, assignment := range read.id.assignments() {
			if name, ok := assignmentName(assignment); ok {
				overrides[name] = true
			}
		}
		program := read.id.program()
		for _, layer := range read.id.Layers {
			if layer.Name != "env" {
				continue
			}
			if read.anyDynamic() {
				overrides["*"] = true
			}
			for _, option := range prefixWrappers["env"].wrapperOptions(layer.Args).options {
				if option.name == "-" || option.name == "-i" || option.name == "--ignore-environment" {
					overrides["*"] = true
				}
			}
			for _, name := range envUnsetVariables(layer) {
				overrides[name] = true
			}
		}
		if program.Name == "unset" {
			if read.anyDynamic() {
				overrides["*"] = true
			}
			for _, name := range unsetVariables(program.Args) {
				overrides[name] = true
			}
		}
	}
	return overrides
}
