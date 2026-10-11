package tools

import "strings"

// A proven observation changes nothing, but that alone does not make it
// evidence. `true`, `cd .`, `echo published`, or `date` succeed without
// looking at anything, so bound to an effect claim they would release it, or
// let another Run's claim pass without the person, at no cost. An effect
// observation therefore also has to read some state: a remote, a service, a
// repository, or a file. Whether it reads the right state remains the model's
// judgment.

// observationReadsState reports whether a proven read-only observation reads
// state beyond its own literal arguments, the environment, and the clock. The
// programs come from the same identification the observation proof uses; each
// catalog program declares whether it can read state (readsState), and a
// catalogued filter that provably read only its standard input adds nothing of
// its own. A device that holds nothing, such as /dev/null, is not state. An
// owner-approved observation script reads the state its owner registered it
// to observe.
func observationReadsState(toolName string, args map[string]interface{}) bool {
	if approvedObservationScript(toolName, args) {
		return true
	}
	commands, ok := parseObservationCommands(strings.TrimSpace(execCommandPayload(toolName, args)))
	if !ok {
		return false
	}
	for _, fields := range commands {
		program := fields[0]
		rule, known := observationRuleByProgram[program]
		if !known {
			return false
		}
		operands := withoutStatelessDevices(fields[1:])
		if rule.readsState != nil && !rule.readsState(operands) {
			continue
		}
		if filterReadsOnlyStdin(program, operands) {
			continue
		}
		return true
	}
	return false
}

// statelessDevices read as empty, as noise, or as the standard input; none of
// them is a target's state.
var statelessDevices = map[string]bool{
	"/dev/null": true, "/dev/zero": true, "/dev/random": true, "/dev/urandom": true,
	"/dev/stdin": true, "/dev/fd/0": true,
}

func withoutStatelessDevices(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if !statelessDevices[strings.TrimSpace(arg)] {
			out = append(out, arg)
		}
	}
	return out
}

// readsNoState marks a program whose output comes only from its arguments, the
// environment, or the clock.
func readsNoState([]string) bool { return false }

// testFileUnary are the test/[ unary operators that look at the filesystem;
// testFileBinary compare two files. -a and -o are left out: as connectives
// they combine other tests.
var (
	testFileUnary = map[string]bool{
		"-b": true, "-c": true, "-d": true, "-e": true, "-f": true, "-g": true, "-h": true, "-k": true,
		"-p": true, "-r": true, "-s": true, "-u": true, "-w": true, "-x": true,
		"-G": true, "-L": true, "-N": true, "-O": true, "-S": true,
	}
	testOtherUnary = map[string]bool{"-n": true, "-z": true, "-t": true, "-v": true, "-R": true, "-o": true}
	testFileBinary = map[string]bool{"-nt": true, "-ot": true, "-ef": true}
	testBinary     = map[string]bool{
		"=": true, "==": true, "!=": true, "<": true, ">": true,
		"-eq": true, "-ne": true, "-lt": true, "-le": true, "-gt": true, "-ge": true,
		"-nt": true, "-ot": true, "-ef": true,
	}
)

// testReadsFiles reports whether a test expression evaluates a file test.
// Like the shell, it reads up to four arguments by their count, so a lone
// operator such as `test -f` is a string test, and longer expressions by
// precedence (! before -a before -o, with parentheses).
func testReadsFiles(args []string) bool {
	switch len(args) {
	case 0, 1:
		return false
	case 2:
		return args[0] != "!" && testFileUnary[args[0]]
	case 3:
		if testBinary[args[1]] {
			return testFileBinary[args[1]]
		}
		if args[1] == "-a" || args[1] == "-o" {
			return false
		}
		if args[0] == "!" {
			return testReadsFiles(args[1:])
		}
		if args[0] == "(" && args[2] == ")" {
			return false
		}
	case 4:
		if args[0] == "!" {
			return testReadsFiles(args[1:])
		}
		if args[0] == "(" && args[3] == ")" {
			return testReadsFiles(args[1:3])
		}
	}
	p := testExprParser{args: args}
	if !p.or() || p.pos != len(args) {
		return false
	}
	return p.files
}

// bracketReadsFiles is testReadsFiles for `[`, whose last argument is `]`.
func bracketReadsFiles(args []string) bool {
	if len(args) == 0 || args[len(args)-1] != "]" {
		return false
	}
	return testReadsFiles(args[:len(args)-1])
}

// testExprParser reads a test expression longer than four arguments.
type testExprParser struct {
	args  []string
	pos   int
	files bool
}

func (p *testExprParser) at(token string) bool { return p.pos < len(p.args) && p.args[p.pos] == token }

func (p *testExprParser) or() bool {
	if !p.and() {
		return false
	}
	for p.at("-o") {
		p.pos++
		if !p.and() {
			return false
		}
	}
	return true
}

func (p *testExprParser) and() bool {
	if !p.not() {
		return false
	}
	for p.at("-a") {
		p.pos++
		if !p.not() {
			return false
		}
	}
	return true
}

func (p *testExprParser) not() bool {
	if p.at("!") {
		p.pos++
		return p.not()
	}
	return p.primary()
}

func (p *testExprParser) primary() bool {
	if p.pos >= len(p.args) {
		return false
	}
	if p.at("(") {
		p.pos++
		if !p.or() || !p.at(")") {
			return false
		}
		p.pos++
		return true
	}
	token := p.args[p.pos]
	switch {
	case p.pos+2 < len(p.args) && testBinary[p.args[p.pos+1]]:
		p.files = p.files || testFileBinary[p.args[p.pos+1]]
		p.pos += 3
	case (testFileUnary[token] || testOtherUnary[token]) && p.pos+1 < len(p.args):
		p.files = p.files || testFileUnary[token]
		p.pos += 2
	default:
		p.pos++
	}
	return true
}

// jqValueOptions take that many following arguments.
var jqValueOptions = map[string]int{
	"--arg": 2, "--argjson": 2, "--indent": 1, "-L": 1, "--library-path": 1,
}

// jqReadsFile: jq reads state only from an input file operand or a file it
// loads into a variable. With -n it reads no input at all, and the
// environment ($ENV, env) is not observed state. A filter given with -f is
// the program, not the input.
func jqReadsFile(args []string) bool {
	filterSeen, positionalArgs, endOfOptions := false, false, false
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if !endOfOptions && strings.HasPrefix(arg, "-") && arg != "-" {
			if arg == "--" {
				endOfOptions = true
				continue
			}
			name, attached := arg, false
			if eq := strings.IndexByte(arg, '='); eq >= 0 && strings.HasPrefix(arg, "--") {
				name, attached = arg[:eq], true
			}
			switch {
			case name == "--slurpfile" || name == "--rawfile":
				return true
			case name == "--args" || name == "--jsonargs":
				positionalArgs = true
			case name == "-f" || name == "--from-file":
				filterSeen = true
				if !attached {
					i++
				}
			case jqValueOptions[name] > 0:
				if !attached {
					i += jqValueOptions[name]
				}
			case !strings.HasPrefix(arg, "--") && strings.HasSuffix(arg, "f"):
				// A bundle such as -nf ends with the filter-file option.
				filterSeen = true
				i++
			case !strings.HasPrefix(arg, "--") && strings.HasSuffix(arg, "L"):
				i++
			}
			continue
		}
		if !filterSeen {
			filterSeen = true
			continue
		}
		if positionalArgs {
			continue
		}
		return true
	}
	return false
}

// pasteReadsFile: paste reads its standard input unless it names a file.
func pasteReadsFile(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "" || arg == "-" || arg == "-s" || arg == "-z" || arg == "--serial" || arg == "--zero-terminated" ||
			strings.HasPrefix(arg, "--delimiters="):
		case arg == "-d" || arg == "--delimiters":
			i++
		case strings.HasPrefix(arg, "-d"):
		default:
			return true
		}
	}
	return false
}

// clientOnly reports a CLI invocation that answers from the local client
// alone: its version or its own environment, not the service it manages.
func clientOnly(program string, args []string) bool {
	rest, ok := observationCommandArgs(program, args)
	if !ok || len(rest) == 0 {
		return false
	}
	client := false
	for _, arg := range rest[1:] {
		switch strings.TrimSpace(arg) {
		case "--client", "--client=true":
			client = true
		}
	}
	switch program {
	case "helm":
		return rest[0] == "version" || rest[0] == "env"
	case "kubectl", "argocd":
		return rest[0] == "version" && client
	}
	return false
}

func helmReadsState(args []string) bool    { return !clientOnly("helm", args) }
func kubectlReadsState(args []string) bool { return !clientOnly("kubectl", args) }
func argocdReadsState(args []string) bool  { return !clientOnly("argocd", args) }

// yqReadsFile: yq reads a file through an operand after its expression, or
// through --from-file; otherwise it transforms its standard input.
func yqReadsFile(args []string) bool {
	operands := 0
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "--from-file" || strings.HasPrefix(arg, "--from-file="):
			return true
		case arg == "-o" || arg == "-p" || arg == "-I" || arg == "--output-format" || arg == "--input-format" || arg == "--indent":
			i++
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			if operands == 0 && (arg == "e" || arg == "eval" || arg == "ea" || arg == "eval-all") {
				continue
			}
			if arg != "-" {
				operands++
			}
		}
	}
	return operands > 1
}
