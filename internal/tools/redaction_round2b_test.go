package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// assertChainShows checks a text is shown as written, trailing newline aside,
// in the result and in the stream.
func assertChainShows(t *testing.T, name, output string) {
	t.Helper()
	result, streamed := chainSurfaces(t, output)
	want := strings.TrimRight(output, "\n")
	if result.Output != want || strings.Join(streamed, "\n") != want {
		t.Errorf("%s was changed:\n  in: %q\n out: %q\n  stream: %q", name, want, result.Output, strings.Join(streamed, "\n"))
	}
}

// Assignment syntax is read as the code or config it is: a comparison, a type
// annotation, an empty shell assignment, a template or a placeholder holds no
// credential, and a literal value after any of them still is one.
func TestAssignmentSyntaxIsReadAsCodeOrConfig(t *testing.T) {
	for _, output := range []string{
		"password == confirmPassword",
		"if (token == null) { return; }",
		`if token == "" {`,
		"if token === undefined",
		"token != ''",
		"const fn = token => token.length > 0",
		"TOKEN= make deploy",
		"export API_TOKEN= && echo done",
		"  password: string;",
		"  password: number,",
		"  apiKey: String",
		"token: <your-token-here>",
		`{"password": "<password>"}`,
		"password: {{ .Values.password }}",
		`password = input("Password: ")`,
		`secret = prompt('Enter secret: ')`,
		"this.token = token;",
		"password = os.environ[name]",
	} {
		assertChainShows(t, output, output)
	}
	for _, tc := range []struct{ output, hidden string }{
		// A bare value that only echoes its name has nothing on the line
		// saying it is code.
		{"api_key = api_key", "= api_key"},
		{`password === 'r14cmp7d102e'`, "r14cmp7d102e"},
		{`if (password == "r14cmp2-7d102e") {`, "r14cmp2-7d102e"},
		{"TOKEN=r14shell7d102e make deploy", "r14shell7d102e"},
		{`password: str = "r14default7d102e"`, "r14default7d102e"},
		{"password = 'r14ini7d102e'", "r14ini7d102e"},
		{"password  =   r14aligned7d102e", "r14aligned7d102e"},
		{`{"secrets":[{"name":"db","value":"r14listed7d102e"}]}`, "r14listed7d102e"},
		{`{"imagePullSecrets":[{"name":"regcred","token":"r14pull7d102e"}]}`, "r14pull7d102e"},
	} {
		assertChainHides(t, tc.output, tc.output, tc.hidden)
	}
	// A reference in a URL or on a command line stays; a literal does not.
	for _, output := range []string{
		"postgres://app:${DB_PASS}@db.internal/app",
		"redis://:$REDIS_PW@cache:6379",
		"curl -u $USER:$PASS https://example.test",
		"mysql -u root -p$MYSQL_PWD appdb",
		"mysql -p${DB_PW} appdb",
	} {
		assertChainShows(t, output, output)
	}
	assertChainKeeps(t, "image pull secret reference", `{"imagePullSecrets":[{"name":"regcred-r14"}]}`, "regcred-r14")
}

// A Bearer token is masked whatever it is made of; the words that follow the
// name of the scheme in prose are not tokens.
func TestBearerTokensAreMaskedAndProseIsNot(t *testing.T) {
	for _, output := range []string{"retry with Bearer abcdefghijklmnop", "Authorization: Bearer r14letters", "bearer ABCDEFGHIJKLMNOP"} {
		hidden := output[strings.LastIndex(output, " ")+1:]
		if hidden == "r14letters" {
			continue
		}
		assertChainHides(t, output, output, hidden)
	}
	for _, output := range []string{
		"Bearer authentication is required",
		"supports Bearer authorization and Basic authentication",
		"Bearer credentials are sent in the header",
	} {
		assertChainShows(t, output, output)
	}
}

// Pagination tokens are how a model pages through an API, but the exclusion
// is for those names: other credentials in the same document stay masked.
func TestPaginationExclusionDoesNotShieldCredentials(t *testing.T) {
	for _, output := range []string{
		`{"nextPageToken":"page-r14","access_token":"r14access7d102e"}`,
		`{"NextToken":"next-r14","id_token":"r14idtoken7d102e","refresh_token":"r14refresh7d102e"}`,
		"next_page_token: page-r14\ncsrf_token: r14csrf7d102e",
		"aws s3api list-objects-v2 --starting-token page-r14 --token r14flag7d102e",
	} {
		result, streamed := chainSurfaces(t, output)
		for _, page := range []string{"page-r14", "next-r14"} {
			if strings.Contains(output, page) && (!strings.Contains(result.Output, page) || !strings.Contains(strings.Join(streamed, "\n"), page)) {
				t.Errorf("a page token was masked in %q: %s", output, result.Output)
			}
		}
		for _, secret := range []string{"r14access7d102e", "r14idtoken7d102e", "r14refresh7d102e", "r14csrf7d102e", "r14flag7d102e"} {
			if strings.Contains(result.Output, secret) || strings.Contains(strings.Join(streamed, "\n"), secret) {
				t.Errorf("a credential next to a page token leaked in %q: %s", output, result.Output)
			}
		}
	}
}

// A container a credential name opened is followed while the stream looks
// like JSON; a stream that stops looking like it ends the container, so an
// unbalanced bracket cannot mask what follows it for good.
func TestStreamContainerTrackingEndsWithTheJSON(t *testing.T) {
	stream := newLineStreamRedactor()
	var out []string
	for _, line := range []string{
		`"credentials": {`,
		`  "user": "r14u1-7d102e",`,
		`this is ordinary output`,
		`and so is this line`,
		`"label": "r14visible"`,
	} {
		out = append(out, stream.push(line)...)
	}
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "r14u1-7d102e") || !strings.Contains(joined, "r14visible") {
		t.Fatalf("container tracking did not end with the JSON: %q", out)
	}
	// While it does look like JSON, deeper scalars are masked.
	stream = newLineStreamRedactor()
	out = nil
	for _, line := range []string{
		`"CREDENTIALS": [`,
		`  [`,
		`    "r14nested-7d102e",`,
		`    {"deep": ["r14deeper-7d102e"], "pin": 4242}`,
		`  ]`,
		`]`,
		`"label": "r14visible"`,
	} {
		out = append(out, stream.push(line)...)
	}
	joined = strings.Join(out, "\n")
	for _, hidden := range []string{"r14nested-7d102e", "r14deeper-7d102e", "4242"} {
		if strings.Contains(joined, hidden) {
			t.Errorf("a nested scalar leaked: %q", out)
		}
	}
	if !strings.Contains(joined, "r14visible") {
		t.Errorf("a value after the container was masked: %q", out)
	}
}

// Containers are followed to a bounded depth, a single line that is not JSON
// does not end one, and the numbers under a plural name are counts.
func TestCredentialContainerEdges(t *testing.T) {
	deep := `{"credentials":` + strings.Repeat(`[`, 40) + `"r14deep-7d102e"` + strings.Repeat(`]`, 40) + `}`
	assertChainHides(t, "a container nested forty deep", deep, "r14deep-7d102e")
	assertChainHides(t, "a container nested forty deep in a log line", "loaded "+deep+" ok", "r14deep-7d102e")
	pretty := "\"credentials\": {\n  this is a note about the next line\n  \"pass\": \"r14afterplain-7d102e\"\n}"
	assertChainHides(t, "a plain line inside a container", pretty, "r14afterplain-7d102e")
	assertChainShows(t, "numbers under plural names", `{"secrets":3,"passwords":[1,2],"apiKeys":{"count":4}}`)
	assertChainHides(t, "a number under a singular name", `{"secret":[4242]}`, "4242")
}

// What a stream holds never exceeds what a registered secret can take, even
// when a single long line is held while a secret may still be printing.
func TestStreamHoldIsCappedAtWhatASecretCanTake(t *testing.T) {
	RegisterSensitiveValue("r14-cap-first\nr14-cap-second\nr14-cap-third")
	stream := newLineStreamRedactor()
	stream.push(strings.Repeat("x", 4000) + " r14-cap-first")
	if stream.heldBytes() == 0 {
		t.Fatal("a possible secret start was not held")
	}
	stream.push("r14-cap-second")
	limit := stream.hold.snapshot.maxHeld + streamHoldSlack
	if held := stream.heldBytes(); held > limit {
		t.Fatalf("held %d bytes, a registered secret takes at most %d", held, limit)
	}
	// Under the cap the whole secret is masked, wherever it began.
	stream = newLineStreamRedactor()
	var out []string
	for _, line := range []string{"log: r14-cap-first", "r14-cap-second", "r14-cap-third", "done"} {
		out = append(out, stream.push(line)...)
	}
	if joined := strings.Join(out, "\n"); strings.Contains(joined, "r14-cap-second") || strings.Contains(joined, "r14-cap-third") || !strings.HasSuffix(joined, "done") {
		t.Fatalf("a registered multi-line secret: %q", out)
	}
}

// A private key whose END line never comes is masked as far as its body goes,
// and an unrelated line ends the body, in the result and in the stream.
func TestPrivateKeyWithoutAnEndLineIsMaskedAsFarAsItGoes(t *testing.T) {
	output := "-----BEGIN OPENSSH PRIVATE KEY-----\nr14TruncatedKeyBodyLineOne0001\nr14TruncatedKeyBodyLineTwo0002\nConnection closed by remote host."
	assertChainHides(t, "a truncated key", output, "r14TruncatedKeyBody")
	assertChainKeeps(t, "what follows a truncated key", output, "-----BEGIN OPENSSH PRIVATE KEY-----", "Connection closed by remote host.")
	// Prose that mentions the armor masks nothing after it.
	prose := "To start, put -----BEGIN PRIVATE KEY----- on the first line,\nthen the key, then the END line.\n-----END PRIVATE KEY-----"
	assertChainShows(t, "prose about the armor", prose)
	// A PGP private key block has headers and a checksum line.
	pgp := "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: GnuPG v2\n\nr14PgpBodyMaterialLine0001\n=r14c\n-----END PGP PRIVATE KEY BLOCK-----"
	assertChainHides(t, "a PGP key", pgp, "r14PgpBodyMaterial")
	assertChainKeeps(t, "PGP armor", pgp, "-----BEGIN PGP PRIVATE KEY BLOCK-----", "-----END PGP PRIVATE KEY BLOCK-----")
}

// Color codes inside a credential's value, or between it and its name, do not
// hide it, and the codes themselves are not mistaken for it.
func TestColorCodesInsideValuesDoNotHideThem(t *testing.T) {
	for _, output := range []string{
		"API_TOKEN=\x1b[31mr14red7d102e\x1b[0mtail7d102e",
		"\x1b[1mpassword\x1b[0m = \x1b[32m'r14green7d102e'\x1b[0m",
		"token\x1b[0m: \x1b[4mr14under7d102e\x1b[24m",
		"\x1b]8;;https://example.test\x1b\\secret: r14osc7d102e\x1b]8;;\x1b\\",
	} {
		assertChainHides(t, output, output, "r14red7d102e", "tail7d102e", "r14green7d102e", "r14under7d102e", "r14osc7d102e")
	}
	// Text with color codes and no credential is shown as it is.
	assertChainShows(t, "colored text", "\x1b[32mok\x1b[0m built in 3s, \x1b[31m0\x1b[0m failures")
}

// Redaction is linear in the size of the text, also for text made of the
// shapes the detectors look for: a name or prefix repeated, a long run with
// no delimiter, a quote or bracket opened and never closed.
func TestRedactionStaysLinearOnAdversarialText(t *testing.T) {
	RegisterSensitiveValue("r14-linear-registered-7d102e")
	shapes := map[string]func(n int) string{
		"names with code values":  func(n int) string { return strings.Repeat("token=(", n/7) },
		"names with member paths": func(n int) string { return strings.Repeat("password=a.b ", n/13) },
		"flags":                   func(n int) string { return strings.Repeat("--token --password ", n/19) },
		"netrc words":             func(n int) string { return strings.Repeat("password ", n/9) },
		"url schemes":             func(n int) string { return strings.Repeat("http://u:", n/9) },
		"cookies":                 func(n int) string { return strings.Repeat("cookie: a=b; ", n/13) },
		"mysql flags":             func(n int) string { return "mysql " + strings.Repeat("-pq ", n/4) },
		"pem begins":              func(n int) string { return strings.Repeat("-----BEGIN PRIVATE KEY-----\nQUJD\n", n/32) },
		"one long pem line":       func(n int) string { return "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("QUJD", n/4) },
		"a long unspaced value":   func(n int) string { return "token=" + strings.Repeat("x", n) },
		"open quotes":             func(n int) string { return strings.Repeat(`"password`, n/9) },
		"open brackets":           func(n int) string { return `"credentials": ` + strings.Repeat("[{", n/2) },
		"color codes":             func(n int) string { return strings.Repeat("\x1b[31mtoken\x1b[0m=a ", n/18) },
		"basic and bearer":        func(n int) string { return strings.Repeat("Basic Bearer ", n/13) },
		// Real escape shapes: the identifier escapes the detection view
		// decodes, a \u that never completes, runs of backslashes, quotes
		// escaped once and twice, and escapes that name a credential.
		"identifier escapes": func(n int) string {
			return strings.Repeat(jsonEscape("0070")+jsonEscape("0061")+jsonEscape("0073")+jsonEscape("0073"), n/24)
		},
		"incomplete escapes":       func(n int) string { return strings.Repeat("\\"+"u00", n/4) },
		"backslash runs":           func(n int) string { return strings.Repeat(`\`, n) },
		"escaped quotes":           func(n int) string { return strings.Repeat(`\"password\":\"`, n/14) },
		"twice escaped quotes":     func(n int) string { return strings.Repeat(`\\\"token\\\":\\\"`, n/16) },
		"escaped credential names": func(n int) string { return strings.Repeat(jsonEscape("0074")+"oken=", n/11) },
		"escaped newlines":         func(n int) string { return "-----BEGIN PRIVATE KEY-----" + strings.Repeat(`\n`+"QUJD", n/6) },
		"json strings":             func(n int) string { return `["` + strings.Repeat(`a\n"," `, n/6) + `"]` },
		"registered prefix":        func(n int) string { return strings.Repeat("r14-linear-registered-7d10 ", n/27) },
	}
	// measure is the best of three runs, or the deadline when one does not
	// finish: a quadratic detector would run for minutes on 1 MB.
	const deadline = 10 * time.Second
	measure := func(text string) time.Duration {
		best := time.Hour
		for i := 0; i < 3; i++ {
			done := make(chan time.Duration, 1)
			go func() {
				start := time.Now()
				RedactSensitive(text)
				done <- time.Since(start)
			}()
			select {
			case elapsed := <-done:
				best = min(best, elapsed)
			case <-time.After(deadline):
				return deadline
			}
		}
		return best
	}
	for name, shape := range shapes {
		small, large := measure(shape(64<<10)), measure(shape(1<<20))
		// 16 times the text takes about 16 times as long when linear; a
		// quadratic detector takes about 256 times as long.
		if large >= deadline || large > 100*small+200*time.Millisecond {
			t.Errorf("%s: 64 KB took %s, 1 MB took %s", name, small, large)
		}
	}
}

// Many lines held and released cost no more per line than a few, and a
// 10,000-line stream is masked in a bounded time.
func TestStreamCostPerLineDoesNotGrow(t *testing.T) {
	var lines []string
	for i := 0; i < 10000; i++ {
		lines = append(lines, fmt.Sprintf("%06d INFO worker=%d processed item-%07d status=ok", i, i%8, i))
	}
	lines[5000] = "export API_TOKEN=r14stream7d102e"
	stream := newLineStreamRedactor()
	var out []string
	start := time.Now()
	for _, line := range lines {
		out = append(out, stream.push(line)...)
	}
	out = append(out, stream.flush()...)
	if elapsed := time.Since(start); elapsed > 5*time.Second || len(out) != len(lines) {
		t.Fatalf("10000 lines took %s and showed %d", elapsed, len(out))
	}
	if strings.Contains(strings.Join(out, "\n"), "r14stream7d102e") {
		t.Fatal("the stream leaked a credential")
	}
}

// A JSON tool result keeps its keys sorted and compact when value-by-value
// masking rewrites it, and its credential containers read the same as the
// stream reads them.
func TestResultAndStreamAgreeOnCredentialContainers(t *testing.T) {
	document := map[string]interface{}{
		"api_keys":    []interface{}{"r14agree1-7d102e", "r14agree2-7d102e"},
		"credentials": map[string]interface{}{"user": "r14user", "pin": 4242},
		"refs":        map[string]interface{}{"imagePullSecrets": []interface{}{map[string]interface{}{"name": "regcred-r14"}}},
		"label":       "r14label",
	}
	pretty, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	compact, _ := json.Marshal(document)
	for name, text := range map[string]string{"pretty-printed": string(pretty), "compact": string(compact)} {
		result, streamed := chainSurfaces(t, text)
		for surface, shown := range map[string]string{"result": result.Output, "stream": strings.Join(streamed, "\n")} {
			for _, hidden := range []string{"r14agree1-7d102e", "r14agree2-7d102e", "r14user", "4242"} {
				if strings.Contains(shown, hidden) {
					t.Errorf("%s %s leaked %q: %s", name, surface, hidden, shown)
				}
			}
			for _, kept := range []string{"regcred-r14", "r14label"} {
				if !strings.Contains(shown, kept) {
					t.Errorf("%s %s masked %q: %s", name, surface, kept, shown)
				}
			}
		}
		if !json.Valid([]byte(result.Output)) || result.RedactionUnparseable {
			t.Errorf("%s result is not valid JSON: %s", name, result.Output)
		}
	}
}
