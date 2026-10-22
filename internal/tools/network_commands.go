package tools

import (
	"regexp"
	"strings"
)

// Which commands reach a remote service.
//
// The daemon's network is shared with an execution call only when the call needs
// it, and a permission to share it (a trusted workspace, a stored or run
// network:shared grant) is never the reason. This file is the "needs it" side:
// one predicate, `commandNamesRemoteService`, that answers from the command
// itself, without running it.
//
// The answer is data. Each program names the subcommands that fetch from or
// publish to a remote registry, VCS remote, cluster or cloud API; everything
// else the program does (build, test, lint, format, inspect, commit) is local.
// A program the table does not know is not a remote service by name: the
// post-failure path covers it, and the explicit `network: true` request covers
// the model that already knows better. Adding a tool is a change to this table,
// not a branch in the capability middleware.
//
// Commands are read through the shared command identification, so assignments,
// wrappers (sudo, env, nohup, xargs, sh -c), paths, case and compound commands
// read as the shell reads them: a compound command names a remote service when
// any command in it does.

// networkVerb is one subcommand path of a program that reaches a remote service.
type networkVerb struct {
	path []string
	// flags must all appear among the invocation's options. `go list` is local;
	// `go list -m -u` asks the module proxy for updates.
	flags []string
	// unlessFlags: the verb is local when any of these appears. `cargo install
	// --path .` builds the crate in front of you, as `cargo build` does.
	unlessFlags []string
	// operandContains: the verb reaches a remote service only when some operand
	// contains it. `go install pkg@version` fetches a module; `go install
	// ./cmd/tool` builds the current one, as `go build` does.
	operandContains string
	// needsOperand: the verb reaches a remote service only when it names
	// something to fetch. `npm init` alone writes a package.json; `npm init
	// vite@latest` runs a package it downloads.
	needsOperand bool
	// localRemote: the verb takes its remote as the first operand, and is local
	// when that operand is a path on this machine (a clone of ../repo, a fetch
	// from /srv/mirror.git, a push to a file:// URL).
	localRemote bool
	// valueOptions are this verb's own options that take a separate value word,
	// so a clone with `--depth 1` reads the path after it as the remote, not `1`.
	valueOptions map[string]struct{}
	// widenedBy: options that make the transfer reach more than its first
	// operand, so a local path there is not the whole of it: a clone that
	// initialises submodules (which have remotes of their own), a fetch of several
	// remotes. `=no` and `=false` turn them off.
	widenedBy []string
}

// networkRule says which invocations of one program reach a remote service.
type networkRule struct {
	// all: every invocation does, because the program is a client of a remote
	// service by purpose (kubectl, ansible, a cloud CLI).
	all bool
	// valueOptions are the program's own options that take a separate value word,
	// so the subcommand is found after them (`git -C repo push`). An option not
	// listed is read as a flag without a value.
	valueOptions map[string]struct{}
	// verbs are the remote-reaching subcommand paths.
	verbs []networkVerb
	// optionVerbs are option spellings that are themselves the operation, as in
	// pacman's -S (sync) and -Syu.
	optionVerbs []string
	// toolchainWords: a leading `+toolchain` word selects a toolchain and is not
	// the subcommand (`cargo +nightly install`).
	toolchainWords bool
	// bareInstalls: the program with no subcommand installs (`yarn` is
	// `yarn install`), unless it was only asked its version or for help.
	bareInstalls bool
	// networkFlags: an invocation carrying any of these options connects to a
	// host (`psql -h db.example.com`), and one with a connection URI or a
	// `host=` setting does too. Without them the client uses a local socket.
	networkFlags []string
	// offlineFlags: the program's own switches that make the invocation use no
	// network (`npm install --offline`, `pip install --no-index`). Only a flag
	// that means strictly offline belongs here: `--prefer-offline` still goes to
	// the network on a cache miss.
	offlineFlags []string
	// wraps: the program runs another program named by its first operand
	// (`corepack yarn install`), and reaches a remote service when that does.
	wraps bool
	// runner: the program's first operand is a program it runs (`npx pkg`), and
	// every option after that operand belongs to the program it runs.
	runner bool
	// runnerVerbs: subcommands whose next operand is a program they run (`docker
	// run alpine`, `pnpm dlx tool`). Options after it are that program's.
	runnerVerbs []string
	// scriptVerbs: subcommands that run a package script named by the next
	// operand (`npm run lint`); bareScripts: the program runs a script by its
	// bare name (`yarn lint`). Used by the fetching table, which sets aside the
	// scripts that only read the sources.
	scriptVerbs []string
	bareScripts bool
}

func verbs(words ...string) []networkVerb {
	out := make([]networkVerb, 0, len(words))
	for _, word := range words {
		out = append(out, networkVerb{path: []string{word}})
	}
	return out
}

func verbPaths(paths ...string) []networkVerb {
	out := make([]networkVerb, 0, len(paths))
	for _, path := range paths {
		out = append(out, networkVerb{path: strings.Fields(path)})
	}
	return out
}

func joinVerbs(groups ...[]networkVerb) []networkVerb {
	var out []networkVerb
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

// networkRules is the table, keyed by program name as identification reads it
// (lowercase, path and escapes removed).
var networkRules = map[string]networkRule{
	// JavaScript package managers.
	"npm": {
		valueOptions: wordSet("--prefix", "-C", "--workspace", "-w", "--registry", "--cache", "--loglevel", "--userconfig", "--globalconfig", "--tag", "--otp"),
		offlineFlags: []string{"--offline"},
		runnerVerbs:  []string{"exec", "x"},
		verbs: joinVerbs(verbs("install", "i", "add", "ci", "clean-install", "install-test", "it", "update", "up", "upgrade", "publish",
			"view", "v", "info", "show", "audit", "outdated", "search", "ping", "login", "adduser", "logout"),
			// `npm init` alone writes a package.json; with an initializer it runs a
			// package it downloads, as does its alias `create`.
			[]networkVerb{{path: []string{"init"}, needsOperand: true}, {path: []string{"create"}, needsOperand: true}}),
	},
	"yarn": {
		bareInstalls: true,
		valueOptions: wordSet("--cwd", "--cache-folder", "--modules-folder", "--registry"),
		offlineFlags: []string{"--offline"},
		runnerVerbs:  []string{"dlx", "exec"},
		verbs: joinVerbs(verbs("install", "add", "upgrade", "up", "upgrade-interactive", "publish", "info", "audit", "outdated", "dlx", "login"),
			[]networkVerb{{path: []string{"create"}, needsOperand: true}}),
	},
	"pnpm": {
		valueOptions: wordSet("--filter", "-F", "--dir", "-C", "--registry"),
		offlineFlags: []string{"--offline"},
		runnerVerbs:  []string{"dlx", "exec"},
		verbs: joinVerbs(verbs("install", "i", "add", "update", "up", "upgrade", "publish", "view", "info", "audit", "outdated", "dlx", "fetch", "login"),
			[]networkVerb{{path: []string{"create"}, needsOperand: true}}),
	},
	// Programs that run a package they download when it is not already there.
	// `--no-install` (`--no`) runs only what is already installed.
	"npx":  {all: true, runner: true, offlineFlags: []string{"--offline", "--no-install", "--no"}},
	"bunx": {all: true, runner: true},
	"uvx":  {all: true, runner: true, offlineFlags: []string{"--offline"}},
	"bun": {
		runnerVerbs: []string{"x"},
		verbs:       verbs("install", "i", "add", "a", "update", "upgrade", "create", "x", "publish", "outdated"),
	},
	// Node's package-manager shim downloads the manager it is asked for, and runs
	// it: `corepack yarn install` is yarn's install.
	"corepack": {wraps: true, verbs: verbs("prepare", "use", "install", "up", "pack")},
	// Python.
	"pip": {
		valueOptions: wordSet("--proxy", "--retries", "--timeout", "--cache-dir", "--log", "--python", "--cert", "--client-cert", "--exists-action", "--trusted-host"),
		offlineFlags: []string{"--no-index"},
		verbs:        verbs("install", "download", "wheel"),
	},
	"uv": {
		offlineFlags: []string{"--offline", "--no-index"},
		runnerVerbs:  []string{"run"},
		verbs: joinVerbs(verbs("add", "sync", "lock", "publish"),
			verbPaths("pip install", "pip download", "pip sync", "pip compile", "tool install", "tool upgrade", "tool run", "python install", "self update"),
			// `uv run` syncs the environment from the index first, unless told not to.
			[]networkVerb{{path: []string{"run"}, unlessFlags: []string{"--no-sync"}}}),
	},
	"poetry": {
		verbs: joinVerbs(verbs("install", "add", "update", "lock", "publish", "search"), verbPaths("self update", "self add")),
	},
	"pipx": {
		runnerVerbs: []string{"run"},
		verbs:       verbs("install", "run", "upgrade", "upgrade-all", "inject", "reinstall", "reinstall-all"),
	},
	"conda":      {offlineFlags: []string{"--offline"}, verbs: joinVerbs(verbs("install", "create", "update", "upgrade", "search"), verbPaths("env create", "env update"))},
	"mamba":      {offlineFlags: []string{"--offline"}, verbs: joinVerbs(verbs("install", "create", "update", "upgrade", "search"), verbPaths("env create", "env update"))},
	"micromamba": {offlineFlags: []string{"--offline"}, verbs: joinVerbs(verbs("install", "create", "update", "upgrade", "search"), verbPaths("env create", "env update"))},
	// Go, Rust, Ruby, PHP.
	"go": {
		verbs: joinVerbs(verbs("get"), verbPaths("mod download"), []networkVerb{
			{path: []string{"list"}, flags: []string{"-m", "-u"}},
			{path: []string{"install"}, operandContains: "@"},
		}),
	},
	"cargo": {
		toolchainWords: true,
		valueOptions:   wordSet("--manifest-path", "--config", "-Z", "--color"),
		// `--frozen` is `--locked` and `--offline`.
		offlineFlags: []string{"--offline", "--frozen"},
		verbs: joinVerbs(verbs("fetch", "update", "publish", "search", "add", "vendor", "generate-lockfile", "login", "yank", "owner"),
			// `--path` builds the crate in front of you; `--list` prints what is installed.
			[]networkVerb{{path: []string{"install"}, unlessFlags: []string{"--path", "--list"}}}),
	},
	"rustup": {
		verbs: joinVerbs(verbs("update", "check", "install"), verbPaths("toolchain install", "target add", "component add", "self update")),
	},
	"gem":      {offlineFlags: []string{"--local"}, verbs: verbs("install", "i", "update", "push", "fetch", "search", "yank", "outdated")},
	"bundle":   {offlineFlags: []string{"--local"}, verbs: verbs("install", "i", "update", "outdated", "add", "lock")},
	"composer": {verbs: verbs("install", "i", "update", "u", "upgrade", "require", "req", "create-project", "outdated", "audit", "self-update", "selfupdate")},
	"dotnet":   {verbs: verbPaths("restore", "add package", "tool install", "tool update")},
	// System package managers.
	"brew":     {verbs: verbs("install", "reinstall", "update", "upgrade", "fetch", "tap")},
	"apt":      {verbs: verbs("install", "update", "upgrade", "dist-upgrade", "full-upgrade", "reinstall", "download", "source", "build-dep", "satisfy")},
	"apt-get":  {verbs: verbs("install", "update", "upgrade", "dist-upgrade", "full-upgrade", "reinstall", "download", "source", "build-dep", "satisfy")},
	"aptitude": {verbs: verbs("install", "update", "upgrade", "safe-upgrade", "full-upgrade", "reinstall", "download", "build-dep")},
	"dnf": {verbs: verbs("install", "reinstall", "update", "upgrade", "upgrade-minimal", "distro-sync", "downgrade", "makecache",
		"check-update", "download", "builddep", "groupinstall", "localinstall")},
	"yum": {verbs: verbs("install", "reinstall", "update", "upgrade", "upgrade-minimal", "distro-sync", "downgrade", "makecache",
		"check-update", "download", "builddep", "groupinstall", "localinstall")},
	"apk":    {verbs: verbs("add", "update", "upgrade", "fetch")},
	"pacman": {optionVerbs: []string{"-S", "--sync"}},
	// Version control remotes.
	"git": {
		valueOptions: wordSet("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env"),
		verbs: joinVerbs(
			// A remote that is a path on this machine is not a remote service.
			[]networkVerb{
				{path: []string{"clone"}, localRemote: true, valueOptions: gitCloneValueOptions, widenedBy: []string{"--recurse-submodules", "--recursive"}},
				{path: []string{"fetch"}, localRemote: true, valueOptions: gitFetchValueOptions, widenedBy: []string{"--multiple", "--all", "--recurse-submodules"}},
				{path: []string{"pull"}, localRemote: true, valueOptions: gitPullValueOptions, widenedBy: []string{"--all", "--recurse-submodules"}},
				{path: []string{"push"}, localRemote: true, valueOptions: gitPushValueOptions, widenedBy: []string{"--recurse-submodules"}},
				{path: []string{"ls-remote"}, localRemote: true},
			},
			verbPaths("remote update", "remote prune", "submodule update", "submodule add", "lfs pull", "lfs push", "lfs fetch"),
			// `git remote show origin` asks the remote; `-n` reads the local record.
			[]networkVerb{{path: []string{"remote", "show"}, unlessFlags: []string{"-n"}}}),
	},
	"hg":  {verbs: verbs("clone", "pull", "push", "incoming", "outgoing", "fetch")},
	"svn": {verbs: verbs("update", "up", "checkout", "co", "commit", "ci", "switch", "sw", "import", "relocate")},
	// Infrastructure as code, containers and clusters.
	"terraform": {verbs: verbs("init", "plan", "apply", "destroy", "refresh", "import", "providers", "get", "login")},
	"tofu":      {verbs: verbs("init", "plan", "apply", "destroy", "refresh", "import", "providers", "get", "login")},
	"pulumi":    {verbs: verbs("up", "preview", "destroy", "refresh", "import", "login", "new")},
	"docker": {
		valueOptions: wordSet("-H", "--host", "-c", "--context", "--config", "-l", "--log-level"),
		runnerVerbs:  []string{"run", "exec"},
		verbs:        joinVerbs(verbs("pull", "push", "login", "search"), verbPaths("compose pull", "compose push")),
	},
	"podman": {
		valueOptions: wordSet("-c", "--connection", "-r", "--remote", "--url", "--log-level"),
		runnerVerbs:  []string{"run", "exec"},
		verbs:        joinVerbs(verbs("pull", "push", "login", "search"), verbPaths("compose pull", "compose push")),
	},
	"helm": {
		valueOptions: wordSet("-n", "--namespace", "--kube-context", "--kubeconfig"),
		verbs:        joinVerbs(verbs("repo", "pull", "push", "install", "upgrade", "search"), verbPaths("dependency update", "dependency build", "dep update", "dep build")),
	},
	// Programs that are a client of a remote service by purpose.
	"kubectl":          {all: true},
	"oc":               {all: true},
	"ansible":          {all: true},
	"argocd":           {all: true},
	"az":               {all: true},
	"glab":             {all: true},
	"aliyun":           {all: true},
	"doctl":            {all: true},
	"flyctl":           {all: true},
	"heroku":           {all: true},
	"vercel":           {all: true},
	"supabase":         {all: true},
	"ansible-playbook": {all: true},
	"ansible-pull":     {all: true},
	"ansible-galaxy":   {all: true},
	"ansible-console":  {all: true},
	// Network diagnostics: their whole purpose is a remote host.
	"ping":     {all: true},
	"dig":      {all: true},
	"nslookup": {all: true},
	// Database clients connect to a host when told one; without it they use a
	// local socket.
	"psql":      {networkFlags: []string{"-h", "--host"}},
	"mysql":     {networkFlags: []string{"-h", "--host"}},
	"mariadb":   {networkFlags: []string{"-h", "--host"}},
	"redis-cli": {networkFlags: []string{"-h", "--host"}},
}

// The options of a git transfer command that take a separate value word. They
// matter to one question only: which word is the remote.
var (
	gitFetchValueOptions = wordSet("--depth", "--deepen", "--shallow-since", "--shallow-exclude", "--jobs", "-j", "--filter",
		"--server-option", "-o", "--upload-pack", "--refmap", "--negotiation-tip")
	gitPullValueOptions  = wordSet("--depth", "--deepen", "--shallow-since", "--shallow-exclude", "--jobs", "-j", "--filter", "--server-option", "-o", "--upload-pack", "--strategy", "-s", "--strategy-option", "-X")
	gitCloneValueOptions = wordSet("--depth", "--shallow-since", "--shallow-exclude", "--jobs", "-j", "--filter", "--server-option", "-o", "--origin",
		"--branch", "-b", "--reference", "--reference-if-able", "--template", "--separate-git-dir", "--config", "-c", "--upload-pack", "-u")
	gitPushValueOptions = wordSet("--receive-pack", "--exec", "--push-option", "-o", "--repo")
)

// shortHIsNotHelp lists programs whose `-h` is something else: the host of a
// database client, rsync's human-readable sizes, the hostname of a container.
var shortHIsNotHelp = wordSet("psql", "mysql", "mariadb", "redis-cli", "rsync", "docker", "podman")

var pipProgramName = regexp.MustCompile(`^pip[0-9]*(\.[0-9]+)*$`)

// networkRuleFor resolves a program name to its rule. pip and its versioned
// spellings (pip3, pip3.12) share one; `yarnpkg` is yarn.
func networkRuleFor(name string) (networkRule, bool) {
	if rule, ok := networkRules[name]; ok {
		return rule, true
	}
	switch {
	case pipProgramName.MatchString(name):
		return networkRules["pip"], true
	case name == "yarnpkg":
		return networkRules["yarn"], true
	}
	return networkRule{}, false
}

// pythonModuleRules are the modules `python -m <module>` runs as the command of
// the same name.
var pythonModuleRules = wordSet("pip", "uv", "poetry")

// pythonModuleInvocation reads `python [options] -m module args...` and returns
// the module and the words after it. A script or `-c` code before `-m` means
// the `-m` belongs to that program, not to the interpreter.
func pythonModuleInvocation(args []string) (module string, rest []string, ok bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-m":
			if i+1 >= len(args) {
				return "", nil, false
			}
			return strings.ToLower(args[i+1]), args[i+2:], true
		case strings.HasPrefix(arg, "-m") && len(arg) > 2:
			return strings.ToLower(arg[2:]), args[i+1:], true
		case arg == "-W" || arg == "-X":
			i++
		case strings.HasPrefix(arg, "-"):
			if arg == "-c" {
				return "", nil, false
			}
		default:
			return "", nil, false
		}
	}
	return "", nil, false
}

// optionVerbMatches reports whether an option spelling is the operation: a
// short option is the operation by prefix (`-Syu` is `-S`), a long one exactly.
func optionVerbMatches(option, operation string) bool {
	if option == operation {
		return true
	}
	return !strings.HasPrefix(operation, "--") && !strings.HasPrefix(option, "--") && strings.HasPrefix(option, operation)
}

// reachesRemoteService reports whether one program invocation, its words after
// the program name, is a known remote-reaching command that is not an explicit
// offline or local form of it (offline_forms.go).
func reachesRemoteService(name string, args []string) bool {
	if offlineForm(name, args) {
		return false
	}
	if pythonProgram(name) {
		// `python -m pip install` is pip's install.
		module, rest, ok := pythonModuleInvocation(args)
		if !ok {
			return false
		}
		if _, known := pythonModuleRules[module]; !known {
			return false
		}
		return reachesRemoteService(module, rest)
	}
	rule, ok := networkRuleFor(name)
	if !ok {
		return false
	}
	if rule.all {
		return true
	}
	words, flags := networkSubcommand(rule, args)
	if rule.bareInstalls && len(words) == 0 && !flags["--version"] && !flags["-v"] && !flags["--help"] && !flags["-h"] {
		return true
	}
	for _, operation := range rule.optionVerbs {
		for option := range flags {
			if optionVerbMatches(option, operation) {
				return true
			}
		}
	}
	for _, flag := range rule.networkFlags {
		for option := range flags {
			if optionVerbMatches(option, flag) {
				return true
			}
		}
	}
	if len(rule.networkFlags) > 0 && namesConnectionTarget(args) {
		return true
	}
	if rule.wraps && wrappedProgramReachesRemoteService(args) {
		return true
	}
	for _, verb := range rule.verbs {
		if verb.matches(words, flags) && !verb.remoteIsLocal(rule, args) {
			return true
		}
	}
	return false
}

// namesConnectionTarget reports a database client's connection URI or `host=`
// setting among its arguments.
func namesConnectionTarget(args []string) bool {
	for _, arg := range args {
		lower := strings.ToLower(arg)
		if strings.Contains(lower, "://") || strings.Contains(lower, "host=") {
			return true
		}
	}
	return false
}

// wrappedProgramReachesRemoteService reads `corepack yarn install`: the first
// operand that is not an option names the program that runs.
func wrappedProgramReachesRemoteService(args []string) bool {
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		inner := strings.ToLower(arg)
		if _, known := networkRuleFor(inner); known {
			return reachesRemoteService(inner, args[i+1:])
		}
		return false
	}
	return false
}

// matches reports whether an invocation's words (subcommand path first, then
// operands) and options are this verb.
func (v networkVerb) matches(words []string, flags map[string]bool) bool {
	if len(words) < len(v.path) {
		return false
	}
	for i, word := range v.path {
		if words[i] != word {
			return false
		}
	}
	for _, flag := range v.flags {
		if !flags[flag] {
			return false
		}
	}
	for _, flag := range v.unlessFlags {
		if flags[flag] {
			return false
		}
	}
	if v.needsOperand && len(words) == len(v.path) {
		return false
	}
	if v.operandContains == "" {
		return true
	}
	for _, operand := range words[len(v.path):] {
		if strings.Contains(operand, v.operandContains) {
			return true
		}
	}
	return false
}

// networkSubcommand reads the words of args after the program's own global
// options (the subcommand path first, then operands, lowercased) and every
// option the invocation carries.
func networkSubcommand(rule networkRule, args []string) (words []string, flags map[string]bool) {
	flags = map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "--":
			return words, flags
		case arg == "":
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			name, _, attached := strings.Cut(arg, "=")
			flags[name] = true
			if _, takesValue := rule.valueOptions[name]; takesValue && !attached && len(words) == 0 {
				i++
			}
		case rule.toolchainWords && strings.HasPrefix(arg, "+") && len(words) == 0:
		default:
			words = append(words, strings.ToLower(arg))
		}
	}
	return words, flags
}

// commandNamesRemoteService reports whether any command the execution payload
// runs is a known remote-reaching invocation. It reads the payload the way the
// shell does, so a quoted mention (`echo "git push"`, a commit message) is not
// a command, and falls back to fields only when the payload does not parse.
//
// The payload of execute_code is Python, not shell: reading its strings as
// commands would hand the network to code that merely mentions one, so it is
// never a remote service by name here (its explicit egress programs and
// credential tools are still read by the other two parts of the predicate).
func commandNamesRemoteService(toolName string, args map[string]interface{}) bool {
	if !isExecTool(toolName) || toolName == "execute_code" {
		return false
	}
	payload := strings.TrimSpace(execCommandPayload(toolName, args))
	if payload == "" {
		return false
	}
	for _, read := range preparationCommands(payload, 0) {
		program := read.id.program()
		if program.Name != "" && reachesRemoteService(program.Name, program.Args) {
			return true
		}
	}
	return false
}

// networkRequestedByCall reports an explicit `network: true` on a call. terminal,
// verify and execute_code declare the parameter; no other tool does, and a value
// that is not a boolean true is not a request. It asks for the network and
// grants nothing: permission still comes from a grant, trust under the operator
// policy, or an approval.
func networkRequestedByCall(toolName string, args map[string]interface{}) bool {
	if !networkRequestDeclaredBy(toolName) {
		return false
	}
	requested, _ := args["network"].(bool)
	return requested
}

// networkRequestDeclaredBy reports whether a tool declares the `network`
// parameter. On any other execution tool the argument is not a request, and the
// capability middleware strips it so it cannot read as one downstream (an
// approval's displayed arguments, a fingerprint).
//
// execute_code declares it because its payload is a program of the model's own,
// which no table can read: only the model knows the script calls a remote
// service. Without the parameter the typed "retry with network: true" result
// would send the model into a retry that can never be networked.
func networkRequestDeclaredBy(toolName string) bool {
	return toolName == "terminal" || toolName == "verify" || toolName == "execute_code"
}

// commandWantsNetwork is the one question the capability decision starts from:
// does this call reach a remote service by name, or did it ask for the network.
func commandWantsNetwork(toolName string, args map[string]interface{}) bool {
	return networkRequestedByCall(toolName, args) || commandPlausiblyNeedsEgress(toolName, args)
}
