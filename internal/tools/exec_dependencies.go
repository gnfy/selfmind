package tools

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Unlike program discovery, configuration selection needs exact argv values.
// Retain quoted literal boundaries; dynamic words leave selection unknown.
func execLiteralProgramArguments(command string, depth int) (map[string][][]string, bool) {
	arguments := map[string][][]string{}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil || depth > maxWrapperDepth {
		return arguments, true
	}
	unknown := false
	var record func([]string, int)
	record = func(fields []string, level int) {
		if len(fields) == 0 || level > maxWrapperDepth {
			unknown = true
			return
		}
		base := strings.ToLower(filepath.Base(fields[0]))
		if _, shell := shellDashCWrappers[base]; shell {
			if script, found := dashCScript(fields[1:]); found {
				nested, opaque := execLiteralProgramArguments(script, level+1)
				unknown = unknown || opaque
				for name, values := range nested {
					arguments[name] = append(arguments[name], values...)
				}
				return
			}
		}
		if _, wrapper := execPrefixWrappers[base]; wrapper {
			if inner, found := execWrappedCommand(base, fields[1:]); found {
				record(inner, level+1)
				return
			}
		}
		arguments[base] = append(arguments[base], fields)
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok && len(call.Args) > 0 {
			fields := make([]string, 0, len(call.Args))
			for _, word := range call.Args {
				value, literal := staticObservationWord(word)
				if !literal {
					program, known := staticObservationWord(call.Args[0])
					base := strings.ToLower(filepath.Base(program))
					arguments[base] = append(arguments[base], nil)
					_, shell := shellDashCWrappers[base]
					_, wrapper := execPrefixWrappers[base]
					unknown = unknown || !known || shell || wrapper
					return true
				}
				fields = append(fields, value)
			}
			record(fields, depth)
		}
		return true
	})
	return arguments, unknown
}

// Environment analysis shares the command grammar with the safety floor, but
// non-executing interpreter modes don't depend on programs inside a script.
// This must never turn an invocation into a proven read-only or approved call.
func expandEnvironmentSegments(command string, depth int) ([][]string, bool) {
	var segments [][]string
	opaque := false
	for _, part := range splitTopLevelSegments(command) {
		fields := shellFields(part)
		index, ok := segmentProgram(fields)
		if !ok {
			continue
		}
		if index > 0 {
			opaque = true
		}
		fields = fields[index:]
		base := strings.ToLower(filepath.Base(fields[0]))
		if nonExecutingProgram(fields) {
			segments = append(segments, fields)
			continue
		}
		if depth < maxWrapperDepth {
			if _, shell := shellDashCWrappers[base]; shell {
				if script, found := dashCScript(fields[1:]); found {
					nested, unknown := expandEnvironmentSegments(script, depth+1)
					segments = append(segments, nested...)
					opaque = opaque || unknown
					continue
				}
			}
			if _, wrapper := execPrefixWrappers[base]; wrapper {
				if inner, found := execWrappedCommand(base, fields[1:]); found {
					nested, unknown := expandEnvironmentSegments(strings.Join(inner, " "), depth+1)
					segments = append(segments, nested...)
					opaque = opaque || unknown
					continue
				}
			}
		}
		nested, unknown := expandSegment(fields, depth)
		segments = append(segments, nested...)
		opaque = opaque || unknown
	}
	return segments, opaque
}

func nonExecutingProgram(fields []string) bool {
	if len(fields) < 2 {
		return false
	}
	base := strings.ToLower(filepath.Base(fields[0]))
	_, shell := shellDashCWrappers[base]
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
func execEnvironmentOverrides(command string) map[string]bool {
	overrides := map[string]bool{}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		overrides["*"] = true
		return overrides
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if assignment, ok := node.(*syntax.Assign); ok && assignment.Name != nil {
			overrides[assignment.Name.Value] = true
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		words := make([]string, 0, len(call.Args))
		known := true
		for _, word := range call.Args {
			value, literal := staticObservationWord(word)
			known = known && literal
			words = append(words, value)
		}

		base := filepath.Base(words[0])
		if base != "env" && base != "unset" {
			return true
		}
		if !known {
			overrides["*"] = true
			return true
		}
		for i := 1; i < len(words); i++ {
			if base == "unset" {
				if !strings.HasPrefix(words[i], "-") {
					overrides[words[i]] = true
				}
				continue
			}
			if words[i] == "-i" || words[i] == "--ignore-environment" {
				overrides["*"] = true
			}
			if words[i] == "-u" || words[i] == "--unset" {
				if i+1 < len(words) {
					i++
					overrides[words[i]] = true
				}
				continue
			}
			if variable, ok := strings.CutPrefix(words[i], "--unset="); ok {
				overrides[variable] = true
				continue
			}
			if variable, _, ok := strings.Cut(words[i], "="); ok {
				overrides[variable] = true
			}
		}
		return true
	})
	return overrides
}
