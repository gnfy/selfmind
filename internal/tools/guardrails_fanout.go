package tools

import (
	"encoding/json"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// A dependency on the iterator is not proof of a different query: trimming
// two items can produce the same value, and assignments in a subshell do not
// update its parent. Evaluate only straight-line scalar bindings of a bounded
// literal list. This never runs commands, reads files or inherits host env.
// Unsupported control flow or expansions keep the polling redirect.
func loopObservesIterator(clause *syntax.ForClause) bool {
	return loopObservesIteratorWithBindings(clause, nil)
}

func loopObservesIteratorWithBindings(clause *syntax.ForClause, outer map[string]string) bool {
	iter, ok := clause.Loop.(*syntax.WordIter)
	if !ok || iter.Name == nil || !finiteLiteralForLoopWithBindings(clause, outer) {
		return false
	}
	baseOuter := cleanOuterBindingsForLoop(clause, outer)
	seen := map[string]bool{}
	for _, item := range iter.Items {
		fields, known := fanoutFields([]*syntax.Word{item}, baseOuter)
		if !known || len(fields) != 1 {
			return false
		}
		values := cloneObservationBindings(baseOuter)
		values[iter.Name.Value] = fields[0]
		for _, stmt := range clause.Do {
			if !fanoutStatement(stmt, values, seen, false) {
				return false
			}
		}
	}
	if len(iter.Items) == 1 && len(baseOuter) > 0 {
		values := cloneObservationBindings(baseOuter)
		values[iter.Name.Value] = "__fanout_iter_probe__"
		for _, stmt := range clause.Do {
			if !fanoutStatement(stmt, values, seen, false) {
				return false
			}
		}
	}
	return len(seen) > 0
}

func cleanOuterBindingsForLoop(clause *syntax.ForClause, outer map[string]string) map[string]string {
	if len(outer) == 0 || clause == nil {
		return nil
	}
	out := make(map[string]string, len(outer))
	for k, v := range outer {
		if k == "" || k == "IFS" || unsafeObservationVarName(k) {
			continue
		}
		out[k] = v
	}
	if iter, ok := clause.Loop.(*syntax.WordIter); ok && iter.Name != nil {
		delete(out, iter.Name.Value)
	}
	for _, stmt := range clause.Do {
		invalidateOuterAssignedVars(stmt, out)
	}
	return out
}

func fanoutStatement(stmt *syntax.Stmt, values map[string]string, seen map[string]bool, pipeline bool) bool {
	if stmt == nil || stmt.Negated || stmt.Background || stmt.Coprocess {
		return false
	}
	for _, redir := range stmt.Redirs {
		if redir.Hdoc != nil || (redir.Word != nil && !fanoutWordKnown(redir.Word, values)) {
			return false
		}
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:
		// A pipeline or short-circuit chain is allowed for presentation and
		// multi-command inspection, but its branches cannot supply scalar
		// bindings to the enclosing shell.
		return (cmd.Op == syntax.Pipe || cmd.Op == syntax.PipeAll || cmd.Op == syntax.AndStmt || cmd.Op == syntax.OrStmt) &&
			fanoutStatement(cmd.X, values, seen, true) && fanoutStatement(cmd.Y, values, seen, true)
	case *syntax.DeclClause:
		if pipeline || (cmd.Variant.Value != "export" && cmd.Variant.Value != "declare" && cmd.Variant.Value != "typeset") {
			return false
		}
		for _, assign := range cmd.Args {
			if assign.Name != nil && assign.Name.Value == "IFS" {
				return false
			}
		}
		return fanoutAssignments(cmd.Args, values, seen)
	case *syntax.CallExpr:
		if len(cmd.Args) == 0 {
			for _, assign := range cmd.Assigns {
				if assign.Name != nil && assign.Name.Value == "IFS" {
					return false
				}
			}
			return !pipeline && fanoutAssignments(cmd.Assigns, values, seen)
		}
		name, static := staticObservationWord(cmd.Args[0])
		if !static {
			return false
		}
		if name == "read" {
			return !pipeline && fanoutRead(stmt, cmd, values)
		}
		if name == "set" {
			if pipeline || len(cmd.Assigns) != 0 || len(cmd.Args) < 2 {
				return false
			}
			flag, _ := staticObservationWord(cmd.Args[1])
			args, ok := fanoutFields(cmd.Args[2:], values)
			if flag != "--" || !ok || len(args) > 9 {
				return false
			}
			for i := 1; i <= 9; i++ {
				delete(values, strconv.Itoa(i))
			}
			for i, value := range args {
				values[strconv.Itoa(i+1)] = value
			}
			return true
		}
		if len(cmd.Assigns) != 0 {
			return false
		}
		args, ok := fanoutFields(cmd.Args, values)
		if !ok {
			if (name == "echo" || (name == "printf" && !printfMutatesBindings(cmd.Args[1:]))) && fanoutPresentationArgsKnown(cmd.Args[1:], values) {
				return true
			}
			return false
		}
		if remoteObservationCall(cmd) {
			if !fanoutTargetArgumentsKnown(cmd.Args, values) {
				return false
			}
			encoded, _ := json.Marshal(args)
			key := string(encoded)
			if seen[key] {
				return false
			}
			seen[key] = true
			return true
		}
		// Only catalogued commands without shell-state changes may occur
		// between bindings and observations. Unknown functions, eval/source,
		// directory changes and mutation builtins cannot preserve this proof.
		switch name {
		case "cd", "unset", "umask":
			return false
		case "printf":
			return !printfArgvMutatesBindings(args[1:])
		case "sed":
			return pipeline && !sedArgvInPlace(args[1:])
		}
		rule, ok := observationRuleByProgram[name]
		cmdArgs := fanoutLocalCommandArgs(name, args[1:])
		return ok && !rule.external && rule.matches(cmdArgs)
	case *syntax.ForClause:
		return !pipeline && fanoutNestedForClause(cmd, values, seen)
	case *syntax.IfClause:
		return !pipeline && fanoutGuardIfClause(cmd, values, seen)
	default:
		return false
	}
}

func fanoutLocalCommandArgs(program string, args []string) []string {
	if program != "git" {
		return args
	}
	for i := 0; i < len(args); {
		arg := strings.TrimSpace(args[i])
		if arg == "-C" || arg == "--git-dir" || arg == "--work-tree" || arg == "-c" {
			if i+1 >= len(args) {
				return args
			}
			i += 2
			continue
		}
		if strings.HasPrefix(arg, "--git-dir=") || strings.HasPrefix(arg, "--work-tree=") {
			i++
			continue
		}
		return args[i:]
	}
	return nil
}

func fanoutGuardIfClause(cmd *syntax.IfClause, values map[string]string, seen map[string]bool) bool {
	if cmd == nil || cmd.Else != nil || len(cmd.Cond) == 0 || len(cmd.Then) == 0 {
		return false
	}
	for _, condStmt := range cmd.Cond {
		if !fanoutStatement(condStmt, values, seen, true) {
			return false
		}
	}
	last := cmd.Then[len(cmd.Then)-1]
	if last == nil || last.Negated || last.Background || last.Coprocess || len(last.Redirs) > 0 {
		return false
	}
	call, ok := last.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) != 1 {
		return false
	}
	if word, ok := staticObservationWord(call.Args[0]); !ok || word != "continue" {
		return false
	}
	for _, thenStmt := range cmd.Then[:len(cmd.Then)-1] {
		if !fanoutStatement(thenStmt, values, seen, true) {
			return false
		}
	}
	return true
}

func fanoutPresentationArgsKnown(words []*syntax.Word, values map[string]string) bool {
	for _, word := range words {
		if word == nil {
			continue
		}
		if len(word.Parts) > 0 {
			if lit, ok := word.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "~") {
				return false
			}
		}
		copyWord := *word
		if syntax.SplitBraces(&copyWord) {
			return false
		}
		known := true
		syntax.Walk(word, func(node syntax.Node) bool {
			if node == nil || !known {
				return false
			}
			switch n := node.(type) {
			case *syntax.Word:
				if len(n.Parts) > 0 {
					if lit, ok := n.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "~") {
						known = false
					}
				}
			case *syntax.ProcSubst, *syntax.ArithmExp, *syntax.ExtGlob, *syntax.BraceExp:
				known = false
			case *syntax.CmdSubst:
				if len(n.Stmts) == 0 {
					known = false
					return false
				}
				subSeen := map[string]bool{}
				for _, subStmt := range n.Stmts {
					if !fanoutStatement(subStmt, values, subSeen, true) {
						known = false
						return false
					}
				}
				return false
			case *syntax.ParamExp:
				if n.Param == nil || n.Excl || n.Length || n.Index != nil || n.Slice != nil || n.Repl != nil || n.Names != 0 {
					known = false
					break
				}
				if _, ok := values[n.Param.Value]; !ok {
					known = false
				}
				if n.Exp != nil {
					pattern, static := staticObservationWord(n.Exp.Word)
					if !static || strings.ContainsAny(pattern, "?[]") {
						known = false
					}
					switch n.Exp.Op {
					case syntax.RemSmallPrefix, syntax.RemLargePrefix, syntax.RemSmallSuffix, syntax.RemLargeSuffix:
					default:
						known = false
					}
				}
			}
			return known
		})
		if !known {
			return false
		}
	}
	return true
}

func sedArgvInPlace(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--in-place" || strings.HasPrefix(arg, "--in-place=") {
			return true
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg[1:], "iI") {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			return false
		}
	}
	return false
}

func fanoutNestedForClause(cmd *syntax.ForClause, values map[string]string, seen map[string]bool) bool {
	if cmd == nil || cmd.Select || stmtsWait(cmd.Do) {
		return false
	}
	iter, ok := cmd.Loop.(*syntax.WordIter)
	if !ok || iter.Name == nil || len(iter.Items) == 0 {
		return false
	}
	if _, exists := values[iter.Name.Value]; exists || iter.Name.Value == "IFS" {
		return false
	}
	for _, item := range iter.Items {
		if len(item.Parts) != 1 {
			return false
		}
		cs, ok := item.Parts[0].(*syntax.CmdSubst)
		if !ok || len(cs.Stmts) == 0 {
			return false
		}
		for _, subStmt := range cs.Stmts {
			if !fanoutStatement(subStmt, values, seen, true) {
				return false
			}
		}
	}
	mutatesOuter := false
	for _, bodyStmt := range cmd.Do {
		syntax.Walk(bodyStmt, func(node syntax.Node) bool {
			if node == nil || mutatesOuter {
				return false
			}
			switch n := node.(type) {
			case *syntax.Assign:
				if n.Name != nil && (n.Name.Value == "IFS" || values[n.Name.Value] != "") {
					mutatesOuter = true
				}
			case *syntax.CallExpr:
				if len(n.Args) > 0 {
					name, static := staticObservationWord(n.Args[0])
					if !static {
						mutatesOuter = true
						return false
					}
					switch name {
					case "cd", "unset", "umask", "eval", "source", ".", "read", "set", "let", "shopt", "alias":
						mutatesOuter = true
					case "printf":
						if printfMutatesBindings(n.Args[1:]) {
							mutatesOuter = true
						}
					}
				}
			}
			return !mutatesOuter
		})
		if mutatesOuter {
			return false
		}
	}
	return true
}

func printfMutatesBindings(args []*syntax.Word) bool {
	for _, arg := range args {
		lit, ok := staticObservationWord(arg)
		if !ok {
			continue
		}
		if lit == "--" {
			return false
		}
		if strings.HasPrefix(lit, "-") && strings.ContainsRune(lit[1:], 'v') {
			return true
		}
		if !strings.HasPrefix(lit, "-") {
			return false
		}
	}
	for _, arg := range args {
		if _, ok := staticObservationWord(arg); !ok {
			return true
		}
		break
	}
	return false
}

func printfArgvMutatesBindings(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if strings.HasPrefix(arg, "-") && strings.ContainsRune(arg[1:], 'v') {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			return false
		}
	}
	return false
}

func fanoutAssignments(assigns []*syntax.Assign, values map[string]string, seen map[string]bool) bool {
	for _, assign := range assigns {
		if assign.Name == nil || assign.Index != nil || assign.Array != nil {
			return false
		}
		if assign.Naked {
			continue // plain `export name`, not dynamic declaration options
		}
		value, ok := fanoutLiteral(assign.Value, values)
		if !ok {
			if !assign.Append && seen != nil && assign.Value != nil && len(assign.Value.Parts) == 1 {
				if cs, isCS := assign.Value.Parts[0].(*syntax.CmdSubst); isCS && len(cs.Stmts) > 0 {
					subSeen := map[string]bool{}
					valid := true
					for _, subStmt := range cs.Stmts {
						if !fanoutStatement(subStmt, values, subSeen, true) {
							valid = false
							break
						}
					}
					if valid && len(subSeen) > 0 {
						keys := make([]string, 0, len(subSeen))
						for k := range subSeen {
							if seen[k] {
								return false
							}
							keys = append(keys, k)
						}
						sort.Strings(keys)
						for _, k := range keys {
							seen[k] = true
						}
						values[assign.Name.Value] = "__obs_" + strings.Join(keys, "|")
						continue
					}
				}
			}
			return false
		}
		if assign.Append {
			prior, known := values[assign.Name.Value]
			if !known {
				return false
			}
			value = prior + value
		}
		if len(value) > 16384 {
			return false
		}
		values[assign.Name.Value] = value
	}
	return true
}

func fanoutRead(stmt *syntax.Stmt, call *syntax.CallExpr, values map[string]string) bool {
	if len(stmt.Redirs) != 1 || stmt.Redirs[0].Op != syntax.WordHdoc {
		return false
	}
	input, ok := fanoutLiteral(stmt.Redirs[0].Word, values)
	if !ok || strings.ContainsAny(input, "\r\n") {
		return false
	}
	readValues := make(map[string]string, len(values)+1)
	for name, value := range values {
		readValues[name] = value
	}
	for _, assign := range call.Assigns {
		if assign.Name == nil || assign.Name.Value != "IFS" {
			return false
		}
	}
	if !fanoutAssignments(call.Assigns, readValues, nil) {
		return false
	}
	var names []string
	raw, flags := false, true
	for _, arg := range call.Args[1:] {
		name, ok := staticObservationWord(arg)
		if !ok {
			return false
		}
		if flags && name == "-r" {
			raw = true
			continue
		}
		if flags && name == "--" {
			flags = false
			continue
		}
		flags = false
		if !syntax.ValidName(name) || name == "IFS" {
			return false
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		names = []string{"REPLY"}
	}
	fields := expand.ReadFields(fanoutExpansion(readValues), input, len(names), raw)
	for i, name := range names {
		values[name] = ""
		if i < len(fields) {
			values[name] = fields[i]
		}
	}
	return true
}

func fanoutExpansion(values map[string]string) *expand.Config {
	vars := make([]string, 0, len(values))
	for name, value := range values {
		vars = append(vars, name+"="+value)
	}
	return &expand.Config{Env: expand.ListEnviron(vars...), NoUnset: true}
}

func fanoutWordKnown(word *syntax.Word, values map[string]string) bool {
	if len(word.Parts) > 0 {
		if lit, ok := word.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "~") {
			return false // no user/home lookup during static analysis
		}
	}
	known := true
	syntax.Walk(word, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Word:
			if len(n.Parts) > 0 {
				if lit, ok := n.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "~") {
					known = false
				}
			}
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp, *syntax.ExtGlob, *syntax.BraceExp:
			known = false
		case *syntax.ParamExp:
			if n.Param == nil || n.Excl || n.Length || n.Index != nil || n.Slice != nil || n.Repl != nil || n.Names != 0 {
				known = false
				break
			}
			if _, ok := values[n.Param.Value]; !ok {
				known = false
			}
			if n.Exp != nil {
				pattern, static := staticObservationWord(n.Exp.Word)
				if !static || strings.ContainsAny(pattern, "?[]") {
					known = false // no locale-dependent character matching
				}
				switch n.Exp.Op {
				case syntax.RemSmallPrefix, syntax.RemLargePrefix, syntax.RemSmallSuffix, syntax.RemLargeSuffix:
				default:
					known = false
				}
			}
		}
		return known
	})
	return known
}

func fanoutLiteral(word *syntax.Word, values map[string]string) (string, bool) {
	if word == nil {
		return "", true
	}
	if !fanoutWordKnown(word, values) {
		return "", false
	}
	value, err := expand.Literal(fanoutExpansion(values), word)
	return value, err == nil
}

func fanoutFields(words []*syntax.Word, values map[string]string) ([]string, bool) {
	for _, word := range words {
		if !fanoutWordKnown(word, values) {
			return nil, false
		}
		copy := *word
		if syntax.SplitBraces(&copy) {
			return nil, false // do not expand a generated/unbounded literal list
		}
	}
	cfg := fanoutExpansion(values)
	globbed := false
	cfg.ReadDir2 = func(string) ([]fs.DirEntry, error) {
		globbed = true
		return nil, fs.ErrNotExist
	}
	fields, err := expand.Fields(cfg, words...)
	// The library may keep a glob literal when its directories do not exist.
	// Detect the attempted lookup as well as the error, without reading disk.
	return fields, err == nil && !globbed
}

var commonFanoutTargetFlags = map[string]bool{
	"--image-ids": true, "--names": true, "--name": true,
	"--repository-name": true, "--repository-names": true,
	"--cluster": true, "--cluster-name": true, "--clusters": true,
	"--service": true, "--services": true, "--service-name": true, "--service-arn": true,
	"--tasks": true, "--task": true, "--task-definition": true, "--task-definitions": true, "--family": true, "--family-prefix": true,
	"--arn": true, "--resource-arn": true, "--resource-id": true, "--resource-ids": true,
	"--instance-ids": true, "--instance-id": true, "--function-name": true, "--role-name": true,
	"--policy-arn": true, "--user-name": true, "--group-name": true,
	"--db-instance-identifier": true, "--db-cluster-identifier": true,
	"--log-group-name": true, "--log-group-name-prefix": true,
	"--log-stream-name": true, "--log-stream-names": true,
	"--pipeline-name": true, "--pipeline-execution-id": true, "--project-name": true, "--project-names": true,
	"--stack-name": true, "--change-set-name": true, "--rule-name": true, "--queue-url": true, "--queue-name-prefix": true, "--topic-arn": true, "--table-name": true, "--stream-name": true, "--state-machine-arn": true, "--execution-arn": true, "--load-balancer-arn": true, "--load-balancer-names": true, "--target-group-arn": true, "--target-group-names": true, "--distribution-id": true, "--hosted-zone-id": true,
	"--id": true, "--ids": true, "--key-id": true, "--secret-id": true, "--parameter-name": true,
	"--bucket": true, "--key": true, "--prefix": true,
	"-n": true, "--namespace": true, "--context": true, "--app": true, "-l": true, "--selector": true,
	"-R": true, "--repo": true, "--project": true, "--region": true, "-w": true, "--workflow": true, "--branch": true, "-b": true,
}

// Positional resources and explicitly catalogued target flags may vary.
// An unknown option's value may only change timing, caching or presentation;
// its variability is not target proof. Keep it static rather than guessing.
func fanoutTargetArgumentsKnown(words []*syntax.Word, values map[string]string) bool {
	program, _ := staticObservationWord(words[0])
	selectors := observationRuleByProgram[program].targetValueFlags
	selector := func(flag string) bool {
		if commonFanoutTargetFlags[flag] {
			return true
		}
		for _, name := range selectors {
			if name == flag {
				return true
			}
		}
		return false
	}
	priorFlag := ""
	for i := 1; i < len(words); i++ {
		_, static := staticObservationWord(words[i])
		fields, known := fanoutFields([]*syntax.Word{words[i]}, values)
		if !known || len(fields) != 1 {
			return false
		}
		flag := fields[0]
		if strings.HasPrefix(flag, "-") {
			name, _, attached := strings.Cut(flag, "=")
			if !static && !selector(name) {
				return false
			}
			priorFlag = name
			if attached {
				priorFlag = ""
			}
			continue
		}
		if !static && priorFlag != "" && !selector(priorFlag) {
			return false
		}
		priorFlag = ""
	}
	return true
}
