package tools

import (
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// GNU parallel's grammar and input sources. The words after `:::` are literal
// arguments; those after `::::` name files of arguments, whose contents
// analysis cannot read. Each command line takes one argument from every
// source.

// parallelGrammar is GNU parallel's.
var parallelGrammar = prefixWrapper{
	valueFlags:          parallelValueOptions,
	optionalFlags:       wordSet("-i", "--replace", "-e", "--eof", "-l", "--max-lines"),
	optionalTakesNext:   true,
	longFlags:           parallelLongFlags,
	joinsScript:         true,
	terminators:         wordSet(":::", "::::", ":::+", "::::+"),
	argumentTerminators: wordSet(":::", ":::+"),
	separatorFlags: map[string]string{"--arg-sep": ":::", "--argsep": ":::",
		"--arg-file-sep": "::::", "--argfilesep": "::::"},
	replaceFlags: wordSet("-I", "-i", "--replace", "--extensionreplace", "--er", "--basenamereplace",
		"--bnr", "--dirnamereplace", "--dnr", "--basenameextensionreplace", "--bner", "--seqreplace",
		"--slotreplace"),
	replacesBraces: true,
}

// parallelValueOptions are GNU parallel's options that take a value
// (`=s`, `=i` and `=f` in its Getopt::Long table), with their aliases.
var parallelValueOptions = wordSet(
	"-a", "--arg-file", "-C", "--colsep", "--col-sep", "-d", "--delimiter", "-E", "-I", "-j", "--jobs",
	"-J", "--profile", "-L", "-n", "--max-args", "--maxargs", "-N", "--max-replace-args", "--maxreplaceargs",
	"-P", "--max-procs", "-S", "--sshlogin", "-s", "--max-chars", "-D", "--debug", "-U", "-W", "-B", "-H",
	"--joblog", "--jl", "--results", "--result", "--res", "--parens", "--rpl", "--extensionreplace", "--er",
	"--basenamereplace", "--bnr", "--dirnamereplace", "--dnr", "--basenameextensionreplace", "--bner",
	"--seqreplace", "--slotreplace", "--delay", "--ssh-delay", "--sshdelay", "--load", "--nice",
	"--tag-string", "--tagstring", "--ctag-string", "--ctagstring", "--sshloginfile", "--slf", "--ssh",
	"--transfer-file", "--transferfile", "--transfer-files", "--transferfiles", "--tf", "--return", "--trc",
	"--basefile", "--bf", "--template", "--tmpl", "--workdir", "--work-dir", "--wd", "--rsync-opts",
	"--rsyncopts", "--tmpdir", "--tempdir", "--use-compress-program", "--compress-program",
	"--usecompressprogram", "--compressprogram", "--use-decompress-program", "--decompress-program",
	"--usedecompressprogram", "--decompressprogram", "--total-jobs", "--totaljobs", "--total",
	"--arg-sep", "--argsep", "--arg-file-sep", "--argfilesep", "--trim", "--env", "--linkinputsource",
	"--xapplyinputsource", "--halt-on-error", "--haltonerror", "--halt", "--limit", "--memfree",
	"--memsuspend", "--retries", "--timeout", "--term-seq", "--termseq", "--minversion", "--min-version",
	"--semaphore-timeout", "--semaphoretimeout", "--st", "--semaphore-name", "--semaphorename", "--id",
	"--header", "--shard", "--bin", "--group-by", "--groupby", "--filter", "--combineexec",
	"--combine-exec", "--combineexecutable", "--combine-executable", "--recstart", "--recend",
	"--block-size", "--blocksize", "--block", "--block-timeout", "--blocktimeout", "--bt",
	"--process-slot-var", "--sql", "--sqlmaster", "--sqlworker", "--sqlandworker", "--shellcompletion",
)

// parallelLongFlags are GNU parallel's long options that take no value; with
// parallelValueOptions they decide whether an abbreviation is unique.
var parallelLongFlags = wordSet(
	"--xargs", "--silent", "--keep-order", "--keeporder", "--no-keep-order", "--nokeeporder", "--nok",
	"--no-k", "--group", "--ungroup", "--latest-line", "--latestline", "--ll", "--linebuffer",
	"--linebuffered", "--line-buffer", "--line-buffered", "--lb", "--tmux", "--tmuxpane", "--null",
	"--quote", "--plus", "--noswap", "--max-line-length-allowed", "--maxlinelengthallowed",
	"--number-of-cpus", "--numberofcpus", "--number-of-sockets", "--numberofsockets", "--number-of-cores",
	"--numberofcores", "--number-of-threads", "--numberofthreads", "--use-sockets-instead-of-threads",
	"--usesocketsinsteadofthreads", "--use-cores-instead-of-threads", "--usecoresinsteadofthreads",
	"--use-cpus-instead-of-cores", "--usecpusinsteadofcores", "--shell-quote", "--shellquote", "--tag",
	"--ctag", "--color", "--colour", "--color-failed", "--colour-failed", "--colorfailed", "--colourfailed",
	"--cf", "--onall", "--nonall", "--filter-hosts", "--filterhosts", "--filter-host", "--controlmaster",
	"--transfer", "--cleanup", "--compress", "--tty", "--dry-run", "--dryrun", "--dr", "--progress",
	"--eta", "--bar", "--shuf", "--recordenv", "--record-env", "--session", "--plain", "--tollef", "--gnu",
	"--link", "--xapply", "--bibtex", "--will-cite", "--willcite", "--nn", "--nonotice", "--no-notice",
	"--resume", "--resume-failed", "--resumefailed", "--retry-failed", "--retryfailed", "--no-run-if-empty",
	"--csv", "--help", "--show-limits", "--showlimits", "--exit", "--semaphore", "--fg", "--bg", "--wait",
	"--shebang", "--hashbang", "--shebang-wrap", "--internal-pipe-means-argfiles", "--skip-first-line",
	"--bug", "--cat", "--fifo", "--pipe-part", "--pipepart", "--tee", "--hgrp", "--hostgrp", "--hostgroup",
	"--hostgroups", "--embed", "--pipe", "--spreadstdin", "--regexp", "--regex", "--remove-rec-sep",
	"--removerecsep", "--rrs", "--files", "--output-as-files", "--outputasfiles", "--files0",
	"--output-as-files0", "--outputasfiles0", "--round-robin", "--roundrobin", "--round", "--keep-timeout",
	"--keeptimeout", "--interactive", "--verbose", "--version",
)

// inputSource is the words one terminator introduces; files: they name files
// of arguments (`::::`).
type inputSource struct {
	words []string
	files bool
}

// splitSources reads the input sources that follow a command template; words
// starts at a terminator.
func splitSources(words []string, invocation wrapperInvocation) []inputSource {
	var sources []inputSource
	for _, word := range words {
		if _, ends := invocation.terminators[word]; ends {
			_, arguments := invocation.arguments[word]
			sources = append(sources, inputSource{files: !arguments})
			continue
		}
		if len(sources) > 0 {
			sources[len(sources)-1].words = append(sources[len(sources)-1].words, word)
		}
	}
	return sources
}

// argumentCombinations lists every combination of one argument from each
// source (a superset of what `:::+` links). A file source, or more
// combinations than the bound, cannot be listed.
func argumentCombinations(sources []inputSource) ([][]string, bool) {
	if len(sources) == 0 {
		return nil, false
	}
	combinations := [][]string{nil}
	for _, source := range sources {
		if source.files || len(source.words) == 0 {
			return nil, false
		}
		var next [][]string
		for _, combination := range combinations {
			for _, argument := range source.words {
				if len(next) >= maxArgumentCommandLines {
					return nil, false
				}
				next = append(next, append(append([]string(nil), combination...), argument))
			}
		}
		combinations = next
	}
	return combinations, true
}

// argumentCommandLines lists the command lines a wrapper with no command runs:
// each combination of arguments, joined by spaces.
func argumentCommandLines(sources []inputSource) ([]string, bool) {
	combinations, ok := argumentCombinations(sources)
	lines := make([]string, len(combinations))
	for i, combination := range combinations {
		lines[i] = strings.Join(combination, " ")
	}
	return lines, ok
}

// parallelPlaceholder matches the replacement strings GNU parallel itself
// substitutes: `{}`, `{= expr =}`, and a brace group whose content begins with
// a digit or one of `. / # % + : ^ ,` (`{1}`, `{.}`, `{//}`, `{#}`). Neither
// `${NAME}`, which the shell expands, nor a brace group of letters is one.
var parallelPlaceholder = regexp.MustCompile(`(^|[^$])\{([0-9./#%+:^,][^\s{}]*)?\}|\{=.*=\}`)

// templateCommandLines lists the command lines a template runs with literal
// arguments, as GNU parallel composes them: each argument shell-quoted, put in
// place of `{}` (all of a combination's arguments), `{N}` (the Nth source's)
// and the strings -I names, or appended when the template holds no
// replacement string (`parallel rm -rf ::: /` runs `rm -rf /`). Other
// replacement strings (`{.}`, `{//}`, `{= expr =}`) are left as written. A
// line the shell cannot parse (`{ cmd; } arg`) runs nothing and is left out.
func templateCommandLines(template []string, sources []inputSource, replaced replacementSet) ([]string, bool) {
	combinations, ok := argumentCombinations(sources)
	if !ok {
		return nil, false
	}
	line := strings.Join(template, " ")
	placeholders := replaced.braces && parallelPlaceholder.MatchString(line)
	for _, word := range replaced.words {
		placeholders = placeholders || (word != "" && strings.Contains(line, word))
	}
	lines := make([]string, 0, len(combinations))
	for _, combination := range combinations {
		quoted := make([]string, len(combination))
		for i, argument := range combination {
			word, err := syntax.Quote(argument, syntax.LangBash)
			if err != nil {
				return nil, false
			}
			quoted[i] = word
		}
		all := strings.Join(quoted, " ")
		composed := line + " " + all
		if placeholders {
			composed = line
			for _, word := range replaced.words {
				composed = strings.ReplaceAll(composed, word, all)
			}
			if replaced.braces {
				composed = strings.ReplaceAll(composed, "{}", all)
				for i, word := range quoted {
					composed = strings.ReplaceAll(composed, "{"+strconv.Itoa(i+1)+"}", word)
				}
			}
		}
		if _, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(composed), ""); err == nil {
			lines = append(lines, composed)
		}
	}
	return lines, true
}
