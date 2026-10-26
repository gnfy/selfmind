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
	bindings := make(map[string]string)
	commands := make([][]string, 0, len(file.Stmts))
	for _, stmt := range file.Stmts {
		part, ok := observationStmtCommands(stmt, depth, bindings)
		if !ok {
			return nil, false
		}
		commands = append(commands, part...)
	}
	return commands, len(commands) > 0
}

func observationStmtCommands(stmt *syntax.Stmt, depth int, bindings map[string]string) ([][]string, bool) {
	if stmt == nil || stmt.Cmd == nil || stmt.Negated || stmt.Background || stmt.Coprocess || stmt.Disown || !observationRedirectionsSafe(stmt.Redirs) {
		return nil, false
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		if len(cmd.Args) == 0 && len(cmd.Assigns) > 0 {
			if !recordObservationBindings(cmd.Assigns, bindings) {
				return nil, false
			}
			return nil, true
		}
		if len(cmd.Assigns) != 0 || len(cmd.Args) == 0 {
			return nil, false
		}
		argv := make([]string, 0, len(cmd.Args))
		for i, word := range cmd.Args {
			var (
				value string
				ok    bool
			)
			if i == 0 {
				value, ok = staticObservationWord(word)
			} else {
				argBindings := bindings
				if (argv[0] == "echo" || argv[0] == "printf") && argBindings["?"] == "" {
					argBindings = cloneObservationBindings(bindings)
					argBindings["?"] = "0"
				}
				value, ok = staticObservationWordWithBindings(word, argBindings)
			}
			if !ok {
				return nil, false
			}
			argv = append(argv, value)
		}
		if argv[0] == "unset" {
			for _, arg := range argv[1:] {
				if !strings.HasPrefix(arg, "-") {
					delete(bindings, arg)
				}
			}
		}
		return observationArgvCommands(argv, depth)
	case *syntax.BinaryCmd:
		if cmd.Op == syntax.AndStmt {
			left, ok := observationStmtCommands(cmd.X, depth, bindings)
			if !ok {
				return nil, false
			}
			right, ok := observationStmtCommands(cmd.Y, depth, bindings)
			if !ok {
				return nil, false
			}
			return append(left, right...), true
		}
		left, ok := observationStmtCommands(cmd.X, depth, cloneObservationBindings(bindings))
		if !ok {
			return nil, false
		}
		right, ok := observationStmtCommands(cmd.Y, depth, cloneObservationBindings(bindings))
		if !ok {
			return nil, false
		}
		return append(left, right...), true
	case *syntax.ForClause:
		if loopHasRemoteObservation(cmd.Do) {
			if !loopObservesIteratorWithBindings(cmd, bindings) {
				return nil, false
			}
		} else if !loopObservesIteratorWithBindings(cmd, bindings) && !localLoopObservesIteratorWithBindings(cmd, depth, bindings) {
			return nil, false
		}
		iter, ok := cmd.Loop.(*syntax.WordIter)
		if !ok || iter.Name == nil || len(iter.Items) == 0 {
			return nil, false
		}
		var commands [][]string
		for _, item := range iter.Items {
			itemVal, ok := staticObservationWordWithBindings(item, bindings)
			if !ok {
				return nil, false
			}
			iterBindings := cloneObservationBindings(bindings)
			iterBindings[iter.Name.Value] = itemVal
			for _, bodyStmt := range cmd.Do {
				part, ok := observationStmtCommands(bodyStmt, depth+1, iterBindings)
				if !ok {
					return nil, false
				}
				commands = append(commands, part...)
			}
		}
		return commands, len(commands) > 0
	default:
		return nil, false
	}
}

func localLoopObservesIteratorWithBindings(loop *syntax.ForClause, depth int, outer map[string]string) bool {
	if loop == nil || loop.Select || stmtsWait(loop.Do) || !finiteLiteralForLoopWithBindings(loop, outer) {
		return false
	}
	iter, ok := loop.Loop.(*syntax.WordIter)
	if !ok || !iter.InPos.IsValid() || iter.Name == nil || unsafeObservationVarName(iter.Name.Value) || len(iter.Items) == 0 {
		return false
	}
	seenItems := make(map[string]struct{}, len(iter.Items))
	seenCmdLists := make(map[string]struct{}, len(iter.Items))
	for _, item := range iter.Items {
		itemVal, ok := staticObservationWordWithBindings(item, outer)
		if !ok {
			return false
		}
		if _, dup := seenItems[itemVal]; dup {
			return false
		}
		seenItems[itemVal] = struct{}{}
		iterBindings := cloneObservationBindings(outer)
		iterBindings[iter.Name.Value] = itemVal
		var iterCommands [][]string
		for _, bodyStmt := range loop.Do {
			part, ok := observationStmtCommands(bodyStmt, depth+1, iterBindings)
			if !ok {
				return false
			}
			iterCommands = append(iterCommands, part...)
		}
		if len(iterCommands) == 0 {
			return false
		}
		var sig strings.Builder
		for _, argv := range iterCommands {
			sig.WriteString(strings.Join(argv, "\x00"))
			sig.WriteByte('\x01')
		}
		key := sig.String()
		if _, dup := seenCmdLists[key]; dup {
			return false
		}
		seenCmdLists[key] = struct{}{}
	}
	return len(seenCmdLists) == len(iter.Items)
}

// observationShells are the shells whose -c script the bash parser reads
// faithfully enough to prove.
var observationShells = map[string]struct{}{"sh": {}, "bash": {}, "zsh": {}, "dash": {}}

// observationArgvCommands proves the program identifyCommand names, the same
// program every other analysis names. Each wrapper on the way must also pass
// the narrow proof grammar, and that grammar must reach the same inner
// command; any disagreement is unprovable.
func observationArgvCommands(argv []string, depth int) ([][]string, bool) {
	if depth > maxObservationShellDepth || len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, false
	}
	id := identifyCommand(argv)
	last := len(id.Layers) - 1
	// An assignment can choose which binary runs (PATH) or load code into it
	// (LD_PRELOAD), including one made by a builtin the catalog would
	// otherwise accept (`command export PATH=…`, `printf -v PATH …`); none
	// keeps read-only proof.
	if id.Opaque || len(id.Nested) > 0 || depth+last > maxObservationShellDepth || len(id.assignments()) > 0 {
		return nil, false
	}
	for i, layer := range id.Layers {
		if layer.Word == "" {
			return nil, false
		}
		if i == last {
			break
		}
		// Proof vouches for the program a wrapper's exact name resolves to. A
		// path runs whatever file is there, and a case-sensitive filesystem may
		// hold a different `Env` or `TIMEOUT`.
		if layer.Word != layer.Name {
			return nil, false
		}
		inner, ok := observationWrapperInner(layer)
		if !ok || !sameWords(inner, layer.Inner) {
			return nil, false
		}
	}
	program := id.Layers[last]
	if !id.HasScript {
		return [][]string{program.argv()}, true
	}
	if _, shell := observationShells[program.Name]; !shell || program.Word != program.Name {
		return nil, false
	}
	script, ok := exactShellCommand(program.Name, program.Args)
	if !ok || script != id.Script {
		return nil, false
	}
	return parseObservationScript(script, depth+last+1)
}

// observationWrapperInner applies the proof grammar of the wrappers whose
// effect on which program runs is fully known. Privilege wrappers, xargs,
// exec, nice and the rest are intentionally absent: even when their visible
// argv looks read-only, their effective execution authority or command source
// is not bounded tightly enough for automatic approval.
func observationWrapperInner(layer commandLayer) ([]string, bool) {
	switch layer.Name {
	case "env":
		inner, _, ok := proxyEnvCommand(layer.Args)
		return inner, ok
	case "command", "timeout":
		return observationWrappedCommand(layer.Name, layer.Args)
	}
	return nil, false
}

func sameWords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// staticObservationWord is the value the shell passes for a word that needs no
// expansion. The parser keeps escapes as written, so they are removed here as
// the shell removes them: an analysis that read `-\o` or `-\X` literally would
// miss the `-o` and `-X` the program receives.
func staticObservationWord(word *syntax.Word) (string, bool) {
	return staticObservationWordWithBindings(word, nil)
}

func staticObservationWordWithBindings(word *syntax.Word, bindings map[string]string) (string, bool) {
	if word == nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range word.Parts {
		switch value := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescapeShellWord(value.Value, false))
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
				switch elem := inner.(type) {
				case *syntax.Lit:
					b.WriteString(unescapeShellWord(elem.Value, true))
				case *syntax.ParamExp:
					resolved, ok := resolveObservationParam(elem, bindings)
					if !ok {
						return "", false
					}
					b.WriteString(resolved)
				default:
					return "", false
				}
			}
		case *syntax.ParamExp:
			resolved, ok := resolveObservationParam(value, bindings)
			if !ok || strings.IndexAny(resolved, " \t\r\n*?[]") >= 0 {
				return "", false
			}
			b.WriteString(resolved)
		default:
			return "", false
		}
	}
	return b.String(), true
}

func recordObservationBindings(assigns []*syntax.Assign, bindings map[string]string) bool {
	if bindings == nil || len(assigns) == 0 {
		return false
	}
	for _, assign := range assigns {
		if assign == nil || assign.Append || assign.Naked || assign.Index != nil || assign.Array != nil || assign.Name == nil || assign.Value == nil {
			return false
		}
		name := assign.Name.Value
		if unsafeObservationVarName(name) {
			return false
		}
		val, ok := staticObservationWordWithBindings(assign.Value, bindings)
		if !ok || strings.IndexAny(val, "\r\n\x00") >= 0 {
			return false
		}
		bindings[name] = val
	}
	return true
}

func resolveObservationParam(param *syntax.ParamExp, bindings map[string]string) (string, bool) {
	if len(bindings) == 0 || param == nil || param.Excl || param.Length || param.Width ||
		param.Index != nil || param.Slice != nil || param.Repl != nil || param.Param == nil {
		return "", false
	}
	if param.Exp == nil {
		val, ok := bindings[param.Param.Value]
		return val, ok
	}
	return fanoutLiteral(&syntax.Word{Parts: []syntax.WordPart{param}}, bindings)
}

func unsafeObservationVarName(name string) bool {
	if name == "" {
		return true
	}
	upper := strings.ToUpper(name)
	switch upper {
	case "PATH", "IFS", "ENV", "BASH_ENV", "CDPATH", "PROMPT_COMMAND", "SHELLOPTS", "BASHOPTS", "GLOBIGNORE", "HOME", "PWD", "OLDPWD", "KUBECONFIG":
		return true
	}
	for _, prefix := range []string{"LD_", "DYLD_", "AWS_", "GOOGLE_", "CLOUDSDK_", "GH_", "GITHUB_", "GIT_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func cloneObservationBindings(src map[string]string) map[string]string {
	if len(src) == 0 {
		return make(map[string]string)
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// unescapeShellWord removes the backslashes the shell consumes. Unquoted, a
// backslash makes the next character literal; inside double quotes it does so
// only before $, `, ", \ and newline. A backslash-newline joins lines.
func unescapeShellWord(value string, doubleQuoted bool) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			next := value[i+1]
			if !doubleQuoted || strings.IndexByte("$`\"\\\n", next) >= 0 {
				i++
				if next != '\n' {
					b.WriteByte(next)
				}
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}
