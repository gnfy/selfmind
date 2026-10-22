package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel"
)

// redactResult runs output through the tool-result exit boundary.
func redactResult(output string, err error) (kernel.ToolDispatchResult, error) {
	return RedactionMiddleware()(func(map[string]interface{}) (kernel.ToolDispatchResult, error) {
		return kernel.ToolDispatchResult{Output: output}, err
	})(nil)
}

// A JSON tool result must stay JSON after redaction: the plan gate, the model
// and every formatter parse it. A credential inside a JSON string arrives with
// escaped quotes, and masking it must neither break the document nor leak it.
func TestRedactionKeepsJSONToolResultsValid(t *testing.T) {
	const secret = "plain-secret-value-7d102e"
	plan, err := NewUpdatePlanTool().Execute(map[string]interface{}{"plan": []interface{}{
		map[string]interface{}{"step": `Set GITHUB_TOKEN="` + secret + `" in .env`, "status": "in_progress"},
		map[string]interface{}{"step": `Confirm DB_PASSWORD: "` + secret + `" before deploy`, "status": "pending"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _ := json.Marshal(map[string]interface{}{"output": `export API_TOKEN="` + secret + `"` + "\nok", "exit_code": 0})
	structured, _ := json.Marshal(map[string]interface{}{"password": []string{secret}, "note": "rotation pending"})
	for name, output := range map[string]string{"update_plan": plan, "terminal": string(terminal), "array under a credential key": string(structured)} {
		result, _ := redactResult(output, nil)
		if !json.Valid([]byte(result.Output)) || result.RedactionUnparseable {
			t.Errorf("%s became invalid JSON: %s", name, result.Output)
		}
		if strings.Contains(result.Output, secret) {
			t.Errorf("%s leaked the credential: %s", name, result.Output)
		}
	}
	var accepted PlanState
	redacted := RedactSensitive(plan)
	if err := json.Unmarshal([]byte(redacted), &accepted); err != nil || len(accepted.Plan) != 2 ||
		!strings.Contains(accepted.Plan[0].Step, `GITHUB_TOKEN="[REDACTED]"`) || !strings.Contains(accepted.Plan[1].Step, "before deploy") {
		t.Fatalf("the accepted plan did not survive redaction: %+v %v", accepted, err)
	}
	// A numeric credential stays masked and the document stays JSON.
	if got := RedactSensitive(`{"api_key":98765432123,"retries":3}`); !json.Valid([]byte(got)) || strings.Contains(got, "98765432123") || !strings.Contains(got, `"retries":3`) {
		t.Fatalf("numeric credential handling: %s", got)
	}
	// Plain text keeps its masking.
	if got := RedactSensitive(`GITHUB_TOKEN="` + secret + `" make release`); strings.Contains(got, secret) {
		t.Fatalf("plain text leaked the credential: %s", got)
	}
}

// When the text pass breaks a document, the value-by-value form must mask at
// least what the text pass masks: a key ending in a credential word in any
// style, and a key that is itself a secret. Every document carries
// "authToken":4242424, a bare number the text pass masks into invalid JSON.
func TestValueByValueRedactionMasksWhatTheTextPassMasks(t *testing.T) {
	const secret = "S-7d102e-credential"
	const keySecret = "opaque-key-secret-7d102e"
	RegisterSensitiveValue(keySecret)
	for _, tc := range []struct {
		name, key string
		hidden    []string
	}{
		{"camelCase token", "accessToken", []string{secret}},
		{"camelCase secret", "clientSecret", []string{secret}},
		{"auth token", "authToken", []string{secret}},
		{"registry token key", "//registry.npmjs.org/:_authToken", []string{secret}},
		{"spaced key", "Bot Token", []string{secret}},
		{"access key", "AWS_SECRET_ACCESS_KEY", []string{secret}},
		{"registered secret as a key", keySecret, []string{keySecret}},
		{"provider key as a key", "sk-abcdefghijklmnop123456", []string{"sk-abcdefghijklmnop123456"}},
	} {
		key, _ := json.Marshal(tc.key)
		document := `{"authToken":4242424,` + string(key) + `:"` + secret + `"}`
		got := RedactSensitive(document)
		if !json.Valid([]byte(got)) {
			t.Errorf("%s became invalid JSON: %s", tc.name, got)
		}
		for _, hidden := range tc.hidden {
			if strings.Contains(got, hidden) {
				t.Errorf("%s leaked %q: %s", tc.name, hidden, got)
			}
		}
		if !strings.Contains(got, `"authToken":"[REDACTED]"`) {
			t.Errorf("%s lost the number it masks: %s", tc.name, got)
		}
	}
	// Two keys that mask alike keep both values.
	got := RedactSensitive(`{"authToken":4242424,"sk-abcdefghijklmnop111111":"a","sk-abcdefghijklmnop222222":"b"}`)
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil || len(decoded) != 3 {
		t.Fatalf("masked keys collapsed: %s", got)
	}
}

// The escaped-quote form is pinned on the text pass alone, since the
// value-by-value form would otherwise hide a regression of the pattern.
func TestTextRedactionMasksEscapedQuoteValuesExactly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"step":"Set GITHUB_TOKEN=\"abc123\" in .env"}`, `{"step":"Set GITHUB_TOKEN=\"[REDACTED]\" in .env"}`},
		{`{"step":"Confirm DB_PASSWORD: \"x9-value\" before deploy"}`, `{"step":"Confirm DB_PASSWORD: \"[REDACTED]\" before deploy"}`},
		// JSON inside a JSON string escapes its quotes twice.
		{`{"output":"{\"env\":\"API_KEY=\\\"S\\\"\"}"}`, `{"output":"{\"env\":\"API_KEY=\\\"[REDACTED]\\\"\"}"}`},
	} {
		if got, _ := redactSensitiveText(tc.in); got != tc.want {
			t.Errorf("text pass:\n got: %s\nwant: %s", got, tc.want)
		}
	}
}

// Security wins over validity: a value-by-value form that would reveal a
// value the text pass masked is never used, and the result says it is
// unparseable.
func TestRedactionKeepsTheTextMaskWhenTheValueByValueFormRevealsASecret(t *testing.T) {
	document := `{"authToken":4242424,"note":"token=abc-7d102e"}`
	textMasked, masked := redactSensitiveText(document)
	leaky := func(value string) (string, bool) { return value, true }
	got, unparseable := redactBrokenJSON(document, textMasked, masked, leaky)
	if !unparseable || got != textMasked || strings.Contains(got, "abc-7d102e") {
		t.Fatalf("a revealing value-by-value form was used: %s unparseable=%v", got, unparseable)
	}
	if got, unparseable = redactBrokenJSON(document, textMasked, masked, redactJSONDocument); unparseable || !json.Valid([]byte(got)) || strings.Contains(got, "abc-7d102e") {
		t.Fatalf("a safe value-by-value form was refused: %s", got)
	}
	// The flag reaches the result: a short masked value that also occurs
	// elsewhere cannot be told apart from a leak, so the text mask is kept.
	result, _ := redactResult(`{"authToken":4242424,"password":"ab","note":"lab"}`, nil)
	if !result.RedactionUnparseable || strings.Contains(result.Output, `"password":"ab"`) {
		t.Fatalf("an unparseable redaction was not flagged: %+v", result)
	}
}

// printThroughTheToolChain prints output with the terminal tool through the
// production result middleware.
func printThroughTheToolChain(t *testing.T, output string) kernel.ToolDispatchResult {
	t.Helper()
	registry := NewRegistry()
	registry.Register(NewExecuteCommandTool())
	registry.UseResultMiddleware(RedactionMiddleware())
	registry.UseResultMiddleware(EvidenceMiddleware())
	result, err := registry.DispatchResult("terminal", map[string]interface{}{
		"command": "printf '%s' '" + output + "'", "cwd": t.TempDir(), "timeout": "5", "_tool_call_id": "print"})
	if err != nil {
		t.Fatalf("printing %q failed: %v", output, err)
	}
	return result
}

// A registered secret holding JSON structure or whitespace may match across
// JSON tokens, or come out of the value-by-value form with a pattern masking
// only part of a leaf, so a document it matched in keeps the text mask.
func TestRegisteredJSONSecretStaysMaskedThroughTheToolChain(t *testing.T) {
	dockerAuth := `{"auths":{"ghcr.io":{"auth":"Ym9iOmdocF9zM2NyM3Q="}}}`
	spaced := `{ "zone": "zleaf-7d102e-secret", "port": 7102 }`
	partlyCredential := `{"password":"pw-7d102e-hidden","auth":"YXV0aC1sZWFmLTdkMTAyZQ=="}`
	numberList := `40917, 77213`
	cookie := `{"Cookie":"sessionid=s3ss-r5-abcdef0123; csrftoken=r5csrfvalue99"}`
	dsn := `{"database":{"url":"postgres://app:dsn-r5-pass@db.internal:5432/app?sslmode=require&password=dsn-r5-pass"}}`
	secretKey := `{"r5-key-7d102e-secretname":{"password":"pw-r5-7d102e"}}`
	allCredential := `{"token":"tok-7d102e-leaf-secret","pin":{"password":7102994}}`
	passphrase := `correct horse r5 battery`
	for _, secret := range []string{dockerAuth, spaced, partlyCredential, numberList, cookie, dsn, secretKey, allCredential, passphrase} {
		RegisterSensitiveValue(secret)
	}
	for _, tc := range []struct {
		name, output string
		hidden       []string
	}{
		{"the whole result", dockerAuth, []string{"Ym9iOmdocF9zM2NyM3Q="}},
		{"re-encoded inside a larger result", `{"config":` + spaced + `,"ok":true}`, []string{"zleaf-7d102e-secret", "7102"}},
		{"a leaf the value-by-value form leaves alone", `{"config":` + partlyCredential + `}`, []string{"YXV0aC1sZWFmLTdkMTAyZQ==", "pw-7d102e-hidden"}},
		{"a secret across JSON tokens", `{"authToken":4242424,"pins":[` + numberList + `]}`, []string{"40917", "77213"}},
		{"a leaf a pattern masks only in part", cookie, []string{"s3ss-r5-abcdef0123", "r5csrfvalue99"}},
		{"the same leaf nested", `{"request":{"headers":` + cookie + `},"status":200}`, []string{"s3ss-r5-abcdef0123", "r5csrfvalue99"}},
		{"a URL leaf after its password parameter is masked", `{"service":` + dsn + `}`, []string{"dsn-r5-pass"}},
		{"a secret in a key", secretKey, []string{"r5-key-7d102e-secretname", "pw-r5-7d102e"}},
		{"every leaf masked by the value-by-value form", `{"config":` + allCredential + `}`, []string{"tok-7d102e-leaf-secret", "7102994"}},
		{"a passphrase with spaces", `{"authToken":4242424,"note":"` + passphrase + `"}`, []string{passphrase}},
	} {
		result := printThroughTheToolChain(t, tc.output)
		for _, hidden := range tc.hidden {
			if strings.Contains(result.Output, hidden) {
				t.Errorf("%s leaked %q: %s", tc.name, hidden, result.Output)
			}
		}
		if !result.RedactionUnparseable {
			t.Errorf("%s kept an unparseable mask without saying so: %s", tc.name, result.Output)
		}
	}
	// A secret with neither stays masked in the value-by-value form, so
	// another credential that breaks the document leaves it JSON.
	opaque := "opaque-r5-7d102e-token"
	RegisterSensitiveValue(opaque)
	result := printThroughTheToolChain(t, `{"authToken":4242424,"note":"uses `+opaque+`"}`)
	if result.RedactionUnparseable || !json.Valid([]byte(result.Output)) || strings.Contains(result.Output, opaque) {
		t.Fatalf("a safe value-by-value form was refused: %s unparseable=%v", result.Output, result.RedactionUnparseable)
	}
}

// A registered secret is known as written, but a JSON result carries it
// escaped: control characters as \t, \n or \u00XX, quotes and backslashes
// behind a backslash, and <, > and & as \u003c, \u003e and \u0026 when the
// encoder escapes HTML. Every escaped form is masked, in a valid document and
// in one another credential breaks, also where the raw form matched too, and
// an escape sequence before an identifier-like secret is a word boundary. An
// escaped form lies inside one JSON string, so its match never costs a broken
// document the value-by-value form; only a raw match of a structured or
// spaced secret keeps the text mask.
func TestRegisteredSecretsAreMaskedInTheirJSONEscapedForms(t *testing.T) {
	body := func(value string, html bool) string {
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(html)
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(out.String()), `"`), `"`)
	}
	tabbed := "tab\tsep-r6-secret"
	pem := "-----BEGIN R6 KEY-----\nMIIBr6keymaterial\n-----END R6 KEY-----"
	quoted := `say "r6-quoted" secret`
	backslashed := `C:\r6\vault-secret`
	html := "a&b<r6>c-secret"
	spacedHTML := "open <r6> & sesame"
	identifier := "tok7r6abcdef"
	escapeChar := "esc\x1br7-esc-secret"
	for _, secret := range []string{tabbed, pem, quoted, backslashed, html, spacedHTML, identifier, escapeChar} {
		RegisterSensitiveValue(secret)
	}
	for _, tc := range []struct {
		name, fields string
		hidden       string
		textMask     bool
	}{
		{"a tab", `"note":"` + body(tabbed, false) + `"`, "sep-r6-secret", false},
		{`a tab as \u0009`, `"note":"tab\u0009sep-r6-secret"`, "sep-r6-secret", false},
		{"PEM newlines", `"note":"` + body(pem, false) + `"`, "MIIBr6keymaterial", false},
		{"quotes", `"note":"` + body(quoted, false) + `"`, "r6-quoted", false},
		{"backslashes", `"note":"` + body(backslashed, false) + `"`, "vault-secret", false},
		{"HTML characters", `"note":"` + body(html, true) + `"`, "c-secret", false},
		{"the raw form matched too", `"raw":"` + spacedHTML + `","note":"` + body(spacedHTML, true) + `"`, "sesame", true},
		{`an identifier after \n`, `"note":"first line\n` + identifier + `"`, identifier, false},
		{`an identifier after \t`, `"note":"col\t` + identifier + `"`, identifier, false},
		// Some encoders write the hex digits of an escape in uppercase.
		{`PEM newlines as \u000A`, `"note":"-----BEGIN R6 KEY-----\u000AMIIBr6keymaterial\u000A-----END R6 KEY-----"`, "MIIBr6keymaterial", false},
		{"HTML characters in uppercase hex", `"note":"a\u0026b\u003Cr6\u003Ec-secret"`, "c-secret", false},
		{`an escape character as \u001B`, `"note":"esc\u001Br7-esc-secret"`, "r7-esc-secret", false},
	} {
		valid := printThroughTheToolChain(t, `{`+tc.fields+`}`)
		if strings.Contains(valid.Output, tc.hidden) || !json.Valid([]byte(valid.Output)) || valid.RedactionUnparseable {
			t.Errorf("%s in a valid document: %s unparseable=%v", tc.name, valid.Output, valid.RedactionUnparseable)
		}
		broken := printThroughTheToolChain(t, `{"authToken":4242424,`+tc.fields+`}`)
		if strings.Contains(broken.Output, tc.hidden) || broken.RedactionUnparseable != tc.textMask || (!tc.textMask && !json.Valid([]byte(broken.Output))) {
			t.Errorf("%s in a broken document: %s unparseable=%v", tc.name, broken.Output, broken.RedactionUnparseable)
		}
	}
}

// A matched run that only opens a JSON container under a credential name
// hides nothing, but a run with content is checked whatever brackets
// surround it.
func TestMatchedBracketRunsAreCheckedByTheirContent(t *testing.T) {
	opening := `{"credentials":{"type":"service_account"},"api_key":[{"id":1}],"authToken":4242424}`
	if result, _ := redactResult(opening, nil); result.RedactionUnparseable || !json.Valid([]byte(result.Output)) || strings.Contains(result.Output, "service_account") {
		t.Fatalf("a container opening forced the text mask: %s", result.Output)
	}
	document := `{"authToken":4242424,"api_key":[7102994]}`
	textMasked, masked := redactSensitiveText(document)
	leaky := func(value string) (string, bool) { return value, true }
	if got, unparseable := redactBrokenJSON(document, textMasked, masked, leaky); !unparseable || strings.Contains(got, "7102994") {
		t.Fatalf("a bracketed run escaped the check: %s", got)
	}
	if got, unparseable := redactBrokenJSON(document, textMasked, masked, redactJSONDocument); unparseable || strings.Contains(got, "7102994") {
		t.Fatalf("a safe value-by-value form was refused: %s", got)
	}
}

// The safety net indexes the document once and looks every masked value up
// in that index, so tens of thousands of masked values in a multi-megabyte
// result cost about one pass, not one pass per value.
func TestRevealCheckIsOnePassOverTheDocument(t *testing.T) {
	var document strings.Builder
	var masked maskedValues
	document.WriteString(`{"items":[`)
	for i := 0; document.Len() < 4<<20; i++ {
		if i > 0 {
			document.WriteString(",")
		}
		// Each masked value shares a long prefix with an ordinary name: the
		// worst case for a scan per value.
		fmt.Fprintf(&document, `{"name":"item-%07d","password":"[REDACTED]"}`, i)
		masked.matched = append(masked.matched, fmt.Sprintf("item-%07d-credential", i))
	}
	document.WriteString(`]}`)
	start := time.Now()
	if revealsMasked(document.String(), masked) {
		t.Fatal("found a masked value the document does not show")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("checking %d masked values in %d bytes took %s", len(masked.matched), document.Len(), elapsed)
	}
	last := masked.matched[len(masked.matched)-1]
	leaky := strings.TrimSuffix(document.String(), `]}`) + `,{"note":"` + last + `"}]}`
	if !revealsMasked(leaky, masked) {
		t.Fatal("missed a masked value revealed at the end of the document")
	}
}

func TestRedactionMiddlewareMasksToolOutputAndError(t *testing.T) {
	secret := "opaque-test-secret-123"
	RegisterSensitiveValue(secret)
	result, err := redactResult("output="+secret, errors.New("failure "+secret))
	if strings.Contains(result.Output, secret) || err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("output=%q err=%v", result.Output, err)
	}
}
