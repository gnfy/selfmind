package tools

import "strings"

// Read-only proof accepts reviewed option spellings, rather than assuming that
// a familiar program's unknown flags are harmless. Unknown/abbreviated options
// use ordinary approval; none of this grammar grants execution authority.
func observationOperands(args []string, switches, values string, numeric bool) ([]string, bool) {
	flags := map[string]bool{}
	for _, flag := range strings.Fields(switches) {
		flags[flag] = false
	}
	for _, flag := range strings.Fields(values) {
		flags[flag] = true
	}
	var operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(operands, args[i+1:]...), true
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			operands = append(operands, arg)
			continue
		}
		if numeric && len(arg) > 1 && strings.Trim(arg[1:], "0123456789") == "" {
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, _, attached := strings.Cut(arg, "=")
			value, ok := flags[name]
			if !ok || attached && !value {
				return nil, false
			}
			if value && !attached {
				i++
				if i == len(args) {
					return nil, false
				}
			}
			continue
		}
		for j := 1; j < len(arg); j++ {
			value, ok := flags["-"+arg[j:j+1]]
			if !ok {
				return nil, false
			}
			if value {
				if j+1 == len(arg) {
					i++
					if i == len(args) {
						return nil, false
					}
				}
				break
			}
		}
	}
	return operands, true
}

func dateObservationArgs(args []string) bool {
	noSet, conversion := false, false
	var operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-j":
			noSet = true
		case "-u", "-R", "--utc", "--universal", "--rfc-email", "--help", "--version":
		case "-r", "-f", "-v", "-d", "--date", "--reference":
			conversion = conversion || arg == "-f"
			i++
			if i == len(args) {
				return false
			}
		case "--":
			operands = append(operands, args[i+1:]...)
			i = len(args)
		default:
			if strings.HasPrefix(arg, "--date=") || strings.HasPrefix(arg, "--reference=") || strings.HasPrefix(arg, "--iso-8601=") || strings.HasPrefix(arg, "--rfc-3339=") {
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return false
			}
			operands = append(operands, arg)
		}
	}
	// BSD/macOS conversion may set the clock unless -j is explicitly present.
	if conversion && !noSet {
		return false
	}
	for _, operand := range operands {
		if !strings.HasPrefix(operand, "+") && !noSet {
			return false
		}
	}
	return true
}

func sortObservationArgs(args []string) bool {
	_, ok := observationOperands(args, "-b -c -C -d -f -g -h -i -M -n -r -R -s -u -V -z --ignore-leading-blanks --check --dictionary-order --ignore-case --general-numeric-sort --human-numeric-sort --ignore-nonprinting --month-sort --numeric-sort --reverse --stable --unique --version-sort --zero-terminated --help --version",
		"-k -t -S -T --key --field-separator --buffer-size --temporary-directory --parallel --files0-from --sort", false)
	return ok
}

func fileObservationArgs(args []string) bool {
	_, ok := observationOperands(args, "-b -c -d -E -h -i -k -L -l -N -n -p -r -s -v -z -Z -0 --brief --checking-printout --debug --extension --mime --mime-type --mime-encoding --keep-going --dereference --no-dereference --list --no-pad --no-buffer --preserve-date --raw --special-files --version --uncompress --uncompress-noreport --print0 --help",
		"-e -f -F -m -P --exclude --exclude-quiet --files-from --separator --magic-file --parameter", false)
	return ok
}

func rgObservationArgs(args []string) bool {
	_, ok := observationOperands(args, "-a -b -c -F -H -h -i -I -l -L -n -N -o -P -p -q -s -S -U -u -v -w -x -z -0 --text --byte-offset --count --count-matches --fixed-strings --with-filename --no-filename --ignore-case --case-sensitive --smart-case --files-with-matches --files-without-match --line-number --no-line-number --only-matching --pcre2 --pretty --quiet --multiline --invert-match --word-regexp --line-regexp --search-zip --null --null-data --files --hidden --no-ignore --no-ignore-vcs --no-messages --stats --json --heading --no-heading --trim --follow --no-config --help --version",
		"-A -B -C -e -f -g -j -m -M -r -t -T --after-context --before-context --context --regexp --file --glob --iglob --threads --max-count --max-columns --replace --type --type-not --type-add --type-clear --encoding --color --colors --sort --sortr --max-depth --max-filesize --ignore-file --path-separator", false)
	return ok
}

func yqObservationArgs(args []string) bool {
	_, ok := observationOperands(args, "-r -j -y -Y -x -n -N -P -M -C -e -v -0 --raw-output --yaml-output --yaml-roundtrip --xml-output --null-input --no-doc --prettyPrint --no-colors --colors --exit-status --verbose --nul-output --unwrapScalar --help --version",
		"-I -o -p --indent --output-format --input-format --from-file", false)
	return ok
}

func gitObservationArgs(args []string) bool {
	if len(args) > 0 {
		if grammar, ok := gitQueryArguments[args[0]]; ok {
			_, valid := observationOperands(args[1:], grammar.switches, grammar.values, false)
			return valid
		}
	}
	// Prefix matching still decides the subcommand. These are output/query
	// options only: --output, --ext-diff, --textconv and unknown spellings are
	// intentionally absent, including abbreviations accepted by some versions.
	_, ok := observationOperands(args, "-0 -z -p -s -u -w -b -B -C -M -a -r -t -f -v -q --no-pager --no-optional-locks --short --branch --porcelain --stat --numstat --shortstat --name-only --name-status --oneline --no-patch --patch --raw --summary --check --cached --staged --no-color --color --no-ext-diff --no-textconv --no-renames --all --tags --heads --remotes --abbrev-ref --show-toplevel --show-prefix --show-cdup --git-dir --absolute-git-dir --is-inside-work-tree --is-bare-repository --verify --quiet --count --left-right --cherry-pick --first-parent --merges --no-merges --reverse --topo-order --date-order --full-history --exit-code --ancestry-path --boundary --full-name --full-tree --others --exclude-standard --deleted --modified --stage --error-unmatch --object-only --batch-all-objects --follow --help --version",
		"-n -U -S -G -L --max-count --unified --since --until --after --before --author --committer --grep --format --pretty --date --diff-filter --relative --abbrev --encoding --exclude --exclude-from --exclude-per-directory --sort --contains --no-contains --points-at --batch --batch-check", true)
	return ok
}

// Option meaning belongs to the subcommand. In particular grep -n is a
// boolean, whereas log -n consumes a count; treating them alike can swallow a
// dangerous following option. Unknown execution/configuration options fail.
var gitQueryArguments = map[string]struct{ switches, values string }{
	"grep": {
		switches: "-n -i -I -v -w -x -l -L -c -q -h -H -F -E -G -P -a -z --line-number --ignore-case --invert-match --word-regexp --line-regexp --files-with-matches --files-without-match --count --quiet --fixed-strings --extended-regexp --basic-regexp --perl-regexp --text --null --full-name --cached --no-index --untracked --no-exclude-standard --recurse-submodules --no-textconv --no-color --heading --break --all-match --and --or --not",
		values:   "-e -f -A -B -C --after-context --before-context --context --max-depth --threads",
	},
	"rev-list": {
		switches: "--count --left-right --left-only --right-only --cherry-pick --cherry-mark --all --branches --tags --remotes --first-parent --merges --no-merges --reverse --topo-order --date-order --full-history --ancestry-path --boundary --objects --objects-edge --timestamp --header --parents --children --quiet",
		values:   "-n --max-count --skip --since --until --after --before --author --committer --grep --format --pretty --min-parents --max-parents --exclude",
	},
}
