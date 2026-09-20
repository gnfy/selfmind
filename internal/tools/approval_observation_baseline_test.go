package tools

import "testing"

func provenReadOnly(t *testing.T, command string) bool {
	t.Helper()
	return deterministicObservationExec("terminal", map[string]interface{}{"command": command})
}

// TestObservationBaselineCoversOrdinaryShellShape: every effective program in a
// payload must be provable, so ONE unknown segment disqualifies the command
// before anything else is inspected. Measured over a week of real traffic, 64%
// of commands led with `cd` and died there, and a single `| head` disqualified
// the rest; proven-read-only coverage was 10.5%. These are the shapes that were
// losing, and they carry no effect of their own.
func TestObservationBaselineCoversOrdinaryShellShape(t *testing.T) {
	for _, command := range []string{
		"cd /w/cicd && gcloud builds describe abc --project p --format 'value(status)'",
		"cd /w && aws codebuild batch-get-builds --ids x --profile cw3 | jq -r .builds",
		"set -euo pipefail; gcloud builds list --project p | head -20",
		"cat notes.md | tr -d '\\r' | sort | head -5",
		"aws sts get-caller-identity --profile cw2 | base64",
		"gh api repos/owner/name/branches",
	} {
		if !provenReadOnly(t, command) {
			t.Errorf("must be provable read-only: %s", command)
		}
	}
}

// TestObservationBaselineStillRefusesEffects: widening the catalog must not let
// anything with an effect through. A single unprovable segment still
// disqualifies the whole payload.
func TestObservationBaselineStillRefusesEffects(t *testing.T) {
	for _, command := range []string{
		"cd /w && rm -rf build",                      // banned program
		"cd /w && npm install",                       // unknown program
		"cd /w && sed -i s/a/b/ f",                   // rewrites in place
		"cat a > b",                                  // redirection to a real file
		"cd \"$DIR\" && ls",                          // expansion: tokens are not what runs
		"for p in a b; do gcloud builds list; done",  // control flow
		"sort -o out.txt in.txt",                     // sort CAN write
		"base64 -o out.bin in.txt",                   // base64 CAN write
		"date -s 2020-01-01",                         // date CAN set the clock
		"gh api -X DELETE repos/owner/name/git/refs", // method override is a mutation
		"gh api repos/o/n/issues --input body.json",  // request body is a mutation
		"uniq in.txt out.txt",                        // uniq writes its 2nd positional
		"ls | tee saved.txt",                         // tee writes
	} {
		if provenReadOnly(t, command) {
			t.Errorf("must NOT be provable read-only: %s", command)
		}
	}
}

// TestObservationBaselineIsNarrowerThanTheGrantFloorNeutralSet pins the reason
// the two word lists differ. grant_floor.go asks "may this word name a
// remembered class"; this catalog asks "does this run and change nothing". A
// word that can carry a command body, or bind a name a later segment expands,
// answers yes to the first and no to the second.
func TestObservationBaselineIsNarrowerThanTheGrantFloorNeutralSet(t *testing.T) {
	for word := range shellNeutralWords {
		switch word {
		case "trap", "read", "declare", "local", "readonly", "unset", "shift", "wait", "]", "]]", "[[":
			if _, present := observationRuleByProgram[word]; present {
				t.Errorf("%q carries or binds state and must not be an observation word", word)
			}
		}
	}
	for _, word := range []string{"cd", "set", "export", "true", "test"} {
		if _, present := observationRuleByProgram[word]; !present {
			t.Errorf("%q produces no effect and must be an observation word", word)
		}
	}
}

// TestObservationFiltersHaveNoPositionalOutput pins the test every filter in
// this catalog had to pass and one of them did not: a program whose operands
// are not all INPUTS can write a file with no flag to reject, and the rule
// shape here cannot express "no positional output".
//
// `xxd in out` and `xxd -r dump.hex out.bin` were classified as pure
// observations. That is not only an auto-approval: this catalog also decides
// what a durable watcher may re-run unattended for hours.
func TestObservationFiltersHaveNoPositionalOutput(t *testing.T) {
	// Programs whose second operand is an output file, or which write every
	// operand. None may be an observation word or a grantable class.
	for _, program := range []string{"xxd", "uniq", "tee"} {
		if _, present := observationRuleByProgram[program]; present {
			t.Errorf("%q writes a positional operand and must not be an observation word", program)
		}
		if _, plumbing := grantPlumbingPrograms[program]; plumbing {
			t.Errorf("%q writes a positional operand and must not be skipped as plumbing", program)
		}
	}
	for _, command := range []string{`xxd in out`, `xxd -r dump.hex restored.bin`, `xxd input.bin`} {
		args := map[string]interface{}{"command": command}
		if deterministicObservationExec("terminal", args) {
			t.Errorf("%q must not be a proven read-only observation", command)
		}
		if prefix, ok := grantCommandPrefix("terminal", args); ok {
			t.Errorf("%q must not be grant-eligible, got %v", command, prefix)
		}
	}
	// The constraint that must change the result: `od` takes only inputs, so
	// removing it would be over-correction, not safety.
	if !deterministicObservationExec("terminal", map[string]interface{}{"command": `od -A x input.bin other.bin`}) {
		t.Error("od takes only input operands and must stay an observation")
	}
}

func provenReadOnlyWithCredentials(t *testing.T, command string) bool {
	t.Helper()
	return deterministicObservationExec("terminal", map[string]interface{}{
		"command": command, credentialReadArgKey: true,
	})
}

// A credentialed payload requires EVERY program in it to be credential-safe,
// so the shape that dominated the remaining ask volume was an ordinary
// credentialed read wrapped in plumbing: 73 commands died on a leading `cd` and
// 32 more on an `echo` banner. Those two cannot emit a credential — their
// output comes only from arguments the parser already proved static.
func TestObservationCredentialSafePlumbingDoesNotDisqualifyReads(t *testing.T) {
	for _, command := range []string{
		"cd /w/cicd && gcloud builds list --project p",
		`echo "== caller identity ==" ; aws sts get-caller-identity --profile cw2`,
		"cd /w && kubectl get pods -n platform -o json | jq -r '.items[].metadata.name'",
		"which gcloud && gcloud config get-value account",
		"test -d /w && cd /w && aws iam list-roles",
	} {
		if !provenReadOnlyWithCredentials(t, command) {
			t.Errorf("credentialed read must survive its plumbing: %s", command)
		}
	}
}

// The constraint that must change the result: a program that CAN print what is
// inside a file stays disqualified once credentials are in scope, because that
// is exactly how a credential leaves. Uncredentialed, these same commands are
// ordinary reads — which is why the flag is separate from the catalog itself.
func TestObservationCredentialSafeExcludesEveryFileReader(t *testing.T) {
	for _, command := range []string{
		"cat ~/.config/gcloud/application_default_credentials.json",
		"aws sts get-caller-identity | tee saved.txt",
		// `set` and `export` carry no file access but print the whole variable
		// environment when given no operands.
		"set; gcloud builds list",
		"export; aws iam list-roles",
	} {
		if provenReadOnlyWithCredentials(t, command) {
			t.Errorf("a credential emitter must stay gated: %s", command)
		}
	}
}

// Read verbs added from measured usage. Each is paired with the sibling that
// must NOT match, because these were added by pinning the verb rather than
// stopping at the noun.
func TestObservationCatalogCoversMeasuredReadVerbs(t *testing.T) {
	reads := []string{
		"gcloud artifacts repositories list --project=p",
		"gcloud artifacts docker images list us-east4-docker.pkg.dev/p/r",
		"gcloud artifacts docker tags list us-east4-docker.pkg.dev/p/r/i",
		"gcloud artifacts docker versions list us-east4-docker.pkg.dev/p/r/i",
		"gh release view v20260918151055 --repo owner/name",
		"gh release list --repo owner/name",
		"git ls-remote https://github.com/owner/name refs/heads/main",
	}
	for _, command := range reads {
		if !provenReadOnly(t, command) {
			t.Errorf("must be provable read-only: %s", command)
		}
	}
	// These reach the same subcommand families and must stay gated. The first
	// three are why the catalog cannot simply trust a credential-bearing CLI:
	// each prints a secret or changes state through an otherwise read-shaped
	// verb.
	for _, command := range []string{
		"gcloud auth print-access-token",              // emits a credential
		"aws configure get aws_secret_access_key",     // emits a credential
		"kubectl config use-context gke_p_us-east4_c", // rewrites kubeconfig
		"gcloud artifacts docker images delete IMAGE", // sibling of a list
		"gcloud artifacts repositories delete r",      // sibling of a list
		"gh release delete v1 --repo owner/name",      // sibling of a view
		"git push origin develop",                     // not a remote read
	} {
		if provenReadOnly(t, command) {
			t.Errorf("must NOT be provable read-only: %s", command)
		}
	}
}
