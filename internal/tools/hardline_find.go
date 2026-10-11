package tools

import "strings"

// A find whose expression may select nearly every entry under its start paths
// does to them what a recursive delete does: `find / -delete` and
// `find / -exec rm -rf {} +` are `rm -rf /`. The floor reads the expression
// find evaluates — its -a, -o, !, parentheses and commas — and refuses such a
// find as those commands. Only a real test narrows the entries an action
// reaches: an option or an action holds for every entry, a test whose pattern
// is only `*` (or `.*` for -regex) does too, and a command's exit status may
// hold or fail for every one. What a negated test or an earlier -o branch
// leaves is still nearly everything, so `find / -path /proc -prune -o -delete`
// deletes all but /proc. -maxdepth 0 or 1 bounds -delete, which then removes
// no directory that still holds entries; -quit and -files0-from leave the
// find to ordinary approval.

// findPrimaryArity is the number of arguments each primary takes, GNU's and
// BSD's; the -exec family runs to its terminator instead.
var findPrimaryArity = map[string]int{
	"-depth": 0, "-d": 0, "-xdev": 0, "-mount": 0, "-x": 0, "-follow": 0, "-noleaf": 0,
	"-ignore_readdir_race": 0, "-noignore_readdir_race": 0, "-daystart": 0, "-warn": 0, "-nowarn": 0,
	"-maxdepth": 1, "-mindepth": 1, "-regextype": 1,
	"-print": 0, "-print0": 0, "-ls": 0, "-delete": 0, "-prune": 0, "-quit": 0,
	"-printf": 1, "-fprint": 1, "-fprint0": 1, "-fls": 1, "-fprintf": 2, "-files0-from": 1,
	"-true": 0, "-false": 0, "-empty": 0, "-nouser": 0, "-nogroup": 0, "-readable": 0, "-writable": 0,
	"-executable": 0, "-acl": 0, "-xattr": 0,
	"-name": 1, "-iname": 1, "-path": 1, "-ipath": 1, "-wholename": 1, "-iwholename": 1, "-lname": 1,
	"-ilname": 1, "-regex": 1, "-iregex": 1, "-type": 1, "-xtype": 1, "-size": 1, "-newer": 1, "-anewer": 1,
	"-cnewer": 1, "-mnewer": 1, "-mtime": 1, "-atime": 1, "-ctime": 1, "-mmin": 1, "-amin": 1, "-cmin": 1,
	"-used": 1, "-user": 1, "-group": 1, "-uid": 1, "-gid": 1, "-perm": 1, "-links": 1, "-inum": 1,
	"-samefile": 1, "-fstype": 1, "-context": 1, "-flags": 1, "-xattrname": 1, "-Bmin": 1, "-Bnewer": 1,
	"-Btime": 1,
}

var (
	// findAlwaysTrue hold for every entry: options, and actions that report
	// success. Every other primary but the -exec family is a test.
	findAlwaysTrue = wordSet("-depth", "-d", "-xdev", "-mount", "-x", "-follow", "-noleaf", "-ignore_readdir_race",
		"-noignore_readdir_race", "-daystart", "-warn", "-nowarn", "-maxdepth", "-mindepth", "-regextype",
		"-print", "-print0", "-ls", "-delete", "-prune", "-printf", "-fprint", "-fprint0", "-fls", "-fprintf",
		"-true")
	// findPatternTests match an entry's name or path.
	findPatternTests = wordSet("-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename")
)

// findSweep is what a find selects as far as the floor needs.
type findSweep struct {
	starts []string
	// deletes: a -delete that may reach nearly every entry, with no -maxdepth
	// of 0 or 1 bounding it.
	deletes bool
	// commands are the -exec family's commands that may reach nearly every
	// entry.
	commands [][]string
	// below: a -mindepth of one or more selects the entries under each start
	// path but not the path itself.
	below   bool
	bounded bool
}

// findCondition is what the floor knows of an expression's value: whether it
// may hold for nearly every entry, and whether it may fail for nearly every
// entry. A real test holds only for a few; an option holds for all; a command
// may do either.
type findCondition struct{ mostlyTrue, mostlyFalse bool }

// findReader reads find's expression by its grammar — `,` binds loosest, then
// -o, then -a (written or implied), then ! and parentheses — noting for each
// action whether it may reach nearly every entry.
type findReader struct {
	words []string
	i     int
	ok    bool
	sweep *findSweep
}

func (r *findReader) at(words ...string) bool {
	if r.i >= len(r.words) {
		return false
	}
	for _, word := range words {
		if r.words[r.i] == word {
			return true
		}
	}
	return false
}

func (r *findReader) list(reach bool) findCondition {
	c := r.or(reach)
	for r.ok && r.at(",") {
		r.i++
		c = r.or(reach)
	}
	return c
}

func (r *findReader) or(reach bool) findCondition {
	c := r.and(reach)
	for r.ok && r.at("-o", "-or") {
		r.i++
		right := r.and(reach && c.mostlyFalse)
		c = findCondition{mostlyTrue: c.mostlyTrue || right.mostlyTrue, mostlyFalse: c.mostlyFalse && right.mostlyFalse}
	}
	return c
}

func (r *findReader) and(reach bool) findCondition {
	c := r.unary(reach)
	for r.ok && r.i < len(r.words) && !r.at("-o", "-or", ",", ")") {
		if r.at("-a", "-and") {
			r.i++
		}
		right := r.unary(reach && c.mostlyTrue)
		c = findCondition{mostlyTrue: c.mostlyTrue && right.mostlyTrue, mostlyFalse: c.mostlyFalse || right.mostlyFalse}
	}
	return c
}

func (r *findReader) unary(reach bool) findCondition {
	switch {
	case r.i >= len(r.words):
		r.ok = false
		return findCondition{}
	case r.at("!", "-not"):
		r.i++
		c := r.unary(reach)
		return findCondition{mostlyTrue: c.mostlyFalse, mostlyFalse: c.mostlyTrue}
	case r.at("("):
		r.i++
		c := r.list(reach)
		if !r.at(")") {
			r.ok = false
		}
		r.i++
		return c
	}
	return r.primary(reach)
}

func (r *findReader) primary(reach bool) findCondition {
	name := r.words[r.i]
	r.i++
	if _, runs := findCommandPrimaries[name]; runs {
		end := findCommandEnd(r.words, r.i-1)
		if reach && end > r.i {
			r.sweep.commands = append(r.sweep.commands, r.words[r.i:end])
		}
		r.i = min(end+1, len(r.words))
		return findCondition{mostlyTrue: true, mostlyFalse: true}
	}
	arity, known := findPrimaryArity[name]
	test := false
	if name == "-depth" && r.i < len(r.words) &&
		strings.ContainsAny(r.words[r.i], "0123456789") && strings.Trim(r.words[r.i], "+-0123456789") == "" {
		// BSD's -depth n is a test of the entry's depth.
		arity, test = 1, true
	}
	if strings.HasPrefix(name, "-newer") && len(name) == len("-newerXY") {
		arity, known = 1, true
	}
	if !known || name == "-quit" || name == "-files0-from" || r.i+arity > len(r.words) {
		r.ok = false
		return findCondition{}
	}
	args := r.words[r.i : r.i+arity]
	r.i += arity
	switch name {
	case "-delete":
		r.sweep.deletes = r.sweep.deletes || reach
	case "-maxdepth":
		depth := strings.TrimLeft(args[0], "0")
		r.sweep.bounded = r.sweep.bounded || depth == "" || depth == "1"
	case "-mindepth":
		r.sweep.below = r.sweep.below || strings.TrimLeft(args[0], "0") != ""
	}
	if _, always := findAlwaysTrue[name]; (always && !test) || trivialFindTest(name, args) {
		return findCondition{mostlyTrue: true}
	}
	return findCondition{mostlyFalse: true}
}

// trivialFindTest reports a test that holds for every entry: a name or path
// pattern of only `*`, or the regular expression `.*`.
func trivialFindTest(name string, args []string) bool {
	if _, pattern := findPatternTests[name]; pattern {
		return args[0] != "" && strings.Trim(args[0], "*") == ""
	}
	return (name == "-regex" || name == "-iregex") && args[0] == ".*"
}

// readFindSweep reads find's leading options (GNU -H -L -P -D -O, BSD
// -E -H -L -P -X -d -s -x and -f PATH), its start paths (`.` when none), and
// its expression. ok is false when the expression cannot be read.
func readFindSweep(args []string) (findSweep, bool) {
	var sweep findSweep
	i := 0
leading:
	for ; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			i++
			break leading
		case arg == "-D":
			i++
		case arg == "-f":
			if i+1 < len(args) {
				sweep.starts = append(sweep.starts, args[i+1])
			}
			i++
		case strings.HasPrefix(arg, "-O") && len(arg) > 2:
		case len(arg) > 1 && arg[0] == '-' && strings.Trim(arg[1:], "HLPEXdsx") == "":
		default:
			break leading
		}
	}
	for ; i < len(args) && !strings.HasPrefix(args[i], "-") && args[i] != "(" && args[i] != "!" && args[i] != ","; i++ {
		sweep.starts = append(sweep.starts, args[i])
	}
	if len(sweep.starts) == 0 {
		sweep.starts = []string{"."}
	}
	if i < len(args) {
		reader := findReader{words: args[i:], ok: true, sweep: &sweep}
		reader.list(true)
		if !reader.ok || reader.i < len(reader.words) {
			return findSweep{}, false
		}
	}
	sweep.deletes = sweep.deletes && !sweep.bounded
	return sweep, true
}

// hardlineFindSweep refuses a find when the recursive delete or the commands
// it amounts to would be refused: a -delete that may reach nearly every entry
// deletes each start path's tree, and each command the -exec family may run
// for nearly every entry is checked with every start path (past -mindepth,
// every entry under it) in place of `{}`.
func hardlineFindSweep(args []string, home string) (string, bool) {
	sweep, ok := readFindSweep(args)
	if !ok {
		return "", false
	}
	targets := sweep.starts
	if sweep.below {
		targets = make([]string, len(sweep.starts))
		for i, start := range sweep.starts {
			targets[i] = strings.TrimRight(start, "/") + "/*"
		}
	}
	if sweep.deletes {
		if target, hit := hardlineRmProtectedRoot(append([]string{"-r"}, targets...), home); hit {
			return "recursive delete of protected root: " + target, true
		}
	}
	for _, command := range sweep.commands {
		for _, target := range targets {
			words := make([]string, len(command))
			for i, word := range command {
				words[i] = strings.ReplaceAll(word, "{}", target)
			}
			for _, read := range appendWithScript(nil, readCommand{id: identifyCommand(words), fallback: true}, 1, true) {
				if reason, hit := hardlineCommand(read, home); hit {
					return reason, true
				}
			}
		}
	}
	return "", false
}
