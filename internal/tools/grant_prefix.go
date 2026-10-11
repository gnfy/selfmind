package tools

import (
	"regexp"
	"sort"
	"strings"
)

// Reusable-grant PREFIX derivation.
//
// A remembered decision keys on a leading token prefix of the command, not on
// the whole command: `aws codebuild batch-get-builds --ids <a new id every
// time>` must match the same rule tomorrow, or the stored set grows by one row
// per invocation and has to be curated by hand. codex stores the same shape
// (`prefix_rule(pattern=[...], decision="allow")`).
//
// Two questions decide how long the prefix is, and both were settled against a
// week of this deployment's real approvals rather than by taste:
//
//   - Too short conflates reads with writes. A two-token prefix put
//     `aws codebuild start-build` in the same class as
//     `aws codebuild batch-get-builds`, and `gcloud builds import` with
//     `gcloud builds describe`.
//   - Too long is one class per invocation, because the trailing tokens are
//     build ids and resource names.
//
// Taking tokens up to and INCLUDING the first verb-shaped one produced 144
// classes over that week with zero classes mixing a read verb and a write verb,
// while a flat four-token cut produced 161 with the same safety and a flat
// two-token cut produced 24 with two unsafe ones.

// grantPrefixMaxTokens bounds how far the search for a verb looks. Five covers
// the deepest subcommand path in this deployment's traffic
// (`gcloud artifacts docker tags list`).
const grantPrefixMaxTokens = 5

// grantSubcommandPrograms dispatch on a subcommand path, so the program name
// alone names nothing a person could consent to: `gcloud` would cover both
// `gcloud builds describe` and `gcloud builds import`. For these, a prefix that
// does not reach a verb is not grant-eligible at all — failing closed costs one
// approval and removes a whole class of over-broad grant, which is the same
// trade the control-keyword rule already makes.
//
// Everything else is a plain command whose name IS its class (`chmod`,
// `mkdir`), and taking its arguments into the prefix would mint one class per
// invocation.
var grantSubcommandPrograms = map[string]struct{}{
	"aws": {}, "gcloud": {}, "gsutil": {}, "bq": {}, "az": {}, "gh": {}, "glab": {},
	"kubectl": {}, "helm": {}, "argocd": {}, "docker": {}, "podman": {}, "terraform": {},
	"aliyun": {}, "doctl": {}, "flyctl": {}, "heroku": {}, "supabase": {}, "vercel": {},
}

// grantWriteVerbs and grantReadVerbs are the subcommand verbs these CLIs end a
// subcommand path with. The set is data, not a vendor branch: an unmatched verb
// simply falls back to the token bound above. Membership decides only where the
// prefix ENDS — it never decides whether something may run.
var grantWriteVerbs = regexp.MustCompile(`^(create|delete|start|stop|submit|update|put|remove|add|set|apply|destroy|terminate|deploy|import|restore|attach|detach|enable|disable|rotate|revoke|grant|kill|drain|cordon|scale|rollout|edit|patch|upgrade|uninstall|install|push|publish|tag|promote|abort|cancel)(-|$)`)

var grantReadVerbs = regexp.MustCompile(`^(describe|list|get|view|status|show|read|batch-get|log|logs|history|search|check|validate|diff|inspect|top|explain)(-|$)`)

// apiReadMethods are the HTTP verbs that only read. Anything else — including a
// verb this parser cannot resolve — is a mutation as far as approval is
// concerned.
var apiReadMethods = map[string]struct{}{"get": {}, "head": {}}

// ghAPIValueOptions are `gh api`'s options that take a value, by each
// spelling, with the meaning that matters here. ghAPIBoolOptions take none.
var (
	ghAPIValueOptions = map[string]string{
		"-X": "method", "--method": "method", "--hostname": "host",
		"-f": "body", "--raw-field": "body", "-F": "body", "--field": "body", "--input": "body",
		"-H": "", "--header": "", "-q": "", "--jq": "", "-p": "", "--preview": "",
		"-t": "", "--template": "", "--cache": "",
	}
	ghAPIBoolOptions = wordSet("-i", "--include", "--paginate", "--silent", "--slurp", "--verbose", "-h", "--help")
)

// ghAPICall is the request a `gh api` call makes.
type ghAPICall struct {
	endpoint string
	// method is the verb sent, lowercased: the one --method names, else POST
	// when the call sends a body (a field or --input), else GET.
	method string
	body   bool
	// host: --hostname sends the call, and an enterprise token, elsewhere.
	host bool
}

// ghAPIRequest reads the words after `gh api` the way gh's flag parser
// (pflag) does: short options bundle (`-if title=x`), a value is attached
// (`-ftitle=x`, `-XPOST`, `-X=POST`), after `=` (`--method=POST`) or the next
// word, and options and the endpoint may come in any order.
//
// The observation catalog and the class derivation both ask this one parser,
// so a spelling either parses for both or fails for both: they once parsed the
// verb separately, and `-X=DELETE`, `-XDELETE` and a bundled `-f` each read as
// a GET in one of them. ok is false for an option gh api does not have or a
// value that cannot be resolved; callers fail closed.
func ghAPIRequest(args []string) (ghAPICall, bool) {
	var call ghAPICall
	explicit, methodGiven := "", false
	apply := func(meaning, value string) {
		switch meaning {
		case "method":
			explicit, methodGiven = value, true
		case "body":
			call.body = true
		case "host":
			call.host = true
		}
	}
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "--" {
			if call.endpoint == "" && i+1 < len(args) {
				call.endpoint = args[i+1]
			}
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if call.endpoint == "" {
				call.endpoint = arg
			}
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, value, attached := strings.Cut(arg, "=")
			if _, flag := ghAPIBoolOptions[name]; flag {
				continue
			}
			meaning, known := ghAPIValueOptions[name]
			if !known {
				return ghAPICall{}, false
			}
			if !attached {
				if i+1 >= len(args) {
					return ghAPICall{}, false
				}
				i++
				value = args[i]
			}
			apply(meaning, value)
			continue
		}
		for j := 1; j < len(arg); j++ {
			name := "-" + arg[j:j+1]
			if _, flag := ghAPIBoolOptions[name]; flag {
				continue
			}
			meaning, known := ghAPIValueOptions[name]
			if !known {
				return ghAPICall{}, false
			}
			value := strings.TrimPrefix(arg[j+1:], "=")
			if j+1 == len(arg) {
				if i+1 >= len(args) {
					return ghAPICall{}, false
				}
				i++
				value = args[i]
			}
			apply(meaning, value)
			break
		}
	}
	switch {
	case methodGiven:
		call.method = strings.ToLower(strings.Trim(strings.TrimSpace(explicit), `"'`))
		if call.method == "" || strings.HasPrefix(call.method, "-") {
			return ghAPICall{}, false
		}
	case call.body:
		call.method = "post"
	default:
		call.method = "get"
	}
	return call, true
}

// grantAPISubcommands names the (program, subcommand) pairs whose class must
// carry the HTTP method and a bounded resource path.
var grantAPISubcommands = map[string]string{"gh": "api"}

// grantPlumbingPrograms only transform their own input and reach no service or
// resource of their own. They are skipped when deriving a prefix, because
// otherwise a single `| head` becomes the remembered class — over one week of
// real traffic the naive derivation produced 621 classes for 478 approvals, of
// which 461 were used once, because pipeline plumbing dominated it.
//
// This is a strict subset of the observation catalog, and the difference is the
// point: `gcloud builds list` is also provable read-only, but it REACHES a
// production service, so it is an operation and must contribute a class. Only
// programs that would be meaningless to name in a permission belong here.
var grantPlumbingPrograms = map[string]struct{}{
	"cd": {}, "set": {}, "export": {}, "umask": {}, "true": {}, "false": {},
	"test": {}, "[": {}, ":": {}, "which": {}, "pwd": {}, "echo": {}, "printf": {}, "sleep": {},
	"ls": {}, "cat": {}, "head": {}, "tail": {}, "wc": {}, "stat": {}, "file": {},
	"readlink": {}, "realpath": {}, "basename": {}, "dirname": {},
	"grep": {}, "rg": {}, "jq": {}, "yq": {}, "tr": {}, "cut": {}, "nl": {}, "seq": {},
	"paste": {}, "comm": {}, "column": {}, "diff": {}, "od": {},
	"shasum": {}, "sha256sum": {}, "md5": {}, "md5sum": {}, "sort": {}, "base64": {}, "date": {},
}

// grantCommandPrefix derives the token prefix a reusable grant would key on, or
// reports ineligible. Pipeline plumbing is skipped; every remaining segment is
// an operation and they must all agree, so a payload that both reads a service
// and creates a directory still has no single class that describes it.
func grantCommandPrefix(toolName string, args map[string]interface{}) ([]string, bool) {
	if strings.EqualFold(strings.TrimSpace(toolName), "execute_code") {
		return nil, false
	}
	payload := strings.TrimSpace(execCommandPayload(toolName, args))
	if payload == "" {
		return nil, false
	}
	segments, ok := classSegments(payload, leaseProcessEnv(args))
	if !ok {
		return nil, false
	}
	var prefix []string
	for _, fields := range segments {
		progIdx, ok := segmentProgram(fields)
		if !ok {
			return nil, false
		}
		base := programName(strings.TrimSpace(fields[progIdx]))
		if base == "" {
			return nil, false
		}
		if _, control := shellControlKeywords[base]; control {
			return nil, false
		}
		if _, neutral := shellNeutralWords[base]; neutral {
			continue
		}
		if _, banned := bannedGrantPrograms[base]; banned {
			return nil, false
		}
		// Plumbing is skipped only as the observation catalog proves it: by its
		// exact name, never a path or another spelling that may be a different
		// program (`./ls`, `LS`).
		if _, plumbing := grantPlumbingPrograms[base]; plumbing && fields[progIdx] == base && segmentIsBaselineObservation(base, fields[progIdx:]) {
			continue
		}
		candidate := grantSegmentPrefix(base, fields[progIdx+1:])
		if len(candidate) == 0 {
			return nil, false
		}
		bound, ok := boundSelectors(base, fields[progIdx+1:])
		if !ok {
			return nil, false
		}
		candidate = append(candidate, bound...)
		if prefix == nil {
			prefix = candidate
			continue
		}
		if !samePrefix(prefix, candidate) {
			// Several distinct operations in one payload: no single prefix
			// describes what a grant would authorise.
			return nil, false
		}
	}
	if len(prefix) == 0 {
		return nil, false
	}
	return prefix, true
}

// classSegments lists the argv of each command a payload runs when a
// remembered class or rule can stand for it, the field reading's words that
// grantCommandPrefix and approvalCommandPrefix derive keys from. Both readings
// must agree that nothing but the named programs decides what runs: the
// payload is literal, every program and wrapper word is its exact bare name
// (`./kubectl`, `bin/kubectl` and `KUBECTL` may be other files), no assignment
// changes what the run's environment holds, and no option selects another
// configuration or credentials.
func classSegments(payload string, env []string) ([][]string, bool) {
	for _, marker := range grantComplexShellMarkers {
		if strings.Contains(payload, marker) {
			return nil, false
		}
	}
	lower := strings.ToLower(payload)
	for _, marker := range grantDeviceMarkers {
		if strings.Contains(lower, marker) {
			return nil, false
		}
	}
	return classCommands(payload, env)
}

// classCommands lists the field reading's argv for each command a payload
// runs when every command either reading finds can be named by a class or a
// rule (classStandsFor).
func classCommands(payload string, env []string) ([][]string, bool) {
	var segments [][]string
	for _, read := range fieldCommands(payload, 0) {
		if !classStandsFor(read.id, env) {
			return nil, false
		}
		if argv := read.id.program().argv(); len(argv) > 0 && !read.id.HasScript {
			segments = append(segments, argv)
		}
	}
	shell, _ := shellCommands(payload, 0)
	for _, read := range shell {
		if !classStandsFor(read.id, env) {
			return nil, false
		}
	}
	return segments, true
}

// classStandsFor reports whether a class can name one command a reading found.
func classStandsFor(id commandIdentity, env []string) bool {
	if id.Opaque || id.redirectsProgramResolution() || id.boundsNoClass() {
		return false
	}
	for _, layer := range id.Layers {
		if layer.Word != "" && layer.Word != layer.Name {
			return false
		}
	}
	for _, assignment := range id.assignments() {
		if !environmentHolds(env, assignment) {
			return false
		}
	}
	return !selectsConfiguration(id.program())
}

// environmentHolds reports an assignment that changes nothing: the run's
// environment snapshot already holds exactly that value. Any other assignment
// (`KUBECONFIG=./kc`, `export AWS_PROFILE=prod`) changes what the program reads.
func environmentHolds(env []string, assignment string) bool {
	name, value, ok := strings.Cut(assignment, "=")
	if !ok || strings.HasSuffix(name, "+") {
		return false
	}
	current, set := lookupEnv(env, name)
	return set && current == value
}

// grantSelectorFlags point a program at configuration or credentials the
// command itself chooses — a file, an inline secret, another server, a proxy,
// a CA to trust or no verification at all — or name a program for it to run.
// A kubeconfig, a Docker config and an ssh option can each run another program
// (credential plugins, credsStore, ProxyCommand), and a redirected or
// unverified server receives the operator's token, so no class stands for a
// call carrying one (`--kubeconfig ./kc`, `--ca-bundle ./x.pem`,
// `ssh -o ProxyCommand=...`).
var grantSelectorFlags = map[string]map[string]struct{}{
	"kubectl": wordSet("--kubeconfig", "--kuberc", "--as", "--as-group", "--as-uid", "--token",
		"--client-certificate", "--client-key", "--certificate-authority", "--insecure-skip-tls-verify",
		"--tls-server-name", "--server", "-s", "--username", "--password"),
	"helm": wordSet("--kubeconfig", "--kube-apiserver", "--kube-token", "--kube-as-user", "--kube-as-group",
		"--kube-ca-file", "--kube-insecure-skip-tls-verify", "--kube-tls-server-name", "--registry-config",
		"--repository-config", "--repository-cache", "--repo", "--ca-file", "--cert-file", "--key-file",
		"--insecure-skip-tls-verify", "--plain-http", "--username", "--password", "--pass-credentials"),
	"aws":    wordSet("--endpoint-url", "--ca-bundle", "--no-verify-ssl"),
	"gcloud": wordSet("--impersonate-service-account", "--access-token-file", "--flags-file"),
	"gsutil": wordSet("-o"),
	"gh":     wordSet("--hostname"),
	"docker": wordSet("--config", "--host", "-H", "--tls", "--tlsverify", "--tlscacert", "--tlscert", "--tlskey"),
	"podman": wordSet("--url", "--identity", "--remote", "-r"),
	"argocd": wordSet("--config", "--auth-token", "--server", "--insecure", "--plaintext", "--server-crt",
		"--client-crt", "--client-crt-key", "--header", "-H", "--core", "--port-forward", "--grpc-web-root-path"),
	"doctl":  wordSet("--access-token", "-t", "--config", "--api-url", "-u"),
	"flyctl": wordSet("--access-token", "-t", "--config", "-c"),
	"vercel": wordSet("--token", "-t", "--global-config", "-Q", "--local-config", "-A"),
	"aliyun": wordSet("--config-path", "--access-key-id", "--access-key-secret", "--sts-token", "--mode",
		"--ram-role-name", "--ram-role-arn", "--role-session-name", "--private-key", "--key-pair-name",
		"--endpoint", "--skip-secure-verify", "--insecure"),
	"ssh":    wordSet("-F", "-o", "-i", "-J", "-I", "-S", "-P"),
	"scp":    wordSet("-F", "-o", "-i", "-J", "-S", "-D"),
	"sftp":   wordSet("-F", "-o", "-i", "-J", "-S", "-D"),
	"rsync":  wordSet("-e", "--rsh", "--rsync-path", "--password-file", "--remote-option", "-M"),
	"nc":     netcatSelectorFlags,
	"ncat":   netcatSelectorFlags,
	"netcat": netcatSelectorFlags,
}

// netcatSelectorFlags run a program or reach the target through a proxy or
// another trust anchor, in any of the netcats a host may name nc.
var netcatSelectorFlags = wordSet("-e", "-c", "--exec", "--sh-exec", "--lua-exec", "-x", "-X", "--proxy",
	"--proxy-type", "--proxy-auth", "--ssl-trustfile", "--ssl-cert", "--ssl-key")

// grantAbbreviatingPrograms read a long option from any unique prefix of its
// name — Python's argparse (aws, az, gcloud) and getopt (gsutil), and
// getopt_long (ncat, netcat, and the nc that may be either) — so
// `--endpoint-ur` is --endpoint-url. Cobra and pflag (kubectl, helm, docker,
// gh, ...) read only exact names.
var grantAbbreviatingPrograms = wordSet("aws", "az", "gcloud", "gsutil", "nc", "ncat", "netcat")

// longSelector resolves a long option to the selectors it may name: its exact
// spelling, or, for a program that reads abbreviations, every selector it
// begins. drops reports a class-dropping option among them; bound lists the
// bound selectors.
func longSelector(program, name string) (drops bool, bound []string) {
	dropping, binding := grantSelectorFlags[program], grantBoundSelectors[program]
	if _, exact := dropping[name]; exact {
		return true, nil
	}
	if _, exact := binding[name]; exact {
		return false, []string{name}
	}
	if _, abbreviates := grantAbbreviatingPrograms[program]; !abbreviates || len(name) <= len("--") {
		return false, nil
	}
	for option := range dropping {
		drops = drops || strings.HasPrefix(option, name)
	}
	for option := range binding {
		if strings.HasPrefix(option, name) {
			bound = append(bound, option)
		}
	}
	return drops, bound
}

// grantBoundSelectors choose an entry of the operator's own configuration by
// name: a profile, a context, an account. The entry is the identity the call
// acts as, so it becomes part of the class (`aws codebuild batch-get-builds
// --profile=cw3`) and a class remembered for one identity never releases
// another, while the daily workflow that names its profile keeps its answer.
// Each spelling maps to its semantic selector so aliases cannot hide repeats.
var grantBoundSelectors = map[string]map[string]string{
	"kubectl": {"--context": "--context", "--cluster": "--cluster", "--user": "--user"},
	"helm":    {"--kube-context": "--kube-context"},
	"aws":     {"--profile": "--profile"},
	"gcloud":  {"--account": "--account", "--configuration": "--configuration"},
	"docker":  {"--context": "--context", "-c": "--context"},
	"podman":  {"--connection": "--connection", "-c": "--connection"},
	"az":      {"--subscription": "--subscription"},
	"doctl":   {"--context": "--context", "-c": "--context"},
	"vercel":  {"--scope": "--scope", "-S": "--scope"},
	"aliyun":  {"--profile": "--profile", "-p": "--profile"},
}

// selectsConfiguration reports a selector option anywhere in a program's
// arguments: `--flag`, `--flag=value`, an abbreviation its parser reads as
// one, `-f`, an attached `-fvalue`, or a short-option bundle that holds it. A
// bound selector also counts when its value is missing or is not one plain
// word, or when an abbreviation could name more than one.
func selectsConfiguration(program commandLayer) bool {
	flags := grantSelectorFlags[program.Name]
	for _, arg := range program.Args {
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, _, _ := strings.Cut(arg, "=")
			if drops, _ := longSelector(program.Name, name); drops {
				return true
			}
			continue
		}
		for i := 1; i < len(arg); i++ {
			if _, selects := flags["-"+arg[i:i+1]]; selects {
				return true
			}
		}
	}
	_, ok := boundSelectors(program.Name, program.Args)
	return !ok
}

// boundSelectors reads the bound selectors a program's arguments carry as
// canonical `--flag=value` words, sorted; an abbreviation is read as the one
// selector it names. ok is false when one has no value, or a value that is not
// one plain word, when an abbreviation could name more than one option, or
// when a selector repeats, including through an alias. Sorting repeats would
// lose the CLI's selection order, which no reusable class can name.
func boundSelectors(program string, args []string) ([]string, bool) {
	flags := grantBoundSelectors[program]
	if len(flags) == 0 {
		return nil, true
	}
	var bound []string
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, attached := strings.Cut(arg, "=")
		if strings.HasPrefix(arg, "--") {
			drops, named := longSelector(program, name)
			if drops || len(named) > 1 {
				return nil, false
			}
			if len(named) == 0 {
				continue
			}
			name = named[0]
		} else {
			name, value, attached = arg, "", false
			if len(arg) > 2 && strings.HasPrefix(arg, "-") {
				if _, short := flags[arg[:2]]; short {
					name, value, attached = arg[:2], strings.TrimPrefix(arg[2:], "="), true
				}
			}
		}
		selector, selects := flags[name]
		if !selects {
			continue
		}
		if seen[selector] {
			return nil, false
		}
		seen[selector] = true
		if !attached {
			if i+1 >= len(args) {
				return nil, false
			}
			i++
			value = args[i]
		}
		if value == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, " \t\n\"'=") {
			return nil, false
		}
		bound = append(bound, name+"="+value)
	}
	sort.Strings(bound)
	return bound, true
}

// segmentIsBaselineObservation reports whether this one segment is provable
// read-only on its own, using the same catalog the approval funnel uses. Being
// plumbing is not enough on its own: `sort -o out.txt` writes, and the catalog
// rule is what knows that.
func segmentIsBaselineObservation(program string, fields []string) bool {
	rule, ok := observationRuleByProgram[program]
	if !ok {
		return false
	}
	commandArgs, ok := observationCommandArgs(program, fields[1:])
	if !ok {
		return false
	}
	return rule.matches(commandArgs)
}

// grantSegmentPrefix takes leading tokens up to and including the first
// verb-shaped one. A flag ends the subcommand path, because everything after it
// is an option or its value.
func grantSegmentPrefix(program string, rest []string) []string {
	if subcommand, ok := grantAPISubcommands[program]; ok {
		if prefix, matched := grantAPIPrefix(program, subcommand, rest); matched {
			return prefix
		}
	}
	_, subcommandStyle := grantSubcommandPrograms[program]
	prefix := []string{program}
	for _, token := range rest {
		token = strings.TrimSpace(token)
		if token == "" || strings.HasPrefix(token, "-") {
			break
		}
		if len(prefix) >= grantPrefixMaxTokens {
			break
		}
		lowered := strings.ToLower(token)
		prefix = append(prefix, lowered)
		if grantWriteVerbs.MatchString(lowered) || grantReadVerbs.MatchString(lowered) {
			return prefix
		}
	}
	if subcommandStyle {
		// No verb: the operation was not identified, so nothing here bounds a
		// standing permission.
		return nil
	}
	return []string{program}
}

// grantAPIPrefix renders the class for a subcommand that can perform any HTTP
// method against any resource. The method is explicit, so a remembered GET can
// never release a DELETE, and the resource path is bounded so the stored set
// does not grow one row per URL.
func grantAPIPrefix(program, subcommand string, rest []string) ([]string, bool) {
	if len(rest) == 0 || strings.ToLower(strings.TrimSpace(rest[0])) != subcommand {
		return nil, false
	}
	// The endpoint is read by gh's own grammar, so a method value is never
	// mistaken for a path, and a path spelled like a verb survives. A call the
	// grammar cannot read has no class at all, never the generic prefix.
	call, ok := ghAPIRequest(rest[1:])
	if !ok || call.host {
		return nil, true
	}
	prefix := []string{program, subcommand, call.method}
	if path := boundedAPIPath(call.endpoint); path != "" {
		prefix = append(prefix, path)
	}
	return prefix, true
}

// boundedAPIPath keeps the leading segments that identify the owner and
// resource family and drops the rest, so one rule covers a repository's
// branches without naming every branch.
func boundedAPIPath(raw string) string {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) > 4 {
		parts = parts[:4]
	}
	for _, part := range parts {
		if strings.ContainsAny(part, "$*?{}") {
			return ""
		}
	}
	return strings.ToLower(strings.Join(parts, "/"))
}

func samePrefix(a, b []string) bool {
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

// grantPrefixLabel renders a prefix for a person to read at decision time.
func grantPrefixLabel(prefix []string) string {
	return strings.Join(prefix, " ")
}
