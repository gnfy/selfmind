package tools

import (
	"strings"
	"testing"
)

const r5Secret = "Xk9mQz2pLw4r"

// A reference is a value that points at a credential, and a quoted one is a
// literal unless nothing but the reference is in it. An expansion can carry the
// literal itself: a default (${A:-x}), a command that prints it ($(echo x)), a
// template that quotes it ({{ 'x' }}); the literal is masked, the reference is
// not.
func TestReferencesThatCarryALiteralAreMasked(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{`password: "${A:-` + r5Secret + `}"`, r5Secret},
		{`password: "${A:+` + r5Secret + `}"`, r5Secret},
		{`password: "${A:=` + r5Secret + `}"`, r5Secret},
		{`password: "${A=` + r5Secret + `}"`, r5Secret},
		{`password: "${A-` + r5Secret + `}"`, r5Secret},
		{`password: "$(echo hunter2)"`, "hunter2"},
		{`password: "$(echo -n hunter2)"`, "hunter2"},
		{`password: "$(printf %s ` + r5Secret + `)"`, r5Secret},
		{`password: "$(printf '%s' hunter2)"`, "hunter2"},
		{`password: "{{ 'hunter2' }}"`, "hunter2"},
		{`password: "{{ .Values.x | default 'hunter2' }}"`, "hunter2"},
		{"password: \"`echo hunter2`\"", "hunter2"},
		{"password: \"`printf %s hunter2`\"", "hunter2"},
		{`{"password": "${B64:-` + r5Secret + `}"}`, r5Secret},
		{`export PASSWORD="$(printf %s ` + r5Secret + `)"`, r5Secret},
		// An unquoted one says nothing more than the quoted one.
		{`password: ${A:-` + r5Secret + `}`, r5Secret},
		{`PASSWORD=${X:-` + r5Secret + `}`, r5Secret},
		{`PASSWORD=$(printf %s ` + r5Secret + `)`, r5Secret},
		{`PASSWORD=$(echo hunter2)`, "hunter2"},
		{`PASSWORD=${X:-changeme}`, "changeme"},
		// What the quotes hold beside the reference is a literal.
		{`password: "${X}x9"`, "x9"},
		{`password: "$3cr3tP@ss"`, "3cr3tP@ss"},
		{`password: "$pass9w0rd"`, "pass9w0rd"},
		{`password: "${A}${B}` + r5Secret + `"`, r5Secret},
		{`password: "$(a)b9"`, "b9"},
		{`password: "${A}${B}"`, "${A}${B}"},
		{`password: "$A$B"`, "$A$B"},
	} {
		checkBothSurfaces(t, "reference", tc.in, tc.secret)
	}
	for _, in := range []string{
		`password: "${DB_PASSWORD}"`, `password: "${DB_PASSWORD:-}"`, `password: "${A:-$B}"`, `password: "$DB_PASSWORD"`,
		`password: "$(cat /run/secrets/db)"`, `password: "$(echo $DB_PASSWORD)"`, `password: "$(echo $DB_PASSWORD | base64)"`,
		`password: "$(pass show db)"`, `password: "{{ .Values.password }}"`, `password: "{{ index .Values 'password' }}"`,
		`password: "{{ env 'DB_PASSWORD' }}"`,
		"password: \"`cat /run/secrets/db`\"", "password: \"`echo $DB_PASSWORD`\"",
		`PASSWORD=${DB_PASSWORD}`, `PASSWORD=$(cat /run/secrets/db)`, `PASSWORD=$(echo $X)`, `PASSWORD=${X:-$Y}`, `PASSWORD=${X:?unset}`,
	} {
		assertChainShows(t, in, in)
	}
}

// A weak name's word is a word: letters, joined by _ . : / or -. A digit
// anywhere makes it a credential, after a joiner and past a letter of another
// script too, in a sentence and after a bare colon.
func TestWeakNameWordsHaveNoDigit(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{"credentials: ab_" + r5Secret + " more words here", r5Secret},
		{"credentials:hunter/" + r5Secret, r5Secret},
		{"credentials:Passwörd123", "Passwörd123"},
		{"credentials: Passwörd123 and more", "Passwörd123"},
		{"credentials: abcdeféXk9mQz2pLw4r more", "Xk9mQz2pLw4r"},
		{"credentials: hunter_2024 and more", "hunter_2024"},
		{"credentials:Summer-2024", "Summer-2024"},
		{"credentials: us-west-2 and more", "us-west-2"},
		{"credentials: " + r5Secret + " more words", r5Secret},
		{"credentials:" + r5Secret, r5Secret},
	} {
		checkBothSurfaces(t, "weak", tc.in, tc.secret)
	}
	for _, in := range []string{"credentials: try the next one", "credentials: mysecretvalue and more", "credential:read", "credentials: default", "credentials: Summer-Winter and more"} {
		assertChainShows(t, in, in)
	}
}

// A name that counts or selects a pass (first_pass, since_pass) is no
// credential, and only when its value is a count, a boolean or a word. A
// qualifier a password is also given (current, last, main, no, ...) names a
// password, and a value that is not a word under a counter's name is masked.
func TestPassNamesAreCountersOnlyForCounters(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{"current_pass: " + r5Secret, r5Secret},
		{"CURRENT_PASS=hunter2", "hunter2"},
		{"last_pass = hunter2", "hunter2"},
		{"LAST_PASS=" + r5Secret, r5Secret},
		{"prev_pass: " + r5Secret, r5Secret},
		{"next_pass: " + r5Secret, r5Secret},
		{"main_pass=" + r5Secret, r5Secret},
		{"other_pass=" + r5Secret, r5Secret},
		{"one_pass=" + r5Secret, r5Secret},
		{"full_pass: " + r5Secret, r5Secret},
		{"no_pass: " + r5Secret, r5Secret},
		{"final_pass: " + r5Secret, r5Secret},
		{"this_pass: " + r5Secret, r5Secret},
		{"initial_pass: " + r5Secret, r5Secret},
		{"old_pass=hunter2", "hunter2"},
		{"new_pass=hunter2", "hunter2"},
		{"--current-pass hunter2", "hunter2"},
		{"mysql --extra-pass=" + r5Secret, r5Secret},
		{"last-pass=2", "2"},
		{"current_pass = 123456", "123456"},
		// A counter's name over a value that is none is not exempt.
		{"first_pass: " + r5Secret, r5Secret},
		{"selected_pass='" + r5Secret + "'", r5Secret},
		{"max_pass = \"" + r5Secret + "\"", r5Secret},
		{"--first-pass " + r5Secret, r5Secret},
	} {
		checkBothSurfaces(t, "pass", tc.in, tc.secret)
	}
	for _, in := range []string{
		"first_pass = 0", "first_pass: 3", "self._micro_compact_turns_since_pass = 0", "selected_pass='all'", "selected_pass='open-fallback'",
		"second_pass: 3", "retry_pass = 1", "max_pass=10", "retry_pass: true", "total_pass: null", "dry_pass = False", "--first-pass 3",
		"scan_pass: fast", "warm_pass=2;",
	} {
		assertChainShows(t, in, in)
	}
}

// Strong evidence (a declaration, := on a name that is not ALL_CAPS) reads
// an identifier as code, and only one that is made like a variable: no digit
// and no symbol, and camelCase or snake_case, a dotted path of letters, a ::
// path, a statement keyword or a short word. A symbol or a long single word
// is a credential on that line too.
func TestStrongEvidenceReadsOnlyIdentifiersMadeLikeCode(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{"var password = Summer!", "Summer!"},
		{"var password = $Summer", "$Summer"},
		{"var password = Sum(mer", "Sum(mer"},
		{"var password = Summer-Winter", "Summer-Winter"},
		{"var password = Summer@home", "Summer@home"},
		{"var password = correcthorsebatterystaple", "correcthorsebatterystaple"},
		{"const secret = mysecretvalue", "mysecretvalue"},
		{"token := correcthorse", "correcthorse"},
		{"string password = abcdefghijkl", "abcdefghijkl"},
		{"const password: hunterhunter", "hunterhunter"},
		{"var password = hunter_2", "hunter_2"},
		{"var password = " + r5Secret, r5Secret},
		{"final String token = Summer2024", "Summer2024"},
		{"var password = abc[0]!", "abc[0]!"},
		// Environment assignments in a group or a subshell are literals.
		{"(DB_PASSWORD=correct_horse_battery ./run)", "correct_horse_battery"},
		{"$(DB_PASSWORD=correct_horse_battery ./run)", "correct_horse_battery"},
		{"[ -f x ] && (cd d && PGPASSWORD=correct_horse psql)", "correct_horse"},
		{"{ export PASSWORD=correct_horse; }", "correct_horse"},
	} {
		checkBothSurfaces(t, "strong", tc.in, tc.secret)
	}
	for _, in := range []string{
		"var password = hunter", "let token = tok", "token := tok", "var password = pw", "const token = getToken()", "var token = someVar",
		"var token = loadToken", "var password = a.b.c", "var token = AuthMode::ApiKey", "var token = opts?.token", "var secret = SECRET_KEY",
		"const api_key = apiKey", "let password = config.password", "String password = null", "let token = *tok",
	} {
		assertChainShows(t, in, in)
	}
}

// A bare value is a call, and code, when it is closed or goes on: f(a, g(b)),
// f(, f(a, "x"). One that stops, with a digit of a number or a symbol, is a
// word with a parenthesis in it.
func TestCallShapeNeedsAClosedOrContinuedCall(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{"password = Hunter(2024", "Hunter(2024"},
		{"password = Hunter(2024)", "Hunter(2024)"},
		{"password = Summer(2024)x", "Summer(2024)x"},
		{"password: abc(def;", "abc(def"},
		{"password: abc(def", "abc(def"},
		{"var password = Sum(mer", "Sum(mer"},
		{"var password = Summer(2024)", "Summer(2024)"},
		{"password = Xk9(2mQz", "Xk9(2mQz"},
		{"x.password = Sum(mer", "Sum(mer"},
		{"x.password = abc(def;", "abc(def"},
	} {
		checkBothSurfaces(t, "call shape", tc.in, tc.secret)
	}
	for _, in := range []string{
		"password = os.getenv(", "password = load(a, b)", "token = f(a, g(b))", "token = f(a, \"x\")", "token = getToken()",
		"password = os.environ.get(\"X\")", "token = str(getattr(state.agent, \"api_key\", \"\") or \"\")", "token = Println(1)",
		"case \"x_token\":\n\tfmt.Println(1)", "case \"x_token\":\n\t\tfmt.Fprintf(w, \"x\")",
		"var password = abc(def)", "password = abc(def)",
	} {
		assertChainShows(t, in, in)
	}
}

// A name and its value field are a pair with the entry's other fields between
// them, in JSON (compact and pretty-printed, as `az webapp config appsettings
// list` prints it), in an element's attributes and in YAML, with the value
// before or after the name, in the result and in the stream.
func TestPairsWithOtherFieldsBetweenAreMasked(t *testing.T) {
	for _, in := range []string{
		`{"name": "DB_PASSWORD", "slotSetting": false, "value": "` + r5Secret + `"}`,
		`{"name":"DB_PASSWORD","other":1,"value":"` + r5Secret + `"}`,
		`{"name":"DB_PASSWORD","a":"x","b":true,"value":"` + r5Secret + `"}`,
		`{"value":"` + r5Secret + `","other":1,"name":"DB_PASSWORD"}`,
		`[{"name": "DB_PASSWORD", "slotSetting": false, "value": "` + r5Secret + `"}, {"name": "B", "value": "z"}]`,
		"[\n  {\n    \"name\": \"DB_PASSWORD\",\n    \"slotSetting\": false,\n    \"value\": \"" + r5Secret + "\"\n  }\n]",
		"[\n  {\n    \"name\": \"DB_PASSWORD\",\n    \"value\": \"" + r5Secret + "\"\n  }\n]",
		"{\n  \"value\": \"" + r5Secret + "\",\n  \"name\": \"DB_PASSWORD\"\n}",
		"{\n  \"value\": \"" + r5Secret + "\",\n  \"other\": 1,\n  \"name\": \"DB_PASSWORD\"\n}",
		"{\n  \"environment\": [\n    {\n      \"value\": \"" + r5Secret + "\",\n      \"name\": \"DB_PASSWORD\"\n    }\n  ]\n}",
		`<add key="DbPassword" other="y" value="` + r5Secret + `"/>`,
		`<add value="` + r5Secret + `" other="y" key="DbPassword"/>`,
		"<add key=\"DbPassword\"\n     other=\"y\"\n     value=\"" + r5Secret + "\"/>",
		"- name: DB_PASSWORD\n  valueFrom: null\n  value: " + r5Secret,
		"- value: " + r5Secret + "\n  other: 1\n  name: DB_PASSWORD",
		"- value: " + r5Secret + "\n  # note\n  name: DB_PASSWORD",
		"- value: " + r5Secret + "\n\n  name: DB_PASSWORD",
	} {
		checkBothSurfaces(t, "pair", in, r5Secret)
	}
	for _, in := range []string{
		`{"name": "DB_HOST", "slotSetting": false, "value": "db.example.com"}`,
		`<add key="Timeout" other="y" value="30"/>`,
		"- name: DB_HOST\n  other: 1\n  value: db.example.com",
		"{\n  \"value\": \"db.example.com\",\n  \"other\": 1,\n  \"name\": \"DB_HOST\"\n}",
	} {
		assertChainShows(t, in, in)
	}
}

// A YAML block scalar under a credential's name (password: |, key: >-) is the
// credential: every line indented deeper than the key is masked, in the result
// and as each line arrives in a stream, and the line that is not ends it. A
// tag before a value (!!str) is not the value.
func TestYAMLBlockScalarsAndTagsAreMasked(t *testing.T) {
	long := strings.Repeat("line-"+r5Secret+"\n  ", 50) + "last-" + r5Secret
	for _, tc := range []struct{ in, secret string }{
		{"password: |\n  " + r5Secret + "\n  line2-" + r5Secret, r5Secret},
		{"password: >\n  " + r5Secret, r5Secret},
		{"password: |-\n  " + r5Secret, r5Secret},
		{"password: >-\n  " + r5Secret + "\nother: x", r5Secret},
		{"password: |2\n    " + r5Secret, r5Secret},
		{"password: |  # note\n  " + r5Secret, r5Secret},
		{"spring:\n  datasource:\n    password: |\n      " + r5Secret + "\n      line2-" + r5Secret + "\n    url: x", r5Secret},
		{"- password: |\n    " + r5Secret + "\n  other: x", r5Secret},
		{"api_key: |\n\n  " + r5Secret, r5Secret},
		{"client_secret: >\n  " + long, r5Secret},
		{"password: !!str " + r5Secret, r5Secret},
		{"password: !!str " + r5Secret + "\nnext: 1", r5Secret},
		{"password: !secret " + r5Secret, r5Secret},
		{"- name: DB_PASSWORD\n  value: !!str " + r5Secret, r5Secret},
		{"password: |\n  # nothing here is read\nnext: 1", "nothing here is read"},
	} {
		checkBothSurfaces(t, "block scalar", tc.in, tc.secret)
	}
	for _, in := range []string{
		"description: |\n  " + r5Secret, "password: |\nother: x", "password: |", "password: |\n",
		"password: !!str ${DB_PASSWORD}", "password: !!str null", "description: !!str " + r5Secret,
	} {
		if strings.Contains(in, r5Secret) {
			continue
		}
		assertChainShows(t, in, strings.TrimRight(in, "\n"))
	}
	// The line after the block, and a sibling key, stay visible.
	if out := RedactSensitive("password: |\n  " + r5Secret + "\nother: visible-value"); !strings.Contains(out, "other: visible-value") {
		t.Errorf("the key after a block scalar was masked: %q", out)
	}
	if out := streamRedact("password: |\n  " + r5Secret + "\nother: visible-value"); !strings.Contains(out, "other: visible-value") {
		t.Errorf("the key after a block scalar was masked in the stream: %q", out)
	}
	if out := RedactSensitive("description: |\n  " + r5Secret); !strings.Contains(out, r5Secret) {
		t.Errorf("a block scalar under an ordinary name was masked: %q", out)
	}
}

// A tripled quote or a backtick that opens a literal masks it, closed or not,
// with the text right after it or after blanks; the stream masks the lines of
// a multi-line one as they come.
func TestUnclosedAndSpacedTripleQuotesAreMasked(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{`password = """` + r5Secret, r5Secret},
		{`password = '''` + r5Secret, r5Secret},
		{`password = ''' ` + r5Secret + ` '''`, r5Secret},
		{`password = """ ` + r5Secret + ` """`, r5Secret},
		{`password: """ ` + r5Secret, r5Secret},
		{`password = """` + r5Secret + "\nline2\nline3\"\"\"", r5Secret},
		{`password = """` + r5Secret + "\nline2-" + r5Secret + "\nline3\"\"\"", "line2-" + r5Secret},
		{"password = `" + r5Secret, r5Secret},
		{"const password = `" + r5Secret + "\nline2-" + r5Secret, "line2-" + r5Secret},
		{`os.Setenv("API_TOKEN", """` + r5Secret, r5Secret},
	} {
		checkBothSurfaces(t, "triple quote", tc.in, tc.secret)
	}
	for _, in := range []string{`password = ""`, `password = """"""`, `password = '''`, "password = \"\"\"\n", `x = ''' ` + r5Secret} {
		if strings.Contains(in, r5Secret) {
			continue
		}
		assertChainShows(t, in, strings.TrimRight(in, "\n"))
	}
}

// A call that neither sets a value nor reads one by its name, and a tuple, are
// read by their literals: a name or a flag is visible, a digit outside a
// trailing one or two digits makes a literal a credential.
func TestCallLiteralsWithDigitsAreMasked(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{`vault.read("API_KEY", "hunter_2024")`, "hunter_2024"},
		{`os.environ.get("API_KEY", "Summer-2024")`, "Summer-2024"},
		{`getenv("DB_PASSWORD", "ab_` + r5Secret + `")`, r5Secret},
		{`("DB_PASSWORD", "ab_` + r5Secret + `")`, r5Secret},
		{`config.set("password", "hunter_2024")`, "hunter_2024"},
		{`ENV.fetch("API_KEY", "p4ss_w0rd")`, "p4ss_w0rd"},
		{`req.Header.Add("X-Api-Key", "hunter_2024")`, "hunter_2024"},
		{`os.Setenv("API_TOKEN", "us_west_2024")`, "us_west_2024"},
	} {
		checkBothSurfaces(t, "call", tc.in, tc.secret)
	}
	for _, in := range []string{
		`vault.read("API_KEY", "us-west-1")`, `vault.read("API_KEY", "us-west-2")`, `fields.set("token", "access_token")`,
		`os.environ.get("API_KEY", "API_KEY_NAME")`, `("API_KEY", "access_token")`, `vault.read("API_KEY", "region_2")`,
	} {
		assertChainShows(t, in, in)
	}
	if !secretShaped("hunter_2024") || !secretShaped("Summer-2024") || secretShaped("us-west-1") || secretShaped("region_2") || secretShaped("access_token") {
		t.Error("secretShaped must treat a digit outside a trailing count as a credential's")
	}
}
