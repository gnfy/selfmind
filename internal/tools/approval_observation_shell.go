package tools

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const maxObservationShellDepth = 3

// parseObservationCommands is a deliberately narrow proof parser. It accepts
// static simple commands, pipelines, &&/|| chains, quoted literal arguments,
// and output suppression to /dev/null. Any construct whose executed text or
// filesystem effect depends on runtime expansion is rejected.
func parseObservationCommands(payload string) ([][]string, bool) {
	return parseObservationScript(payload, 0)
}

func parseObservationScript(payload string, depth int) ([][]string, bool) {
	if depth > maxObservationShellDepth || strings.TrimSpace(payload) == "" {
		return nil, false
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(payload), "")
	if err != nil || len(file.Stmts) == 0 {
		return nil, false
	}
	commands := make([][]string, 0, len(file.Stmts))
	for _, stmt := range file.Stmts {
		part, ok := observationStmtCommands(stmt, depth)
		if !ok {
			return nil, false
		}
		commands = append(commands, part...)
	}
	return commands, len(commands) > 0
}

func observationStmtCommands(stmt *syntax.Stmt, depth int) ([][]string, bool) {
	if stmt == nil || stmt.Cmd == nil || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown || !observationRedirectionsSafe(stmt.Redirs) {
		return nil, false
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		if len(cmd.Assigns) != 0 || len(cmd.Args) == 0 {
			return nil, false
		}
		argv := make([]string, 0, len(cmd.Args))
		for _, word := range cmd.Args {
			value, ok := staticObservationWord(word)
			if !ok {
				return nil, false
			}
			argv = append(argv, value)
		}
		return observationArgvCommands(argv, depth)
	case *syntax.BinaryCmd:
		left, ok := observationStmtCommands(cmd.X, depth)
		if !ok {
			return nil, false
		}
		right, ok := observationStmtCommands(cmd.Y, depth)
		if !ok {
			return nil, false
		}
		return append(left, right...), true
	default:
		return nil, false
	}
}

func observationArgvCommands(argv []string, depth int) ([][]string, bool) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, false
	}
	program := strings.ToLower(filepath.Base(argv[0]))
	if _, shell := shellDashCWrappers[program]; shell {
		script, ok := exactDashCScript(argv[1:])
		if !ok {
			return nil, false
		}
		return parseObservationScript(script, depth+1)
	}
	if program == "timeout" || program == "command" {
		inner, ok := observationWrappedCommand(program, argv[1:])
		if !ok {
			return nil, false
		}
		return observationArgvCommands(inner, depth+1)
	}
	// Privilege wrappers and xargs are intentionally absent: even when their
	// visible argv looks read-only, their effective execution authority or
	// command source is not bounded tightly enough for automatic approval.
	return [][]string{argv}, true
}

// observationWrappedCommand accepts only wrapper forms whose effect on command
// resolution is fully known. In particular, env assignments are not accepted:
// `env PATH=/tmp git status` could execute an attacker-controlled program while
// presenting a catalogued command name to the proof layer.
func observationWrappedCommand(wrapper string, args []string) ([]string, bool) {
	switch wrapper {
	case "command":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return nil, false
		}
		return args, true
	case "timeout":
		for len(args) > 0 {
			switch args[0] {
			case "--":
				args = args[1:]
				if len(args) < 2 || !isDurationToken(args[0]) {
					return nil, false
				}
				return args[1:], true
			case "-k", "--kill-after", "-s", "--signal":
				if len(args) < 2 {
					return nil, false
				}
				args = args[2:]
			case "-v", "--verbose", "--foreground", "--preserve-status":
				args = args[1:]
			default:
				if strings.HasPrefix(args[0], "-") || !isDurationToken(args[0]) || len(args) < 2 {
					return nil, false
				}
				return args[1:], true
			}
		}
	}
	return nil, false
}

func exactDashCScript(args []string) (string, bool) {
	for i, arg := range args {
		isDashC := arg == "-c" || arg == "--command" ||
			(strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 1 && strings.HasSuffix(arg, "c"))
		if !isDashC {
			continue
		}
		// Extra positional shell parameters make the effective expansion of $0,
		// $1, ... less obvious. The static proof stays conservative and rejects
		// them rather than reconstructing shell parameter semantics.
		if i+1 >= len(args) || i+2 != len(args) {
			return "", false
		}
		return args[i+1], strings.TrimSpace(args[i+1]) != ""
	}
	return "", false
}

func observationRedirectionsSafe(redirs []*syntax.Redirect) bool {
	for _, redir := range redirs {
		if redir == nil {
			return false
		}
		switch redir.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.AppClob,
			syntax.RdrAll, syntax.RdrAllClob, syntax.AppAll, syntax.AppAllClob:
			target, ok := staticObservationWord(redir.Word)
			if !ok || filepath.ToSlash(target) != "/dev/null" {
				return false
			}
		case syntax.DplOut:
			target, ok := staticObservationWord(redir.Word)
			if !ok || (target != "1" && target != "2" && target != "-") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func staticObservationWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range word.Parts {
		switch value := part.(type) {
		case *syntax.Lit:
			b.WriteString(value.Value)
		case *syntax.SglQuoted:
			if value.Dollar {
				return "", false
			}
			b.WriteString(value.Value)
		case *syntax.DblQuoted:
			if value.Dollar {
				return "", false
			}
			for _, inner := range value.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}
