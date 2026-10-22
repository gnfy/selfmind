package tools

import (
	"slices"
	"strings"
)

// Which commands can fetch dependencies when they run.
//
// A check that passes while offline in a workspace that permits the network is
// still a pass, and the person and the model are told when the pass could have
// covered less than it appears to: a build or a test that downloads its
// dependencies on a cold cache may have been spared that by the cache, or never
// have exercised the code that needs them. That sentence is worth reading only
// when the command can fetch. `go vet`, a linter, a formatter, `ls` and `grep`
// cannot, and a notice on every one of them teaches a reader to skip it.
//
// The answer is data: the programs and subcommands below. It is deliberately
// limited to named programs. `make test` may run any of them, but that cannot
// be read from the command, and a notice with no basis is the noise this table
// exists to remove.

// mayFetchRules is keyed like networkRules (program name as identification reads
// it). `all` means every invocation can fetch; `verbs` names the subcommands that
// can, the rest of the program being local.
var mayFetchRules = map[string]networkRule{
	"go": {verbs: verbs("build", "test", "run", "mod", "install", "get")},
	"cargo": {
		toolchainWords: true,
		valueOptions:   wordSet("--manifest-path", "--config", "-Z", "--color"),
		// clippy and doc resolve the dependency graph as a build does.
		verbs: joinVerbs(verbs("build", "b", "test", "t", "check", "c", "run", "r", "bench", "fetch", "clippy", "doc"),
			// Installing the crate in front of you builds it with its dependencies;
			// listing what is installed reads nothing.
			[]networkVerb{{path: []string{"install"}, unlessFlags: []string{"--list"}}}),
	},
	"npm": {
		valueOptions: networkRules["npm"].valueOptions,
		scriptVerbs:  []string{"run", "run-script", "rum", "urn"},
		verbs: verbs("test", "t", "tst", "run", "run-script", "rum", "urn", "start", "restart", "exec", "x", "rebuild",
			"ci", "install", "i", "install-test", "it"),
	},
	// yarn and pnpm run a package script by its bare name (`yarn build`), so
	// every invocation can fetch unless the script is one that only reads.
	"yarn": {all: true, valueOptions: networkRules["yarn"].valueOptions, scriptVerbs: []string{"run"}, bareScripts: true},
	"pnpm": {all: true, valueOptions: networkRules["pnpm"].valueOptions, scriptVerbs: []string{"run", "run-script"}, bareScripts: true},
	"bun":  {scriptVerbs: []string{"run"}, verbs: verbs("test", "install", "i", "add", "run")},
	"pip":  {verbs: verbs("install", "download", "wheel")},
	// poetry, uv and pipx resolve and install on most of what they run.
	"poetry":  {all: true},
	"uv":      {all: true},
	"pipx":    {all: true},
	"tox":     {all: true},
	"nox":     {all: true},
	"mvn":     {all: true, offlineFlags: []string{"-o", "--offline"}},
	"mvnw":    {all: true, offlineFlags: []string{"-o", "--offline"}},
	"gradle":  {all: true, offlineFlags: []string{"--offline"}},
	"gradlew": {all: true, offlineFlags: []string{"--offline"}},
	"sbt":     {all: true},
	// `bundle exec` runs what is already installed; the rest resolves gems.
	"bundle": {verbs: verbs("install", "i", "update", "add", "lock", "outdated")},
	// composer has no offline switch; --prefer-dist and the cache are not one.
	"composer":   {all: true},
	"conda":      {all: true, offlineFlags: []string{"--offline"}},
	"mamba":      {all: true, offlineFlags: []string{"--offline"}},
	"micromamba": {all: true, offlineFlags: []string{"--offline"}},
	"dotnet":     {verbs: verbs("build", "test", "run", "restore")},
	"bazel":      {verbs: verbs("build", "test", "fetch", "run")},
	"deno":       {verbs: verbs("run", "test", "cache", "install")},
	"mix":        {verbs: verbs("deps.get", "test", "compile")},
	"swift":      {verbs: verbs("build", "test", "package")},
	"flutter":    {verbs: joinVerbs(verbs("test", "build"), verbPaths("pub get"))},
}

// localScriptPrefixes name package scripts that only read the sources (lint,
// format, type-check). `npm run lint` fetches nothing, whatever its program is
// able to do.
var localScriptPrefixes = []string{"lint", "format", "fmt", "prettier", "eslint", "stylelint", "typecheck", "type-check", "check-types", "tsc"}

// localPackageScript reports an invocation that runs a package script by a name
// that says it only reads: `npm run lint`, `yarn lint`, `pnpm run format`.
func localPackageScript(rule networkRule, words []string) bool {
	script := ""
	switch {
	case len(words) >= 2 && slices.Contains(rule.scriptVerbs, words[0]):
		script = words[1]
	case len(words) >= 1 && rule.bareScripts:
		script = words[0]
	}
	for _, prefix := range localScriptPrefixes {
		if script != "" && strings.HasPrefix(script, prefix) {
			return true
		}
	}
	return false
}

// offlineFlagsFor lists a program's own strictly-offline switches, from either
// table: a program the remote-reaching table knows (npm, cargo, pip) and a build
// tool only the fetching table knows (mvn -o, gradle --offline).
func offlineFlagsFor(name string) []string {
	var flags []string
	if rule, ok := networkRuleFor(name); ok {
		flags = append(flags, rule.offlineFlags...)
	}
	if rule, ok := mayFetchRuleFor(name); ok {
		flags = append(flags, rule.offlineFlags...)
	}
	return flags
}

// mayFetchRuleFor resolves a program name to its rule, as networkRuleFor does
// for the remote-reaching table.
func mayFetchRuleFor(name string) (networkRule, bool) {
	if rule, ok := mayFetchRules[name]; ok {
		return rule, true
	}
	switch {
	case pipProgramName.MatchString(name):
		return mayFetchRules["pip"], true
	case name == "yarnpkg":
		return mayFetchRules["yarn"], true
	}
	return networkRule{}, false
}

// invocationMayFetch reports whether one program invocation can fetch
// dependencies: a program or subcommand the table names, that is not asked for
// its help or version and not run offline on purpose.
func invocationMayFetch(name string, args []string) bool {
	if offlineForm(name, args) {
		return false
	}
	if pythonProgram(name) {
		// `python -m pip install` is pip's.
		module, rest, ok := pythonModuleInvocation(args)
		if !ok {
			return false
		}
		if _, known := pythonModuleRules[module]; !known {
			return false
		}
		return invocationMayFetch(module, rest)
	}
	rule, ok := mayFetchRuleFor(name)
	if !ok {
		return false
	}
	words, flags := networkSubcommand(rule, args)
	if localPackageScript(rule, words) {
		return false
	}
	if rule.all {
		return true
	}
	for _, verb := range rule.verbs {
		if verb.matches(words, flags) {
			return true
		}
	}
	return false
}

// CommandMayFetchDependencies reports whether a shell command, read the way the
// shell reads it, runs a program that can fetch dependencies on a cold cache.
// It is the question the offline-verification notice asks (a check that ran
// without network although its workspace permits it), asked of the command text
// alone so that evidence recorded earlier answers it the same way.
func CommandMayFetchDependencies(command string) bool {
	payload := strings.TrimSpace(command)
	if payload == "" {
		return false
	}
	for _, read := range preparationCommands(payload, 0) {
		program := read.id.program()
		if program.Name != "" && invocationMayFetch(program.Name, program.Args) {
			return true
		}
	}
	return false
}
