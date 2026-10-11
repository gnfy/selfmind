package tools

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// streamRedact is text as the live stream shows it: line by line through the
// stream redactor, with whatever it held released at the end.
func streamRedact(text string) string {
	stream := newLineStreamRedactor()
	var shown []string
	for _, line := range strings.Split(text, "\n") {
		shown = append(shown, stream.push(line)...)
	}
	return strings.Join(append(shown, stream.flush()...), "\n")
}

// visibleAfter is what a redaction left of a template's slot: the template's
// own text is taken out, so a value that is a part of it (tok in __token__)
// is not mistaken for a leak.
func visibleAfter(out, template string) string {
	for _, part := range strings.Split(template, "%s") {
		out = strings.Replace(out, part, "", 1)
	}
	return out
}

// Assemble token-shaped synthetic fixtures so source scanners do not treat them as credentials.
var configMatrixValues = []string{
	"correctHorseBattery", "correct_horse_battery", "CORRECT_HORSE_BATTERY", "Correct_Horse_Battery",
	"hunter", "hunter2", "abAbc", "ab_cd", "p4ss.w0rd", "Xk9(2mQz!pR7", "Xk9(2mQz", "Xk92mQz!pR7(",
	"s.rGSC2pPbVeuK31A2OTQEhaB5", strings.Join([]string{"MTk4NjIyNDgzNDcxOTI1MjQ4", "Cl2FMQ", "ZnCjm1XVW7vRze4b7Cq4se7kKWs"}, "."), "abcdef.abcdef0123456789",
	"tok", "pw", "Hunter2", "changeme", "admin",
}

var configMatrixTemplates = []string{
	"PASSWORD=%s",
	"DB_PASSWORD=%s",
	"export API_KEY=%s",
	"export db_password=%s",
	"DB_PASSWORD=%s ./run",
	"docker run -e API_TOKEN=%s img",
	"env TOKEN=%s ./run",
	"password: %s",
	"adminPassword: %s",
	"db:\n  secret: %s",
	"environment:\n  - DB_PASSWORD=%s",
	"environment:\n  DB_PASSWORD: %s",
	"secret = %s",
	"[pypi]\nusername = __token__\npassword = %s",
	"password = %s",
	"X-Api-Key: %s",
	"Authorization: %s",
	"--password=%s",
	"login --api-key=%s",
	"GET /cb?access_token=%s",
	"https://x.test/cb?token=%s&x=1",
	"db.password=%s",
	"spring.datasource.password: %s",
	"$env:API_KEY = %s",
	"API_TOKEN := %s",
	"auth: {password: %s, user: bob}",
	"(PASSWORD=%s ./run)",
	"$(cd x && DB_PASSWORD=%s ./run)",
	"{ export PASSWORD=%s; ./run; }",
	"local password=%s",
	"set -e; PGPASSWORD=%s psql -h db",
}

var probeMatrixValues = []struct{ name, value string }{
	{"camel", "correctHorseBattery"}, {"snake", "correct_horse_battery"}, {"CAPS_SNAKE", "CORRECT_HORSE_BATTERY"},
	{"Mixed_Snake", "Correct_Horse_Battery"}, {"word", "hunter"}, {"longword", "mysecretvalue"}, {"alpha12", "abcdefghijkl"},
	{"Pascal", "CorrectHorseBattery"}, {"kebab", "correct-horse-battery"}, {"dotted3", "correct.horse.battery"},
	{"dotted2", "hunter.two"}, {"digit-dotted", "p4ss.w0rd"}, {"digit", "hunter2"}, {"upperword", "HUNTER"},
	{"2camel", "myPass"}, {"shortcamel", "aBcd"},
}

var probeMatrixContexts = []struct{ name, template string }{
	{"dotenv PASSWORD=", "PASSWORD=%s"},
	{"dotenv DB_PASSWORD=", "DB_PASSWORD=%s"},
	{"export API_KEY=", "export API_KEY=%s"},
	{"export SECRET=", "export SECRET=%s"},
	{"yaml password:", "password: %s"},
	{"yaml nested secret:", "db:\n  secret: %s"},
	{"ini secret =", "secret = %s"},
	{"toml password =", "password = %s"},
	{"flag --password sp", "mysql --password %s -h db"},
	{"flag --password=", "login --password=%s"},
	{"flag --token sp", "gh auth login --token %s"},
	{"http X-Api-Key:", "X-Api-Key: %s"},
	{"http Authorization:", "Authorization: %s"},
	{"json bare", `{"password": %s}`},
	{"json quoted", `{"password": "%s"}`},
	{"quoted dotenv", `API_KEY="%s"`},
	{"xml attr", `<add key="secret" value="%s"/>`},
	{"url query", `https://x.test/cb?access_token=%s&x=1`},
	{"url query token=", `https://x.test/cb?token=%s&x=1`},
	{"cookie", `Cookie: session=%s`},
	{"docker env", `docker run -e DB_PASSWORD=%s img`},
	{"env cmd", `env TOKEN=%s ./run`},
	{"python kw", `connect(host, password=%s)`},
	{"curl -H", `curl -H "X-Api-Key: %s" https://x.test`},
	{"properties", `db.password=%s`},
	{"spring yaml", `spring.datasource.password: %s`},
	{"helm values", "auth:\n  password: %s\n"},
	{"ssh config", `SetEnv TOKEN=%s`},
	{"powershell", `$env:API_KEY = %s`},
	{"makefile", `API_TOKEN := %s`},
	{"terraform", `  token = %s`},
	{"compose env list", "environment:\n  - DB_PASSWORD=%s"},
	{"compose map", "environment:\n  DB_PASSWORD: %s"},
}

// The config matrix: a value after a credential's name in a config line is a
// literal, whatever it looks like, in every config syntax, in the result and
// in the stream. Inside an unclosed ( or { on the line a variable-shaped value
// reads as an argument or a member of an object literal (auth: {password: x});
// those 6 cells are the documented residual.
func TestConfigMatrixMasksLiterals(t *testing.T) { runConfigMatrix(t, directSurfaces) }

// The same matrix through the terminal tool, which prints each cell with cat:
// the result middleware and the live stream of the production path.
func TestConfigMatrixThroughTheTerminalTool(t *testing.T) { runConfigMatrix(t, terminalSurfaces) }

// surfacesOf shows what the tool result and the live stream make of an output.
type surfacesOf func(t *testing.T, output string) (result, stream string)

func directSurfaces(t *testing.T, output string) (string, string) {
	return RedactSensitive(output), streamRedact(output)
}

func terminalSurfaces(t *testing.T, output string) (string, string) {
	result, streamed := chainSurfaces(t, output)
	return result.Output, strings.Join(streamed, "\n")
}

func runConfigMatrix(t *testing.T, surfaces surfacesOf) {
	var leaks, residual []string
	for _, template := range configMatrixTemplates {
		for _, value := range configMatrixValues {
			input := fmt.Sprintf(template, value)
			result, stream := surfaces(t, input)
			for surface, out := range map[string]string{"result": result, "stream": stream} {
				if !strings.Contains(visibleAfter(out, template), value) {
					continue
				}
				cell := fmt.Sprintf("%s %q", surface, input)
				bracketed := strings.ContainsAny(template, "({") && (variableName.MatchString(value) || strings.ContainsAny(value, "(."))
				if bracketed {
					residual = append(residual, cell)
				} else {
					leaks = append(leaks, cell)
				}
			}
		}
	}
	if len(leaks) > 0 {
		t.Errorf("%d of %d config cells leak (first: %s)", len(leaks), len(configMatrixTemplates)*len(configMatrixValues)*2, strings.Join(leaks[:min(40, len(leaks))], "\n  "))
	}
	// Each residual cell is counted on the result and on the stream.
	if want := 6 * 2; len(residual) != want {
		t.Errorf("%d documented residual config cells, want %d: the residual changed, update docs/tool-safety.md with it", len(residual), want)
	}
	t.Logf("config matrix: %d cells x 2 surfaces, %d leaks, %d documented residual\n  %s", len(configMatrixTemplates)*len(configMatrixValues), len(leaks), len(residual), strings.Join(residual, "\n  "))
}

// The probe matrix: letters-only and code-looking values after a credential
// name in 33 syntaxes. Bare JSON values and call keyword arguments are code
// by design: an identifier there names a variable.
func TestProbeMatrixMasksLiterals(t *testing.T) { runProbeMatrix(t, directSurfaces) }

// The same matrix through the terminal tool.
func TestProbeMatrixThroughTheTerminalTool(t *testing.T) { runProbeMatrix(t, terminalSurfaces) }

func runProbeMatrix(t *testing.T, surfaces surfacesOf) {
	// A bare value that is a variable's or a member's name inside a call's
	// arguments or an object literal is code there; a letters-only dotted path
	// after a spaced = or := is one in any assignment. Those cells are the
	// documented residual.
	dottedWords := regexp.MustCompile(`^[A-Za-z_]+(?:\.[A-Za-z_]+)+$`)
	var leaks, residual []string
	cells := 0
	for _, context := range probeMatrixContexts {
		for _, value := range probeMatrixValues {
			cells++
			input := fmt.Sprintf(context.template, value.value)
			result, stream := surfaces(t, input)
			for surface, out := range map[string]string{"result": result, "stream": stream} {
				if !strings.Contains(visibleAfter(out, context.template), value.value) {
					continue
				}
				cell := fmt.Sprintf("%s %s/%s %q", surface, context.name, value.name, input)
				inCode := (context.name == "json bare" || context.name == "python kw") &&
					(variableName.MatchString(value.value) || dottedWords.MatchString(value.value))
				spacedPath := strings.Contains(context.template, " = ") || strings.Contains(context.template, " := ")
				if inCode || (spacedPath && dottedWords.MatchString(value.value)) {
					residual = append(residual, cell)
				} else {
					leaks = append(leaks, cell)
				}
			}
		}
	}
	if len(leaks) > 0 {
		t.Errorf("%d of %d probe cells leak (first: %s)", len(leaks), cells*2, strings.Join(leaks[:min(40, len(leaks))], "\n  "))
	}
	// 10 spaced-assignment cells, 7 bare JSON members and 7 keyword arguments;
	// each is counted on the result and on the stream.
	if want := 24 * 2; len(residual) != want {
		t.Errorf("%d documented residual probe cells, want %d: the residual changed, update docs/tool-safety.md with it", len(residual), want)
	}
	t.Logf("probe matrix: %d cells x 2 surfaces, %d leaks, %d documented residual\n  %s", cells, len(leaks), len(residual), strings.Join(residual, "\n  "))
}

// A quoted value is masked: a quote is positive evidence of a literal,
// whatever the literal starts with, in every syntax and with spaces inside; a
// reference is only one when it is the whole of the quoted value.
func TestQuotedValuesAreLiteralsUnlessAWholeReference(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{`DB_PASSWORD='$3cr3tP@ss'`, "3cr3tP@ss"},
		{`DB_PASSWORD="$3cr3tP@ss"`, "3cr3tP@ss"},
		{`{"password":"$3cr3tP@ssw0rd"}`, "3cr3tP@ssw0rd"},
		{`{"password":"<Xk92mQzpR7>"}`, "Xk92mQzpR7"},
		{`{"password":"%Xk92mQzpR7%"}`, "Xk92mQzpR7"},
		{"{\"password\":\"`Xk92mQzpR7\"}", "Xk92mQzpR7"},
		{`password: "$3cr3tP@ssw0rd"`, "3cr3tP@ssw0rd"},
		{`password: '$abc12'`, "$abc12"},
		{`password = "$3cr3tP@ss"`, "3cr3tP@ss"},
		{`password: "correct horse battery staple"`, "horse battery"},
		{`DB_PASSWORD='two words here'`, "words here"},
		{`{"note":"x","password":"correct horse battery"}`, "horse battery"},
		{`login --password "correct horse"`, "horse"},
	} {
		assertChainHides(t, tc.in, tc.in, tc.secret)
	}
	// What stays visible is a reference by shape, unquoted.
	for _, in := range []string{
		"DB_PASSWORD=$DB_PASSWORD", "export TOKEN=${TOKEN}", "PASSWORD=$(cat /run/secrets/db)", "password=%DB_PASS%",
		"token: <your-token-here>", "TOKEN=`cat token.txt`", "GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
		"password: {{ .Values.password }}", "curl -u $USER:$PASS https://example.test",
		`{"password":"${DB_PASSWORD}"}`, `password: "{{ .Values.password }}"`, `api_key: "<your-key-here>"`,
	} {
		assertChainShows(t, in, in)
	}
	// A leading $ that is not an environment variable's whole name is a literal.
	for _, tc := range []struct{ in, secret string }{
		{"DB_PASSWORD=$Xk92mQzpR7", "Xk92mQzpR7"},
		{"DB_PASSWORD=$3cr3tP@ss", "3cr3tP@ss"},
		{"PASSWORD=%Xk92mQzpR7%", "Xk92mQzpR7"},
		{"PASSWORD=<Xk92mQzpR7>", "Xk92mQzpR7"},
		{"PASSWORD={abc12345", "abc12345"},
		{"PASSWORD=[abc12345", "abc12345"},
		{"DB_PASSWORD=Winter2024:", "Winter2024"},
		{"PASSWORD=ab:", "ab"},
		{"REDIS_PASSWORD=redis_password", "redis_password"},
		{"PASSWORD=password", "=password"},
	} {
		assertChainHides(t, tc.in, tc.in, tc.secret)
	}
}

// Evidence that an assignment is code reads a bare value as code; none reads
// it as a literal. Every case is checked in the result and in the stream.
func TestAssignmentEvidenceDecidesBareValues(t *testing.T) {
	code := []string{
		// strong evidence: := on a name that is not ALL_CAPS, a declaration, ==, ;
		`token := os.Getenv("GITHUB_TOKEN")`,
		`token := tok`,
		`c.token = tokValue`,
		`cfg.Password = passwordValue`,
		`this.apiKey = config.apiKey;`,
		`var password = pw`,
		`let token = null;`,
		`const token = await getToken();`,
		`String password = config.getPassword();`,
		`export const API_KEY = process.env.API_KEY;`,
		`if token == previousToken {`,
		`password == confirmPassword`,
		`if (token == null) { return; }`,
		`x := token;`,
		`case "x_token":` + "\n\t\tfmt.Fprintf(w, \"x\")",
		"if not refresh_token:\n    return None",
		// medium evidence: inside brackets, after a line that opens one, trailing , ) }
		`const client = new Client({ apiKey: someVar, timeout: 3 })`,
		"const client = new Client({\n  timeout: 3,\n  apiKey: someVar\n});",
		"client = Client(\n    host=host,\n    password=db_password\n)",
		`connect(host, password=db_password)`,
		`apiKey: someVar,`,
		`	Password: password,`,
		`	Token: token,`,
		"module.exports = {\n  password: dbPassword\n};",
		// no evidence, but a call, an index or a path on a spaced =
		`password = os.environ["DB_PASSWORD"]`,
		`API_KEY = os.environ["API_KEY"]`,
		`password = input("Password: ")`,
		`password = args.password`,
		`  password = var.db_password`,
		`token = settings.token`,
		`secret := generateSecret()`,
		// types and templates
		`password: string;`,
		`token?: string;`,
		`  apiKey: String`,
		`password: {{ .Values.password }}`,
		`token: <your-token-here>`,
		`PASSWORD=$(cat /run/secrets/db)`,
		`export TOKEN=${TOKEN}`,
		// help output, YAML structure and booleans
		"      --token string        API token used to authenticate",
		"  -p, --password string     Password for the registry",
		"      --token='': Bearer token for authentication to the API server",
		"serviceAccountName: default\nautomountServiceAccountToken: false",
		"automountServiceAccountToken: true",
		"credentials: include",
		"type Credentials struct {\n\tUsername string\n\tPassword string\n}",
		"secretKeyRef:\n  name: db-secret\n  key: password",
		"volumes:\n- name: tls\n  secret:\n    secretName: app-tls\n",
		"password:\nusername: bob",
		// Lists and tuples of names, calls that read, type annotations,
		// interpolations and references with a path.
		`_SENSITIVE_KEYS = {"api_key", "authorization", "proxy_authorization"}`,
		`for k in ("base_url", "api_key", "api_mode"):`,
		`preferred = ("agent_key", "access_token", "refresh_token")`,
		`[]string{"authorization", "api_key", "apikey", "token"}`,
		`settings: omit(options, ["apiKey", "authToken", "baseURL"]),`,
		`token = _first_env("CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_TOKEN")`,
		`client_kwargs.get("api_key", "none")`,
		`parser.add_argument("--api-key", "--api_key", dest="api_key")`,
		`"doctl": wordSet("--access-token", "-t", "--config")`,
		`os.environ.setdefault("HERMES_REDACT_SECRETS", "true")`,
		"    refresh_token: Optional[str] = None",
		"    secret: Optional[str]",
		"  Token: Option<String>,",
		`"refresh_token": {refreshToken},`,
		`format!("failed to save api key: {err}")`,
		`f"--remote_header=x-api-key={api_key}"`,
		`echo "APPLE_API_KEY=$RUNNER_TEMP/apple-api-key.p8" >> "$GITHUB_ENV"`,
	}
	for _, in := range code {
		want := strings.TrimRight(in, "\n")
		if got := RedactSensitive(want); got != want {
			t.Errorf("result masked code:\n  in: %q\n out: %q", want, got)
		}
		if got := streamRedact(want); got != want {
			t.Errorf("stream masked code:\n  in: %q\n out: %q", want, got)
		}
	}
	// The same words with no evidence of code are literals.
	for _, tc := range []struct{ in, secret string }{
		// A single word after member access, a comparison, a trailing ; or a
		// block label is read as the literal it can be.
		{"c.token = tok", "= tok"},
		{"cfg.Password = pw", "= pw"},
		{"if token == tok {", "== tok"},
		{"spring.datasource.password = hunter", "= hunter"},
		{"db.password = secret", "= secret"},
		{"PASSWORD=ab_cd", "ab_cd"},
		{"password: correct_horse_battery", "correct_horse_battery"},
		{"password = correct_horse_battery", "correct_horse_battery"},
		{"X-Api-Key: someVar", "someVar"},
		{"adminPassword: someVar", "someVar"},
		{"apiKey: someVar", "someVar"},
		{"api_key = api_key", "= api_key"},
		{"PASSWORD=a.b", "a.b"},
		{"password: a.b", "a.b"},
		{"DB_PASSWORD=Xk9(2mQz!pR7", "Xk9(2mQz!pR7"},
		{"PASSWORD=abc[", "abc["},
		{"password: abc[", "abc["},
		{"PASSWORD=os.getenv(x)", "os.getenv(x)"},
		{"API_TOKEN := someVar", "someVar"},
		{"VAULT_TOKEN=s.rGSC2pPbVeuK31A2OTQEhaB5", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"bootstrapTokens:\n- token: abcdef.abcdef0123456789", "abcdef0123456789"},
		{strings.Join([]string{"DISCORD_TOKEN=MTk4NjIyNDgzNDcxOTI1MjQ4", "Cl2FMQ", "ZnCjm1XVW7vRze4b7Cq4se7kKWs"}, "."), "ZnCjm1XVW7vRze4b7Cq4se7kKWs"},
	} {
		assertChainHides(t, tc.in, tc.in, tc.secret)
	}
}

// A value that is a literal but is separated from its name by a long run of
// whitespace is still seen.
func TestLongWhitespaceBetweenNameAndValue(t *testing.T) {
	// 511 and 512 are the edges of the window a value is looked for in; the
	// stream reads blank lines up to 64 KB between a name and its value, and
	// the result any number.
	for _, gap := range []int{1, 40, 90, 94, 95, 96, 97, 98, 300, 509, 510, 511, 512, 513, 600, 2048, 5000} {
		for _, in := range []string{
			"password:" + strings.Repeat(" ", gap) + "hunter2xyz",
			"password:\n" + strings.Repeat(" ", gap) + "hunter2xyz",
			"password" + strings.Repeat(" ", gap) + "= hunter2xyz",
			"API_KEY: " + strings.Repeat("\t", gap) + "hunter2xyz",
			"password =" + strings.Repeat("\n", min(gap, 2048)) + "hunter2xyz",
			"password:" + strings.Repeat(" \n", min(gap, 2048)/2) + "hunter2xyz",
		} {
			if got := RedactSensitive(in); strings.Contains(got, "hunter2xyz") {
				t.Errorf("result leaked after %d blanks: %q", gap, got[:min(60, len(got))])
			}
			if got := streamRedact(in); strings.Contains(got, "hunter2xyz") {
				t.Errorf("stream leaked after %d blanks: %q", gap, got[:min(60, len(got))])
			}
		}
	}
}
