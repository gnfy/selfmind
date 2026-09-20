package tools

import "strings"

// A credentialed payload requires every program in it to be credential-safe,
// because a credential leaves through whatever prints it. That flag is per
// PROGRAM, which is too coarse for a filter: `grep tag` in a pipeline reads the
// previous command's stdout and cannot open a file at all, while `grep -r . ~/.aws`
// obviously can. Judging the invocation instead of the name releases the first
// without releasing the second.
//
// The rule below answers exactly one question: does this invocation provably
// read nothing but its standard input? It is not a claim that the program is
// harmless — `sort -o out` writes, and stays rejected by the catalog's own
// reject list.
//
// It can only ever WIDEN: a call it does not understand falls back to the
// existing per-program flag, which for these filters is "not credential-safe".
// So every unrecognised flag, bundle, or operand fails closed at no cost.

// stdinFilterRule is the argument grammar needed to tell a file operand from a
// flag's value for one filter.
type stdinFilterRule struct {
	// boolFlags stand alone.
	boolFlags map[string]bool
	// valueFlags consume the rest of their token, or the next argument.
	valueFlags map[string]bool
	// fileFlags name a file even though they look like options. Their presence
	// disqualifies the invocation outright.
	fileFlags map[string]bool
	// operandBudget is how many leading operands are NOT files: grep's pattern,
	// tr's two SETs. Anything beyond it is a file.
	operandBudget int
	// patternFlags supply the non-file operand through a flag instead, so the
	// budget drops and a following operand becomes a file.
	patternFlags map[string]bool
	// numericShort accepts the legacy `-5` abbreviation (head, tail).
	numericShort bool
}

func flagSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// stdinOnlyFilters covers the catalogued filters that read standard input when
// given no file operand.
//
// Deliberately absent:
//   - `rg`, because with no path operand it recursively searches the working
//     directory: "no file operand" does not mean "no file access" for it.
//   - `paste`, `comm`, `diff`, which require file operands to do anything.
//   - `ls`, `stat`, `file`, `readlink`, `realpath`, whose whole purpose is a
//     path.
//   - `jq` and `yq`, which already carry their own credential handling.
var stdinOnlyFilters = map[string]stdinFilterRule{
	"cat": {
		boolFlags: flagSet("-b", "-e", "-n", "-s", "-t", "-u", "-v", "-A", "-E", "-T",
			"--number", "--number-nonblank", "--squeeze-blank", "--show-ends",
			"--show-tabs", "--show-all", "--show-nonprinting"),
	},
	"head": {
		boolFlags:    flagSet("-q", "-v", "-z", "--quiet", "--silent", "--verbose", "--zero-terminated"),
		valueFlags:   flagSet("-n", "-c", "--lines", "--bytes"),
		numericShort: true,
	},
	"tail": {
		// `-f` with no operand follows stdin, which reads no file.
		boolFlags:    flagSet("-f", "-F", "-q", "-v", "-z", "--follow", "--quiet", "--silent", "--verbose", "--zero-terminated", "--retry"),
		valueFlags:   flagSet("-n", "-c", "-s", "--lines", "--bytes", "--sleep-interval", "--pid", "--max-unchanged-stats"),
		numericShort: true,
	},
	"wc": {
		boolFlags: flagSet("-c", "-l", "-m", "-w", "-L", "--bytes", "--chars", "--lines", "--words", "--max-line-length"),
		// --files0-from reads the LIST of files from a file.
		fileFlags: flagSet("--files0-from"),
	},
	"base64": {
		boolFlags:  flagSet("-d", "-i", "-D", "--decode", "--ignore-garbage"),
		valueFlags: flagSet("-w", "--wrap"),
	},
	"tr": {
		// Operands are SET1 and SET2. tr has no file operand at all.
		boolFlags:     flagSet("-c", "-C", "-d", "-s", "-t", "--complement", "--delete", "--squeeze-repeats", "--truncate-set1"),
		operandBudget: 2,
	},
	"cut": {
		boolFlags:  flagSet("-n", "-s", "-z", "--complement", "--only-delimited", "--zero-terminated"),
		valueFlags: flagSet("-b", "-c", "-f", "-d", "--bytes", "--characters", "--fields", "--delimiter", "--output-delimiter"),
	},
	"sort": {
		boolFlags: flagSet("-b", "-c", "-C", "-d", "-f", "-g", "-h", "-i", "-M", "-n", "-r", "-R", "-s", "-u", "-V", "-z",
			"--ignore-leading-blanks", "--dictionary-order", "--ignore-case", "--general-numeric-sort",
			"--human-numeric-sort", "--month-sort", "--numeric-sort", "--random-sort", "--reverse",
			"--stable", "--unique", "--version-sort", "--zero-terminated", "--check", "--debug"),
		valueFlags: flagSet("-k", "-t", "-S", "-T", "--key", "--field-separator", "--buffer-size",
			"--temporary-directory", "--compress-program", "--parallel"),
		// -o is already in the catalog's reject list; --files0-from reads a list
		// of files, and --random-source reads a file.
		fileFlags: flagSet("-o", "--output", "--files0-from", "--random-source"),
	},
	"nl": {
		boolFlags: flagSet("-p", "--no-renumber"),
		valueFlags: flagSet("-b", "-d", "-f", "-h", "-i", "-l", "-n", "-s", "-v", "-w",
			"--body-numbering", "--section-delimiter", "--footer-numbering", "--header-numbering",
			"--line-increment", "--join-blank-lines", "--number-format", "--number-separator",
			"--starting-line-number", "--number-width"),
	},
	"od": {
		boolFlags: flagSet("-a", "-b", "-c", "-d", "-f", "-i", "-l", "-o", "-s", "-x", "-v", "-C",
			"--canonical", "--output-duplicates"),
		valueFlags: flagSet("-A", "-j", "-N", "-S", "-t", "-w",
			"--address-radix", "--skip-bytes", "--read-bytes", "--strings", "--format", "--width"),
	},
	"column": {
		boolFlags:  flagSet("-t", "-x", "-e", "--table"),
		valueFlags: flagSet("-c", "-s", "-o", "-N", "-R", "-H", "--columns", "--separator", "--output-separator", "--table-columns"),
	},
	"md5":       {boolFlags: flagSet("-q", "-r", "-p", "-n"), fileFlags: flagSet("-s", "-c", "--check")},
	"md5sum":    {boolFlags: flagSet("-b", "-t", "-z", "--binary", "--text", "--tag", "--zero"), fileFlags: flagSet("-c", "--check")},
	"sha256sum": {boolFlags: flagSet("-b", "-t", "-z", "--binary", "--text", "--tag", "--zero"), fileFlags: flagSet("-c", "--check")},
	"shasum": {
		boolFlags:  flagSet("-b", "-t", "-p", "-U", "--binary", "--text", "--portable", "--UNIVERSAL", "--tag"),
		valueFlags: flagSet("-a", "--algorithm"),
		fileFlags:  flagSet("-c", "--check"),
	},
	"grep": {
		boolFlags: flagSet("-a", "-b", "-c", "-E", "-F", "-G", "-H", "-h", "-i", "-I", "-L", "-l", "-n", "-o",
			"-P", "-q", "-s", "-U", "-v", "-w", "-x", "-y", "-z", "-Z",
			"--text", "--byte-offset", "--count", "--extended-regexp", "--fixed-strings",
			"--basic-regexp", "--with-filename", "--no-filename", "--ignore-case",
			"--files-without-match", "--files-with-matches", "--line-number", "--only-matching",
			"--perl-regexp", "--quiet", "--silent", "--no-messages", "--invert-match",
			"--word-regexp", "--line-regexp", "--null", "--null-data", "--line-buffered"),
		valueFlags: flagSet("-m", "-A", "-B", "-C", "--max-count", "--after-context",
			"--before-context", "--context", "--color", "--colour", "--label", "--binary-files"),
		// -f reads the PATTERNS from a file; -r/-R and the include/exclude family
		// all reach the filesystem.
		fileFlags: flagSet("-r", "-R", "-d", "-D", "-f", "--file", "--recursive",
			"--dereference-recursive", "--directories", "--devices",
			"--include", "--exclude", "--exclude-from", "--exclude-dir"),
		operandBudget: 1,
		patternFlags:  flagSet("-e", "--regexp"),
	},
}

// filterReadsOnlyStdin reports whether this invocation of a catalogued filter
// can only have read its standard input. It fails closed on anything it does
// not fully understand.
func filterReadsOnlyStdin(program string, args []string) bool {
	rule, known := stdinOnlyFilters[program]
	if !known {
		return false
	}
	budget := rule.operandBudget
	operands := 0
	endOfFlags := false

	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		// A bare "-" names standard input, not a file.
		if endOfFlags || arg == "-" || !strings.HasPrefix(arg, "-") {
			if arg != "-" {
				operands++
			}
			continue
		}
		if arg == "--" {
			endOfFlags = true
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, attached := arg, false
			if eq := strings.IndexByte(arg, '='); eq >= 0 {
				name, attached = arg[:eq], true
			}
			switch {
			case rule.fileFlags[name]:
				return false
			case rule.patternFlags[name]:
				budget--
				if !attached {
					i++
				}
			case rule.valueFlags[name]:
				if !attached {
					i++
				}
			case rule.boolFlags[name]:
			default:
				return false
			}
			continue
		}
		// Short form, possibly a bundle (-in), possibly with an attached value
		// (-m5), possibly the legacy numeric abbreviation (-5).
		if rule.numericShort && isAllDigits(arg[1:]) {
			continue
		}
		consumedNext, ok := consumeShortBundle(arg, rule, &budget)
		if !ok {
			return false
		}
		if consumedNext {
			i++
		}
	}
	return operands <= budget
}

// consumeShortBundle walks the letters of a short-flag token. It reports
// whether the following argument is this token's value, and false when any
// letter is unknown or reaches a file.
func consumeShortBundle(arg string, rule stdinFilterRule, budget *int) (consumesNext bool, ok bool) {
	for j := 1; j < len(arg); j++ {
		letter := "-" + string(arg[j])
		switch {
		case rule.fileFlags[letter]:
			return false, false
		case rule.patternFlags[letter]:
			*budget--
			// The pattern is either attached to this token or the next argument.
			return j == len(arg)-1, true
		case rule.valueFlags[letter]:
			// Everything after the letter is the value; if nothing follows, the
			// next argument is.
			return j == len(arg)-1, true
		case rule.boolFlags[letter]:
		default:
			return false, false
		}
	}
	return false, true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
