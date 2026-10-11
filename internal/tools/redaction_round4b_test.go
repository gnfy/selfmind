package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A literal that has no closing quote, or whose closing quote is far away or
// on another line, is masked as far as its value goes; it is never exempted
// for lack of a quote. The quote forms are doubled, tripled and backtick.
func TestUnterminatedAndLongLiteralsAreMasked(t *testing.T) {
	long := strings.Repeat("A1b2", 640)
	for _, tc := range []struct{ in, secret string }{
		{`os.Setenv("API_TOKEN", "` + long + `")`, long},
		{`os.Setenv("API_TOKEN", "` + long, long},
		{`var password string = "` + long + `"`, long},
		{`const API_TOKEN: &str = "` + long + `";`, long},
		{`password = "` + long + `"`, long},
		{`password: "` + long + `"`, long},
		{`os.Setenv("API_TOKEN", "Xk9ab12Cd`, "Xk9ab12Cd"},
		{`var password string = "Xk9ab12Cd`, "Xk9ab12Cd"},
		{`const T_TOKEN: &str = "Xk9ab12Cd`, "Xk9ab12Cd"},
		{`password = "Xk9ab12Cd`, "Xk9ab12Cd"},
		{`password: "two words here`, "words here"},
		{`password: 'two words here`, "words here"},
		{"os.Setenv(\"API_TOKEN\", \"line1-Xk9\nline2-Xk9\")", "line1-Xk9"},
		{"os.Setenv(\"API_TOKEN\", \"line1-Xk9\nline2-Xk9\")", "line2-Xk9"},
		{"password = \"\"\"\nXk9ab12Cd\nsecond-Xk9ab12\n\"\"\"", "Xk9ab12Cd"},
		{"password = \"\"\"\nXk9ab12Cd\nsecond-Xk9ab12\n\"\"\"", "second-Xk9ab12"},
		{"password = '''Xk9ab12Cd''' # note", "Xk9ab12Cd"},
		{"password = \"\"\"Xk9ab12Cd\"\"\" # note", "Xk9ab12Cd"},
		{"password = `Xk9ab12Cd`", "Xk9ab12Cd"},
		{"token := `line1-Xk9\nline2-Xk9`", "line2-Xk9"},
		{"const token = `Xk9ab12Cd`;", "Xk9ab12Cd"},
		{"password: 'it''s-Xk9ab12Cd-secret'", "Xk9ab12Cd"},
		{"password: 'it''s-Xk9ab12Cd-secret'", "secret'"},
		{"password: ''Xk9ab12Cd''", "Xk9ab12Cd"},
		{"PASSWORD=''Xk9ab12Cd''", "Xk9ab12Cd"},
		{`PASSWORD=""Xk9ab12Cd""`, "Xk9ab12Cd"},
		{`{"password": "x9\"Xk9ab12Cd"}`, "Xk9ab12Cd"},
		{`password: "Xk9""ab12Cd"`, "ab12Cd"},
		{"password: \"Xk9ab\n  12Cd\"", "12Cd"},
		{"password: 'Xk9ab\n  12Cd\n  34Ef'", "34Ef"},
		{`password: "` + strings.Repeat("word ", 130) + `"`, strings.Repeat("word ", 20)},
		{`password: "` + strings.Repeat("word ", 130) + `"`, "word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word word"},
		{`credentials: mysecretvalue`, "mysecretvalue"},
		{`credentials: 'mysecretvalue'`, "mysecretvalue"},
		{`"credentials": "mysecretvalue"`, "mysecretvalue"},
		// A weak name keeps its prose and capability words, but a value with a
		// digit among its letters is a credential in a sentence or after a
		// bare colon as well.
		{`credentials: Xk9mQz2pLw4r more words`, "Xk9mQz2pLw4r"},
		{`credentials: Xk9mQz2pLw4r, then the rest`, "Xk9mQz2pLw4r"},
		{`see credentials: Xk9mQz2pLw4r for the login`, "Xk9mQz2pLw4r"},
		{`credentials:Xk9mQz2pLw4r`, "Xk9mQz2pLw4r"},
	} {
		checkBothSurfaces(t, "literal", tc.in, tc.secret)
	}
	// Empty strings and plain settings hold nothing.
	for _, in := range []string{
		`password = ""`, `password: ''`, `PASSWORD=`, "credentials: include", "credentials: same-origin", "credentials: omit",
		"persist-credentials: false", `credentials: "include"`, `credentials: 'same-origin'`,
	} {
		assertChainShows(t, in, in)
	}
}

// A call that hard-codes a credential in a literal is masked whether it sets
// a value or reads one with a default; a call that only lists names, a
// tuple of names and a name mapped to another name are not.
func TestHardCodedLiteralsInCallsAreMasked(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{`req.Header.Add("X-Api-Key","Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`req.Header.Set("Authorization", "Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`headers.append("Authorization","Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`os.environ.get("API_KEY","Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`os.environ.setdefault("API_KEY", "Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`getenv("API_KEY","Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`os.Getenv("API_KEY", "Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`config.set("auth", "password", "Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`config.set(section, "password", "Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`vault.read("API_KEY", "Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`args.add_argument("--token", default="Xk9ab12Cd")`, "Xk9ab12Cd"},
		{`credentials = [("password", "Xk9ab12Cd")]`, "Xk9ab12Cd"},
		{`x[password] = "Xk9ab12Cd"`, "Xk9ab12Cd"},
		{`password => Xk9ab12Cd`, "Xk9ab12Cd"},
		{`secret => hunter2`, "hunter2"},
		{"<password>Xk9ab12Cd\n</password>", "Xk9ab12Cd"},
		{"<password>\n  Xk9ab12Cd\n</password>", "Xk9ab12Cd"},
		{"<password>" + strings.Repeat("Xk9ab12Cd", 30) + "</password>", strings.Repeat("Xk9ab12Cd", 30)},
		{"<password>" + strings.Repeat("Xk9 ab12", 40) + "</password>", strings.Repeat("Xk9 ab12", 40)},
	} {
		checkBothSurfaces(t, "call", tc.in, tc.secret)
	}
	// One letter is a value too.
	for in, want := range map[string]string{
		`req.Header.Add("X-Api-Key","k")`:         `req.Header.Add("X-Api-Key","[REDACTED]")`,
		`os.environ.get("API_KEY","k")`:           `os.environ.get("API_KEY","[REDACTED]")`,
		`headers.append("Authorization","k")`:     `headers.append("Authorization","[REDACTED]")`,
		`os.Setenv("API_TOKEN", "k")`:             `os.Setenv("API_TOKEN", "[REDACTED]")`,
		`getenv("API_KEY", "k")`:                  `getenv("API_KEY", "[REDACTED]")`,
		`req.Header.Add("X-Api-Key", "Bearer k")`: `req.Header.Add("X-Api-Key", "[REDACTED]")`,
	} {
		if got := RedactSensitive(in); got != want {
			t.Errorf("result: %q -> %q, want %q", in, got, want)
		}
		if got := streamRedact(in); got != want {
			t.Errorf("stream: %q -> %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		`fields.set("token","access_token")`,
		`fields.set("api_key", "API_KEY_NAME")`,
		`client_kwargs.get("api_key", "none")`,
		`os.environ.setdefault("HERMES_REDACT_SECRETS", "true")`,
		`_first_env("CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_TOKEN")`,
		`wordSet("--access-token", "-t", "--config")`,
		`parser.add_argument("--api-key", "--api_key", dest="api_key")`,
		`_contains_any("auth", "token", "unauthorized")`,
		`send_request("managed-bedrock-api-key", "us-west-1")`,
		`x = ("api_key", "base_url", "api_mode")`,
		`for k in ("base_url", "api_key", "api_mode"):`,
		`[]string{"authorization", "api_key", "apikey", "token"}`,
	} {
		assertChainShows(t, in, in)
	}
}

// A name and its value are paired in any order and across a short gap: the
// value field before the name in a list entry, a JSON object or an element's
// attributes, and a comment, blank lines or another field between them.
func TestSeparatedAndReversedNameValuePairs(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{"- value: Xk9ab12Cd\n  name: DB_PASSWORD", "Xk9ab12Cd"},
		{"env:\n- value: \"Xk9ab12Cd\"\n  name: DB_PASSWORD\n- name: HOME\n  value: /root", "Xk9ab12Cd"},
		{"- name: DB_PASSWORD\n  # note\n  value: Xk9ab12Cd", "Xk9ab12Cd"},
		{"- name: DB_PASSWORD\n\n  value: Xk9ab12Cd", "Xk9ab12Cd"},
		{"- name: DB_PASSWORD\n  # one\n  # two\n\n  value: \"Xk9ab12Cd\"", "Xk9ab12Cd"},
		{`{"value":"Xk9ab12Cd","name":"DB_PASSWORD"}`, "Xk9ab12Cd"},
		{`{"value": "Xk9ab12Cd", "key": "API_TOKEN"}`, "Xk9ab12Cd"},
		{`<add value="Xk9ab12Cd" key="DbPassword"/>`, "Xk9ab12Cd"},
		{`<setting value='Xk9ab12Cd' name='ApiKey' />`, "Xk9ab12Cd"},
		{"name: DB_PASSWORD\nvalueFrom: x\nvalue: Xk9ab12Cd", "Xk9ab12Cd"},
	} {
		checkBothSurfaces(t, "pair", tc.in, tc.secret)
	}
	// Values of other names, and names with no value of their own, stay.
	for _, in := range []string{
		"env:\n- name: HOME\n  value: /root\n- name: PATH\n  value: /usr/bin",
		"- value: /root\n  name: HOME",
		`{"value":"/root","name":"HOME"}`,
		`<add value="x" key="Timeout"/>`,
		"- name: DB_PASSWORD\n  valueFrom:\n    secretKeyRef:\n      name: db\n      key: password\n- name: HOME\n  value: /root",
		"- name: DB_PASSWORD\n- name: HOME\n  value: /root",
	} {
		assertChainShows(t, in, in)
	}
}

// A quoted value that is a whole reference by its shape is a reference, and
// a quoted value that only starts like one is a literal.
func TestQuotedWholeReferencesAreKept(t *testing.T) {
	for _, in := range []string{
		`{"password":"${DB_PASSWORD}"}`,
		`api_key: "${OPENAI_API_KEY}"`,
		`api_key: '${OPENAI_API_KEY}'`,
		`TOKEN="$(gcloud auth print-access-token)"`,
		`password: "{{ .Values.password }}"`,
		`token: "$GITHUB_TOKEN"`,
		`api_key: "<your-key-here>"`,
		`telegram_token: "${TELEGRAM_BOT_TOKEN}"`,
		`password: "%DB_PASSWORD%"`,
		`  "token:" "${gateway_token}"`,
	} {
		assertChainShows(t, in, in)
	}
	for _, tc := range []struct{ in, secret string }{
		{`DB_PASSWORD='$3cr3tP@ss'`, "3cr3tP@ss"},
		{`{"password":"${DB_PASSWORD}x9Zq"}`, "x9Zq"},
		{`{"password":"${DB_PASSWORD} two"}`, "two"},
		{`{"password":"$Xk92mQzpR7"}`, "Xk92mQzpR7"},
		{`{"password":"<Xk92mQzpR7>"}`, "Xk92mQzpR7"},
		{`{"password":"%Xk92mQzpR7%"}`, "Xk92mQzpR7"},
		{"{\"password\":\"`Xk92mQzpR7\"}", "Xk92mQzpR7"},
		{`password: "$(cat x) Xk92mQzpR7"`, "Xk92mQzpR7"},
	} {
		assertChainHides(t, tc.in, tc.in, tc.secret)
	}
	// A credential container's strings are masked, a reference excepted.
	assertChainShows(t, "references in a container", `{"credentials":{"pass":"${DB_PASSWORD}"}}`)
	assertChainHides(t, "literals in a container", `{"credentials":{"pass":"x9Zq-r4"}}`, "x9Zq-r4")
}

// A name that ends in _pass is a credential unless its words say it counts
// something: first_pass, since_pass, selected_pass.
func TestPassSuffixNamesThatCountAreNotCredentials(t *testing.T) {
	for _, in := range []string{
		"first_pass = 0", "self._micro_compact_turns_since_pass = 0", "selected_pass='all'", "second_pass: 3", "since-pass=2",
		"retry_pass = 1", "no_pass: true",
	} {
		assertChainShows(t, in, in)
	}
	for _, tc := range []struct{ in, secret string }{
		{"SMTP_PASS=Xk9ab12Cd", "Xk9ab12Cd"},
		{"MAIL_PASS: hunter2", "hunter2"},
		{"DB_PASS=123456", "123456"},
		{"db-pass = Xk9ab12Cd", "Xk9ab12Cd"},
		{"root_pass: 0", "0"},
		{"last-pass=2", "2"},
		{"current_pass: Xk9ab12Cd", "Xk9ab12Cd"},
		{"run --db-pass hunter2", "hunter2"},
	} {
		assertChainHides(t, tc.in, tc.in, tc.secret)
	}
}

// The captured result of a command is masked per stream before the streams
// are merged: a credential split by the other stream's lines is still found,
// by name and value, as an AWS key id and its secret, in a private key's body
// and in a credential container.
func TestInterleavedStreamsAreDetectedPerStream(t *testing.T) {
	awsSecret := strings.Repeat("aB3/", 10)
	for name, tc := range map[string]struct {
		steps  []stagedWrite
		hidden []string
	}{
		"name and value": {
			[]stagedWrite{{false, "password:\n"}, {true, "warning: retrying\n"}, {false, "  Xk9ab12Cd\n"}},
			[]string{"Xk9ab12Cd"},
		},
		"AWS key and secret": {
			[]stagedWrite{{false, "AKIAABCDEFGHIJKLMNOP\n"}, {true, "warning: retrying\n"}, {false, awsSecret + "\n"}},
			[]string{awsSecret},
		},
		"private key body": {
			[]stagedWrite{{false, "-----BEGIN PRIVATE KEY-----\n"}, {false, "r4PemLineOneAbCdEfGhIjKlMnOpQrStUvWxYz0123456789\n"},
				{true, "warning: retrying\n"}, {false, "r4PemLineTwoAbCdEfGhIjKlMnOpQrStUvWxYz0123456789\n"}, {false, "-----END PRIVATE KEY-----\n"}},
			[]string{"r4PemLineOne", "r4PemLineTwo"},
		},
		"credential container": {
			[]stagedWrite{{false, "{\"api_keys\": [\n"}, {true, "warning: retrying\n"}, {false, "  \"r4ContainerSecret-7d102e\"\n]}\n"}},
			[]string{"r4ContainerSecret-7d102e"},
		},
		"stderr is the secret's stream": {
			[]stagedWrite{{true, "Authorization: Bearer\n"}, {false, "progress 1\n"}, {true, "r4BearerToken-AbCdEf0123456789\n"}},
			[]string{"r4BearerToken-AbCdEf0123456789"},
		},
	} {
		var out string
		for attempt := 0; ; attempt++ {
			runner := &stagedRunner{steps: tc.steps, pause: 40 * time.Millisecond}
			var err error
			if out, err = runCommandStreaming(context.Background(), runner, "stage", "terminal", "call"); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// The two streams' lines are interleaved in the capture.
			lines := strings.Split(out, "\n")
			interleaved := false
			for i, line := range lines {
				if (strings.HasPrefix(line, "warning: ") || strings.HasPrefix(line, "progress ")) && i > 0 && i < len(lines)-1 {
					interleaved = true
				}
			}
			if interleaved {
				break
			}
			if attempt == 2 {
				t.Fatalf("%s: the streams did not interleave: %q", name, out)
			}
		}
		for _, secret := range tc.hidden {
			// The capture goes through the result middleware next; what
			// the merge hides from it is masked in the capture already.
			if final := RedactSensitive(out); strings.Contains(final, secret) {
				t.Errorf("%s: the result shows %q:\n%s", name, secret, final)
			}
		}
	}
}
