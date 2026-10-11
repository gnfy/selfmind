package tools

import (
	"strings"
	"testing"
)

// checkBothSurfaces runs text through the result redactor and the line stream.
func checkBothSurfaces(t *testing.T, group, in string, hidden ...string) {
	t.Helper()
	for surface, out := range map[string]string{"result": RedactSensitive(in), "stream": streamRedact(in)} {
		for _, secret := range hidden {
			if strings.Contains(out, secret) {
				t.Errorf("[%s] the %s leaked %.40q:\n  in:  %.120q\n  out: %.120q", group, surface, secret, in, out)
			}
		}
	}
}

// The evidence a line gives that a bare value is code never exempts a value
// that looks like a credential: with a digit and a letter, a symbol, or dotted
// with digits. A line can be written to look like code (a trailing ;, a
// declaration word, x. before the name, := or ==, a block label with the value
// on the next line, a previous line that ends in a comma or an opening
// bracket); none of that shows its value is.
func TestEvidenceNeverExemptsAValueThatLooksLikeACredential(t *testing.T) {
	long := strings.Repeat("A1b2", 640)
	for _, tc := range []struct{ group, in, secret string }{
		{"semicolon", "password: Xk9ab12Cd;", "Xk9ab12Cd"},
		{"semicolon", "password = Xk9ab12Cd;", "Xk9ab12Cd"},
		{"semicolon", "secret: hunter2;", "hunter2"},
		{"semicolon", "X-Api-Key: Xk9ab12Cd;", "Xk9ab12Cd"},
		{"semicolon", "token = Xk9ab12Cd;", "Xk9ab12Cd"},
		{"semicolon", "db:\n  password: Xk9ab12Cd;\n", "Xk9ab12Cd"},
		{"semicolon", "[pypi]\npassword = Xk9ab12Cd;", "Xk9ab12Cd"},
		{"declaration", "const password: Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "var password = Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "let SECRET=Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "string password: Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "int token=Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "static secret = Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "public password: Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "final String api_key = Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "readonly token: hunter2", "hunter2"},
		{"declaration", "auto password: Xk9ab12Cd", "Xk9ab12Cd"},
		{"declaration", "- const password: Xk9ab12Cd", "Xk9ab12Cd"},
		{"member", "x.password = Xk9ab12Cd", "Xk9ab12Cd"},
		{"member", "db.password = Xk9ab12Cd", "Xk9ab12Cd"},
		{"member", "spring.datasource.password = Xk9ab12Cd", "Xk9ab12Cd"},
		{"member", "this.token = Xk9ab12Cd", "Xk9ab12Cd"},
		{"member", "cfg->secret = Xk9ab12Cd", "Xk9ab12Cd"},
		{"member", "a().password = Xk9ab12Cd", "Xk9ab12Cd"},
		{"member", "app.config.api_key = hunter2", "hunter2"},
		{"operator", "password := Xk9ab12Cd", "Xk9ab12Cd"},
		{"operator", "api_key := hunter2", "hunter2"},
		{"operator", "password == Xk9ab12Cd", "Xk9ab12Cd"},
		{"operator", "password === Xk9ab12Cd", "Xk9ab12Cd"},
		{"operator", "secret != Xk9ab12Cd", "Xk9ab12Cd"},
		{"operator", "secret !== Xk9ab12Cd", "Xk9ab12Cd"},
		// A single word without a digit is no more code than a credential on a
		// line that only ends its statement, compares or labels a block, or
		// follows member access.
		{"word", "password: hunter;", "hunter"},
		{"word", "password = changeme;", "changeme"},
		{"word", "if token == hunter", "hunter"},
		{"word", "password == changeme", "changeme"},
		{"word", "if password:\n    hunter", "hunter"},
		{"word", "spring.datasource.password = hunter", "hunter"},
		{"word", "db.password = secret", "secret"},
		{"word", "this.apiKey = changeme", "changeme"},
		{"label", "if password:\n    Xk9ab12Cd", "Xk9ab12Cd"},
		{"label", "while token:\n  Xk9ab12Cd", "Xk9ab12Cd"},
		{"label", "case \"password\":\n  Xk9ab12Cd", "Xk9ab12Cd"},
		{"label", "\"password\":\n  Xk9ab12Cd", "Xk9ab12Cd"},
		{"label", "else secret:\n  hunter2", "hunter2"},
		{"label", "def password:\n  Xk9ab12Cd", "Xk9ab12Cd"},
		{"label", "for token:\nXk9ab12Cd", "Xk9ab12Cd"},
		{"medium", "(password: s.rGSC2pPbVeuK31A2OTQEhaB5", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"medium", "{password: s.rGSC2pPbVeuK31A2OTQEhaB5}", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"medium", "a,\npassword: s.rGSC2pPbVeuK31A2OTQEhaB5", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"medium", "x = {\npassword: s.rGSC2pPbVeuK31A2OTQEhaB5", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"medium", "[\n  password: Xk9(2mQz!pR7\n]", "Xk9(2mQz!pR7"},
		{"medium", "foo(\n  password: Xk9(2mQz!pR7,", "Xk9(2mQz!pR7"},
		{"medium", "{\npassword: Summer2024.Winter", "Summer2024.Winter"},
		// An argument that spans the call's own punctuation is a credential.
		{"medium", "connect(\n    password=Xk9(2mQz!pR7\n)", "Xk9(2mQz!pR7"},
		{"medium", "opts = [\n  token: a1b2.c3d4.e5f6\n]", "a1b2.c3d4.e5f6"},
		{"literal", `os.Setenv("API_TOKEN", "` + long + `")`, long},
		{"literal", `var password string = "` + long + `"`, long},
		{"literal", `const API_TOKEN: &str = "` + long + `";`, long},
	} {
		checkBothSurfaces(t, tc.group, tc.in, tc.secret)
	}
}

// What the line shows to be code stays code: a call, an index or an
// identifier without a digit, wherever strong evidence reads it, and the
// restricted-argument calls and letters-only paths that medium evidence reads.
func TestEvidenceStillReadsCodeAsCode(t *testing.T) {
	for _, in := range []string{
		`token := os.Getenv("GITHUB_TOKEN")`,
		`token := tok`,
		`secret := generateSecret()`,
		`c.token = tokValue`,
		`c.token = loadToken(cfg)`,
		`this.apiKey = config.apiKey;`,
		`var password = pw`,
		`const token = await getToken();`,
		`String password = config.getPassword();`,
		`if password == confirmPassword`,
		`if token != previousToken {`,
		`case "x_token":` + "\n\tfmt.Println(1)",
		"connect(\n    host=host,\n    password=db_password\n)",
		"const options = {\n  timeout: 3,\n  apiKey: someVar,\n}",
		"foo(a,\n  password: settings.token,\n  b)",
	} {
		for surface, out := range map[string]string{"result": RedactSensitive(in), "stream": streamRedact(in)} {
			if out != in {
				t.Errorf("the %s masked code:\n  in:  %q\n  out: %q", surface, in, out)
			}
		}
	}
}

// What stays visible on purpose, so that a change of the code rule shows up:
// a variable-shaped value (letters-only camelCase or snake_case) in a
// bracketed block, and a bare reference by its shape.
func TestDocumentedResidualOfTheCodeRule(t *testing.T) {
	for _, in := range []string{
		"{\n  password: correct_horse_battery\n}",
		"{\n  password: correctHorseBattery\n}",
		`{"password": someVar}`,
		`connect(host, password=db_password)`,
		`token = hunter.two`,
		`password = os.environ["X"]`,
		// A declaration word or := reads an identifier without a digit as code.
		`var password = hunter`, `let password = hunter`, `const token = tok`, `token := tok`, `password := changeme`,
		// A weak name's setting word, prose and a colon with no gap.
		`credentials: default`, `credential:read`, `credentials: try the next one`, `credentials: mysecretvalue and more`,
		// A literal that reads as a name, in a call that sets, and a literal of
		// letters alone in a call that neither sets nor reads.
		`fields.set("token", "access_token")`, `vault.read("API_KEY", "mysecretvalue")`,
	} {
		for surface, out := range map[string]string{"result": RedactSensitive(in), "stream": streamRedact(in)} {
			if out != in {
				t.Errorf("the %s masked a documented residual (update docs/tool-safety.md if this is a fix):\n  in:  %q\n  out: %q", surface, in, out)
			}
		}
	}
	for _, in := range []string{
		"password: $hunter", "password: ${Xk9ab12Cd}", "password: $(cat /run/secrets/Xk9ab12Cd)",
		"password: <hunter>", "password: %HUNTER%", "password: $HUNTER", "password: {hunter}", "password: {{ Xk9ab12Cd }}", "password: yes",
	} {
		assertChainShows(t, in, in)
	}
}
