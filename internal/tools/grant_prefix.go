package tools

import (
	"path/filepath"
	"regexp"
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

// grantAPIMethodFlags are the flags that change an HTTP verb for a CLI whose
// single subcommand can perform every method. `gh api` is the live example: it
// was the largest single approval class in one week (84 asks) and
// `gh api -X DELETE …` shares its leading tokens with an ordinary read.
var grantAPIMethodFlags = map[string]struct{}{"-x": {}, "--method": {}}

// apiReadMethods are the HTTP verbs that only read. Anything else — including a
// verb this parser cannot resolve — is a mutation as far as approval is
// concerned.
var apiReadMethods = map[string]struct{}{"get": {}, "head": {}}

// apiMethodFromArgs resolves the HTTP verb of an `api`-style invocation.
//
// It exists because the verb was previously parsed twice, by two layers that
// accepted different spellings of the same flag: the observation catalog
// matched the substring "-x " and so read `-X=DELETE` and `-XDELETE` as
// ordinary GETs, while the class derivation handled `-X=DELETE` but labelled
// `-XDELETE` a GET. One destructive call could therefore be both auto-approved
// as an observation and remembered under a read-only class name. Both layers
// now ask this one function, so a spelling either parses for both or fails for
// both.
//
// ok is false when a method flag is present but its value cannot be resolved
// statically; callers must fail closed rather than fall back to the GET
// default.
func apiMethodFromArgs(rest []string) (string, bool) {
	method := "get"
	for i := 0; i < len(rest); i++ {
		lowered := strings.ToLower(strings.Trim(strings.TrimSpace(rest[i]), `"'`))
		if lowered == "" {
			continue
		}
		if name, value, attached := strings.Cut(lowered, "="); attached {
			if _, ok := grantAPIMethodFlags[name]; ok {
				if value == "" {
					return "", false
				}
				method = value
			}
			continue
		}
		if _, ok := grantAPIMethodFlags[lowered]; ok {
			if i+1 >= len(rest) {
				return "", false
			}
			value := strings.ToLower(strings.Trim(strings.TrimSpace(rest[i+1]), `"'`))
			if value == "" || strings.HasPrefix(value, "-") {
				return "", false
			}
			method = value
			i++
			continue
		}
		// Attached short form, e.g. `-XDELETE`. Only the single-dash spelling
		// concatenates; `--methodDELETE` is not a flag at all.
		if strings.HasPrefix(lowered, "-") && !strings.HasPrefix(lowered, "--") && len(lowered) > 2 {
			if _, ok := grantAPIMethodFlags[lowered[:2]]; ok {
				method = lowered[2:]
			}
		}
	}
	if method == "" {
		return "", false
	}
	return method, true
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
	for _, marker := range grantComplexShellMarkers {
		if strings.Contains(payload, marker) {
			return nil, false
		}
	}
	segments, unparsed := expandCommandSegments(payload, 0)
	if unparsed {
		return nil, false
	}
	var prefix []string
	for _, fields := range segments {
		progIdx, ok := segmentProgram(fields)
		if !ok {
			return nil, false
		}
		base := strings.ToLower(strings.TrimSpace(filepath.Base(fields[progIdx])))
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
		if _, plumbing := grantPlumbingPrograms[base]; plumbing && segmentIsBaselineObservation(base, fields[progIdx:]) {
			continue
		}
		candidate := grantSegmentPrefix(base, fields[progIdx+1:])
		if len(candidate) == 0 {
			return nil, false
		}
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
	method, ok := apiMethodFromArgs(rest)
	if !ok {
		return nil, false
	}
	path := ""
	found := false
	for i := 0; i < len(rest); i++ {
		token := strings.TrimSpace(rest[i])
		if token == "" {
			continue
		}
		lowered := strings.ToLower(token)
		// The value of a separated method flag (`-X DELETE`) is a bare word, so
		// it is skipped BY POSITION. Skipping it by comparing against the
		// resolved method instead would also swallow a resource path that
		// happens to be spelled like the verb, and a class with no path covers
		// every path.
		if _, ok := grantAPIMethodFlags[lowered]; ok {
			i++
			continue
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		if !found {
			if lowered != subcommand {
				return nil, false
			}
			found = true
			continue
		}
		if path == "" {
			path = boundedAPIPath(token)
		}
	}
	if !found {
		return nil, false
	}
	prefix := []string{program, subcommand, method}
	if path != "" {
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
