package tools

import (
	"strings"
	"testing"
)

// filterReadsOnlyStdin is a safety boundary: it releases a credentialed payload
// on the claim that a filter opened no file. Every case below that must stay
// gated is a way that claim could be wrong.

func TestStdinFilterAcceptsOnlyPipelineInvocations(t *testing.T) {
	for _, command := range []string{
		// No operand at all: the filter can only have read its stdin.
		"cat", "wc", "wc -l", "base64", "base64 -d", "sort", "sort -u", "nl", "od -c",
		"column -t", "md5sum", "sha256sum", "shasum -a 256",
		// Flag values are not operands, including attached and legacy forms.
		"head -n 20", "head -n20", "head -20", "tail -n 5", "tail -5", "tail -f",
		"cut -d: -f1", "cut -d , -f 2", "sort -k 2 -t :", "od -A x -t x1",
		// grep: the pattern is an operand that is not a file.
		"grep tag", "grep -i tag", "grep -in tag", "grep -E 'a|b'", "grep -m5 tag",
		"grep -A3 tag", "grep -A 3 tag", "grep -e tag", "grep --regexp=tag",
		// tr's operands are SETs, never files.
		"tr -d '\\r'", "tr a-z A-Z", "tr -s ' '",
		// A bare `-` names standard input.
		"cat -", "base64 -d -",
		// End-of-options with nothing after it.
		"sort --",
	} {
		fields := strings.Fields(command)
		if !filterReadsOnlyStdin(fields[0], fields[1:]) {
			t.Errorf("must be recognised as stdin-only: %s", command)
		}
	}
}

func TestStdinFilterRejectsEveryWayAFileCouldBeRead(t *testing.T) {
	cases := []struct {
		command string
		why     string
	}{
		// A plain file operand is the obvious one.
		{"cat /etc/passwd", "operand is a file"},
		{"head ~/.aws/credentials", "operand is a file"},
		{"head -n 5 ~/.aws/credentials", "operand after a flag value"},
		{"head -n5 ~/.aws/credentials", "operand after an attached value"},
		{"head -5 ~/.aws/credentials", "operand after the legacy abbreviation"},
		{"tail -f /var/log/secure", "follow reads a named file"},
		{"wc -l ~/.aws/credentials", "operand is a file"},
		{"base64 ~/.aws/credentials", "operand is a file"},
		{"sort ~/.aws/credentials", "operand is a file"},
		{"nl ~/.aws/credentials", "operand is a file"},
		{"od -c ~/.ssh/id_rsa", "operand is a file"},
		{"column -t ~/.aws/credentials", "operand is a file"},
		{"cut -d: -f1 /etc/passwd", "operand after flag values"},
		{"cat - /etc/passwd", "stdin AND a file"},
		{"cat -- /etc/passwd", "file after end-of-options"},
		{"sort -- ~/.aws/credentials", "file after end-of-options"},

		// grep reaches the filesystem in more ways than an operand.
		{"grep secret ~/.aws/credentials", "pattern then a file"},
		{"grep -r secret ~/.aws", "recursive"},
		{"grep -R secret ~/.aws", "recursive"},
		{"grep -rn secret .", "recursive inside a bundle"},
		{"grep -nr secret .", "recursive later in a bundle"},
		{"grep -f patterns.txt", "patterns come from a file"},
		{"grep --file=patterns.txt", "patterns come from a file"},
		{"grep -e secret ~/.aws/credentials", "pattern via flag frees the operand to be a file"},
		{"grep --regexp=secret ~/.aws/credentials", "same through the long form"},
		{"grep --include=*.json -r secret .", "include implies a filesystem walk"},
		{"grep --exclude-from=list secret .", "reads the exclusion list from a file"},

		// Flags that consume a file even though they look like options.
		{"md5sum -c sums.txt", "check reads a list of files"},
		{"sha256sum --check sums.txt", "check reads a list of files"},
		{"shasum -c sums.txt", "check reads a list of files"},
		{"wc --files0-from=list", "reads the file list from a file"},
		{"sort --files0-from=list", "reads the file list from a file"},
		{"sort --random-source=/dev/urandom", "reads a named source"},
		{"sort -o out.txt", "writes a file"},
		{"sort --output=out.txt", "writes a file"},

		// tr takes two SETs; a third operand is not something this rule models.
		{"tr a b c", "more operands than the grammar allows"},

		// Anything unparsed fails closed rather than being guessed at.
		{"head --bogus", "unknown long flag"},
		{"head -Q", "unknown short flag"},
		{"grep -q -Z -Q tag", "unknown flag inside an otherwise known run"},
		{"cat --files-from=x", "unknown long flag"},
	}
	for _, tc := range cases {
		fields := strings.Fields(tc.command)
		if filterReadsOnlyStdin(fields[0], fields[1:]) {
			t.Errorf("must NOT be treated as stdin-only (%s): %s", tc.why, tc.command)
		}
	}
}

// Programs left out of the table stay out. Each of these looks like a filter
// and is not one.
func TestStdinFilterExcludesProgramsThatReachTheFilesystemAnyway(t *testing.T) {
	for _, tc := range []struct{ command, why string }{
		{"rg tag", "with no path operand ripgrep walks the working directory"},
		{"rg -i tag", "same, flags do not change it"},
		{"ls", "its output IS filesystem content"},
		{"stat x", "reports file metadata"},
		{"file x", "reads the file to classify it"},
		{"readlink -f x", "resolves a path"},
		{"realpath x", "resolves a path"},
		{"paste a b", "requires file operands"},
		{"comm a b", "requires file operands"},
		{"diff a b", "requires file operands"},
		{"sed 's/a/b/'", "not catalogued as a filter here at all"},
		{"awk '{print}'", "not catalogued"},
		{"jq .", "carries its own credential handling"},
		{"yq .", "carries its own credential handling"},
	} {
		fields := strings.Fields(tc.command)
		if filterReadsOnlyStdin(fields[0], fields[1:]) {
			t.Errorf("must stay excluded (%s): %s", tc.why, tc.command)
		}
	}
}

// End to end through the production proof: the rule only widens, and only for
// the invocation it can prove.
func TestCredentialedPipelinesReleaseOnlyStdinFilters(t *testing.T) {
	for _, command := range []string{
		"gcloud builds list --project p | head -20",
		"gcloud config list | base64",
		"aws iam list-roles --profile prod | grep -i deployer",
		"cd /w && gh api repos/o/n/contents/x.yaml --jq .content | base64 -d | grep -i 'tag:'",
		"kubectl get pods -n platform -o json | jq -r '.items[].metadata.name' | sort | head -5",
		"aws sts get-caller-identity --profile cw2 | tr -d '\\n' | wc -c",
	} {
		if !provenReadOnlyWithCredentials(t, command) {
			t.Errorf("a credentialed read piped through stdin-only filters must prove: %s", command)
		}
	}
	// The constraint that must change the result: the same pipelines, with the
	// filter pointed at a file instead of the pipe.
	for _, command := range []string{
		"gcloud builds list --project p | head -20 ~/.aws/credentials",
		"gcloud config list && base64 ~/.config/gcloud/application_default_credentials.json",
		"aws iam list-roles --profile prod && grep -r secret ~/.aws",
		"gh api repos/o/n --jq .x && cat ~/.aws/credentials",
		"kubectl get pods && sort ~/.kube/config",
	} {
		if provenReadOnlyWithCredentials(t, command) {
			t.Errorf("a filter reading a credential file must stay gated: %s", command)
		}
	}
	// And without credentials in scope the rule changes nothing either way:
	// these were already ordinary reads.
	if !provenReadOnly(t, "gcloud builds list | head -20") {
		t.Error("uncredentialed behaviour must be unchanged")
	}
}
