package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

// chainSurfaces prints output with the terminal tool through the production
// result middleware and the production event redactor, and returns the tool
// result together with every line the live stream carried.
func chainSurfaces(t *testing.T, output string) (kernel.ToolDispatchResult, []string) {
	t.Helper()
	kernel.SetAgentEventRedactor(RedactSensitive)
	t.Cleanup(func() { kernel.SetAgentEventRedactor(nil) })
	return chainSurfacesWithoutEventRedactor(t, output)
}

// chainSurfacesWithoutEventRedactor is chainSurfaces with no kernel event
// redactor installed, so the stream shows only what the tool path masked.
func chainSurfacesWithoutEventRedactor(t *testing.T, output string) (kernel.ToolDispatchResult, []string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	registry.Register(NewExecuteCommandTool())
	registry.UseResultMiddleware(RedactionMiddleware())
	registry.UseResultMiddleware(EvidenceMiddleware())
	events := make(chan string, 4096)
	ctx := kernel.WithEventChannel(context.Background(), events)
	result, err := registry.DispatchResult("terminal", map[string]interface{}{
		"command": "cat output.txt", "cwd": dir, "timeout": "10", "_tool_call_id": "print", "_context": ctx})
	if err != nil {
		t.Fatalf("printing failed: %v", err)
	}
	close(events)
	var streamed []string
	for raw := range events {
		event, ok := kernel.DecodeAgentEvent(raw)
		if !ok || event.Type != "tool.output" {
			continue
		}
		line, _ := event.Payload["line"].(string)
		if line != event.Content {
			t.Fatalf("an output event's content and line differ: %q vs %q", event.Content, line)
		}
		streamed = append(streamed, line)
	}
	return result, streamed
}

// assertChainHides checks the model-facing result and the live stream.
func assertChainHides(t *testing.T, name, output string, hidden ...string) {
	t.Helper()
	result, streamed := chainSurfaces(t, output)
	stream := strings.Join(streamed, "\n")
	for _, value := range hidden {
		if strings.Contains(result.Output, value) {
			t.Errorf("%s: the tool result leaked %q: %s", name, value, result.Output)
		}
		if strings.Contains(stream, value) {
			t.Errorf("%s: the stream leaked %q: %s", name, value, stream)
		}
	}
}

// assertChainKeeps checks that ordinary values survive both surfaces.
func assertChainKeeps(t *testing.T, name, output string, kept ...string) {
	t.Helper()
	result, streamed := chainSurfaces(t, output)
	stream := strings.Join(streamed, "\n")
	for _, value := range kept {
		if !strings.Contains(result.Output, value) || !strings.Contains(stream, value) {
			t.Errorf("%s: an ordinary value %q was masked:\nresult: %s\nstream: %s", name, value, result.Output, stream)
		}
	}
}

// jsonEscape writes the JSON escape for a code point given in hex, as
// producers other than encoding/json do.
func jsonEscape(hex string) string { return "\\u" + hex }

const (
	syntheticGitHubToken = "ghp_R7k2Qx9LmN4pT8vW1yZ3aB6cD0eF5gH2jK7s"
	syntheticFineGrained = "github_pat_11WPRTEST0abcdefghijklm_nopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmn"
	syntheticJWT         = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ3cHItdGVzdCIsImlhdCI6MTcwMDAwMDAwMH0.c2lnbmF0dXJlLXdwci03ZDEwMmUtc3ludGhldGljLXNpZw"
	syntheticAWSKeyID    = "AKIAWPRTEST7D102EXAM"
	syntheticAWSSecret   = "wPr7d102eSecretAccessKeyExampleK7MDENG+1"
)

// Credentials recognized by shape alone, with no name assigning them, are
// masked; ordinary hashes, digests, ids and encoded data that resemble them
// are not.
func TestFreeTextCredentialsAreMaskedByShape(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("wpr-user:wpr-pass-7d102e"))
	for _, tc := range []struct {
		name, output string
		hidden       []string
	}{
		{"a classic GitHub token", "cloned with " + syntheticGitHubToken + " ok", []string{syntheticGitHubToken[4:]}},
		{"GitHub OAuth, user, server and refresh tokens", "gho_" + syntheticGitHubToken[4:] + " ghu_" + syntheticGitHubToken[4:] + " ghs_" + syntheticGitHubToken[4:] + " ghr_" + syntheticGitHubToken[4:], []string{syntheticGitHubToken[4:]}},
		{"a fine-grained GitHub token", "remote: " + syntheticFineGrained, []string{syntheticFineGrained[11:]}},
		{"a JWT", "session " + syntheticJWT + " expires", []string{"c2lnbmF0dXJlLXdwci03ZDEwMmUtc3ludGhldGljLXNpZw", "eyJzdWIiOiJ3cHItdGVzdCIsImlhdCI6MTcwMDAwMDAwMH0"}},
		{"Authorization: Basic", "> Authorization: Basic " + basic, []string{basic}},
		{"a quoted Basic header", `curl -H "Authorization: Basic ` + basic + `" https://example.test`, []string{basic}},
		{"Basic credentials in free text", "sent Basic " + basic + " to the proxy", []string{basic}},
		{"a free-text Bearer token", "retrying with Bearer wpr7d102eBearerToken99", []string{"wpr7d102eBearerToken99"}},
		{"an AWS key pair", syntheticAWSKeyID + "  " + syntheticAWSSecret, []string{syntheticAWSSecret}},
		{"an AWS key pair in CSV", "User,Access key ID,Secret access key\nci," + syntheticAWSKeyID + "," + syntheticAWSSecret, []string{syntheticAWSSecret}},
	} {
		assertChainHides(t, tc.name, tc.output, tc.hidden...)
	}
	// The same shapes inside JSON stay masked and leave the document JSON.
	document, _ := json.Marshal(map[string]string{"log": "token " + syntheticGitHubToken, "session": syntheticJWT, "header": "Authorization: Basic " + basic})
	result, _ := chainSurfaces(t, string(document))
	if !json.Valid([]byte(result.Output)) || result.RedactionUnparseable || strings.Contains(result.Output, syntheticGitHubToken[4:]) ||
		strings.Contains(result.Output, "c2lnbmF0dXJl") || strings.Contains(result.Output, basic) {
		t.Fatalf("shapes inside JSON: %s unparseable=%v", result.Output, result.RedactionUnparseable)
	}
}

func TestOrdinaryHashesAndEncodedDataAreNotMaskedAsCredentials(t *testing.T) {
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	for _, value := range []string{
		"commit 3b18e512dba79e4c8300dd08aeb37f8e728b8dad",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"request 123e4567-e89b-12d3-a456-426614174000 done",
		png,
		"Basic usage: run the tool with Basic authentication disabled",
		"key id " + syntheticAWSKeyID + " rotated",
		"max_tokens=4096 total_tokens: 1234",
		"ghp_short is not a token",
		"version eyJhbGciOiJIUzI1NiJ9 has no signature",
	} {
		assertChainKeeps(t, value, value, value)
	}
}

// A credential's name marks its value in every spelling: spaced, camel,
// plural, kebab, YAML, env, CLI flags and HTTP headers.
func TestCredentialNameVariantsMarkTheirValues(t *testing.T) {
	const value = "v7d102e-name-variant"
	for _, output := range []string{
		"API Key: " + value,
		"Secret Key: " + value,
		"secret_key=" + value,
		"SECRET_KEY=" + value,
		"client-secret: " + value,
		"Client Secret = " + value,
		"apiKeys: " + value,
		"api_keys=" + value,
		"accessKeys: " + value,
		"passwords: " + value,
		"database:\n  password: " + value,
		"export DB_PASSWORD=" + value,
		"mysql --password " + value + " -h db",
		"login --api-key " + value,
		"login --password=" + value,
		"gh auth --token '" + value + "'",
		"X-Api-Key: " + value,
		`curl -H "X-Api-Key: ` + value + `" https://example.test`,
	} {
		assertChainHides(t, output, output, value)
	}
	// Names that only resemble a credential keep their values.
	for _, output := range []string{
		"max_tokens: 4096",
		"login --api-key-file /etc/creds.txt",
		"docker login --password-stdin < pass.txt",
		"monkey: banana",
		"keyboard: qwerty",
		"public key: ssh-ed25519",
	} {
		assertChainKeeps(t, output, output, output)
	}
}

// Invisible format characters inside a name, JSON escapes spelling it, and
// mixed case do not hide a credential's value.
func TestObfuscatedCredentialNamesStillMarkTheirValues(t *testing.T) {
	const value = "v7d102e-obfuscated"
	zeroWidth, joiner, softHyphen := string(rune(0x200B)), string(rune(0x200D)), string(rune(0x00AD))
	for _, output := range []string{
		"pass" + zeroWidth + "word=" + value,
		"api" + joiner + "_key: " + value,
		"to" + softHyphen + "ken=" + value,
		`{"p` + jsonEscape("0061") + `ssword":"` + value + `"}`,
		`{"api` + jsonEscape("005F") + `key":"` + value + `"}`,
		`{"authToken":4242424,"p` + jsonEscape("0061") + `ssword":"` + value + `"}`,
		`{"cfg":{"to` + jsonEscape("006b") + `en":[1,"` + value + `"]}}`,
		"PaSsWoRd=" + value,
		`{"ApI_KeY":"` + value + `"}`,
	} {
		assertChainHides(t, output, output, value)
		if strings.HasPrefix(output, "{") {
			result, _ := chainSurfaces(t, output)
			if !json.Valid([]byte(result.Output)) || result.RedactionUnparseable {
				t.Errorf("%s did not stay JSON: %s", output, result.Output)
			}
		}
	}
	// An invisible character inside an ordinary name changes nothing.
	ordinary := "mon" + zeroWidth + "key=banana"
	assertChainKeeps(t, ordinary, ordinary, "banana")
}

// Overlapping registered secrets are masked as the union of where each
// occurs in the original text, so masking one first cannot leave the
// other's remainder visible.
func TestOverlappingRegisteredSecretsAreMaskedAsOneUnion(t *testing.T) {
	RegisterSensitiveValue("alpha-r6-beta")
	RegisterSensitiveValue("beta.gamma-r6-tail")
	RegisterSensitiveValue("ovl-one-r7-mid")
	RegisterSensitiveValue("r7-mid-two-tail")
	for _, tc := range []struct {
		output string
		hidden []string
	}{
		{"value alpha-r6-beta.gamma-r6-tail end", []string{"alpha", "gamma", "r6-tail"}},
		{"value ovl-one-r7-mid-two-tail end", []string{"ovl-one", "two-tail"}},
		{`{"note":"alpha-r6-beta.gamma-r6-tail"}`, []string{"alpha", "gamma"}},
	} {
		assertChainHides(t, tc.output, tc.output, tc.hidden...)
	}
	if got := RedactSensitive("value alpha-r6-beta.gamma-r6-tail end"); got != "value [REDACTED] end" {
		t.Fatalf("overlap = %q", got)
	}
	// An identifier inside a longer resource name is still not a match.
	RegisterSensitiveValue("task-orchestrator")
	assertChainKeeps(t, "resource name", "lid-tm-task-orchestrator-svc-configmap", "lid-tm-task-orchestrator-svc-configmap")
}

// A valid JSON document hides a registered secret behind any escaping its
// producer chose; its decoded strings and keys are what is matched.
func TestRegisteredSecretsAreMatchedInDecodedJSONStrings(t *testing.T) {
	dotnet := `wpr"quote+plus-r8-secret`
	php := "https://wpr.example/r8/path-secret"
	python := "p" + string(rune(0xE4)) + "sswort-r8-secret"
	mixed := "wpr<mixed>r8-secret"
	nested := "tok8r8abcdef"
	keyed := "keyname-r8-7d102e-secret"
	for _, secret := range []string{dotnet, php, python, mixed, nested, keyed} {
		RegisterSensitiveValue(secret)
	}
	for _, tc := range []struct {
		name, fields, hidden string
	}{
		{".NET escapes", `"note":"wpr` + jsonEscape("0022") + `quote` + jsonEscape("002B") + `plus-r8-secret"`, "plus-r8-secret"},
		{"PHP escaped slashes", `"url":"https:\/\/wpr.example\/r8\/path-secret"`, "path-secret"},
		{"Python ensure_ascii", `"word":"p` + jsonEscape("00e4") + `sswort-r8-secret"`, "sswort-r8-secret"},
		{"mixed-case hex", `"note":"wpr` + jsonEscape("003C") + `mixed` + jsonEscape("003e") + `r8-secret"`, "mixed"},
		{"an escaped key", `"keyname-r8-7d102e-` + jsonEscape("0073") + `ecret":1`, "7d102e"},
		{"JSON inside a JSON string", `"body":"{\"note\":\"line\\n` + nested + `\"}"`, nested},
	} {
		for _, document := range []string{`{` + tc.fields + `}`, `{"authToken":4242424,` + tc.fields + `}`} {
			result, streamed := chainSurfaces(t, document)
			if strings.Contains(result.Output, tc.hidden) || !json.Valid([]byte(result.Output)) || result.RedactionUnparseable {
				t.Errorf("%s: %s -> %s unparseable=%v", tc.name, document, result.Output, result.RedactionUnparseable)
			}
			if stream := strings.Join(streamed, "\n"); strings.Contains(stream, tc.hidden) {
				t.Errorf("%s: the stream leaked: %s", tc.name, stream)
			}
		}
	}
}

// When a document keeps the text mask, security over validity, the strings
// its decoded values reveal a secret in are masked too.
func TestTextMaskFallbackStillMasksDecodedSecrets(t *testing.T) {
	spaced := "r12 spaced secret value"
	escaped := `wpr"r12+escaped-secret`
	RegisterSensitiveValue(spaced)
	RegisterSensitiveValue(escaped)
	document := `{"authToken":4242424,"a":"` + spaced + `","note":"wpr` + jsonEscape("0022") + `r12` + jsonEscape("002B") + `escaped-secret"}`
	result, _ := chainSurfaces(t, document)
	if !result.RedactionUnparseable || strings.Contains(result.Output, "spaced secret") || strings.Contains(result.Output, "escaped-secret") {
		t.Fatalf("the text mask kept a decoded secret: %s unparseable=%v", result.Output, result.RedactionUnparseable)
	}
	// A value-by-value form that would reveal a masked value is refused the
	// same way, and the decoded secret stays masked.
	leaky := `{"authToken":4242424,"note":"token=abc-r12-7d102e","x":"wpr` + jsonEscape("0022") + `r12` + jsonEscape("002B") + `escaped-secret"}`
	textMasked, masked := redactSensitiveText(leaky)
	if got, unparseable := redactBrokenJSON(leaky, textMasked, masked, func(value string) (string, bool) { return value, true }); !unparseable || !strings.Contains(got, "escaped-secret") {
		t.Fatalf("precondition: the text mask alone shows the escaped secret: %s", got)
	}
	if got, _ := redactSensitiveReport(leaky); strings.Contains(got, "escaped-secret") || strings.Contains(got, "abc-r12-7d102e") {
		t.Fatalf("redaction leaked: %s", got)
	}
}

// A registered secret that is itself JSON is matched whatever its layout:
// registered compact and shown pretty-printed, or the reverse.
func TestRegisteredJSONSecretsAreMatchedPrettyOrCompact(t *testing.T) {
	compact := `{"type":"service_account","project_id":"wpr-r8","private_key_id":"r8kid7d102e"}`
	pretty := "{\n  \"kind\": \"wpr-auth\",\n  \"user\": \"r8user7d102e\",\n  \"region\": \"r8-east\"\n}"
	RegisterSensitiveValue(compact)
	RegisterSensitiveValue(pretty)
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(compact), "", "    "); err != nil {
		t.Fatal(err)
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, []byte(pretty)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, output, hidden string }{
		{"compact secret shown indented", indented.String(), "r8kid7d102e"},
		{"compact secret shown indented after a log line", "loaded credentials:\n" + indented.String() + "\ndone", "r8kid7d102e"},
		{"pretty secret shown compact", compacted.String(), "r8user7d102e"},
		{"pretty secret inside a document", `{"auth":` + compacted.String() + `,"ok":true}`, "r8user7d102e"},
	} {
		assertChainHides(t, tc.name, tc.output, tc.hidden)
	}
}

// An argument the schema rejects never reaches the tool body, and the
// refusal that quotes it passes the same redaction as a tool result.
func TestArgumentPreparationErrorsAreRedacted(t *testing.T) {
	registered := "prep-r9-7d102e-opaque"
	RegisterSensitiveValue(registered)
	registry := NewRegistry()
	registry.Register(NewExecuteCommandTool())
	registry.UseResultMiddleware(RedactionMiddleware())
	for _, tc := range []struct {
		name   string
		args   map[string]interface{}
		hidden string
	}{
		{"a token where an integer belongs", map[string]interface{}{"command": "true", "timeout": syntheticGitHubToken}, syntheticGitHubToken[4:]},
		{"a registered secret outside an enum", map[string]interface{}{"command": "true", "sandbox": registered}, registered},
		{"an assignment where a boolean belongs", map[string]interface{}{"command": "true", "background": "API_KEY=v7d102e-prep-flag"}, "v7d102e-prep-flag"},
	} {
		_, err := registry.DispatchResult("terminal", tc.args)
		if err == nil {
			t.Fatalf("%s: preparation accepted invalid arguments", tc.name)
		}
		var stable interface {
			ToolErrorCode() string
			ModelSafeMessage() string
			ToolFailurePhase() string
		}
		if !errors.As(err, &stable) || stable.ToolErrorCode() != "tool_arguments_invalid" || stable.ToolFailurePhase() != "preparation" {
			t.Fatalf("%s: the typed preparation failure was lost: %v", tc.name, err)
		}
		for surface, text := range map[string]string{"error": err.Error(), "model message": stable.ModelSafeMessage()} {
			if strings.Contains(text, tc.hidden) {
				t.Errorf("%s: the %s leaked %q: %s", tc.name, surface, tc.hidden, text)
			}
		}
		// The agent prepares arguments before its ledger; that path is the same.
		if _, err := registry.PrepareToolArguments("terminal", tc.args); err == nil || strings.Contains(err.Error(), tc.hidden) {
			t.Errorf("%s: direct preparation leaked: %v", tc.name, err)
		}
	}
}

// Streamed output lines are masked as the tool result is, including a
// secret that spans lines and so never sits whole in one streamed line.
func TestStreamedOutputMasksSecretsAcrossLines(t *testing.T) {
	pem := "-----BEGIN R9 STREAM KEY-----\nMIIEr9streamKeyMaterialLineOne\nMIIEr9streamKeyMaterialLineTwo\n-----END R9 STREAM KEY-----"
	RegisterSensitiveValue(pem)
	serviceAccount := `{"type":"service_account","client_id":"r9client7d102e","token_uri":"https://r9.example/token"}`
	RegisterSensitiveValue(serviceAccount)
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(serviceAccount), "", "  "); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, output string
		hidden       []string
	}{
		{"a registered PEM key", "key follows\n" + pem + "\nnext", []string{"MIIEr9streamKeyMaterial"}},
		{"a PEM key starting mid-line", "key: " + pem, []string{"MIIEr9streamKeyMaterial"}},
		{"a registered JSON secret pretty-printed", pretty.String(), []string{"r9client7d102e"}},
		{"a credential name ending the previous line", "service_password:\n  r9-continued-value", []string{"r9-continued-value"}},
		{"a key id with its secret on the next line", syntheticAWSKeyID + "\n" + syntheticAWSSecret, []string{syntheticAWSSecret}},
	} {
		assertChainHides(t, tc.name, tc.output, tc.hidden...)
	}
	// Lines that only begin like a secret are delayed, never dropped.
	result, streamed := chainSurfaces(t, "-----BEGIN R9 STREAM KEY-----\nnot the key\ntail")
	if got := strings.Join(streamed, "\n"); got != "-----BEGIN R9 STREAM KEY-----\nnot the key\ntail" || got != result.Output {
		t.Fatalf("a diverging prefix changed the stream: %q (result %q)", got, result.Output)
	}
}

// The tool path masks its own stream; it does not depend on the kernel's
// event redactor being installed.
func TestStreamedOutputIsMaskedWithoutTheEventRedactor(t *testing.T) {
	registered := "stream-r9-7d102e-opaque"
	RegisterSensitiveValue(registered)
	_, streamed := chainSurfacesWithoutEventRedactor(t, "using "+registered+"\ntoken "+syntheticGitHubToken)
	stream := strings.Join(streamed, "\n")
	if strings.Contains(stream, registered) || strings.Contains(stream, syntheticGitHubToken[4:]) || !strings.Contains(stream, "using [REDACTED]") {
		t.Fatalf("the tool path streamed a credential: %s", stream)
	}
}

// The stream holds lines back only while they may begin a registered secret
// that spans lines, never more than that secret, and keeps a bounded
// lookback of what it already showed.
func TestLineStreamRedactorHoldsOnlyWhileASecretMayContinue(t *testing.T) {
	secret := "-----BEGIN R11 HOLD-----\nr11-hold-line-one\nr11-hold-line-two\n-----END R11 HOLD-----"
	RegisterSensitiveValue(secret)
	stream := newLineStreamRedactor()
	var shown []string
	for i, line := range strings.Split("before\n"+secret+"\nafter", "\n") {
		shown = append(shown, stream.push(line)...)
		// "before" shows at once; the secret's lines wait for its end and
		// then show masked together.
		if want := []int{1, 1, 1, 1, 5, 6}[i]; len(shown) != want {
			t.Fatalf("after line %d %q: shown %d lines, want %d: %q", i, line, len(shown), want, shown)
		}
	}
	if joined := strings.Join(shown, "\n"); strings.Contains(joined, "r11-hold-line") || !strings.HasSuffix(joined, "after") {
		t.Fatalf("stream = %q", joined)
	}
	if stream.lookbackLen() > stream.keep() || len(stream.pending) != 0 {
		t.Fatalf("lookback %d bytes, %d lines held", stream.lookbackLen(), len(stream.pending))
	}
	// A stream that ends inside a possible secret shows what it held.
	stream = newLineStreamRedactor()
	if held := stream.push("-----BEGIN R11 HOLD-----"); len(held) != 0 {
		t.Fatalf("a possible secret start was shown at once: %q", held)
	}
	if flushed := stream.flush(); len(flushed) != 1 || flushed[0] != "-----BEGIN R11 HOLD-----" {
		t.Fatalf("flush = %q", flushed)
	}
	// Long lines keep the lookback bounded: it holds the bytes that are not
	// blanks that it may keep, and the line breaks between them.
	stream = newLineStreamRedactor()
	for i := 0; i < 4; i++ {
		stream.push(strings.Repeat("z", 1000))
	}
	kept := 0
	for _, c := range stream.buf[:stream.base] {
		if !isBlank(c) {
			kept++
		}
	}
	if kept != stream.keep() {
		t.Fatalf("lookback holds %d bytes that are not blanks, want %d", kept, stream.keep())
	}
	// Blank lines do not use it up, to a limit.
	for i := 0; i < 1000; i++ {
		stream.push("")
	}
	if got := stream.lookbackLen(); got > stream.keep()+maxLookbackBlanks {
		t.Fatalf("lookback = %d bytes after blank lines, limit %d", got, stream.keep()+maxLookbackBlanks)
	}
}
