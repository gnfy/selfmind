package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

// readThroughTheToolChain reads a file with read_file through the production
// result middleware, which shows the file's bytes as they are.
func readThroughTheToolChain(t *testing.T, content string) kernel.ToolDispatchResult {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	registry.Register(NewReadFileTool())
	registry.UseResultMiddleware(RedactionMiddleware())
	result, err := registry.DispatchResult("read_file", map[string]interface{}{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A stream holds lines only while they may begin a secret: never for
// punctuation alone, never for longer than such a secret can be, and the
// cost per line does not grow with what is held.
func TestStreamHoldIsBoundedAndCheap(t *testing.T) {
	RegisterSensitiveValue(`{"type":"r13_account","client_id":"r13client7d102e"}`)
	// A pretty-printed JSON secret is a multi-line one that opens with a
	// line of punctuation.
	RegisterSensitiveValue("{\n  \"kind\": \"r13-pretty\",\n  \"user\": \"r13prettyuser7d102e\"\n}")
	stream := newLineStreamRedactor()
	if shown := stream.push("config {"); len(shown) != 1 {
		t.Fatalf("a line ending in { was held: %q", shown)
	}
	stream = newLineStreamRedactor()
	var pretty []string
	for _, line := range strings.Split("{\n  \"kind\": \"r13-pretty\",\n  \"user\": \"r13prettyuser7d102e\"\n}\nafter", "\n") {
		pretty = append(pretty, stream.push(line)...)
	}
	if joined := strings.Join(pretty, "\n"); strings.Contains(joined, "r13-pretty") || strings.Contains(joined, "r13prettyuser7d102e") || len(pretty) != 5 || !strings.HasSuffix(joined, "after") {
		t.Fatalf("a registered pretty-printed secret: %q", pretty)
	}
	stream = newLineStreamRedactor()
	stream.push("{")
	start := time.Now()
	shown := 0
	for i := 0; i < 16000; i++ {
		shown += len(stream.push("   "))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second || len(stream.pending) > 2 || shown < 15990 {
		t.Fatalf("whitespace after { was held: %d lines pending, %d shown, %s", len(stream.pending), shown, elapsed)
	}
	// A real beginning is held, but not past what the secret could span, and
	// holding it costs no more per line than not holding it.
	stream = newLineStreamRedactor()
	stream.push(`{"type":`)
	held, shown := 0, 0
	start = time.Now()
	for i := 0; i < 16000; i++ {
		shown += len(stream.push("   "))
		held = max(held, stream.heldBytes())
	}
	if held > 4096 || shown < 15000 || time.Since(start) > 2*time.Second {
		t.Fatalf("held %d bytes, shown %d of 16000 lines, in %s", held, shown, time.Since(start))
	}
	// Once no layout of the secret can match any more, nothing is held, and
	// the next line is shown at once.
	if stream.heldBytes() != 0 || len(stream.push("   ")) != 1 {
		t.Fatalf("a secret that cannot match any more still holds %d bytes", stream.heldBytes())
	}
	// The same secret printed pretty-printed is still masked whole.
	stream = newLineStreamRedactor()
	var out []string
	for _, line := range strings.Split("{\n  \"type\": \"r13_account\",\n  \"client_id\": \"r13client7d102e\"\n}\nafter", "\n") {
		out = append(out, stream.push(line)...)
	}
	if joined := strings.Join(out, "\n"); strings.Contains(joined, "r13_account") || strings.Contains(joined, "r13client7d102e") || !strings.HasSuffix(joined, "after") || len(out) != 5 {
		t.Fatalf("a pretty-printed JSON secret: %q", out)
	}
}

// Held lines are released when the command ends, also when it is cancelled.
func TestStreamReleasesHeldLinesWhenCancelled(t *testing.T) {
	RegisterSensitiveValue("-----BEGIN R13 CANCEL-----\nr13-cancel-body\n-----END R13 CANCEL-----")
	events := make(chan string, 16)
	ctx, cancel := context.WithTimeout(kernel.WithEventChannel(context.Background(), events), 50*time.Millisecond)
	defer cancel()
	runner := &drainOrderRunner{payload: "-----BEGIN R13 CANCEL-----", holdWriters: 2 * time.Second}
	if _, err := runCommandStreaming(ctx, runner, "cat key", "terminal", "cancel-flush"); err == nil {
		t.Fatal("cancellation was not reported")
	}
	close(events)
	var lines []string
	for raw := range events {
		if event, ok := kernel.DecodeAgentEvent(raw); ok && event.Type == "tool.output" {
			lines = append(lines, event.Content)
		}
	}
	if len(lines) != 1 || lines[0] != "-----BEGIN R13 CANCEL-----" {
		t.Fatalf("held line after cancel = %q", lines)
	}
}

// Held lines are released when the command exits on its own, too.
func TestStreamReleasesHeldLinesWhenTheCommandExits(t *testing.T) {
	RegisterSensitiveValue("-----BEGIN R14 EXIT-----\nr14-exit-body\n-----END R14 EXIT-----")
	events := make(chan string, 16)
	ctx := kernel.WithEventChannel(context.Background(), events)
	runner := &drainOrderRunner{payload: "before\n-----BEGIN R14 EXIT-----", holdWriters: time.Millisecond}
	if _, err := runCommandStreaming(ctx, runner, "cat key", "terminal", "exit-flush"); err != nil {
		t.Fatal(err)
	}
	close(events)
	var lines []string
	for raw := range events {
		if event, ok := kernel.DecodeAgentEvent(raw); ok && event.Type == "tool.output" {
			lines = append(lines, event.Content)
		}
	}
	if len(lines) != 2 || lines[0] != "before" || lines[1] != "-----BEGIN R14 EXIT-----" {
		t.Fatalf("lines after the command exited = %q", lines)
	}
}

// Scalars inside a container a credential name opened are masked in the
// stream as they are in the result, across lines and on one line.
func TestStreamMasksCredentialContainersAsTheResultDoes(t *testing.T) {
	pretty := "{\n  \"api_keys\": [\n    \"r13k1-7d102e\",\n    \"r13k2-7d102e\"\n  ],\n  \"auth\": {\n    \"credentials\": {\n      \"user\": \"r13user\",\n      \"pass\": \"r13pass-7d102e\"\n    }\n  },\n  \"ok\": true\n}"
	for _, tc := range []struct {
		name, output string
		hidden       []string
	}{
		{"pretty-printed containers", pretty, []string{"r13k1-7d102e", "r13k2-7d102e", "r13pass-7d102e"}},
		{"a container on one line", "{\n  \"cfg\": 1,\n  \"credentials\": {\"pass\": \"r13one-7d102e\", \"pin\": 4242}\n}", []string{"r13one-7d102e", "4242"}},
		{"a container inside a log line", `loaded {"credentials": {"pass": "r13log-7d102e"}} from disk`, []string{"r13log-7d102e"}},
	} {
		assertChainHides(t, tc.name, tc.output, tc.hidden...)
	}
	// References listed under a plural name stay readable.
	refs := "{\n  \"imagePullSecrets\": [\n    {\"name\": \"regcred-r13\"}\n  ]\n}"
	assertChainKeeps(t, "image pull secret references", refs, "regcred-r13")
}

// Nested JSON strings are decoded level by level up to the bound.
func TestDecodedWalkReachesNestedLevels(t *testing.T) {
	secret := `wpr"r13nest+secret`
	RegisterSensitiveValue(secret)
	level := `{"note":"wpr` + jsonEscape("0022") + `r13nest` + jsonEscape("002B") + `secret"}`
	for depth := 1; depth <= 3; depth++ {
		wrapped, _ := json.Marshal(map[string]string{"body": level})
		level = string(wrapped)
		got, unparseable := redactSensitiveReport(level)
		if strings.Contains(got, "r13nest") || unparseable || !json.Valid([]byte(got)) {
			t.Fatalf("nesting %d: %s unparseable=%v", depth, got, unparseable)
		}
	}
}

// A secret registered with LF line endings is found in a file that uses
// CRLF, and the reverse.
func TestRegisteredSecretsMatchEitherLineEnding(t *testing.T) {
	lf := "r13-lf-line-one\nr13-lf-line-two-secret"
	RegisterSensitiveValue(lf)
	if result := readThroughTheToolChain(t, "key:\r\nr13-lf-line-one\r\nr13-lf-line-two-secret\r\n"); strings.Contains(result.Output, "r13-lf-line") {
		t.Fatalf("CRLF file showed an LF secret: %q", result.Output)
	}
	crlf := "r13-crlf-line-one\r\nr13-crlf-line-two-secret"
	RegisterSensitiveValue(crlf)
	if result := readThroughTheToolChain(t, "key:\nr13-crlf-line-one\nr13-crlf-line-two-secret\n"); strings.Contains(result.Output, "r13-crlf-line") {
		t.Fatalf("LF file showed a CRLF secret: %q", result.Output)
	}
}

// "tokens" counts tokens; it is not a credential name.
func TestTokensIsNotACredentialName(t *testing.T) {
	for _, output := range []string{
		`{"tokens": ["alpha-r13", "beta-r13"], "max_tokens": 4096}`,
		`{"usage": {"tokens": 1234}}`,
		"tokens: 1234",
	} {
		result, _ := chainSurfaces(t, output)
		if result.Output != strings.TrimRight(output, "\n") {
			t.Errorf("%s was changed: %s", output, result.Output)
		}
	}
}

// A document after whitespace JSON does not allow, or a byte order mark, is
// still walked decoded.
func TestJSONDocumentPreambleDoesNotStopTheDecodedWalk(t *testing.T) {
	RegisterSensitiveValue(`wpr"r13pre+secret`)
	body := `{"note":"wpr` + jsonEscape("0022") + `r13pre` + jsonEscape("002B") + `secret"}`
	for _, preamble := range []string{"\f", "\v", string(rune(0xA0)), string(rune(0xFEFF)), " \n"} {
		got, _ := redactSensitiveReport(preamble + body + "\n")
		if strings.Contains(got, "r13pre") || !strings.HasPrefix(got, preamble) {
			t.Errorf("preamble %q: %q", preamble, got)
		}
	}
}

// Registering a value again changes nothing, so the snapshot stays.
func TestRegisteringAKnownSecretKeepsTheSnapshot(t *testing.T) {
	registry := NewSecretRegistry()
	registry.Register("r13-known-secret-value")
	first := registry.snapshot()
	registry.Register("r13-known-secret-value")
	if registry.snapshot() != first {
		t.Fatal("re-registering an unchanged value rebuilt the snapshot")
	}
	registry.Register("r13-other-secret-value")
	if registry.snapshot() == first {
		t.Fatal("a new value did not rebuild the snapshot")
	}
}

// Code previews and logged results are masked before they are cut.
func TestPreviewsAreMaskedBeforeTheyAreCut(t *testing.T) {
	const token = "ghp_R7k2Qx9LmN4pT8vW1yZ3aB6cD0eF5gH2jK7s"
	code := strings.Repeat("x", 2040) + " " + token
	args := ApprovalPersistentArgs("execute_code", map[string]interface{}{"code": code})
	if preview, _ := args["code_preview"].(string); strings.Contains(preview, "R7k") {
		t.Fatalf("code preview kept part of a token: %q", preview[len(preview)-40:])
	}
	var logged bytes.Buffer
	log.Init(log.Options{Level: "debug", Output: &logged})
	t.Cleanup(func() { log.Init(log.Options{}) })
	exec := LoggingMiddleware(func(map[string]interface{}) (string, error) {
		return strings.Repeat("y", 190) + " " + token, nil
	})
	if _, err := exec(map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "R7k") || !strings.Contains(logged.String(), "tool result") {
		t.Fatalf("logged preview kept part of a token: %q", logged.String())
	}
}

// Private keys are masked by their armor: the body goes, the armor lines
// stay, in the result and in the stream, whatever the key type.
func TestPrivateKeysAreMaskedByTheirArmor(t *testing.T) {
	body := strings.Repeat("b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n", 12)
	for _, kind := range []string{"OPENSSH PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "PRIVATE KEY", "ENCRYPTED PRIVATE KEY"} {
		key := "-----BEGIN " + kind + "-----\n" + body + "-----END " + kind + "-----"
		output := "id_key:\n" + key + "\ndone"
		assertChainHides(t, kind, output, "b3BlbnNzaC1rZXkt")
		assertChainKeeps(t, kind+" armor", output, "-----BEGIN "+kind+"-----", "-----END "+kind+"-----", "done")
	}
	encrypted := "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,0123456789ABCDEF\n\nr13EncryptedBodyMaterialLine0001\nr13EncryptedBodyMaterialLine0002\n-----END RSA PRIVATE KEY-----"
	assertChainHides(t, "an encrypted PEM", encrypted, "r13EncryptedBodyMaterial")
	escaped, _ := json.Marshal(map[string]string{"private_key_pem": "-----BEGIN PRIVATE KEY-----\nr13JsonKeyBodyMaterial\n-----END PRIVATE KEY-----\n"})
	result, _ := chainSurfaces(t, `{"cfg":`+string(escaped)+`}`)
	if strings.Contains(result.Output, "r13JsonKeyBody") || !json.Valid([]byte(result.Output)) {
		t.Fatalf("an escaped PEM: %s", result.Output)
	}
	// Certificates and public keys are not secrets.
	cert := "-----BEGIN CERTIFICATE-----\nMIIBr13CertificateBody\n-----END CERTIFICATE-----\n-----BEGIN PUBLIC KEY-----\nMIIBr13PublicKeyBody\n-----END PUBLIC KEY-----"
	assertChainKeeps(t, "certificate", cert, "MIIBr13CertificateBody", "MIIBr13PublicKeyBody")
}

// Credentials in common formats that have neither a credential name nor a
// distinctive prefix of their own.
func TestCommonCredentialFormatsAreMasked(t *testing.T) {
	dockerAuth := base64.StdEncoding.EncodeToString([]byte("r13docker:r13dockerpass"))
	for _, tc := range []struct {
		name, output string
		hidden       []string
	}{
		{"URL userinfo", "DATABASE_URL_HOST postgres://app:r13urlpw-7d102e@db.internal:5432/app", []string{"r13urlpw-7d102e"}},
		{"git remote with a token", "origin https://r13user:r13gittoken7d102e@github.com/org/repo.git (fetch)", []string{"r13gittoken7d102e"}},
		{"Cookie header", "> Cookie: sid=r13sid7d102e; csrftoken=r13csrf7d102e", []string{"r13sid7d102e", "r13csrf7d102e"}},
		{"Set-Cookie header", "< Set-Cookie: session=r13session7d102e; Path=/; HttpOnly", []string{"r13session7d102e"}},
		{"kubeconfig", "users:\n- name: dev\n  user:\n    client-key-data: TFMwdHIxM2tleWRhdGE=\n    token: r13kubetoken7d102e", []string{"TFMwdHIxM2tleWRhdGE=", "r13kubetoken7d102e"}},
		{"docker config", `{"auths":{"registry.example":{"auth":"` + dockerAuth + `"}}}`, []string{dockerAuth}},
		{"npm _auth", "_auth=" + dockerAuth + "\n//registry.npmjs.org/:_authToken=npm_r13token7d102e", []string{dockerAuth, "npm_r13token7d102e"}},
		{".netrc on one line", "machine api.example.com login bob password r13netrc7d102e", []string{"r13netrc7d102e"}},
		{".netrc on lines", "machine api.example.com\n  login bob\n  password r13netrc2-7d102e\n", []string{"r13netrc2-7d102e"}},
		{"Azure connection string", "DefaultEndpointsProtocol=https;AccountName=r13acct;AccountKey=r13azurekey7d102e==;EndpointSuffix=core.windows.net", []string{"r13azurekey7d102e"}},
		{"Azure SAS", "Endpoint=sb://x.servicebus.windows.net/;SharedAccessSignature=sv=2020-08-04&sig=r13sas7d102e%3D", []string{"r13sas7d102e"}},
		{"Azure SAS written with separators", "shared_access_signature: sv=2020-08-04&sig=r14sas2-7d102e", []string{"r14sas2-7d102e"}},
		{"Slack tokens", strings.Join([]string{"xoxb", "1234567890", "r13slackbot7d102e and xoxp", "1234567890", "r13slackuser7d102e"}, "-"), []string{"r13slackbot7d102e", "r13slackuser7d102e"}},
		{"Stripe keys", "sk_live_r13stripe7d102eABCDEFGH rk_live_r13restrict7d102eABCD", []string{"r13stripe7d102e", "r13restrict7d102e"}},
		{"Google API key", "key AIzaSyR13googleKey7d102eABCDEFGHIJKLMNO", []string{"SyR13googleKey7d102e"}},
		{"GitLab token", strings.Join([]string{"glpat", "r13gitlab7d102eABCDE"}, "-"), []string{"r13gitlab7d102e"}},
		{"Hugging Face token", "hf_r13huggingface7d102eABCDEFGHIJKLMN", []string{"r13huggingface7d102e"}},
		{"mysql -p", "mysql -h db -ur13 -pr13mysql7d102e appdb", []string{"r13mysql7d102e"}},
		{"curl -u", "curl -u r13curluser:r13curlpw7d102e https://example.test", []string{"r13curlpw7d102e"}},
		{"curl --user", "curl --user r13curluser:r13curlpw2-7d102e https://example.test", []string{"r13curlpw2-7d102e"}},
	} {
		assertChainHides(t, tc.name, tc.output, tc.hidden...)
	}
	for _, output := range []string{
		"http://localhost:8080/api/v1/users",
		"git@github.com:org/repo.git",
		"mail me at user@example.com",
		"cookie: banner dismissed",
		"Set-Cookie attributes: Path=/; HttpOnly",
		"client-certificate-data: TFMwdHIxM2NlcnRkYXRh",
		`{"auth": "required", "mode": "oauth2"}`,
		"password reset is required for all users",
		"AccountName=r13acct;EndpointSuffix=core.windows.net",
		"hf_model_config_name",
		"mkdir -p build/out && ssh -p 2222 host",
		"docker run -p 8080:80 -u 1000:1000 image",
		"git push -u origin main",
		"mysql -p appdb",
	} {
		assertChainKeeps(t, output, output, output)
	}
	// The user name of a URL or curl -u stays.
	assertChainKeeps(t, "URL user", "postgres://app:r13urlpw-7d102e@db.internal:5432/app", "postgres://app:", "@db.internal:5432/app")
	assertChainKeeps(t, "curl user", "curl -u r13curluser:r13curlpw7d102e https://example.test", "r13curluser:")
}

// Color codes between a credential's parts do not hide it.
func TestColorCodesDoNotHideCredentials(t *testing.T) {
	RegisterSensitiveValue("r13colored-registered-7d102e")
	jq := "\x1b[1;39m{\n  \x1b[0m\x1b[34;1m\"password\"\x1b[0m\x1b[1;39m:\x1b[0m \x1b[0;32m\"r13ansi7d102e\"\x1b[0m\x1b[1;39m\n\x1b[1;39m}\x1b[0m"
	grep := "config.env:3:API_TOKEN=\x1b[01;31m\x1b[Kr13grep7d102e\x1b[m\x1b[K"
	split := "found r13colored-\x1b[01;31mregistered\x1b[0m-7d102e here"
	for name, output := range map[string]string{"jq -C": jq, "grep --color": grep, "a registered secret split by color": split} {
		assertChainHides(t, name, output, "r13ansi7d102e", "r13grep7d102e", "registered")
	}
}

// Pagination tokens are how a model pages through an API; they are not
// credentials.
func TestPaginationTokensAreNotMasked(t *testing.T) {
	for _, output := range []string{
		`{"items":[],"nextPageToken":"CiAKGjBpNDd2r13page"}`,
		`{"NextToken":"r13next-token-AAAA","Items":[]}`,
		`{"continuationToken":"r13continue","pageToken":"r13pagetok","cursor":"r13cursor"}`,
		"next_page_token: r13nextpage",
		"aws s3api list-objects-v2 --starting-token r13starting",
	} {
		result, streamed := chainSurfaces(t, output)
		if result.Output != output || strings.Join(streamed, "\n") != output {
			t.Errorf("a pagination token was masked:\n in: %s\nout: %s\nstream: %s", output, result.Output, strings.Join(streamed, "\n"))
		}
	}
}

// A plural name marks a value only when the value looks like a credential:
// not a count, not a list of object names, not the next key.
func TestPluralNamesMarkOnlyCredentialLookingValues(t *testing.T) {
	for _, output := range []string{
		"secrets: 3 found",
		"passwords: 0 changed",
		"spec:\n  imagePullSecrets:\n  - name: regcred-r13\n",
		"kind: ServiceAccount\nsecrets:\n- name: default-token-r13ab\n",
		`{"imagePullSecrets":[{"name":"regcred-r13"}],"secrets":[{"name":"sa-token-r13"}]}`,
	} {
		result, _ := chainSurfaces(t, output)
		if result.Output != strings.TrimRight(output, "\n") {
			t.Errorf("a plural name masked a reference or count:\n in: %s\nout: %s", output, result.Output)
		}
	}
	for _, tc := range []struct{ output, hidden string }{
		{`{"secrets":["r13plural1-7d102e","r13plural2-7d102e"]}`, "r13plural"},
		{"passwords: r13pwlist-7d102e", "r13pwlist-7d102e"},
		{`{"passwords":{"db":"r13dbpw-7d102e","name":"keep"}}`, "r13dbpw-7d102e"},
		{"apiKeys: r13apikeys-7d102e", "r13apikeys-7d102e"},
	} {
		assertChainHides(t, tc.output, tc.output, tc.hidden)
	}
}

// Code that names a credential without holding one keeps its expression;
// a literal value is still masked.
func TestCodeExpressionsAreNotMaskedAsCredentials(t *testing.T) {
	for _, output := range []string{
		`token := os.Getenv("GITHUB_TOKEN")`,
		"c.token = loadToken(cfg)",
		"this.apiKey = config.apiKey;",
		"const client = new Client({ apiKey: someVar, timeout: 3 })",
		"const options = {\n  timeout: 3,\n  apiKey: someVar,\n}",
		`password = os.environ["DB_PASSWORD"]`,
		"volumes:\n- name: tls\n  secret:\n    secretName: app-tls-r13\n",
		"Bearer authentication is required for this endpoint",
	} {
		result, _ := chainSurfaces(t, output)
		if result.Output != strings.TrimRight(output, "\n") {
			t.Errorf("code was masked:\n in: %s\nout: %s", output, result.Output)
		}
	}
	for _, tc := range []struct{ output, hidden string }{
		{`c.token = "r13literal-7d102e"`, "r13literal-7d102e"},
		{`apiKey: "r13literal2-7d102e",`, "r13literal2-7d102e"},
		{`token := "r13literal3-7d102e"`, "r13literal3-7d102e"},
		{"password: hunter2-r13", "hunter2-r13"},
		{"export API_KEY=r13env7d102e", "r13env7d102e"},
		// Nothing on the line says it is code: a bare identifier after a
		// colon is read as a value, which costs a variable's name, not a
		// secret.
		{"apiKey: r13someVar", "r13someVar"},
	} {
		assertChainHides(t, tc.output, tc.output, tc.hidden)
	}
}
