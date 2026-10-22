package tools

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
)

// credentialKind is what a name says about the value it assigns.
type credentialKind uint8

const (
	notCredential credentialKind = iota
	// singularCredential names one credential: its value is one, and so is
	// every scalar of a container it opens.
	singularCredential
	// pluralCredential names several: a value is one only when it looks
	// like one, not a count, a list marker or a list of references.
	pluralCredential
	// basicAuthCredential is docker's and npm's auth, whose value is one
	// only when it is base64 of user:password.
	basicAuthCredential
	// weakCredential is a name that mostly names a setting (credentials:
	// include): what it opens is a credential container, but a bare value
	// after it is one only when it is not a plain word.
	weakCredential
	// counterCredential is a name that counts or selects a pass over something
	// (first_pass, since_pass, selected_pass): a count, a boolean or a word
	// under it is none, and any other value still is a credential's.
	counterCredential
)

// A credential's name ends in one of these words, in any case and any
// style: API_KEY, apiKeys, "Secret Key", client-secret, accessToken,
// client-key-data, AccountKey, SMTP_PASS, SharedAccessSignature,
// "//registry.npmjs.org/:_authToken". "tokens" is not one: max_tokens and
// total_tokens count tokens. Neither is pwd, the working directory.
var (
	singularEndings = []string{"authorization", "token", "secret", "password", "passwd", "passphrase", "_pass", "-pass"}
	pluralEndings   = []string{"secrets", "passwords", "passwds", "passphrases"}
	// keyQualifiers make "key" a credential word: api key, secret_key,
	// clientKeys. A key alone names a map entry, not a credential.
	keyQualifiers = []string{"api", "access", "account", "private", "secret", "client", "auth", "signing", "encryption", "app", "master"}
	// paginationEndings name the cursors a model needs to page through an
	// API: nextPageToken, NextToken, continuationToken, --starting-token.
	paginationEndings = []string{"pagetoken", "nexttoken", "continuationtoken", "paginationtoken", "startingtoken", "skiptoken", "deltatoken", "synctoken"}
)

// credentialNameKind classifies lowercase text by the name it ends with.
func credentialNameKind(lower string) credentialKind {
	name := trailingName(lower)
	if name == "auth" || name == "_auth" {
		return basicAuthCredential
	}
	if isPaginationName(lower) {
		return notCredential
	}
	for _, word := range pluralEndings {
		if strings.HasSuffix(lower, word) {
			return pluralCredential
		}
	}
	for _, word := range singularEndings {
		if strings.HasSuffix(lower, word) {
			if (word == "_pass" || word == "-pass") && countsPasses(lower[:len(lower)-len(word)]) {
				return counterCredential
			}
			return singularCredential
		}
	}
	if strings.HasSuffix(lower, "credential") || strings.HasSuffix(lower, "credentials") {
		return weakCredential
	}
	// A name of several words, written with or without separators.
	if strings.HasSuffix(lower, "signature") && strings.HasSuffix(squashName(lower), "accesssignature") {
		return singularCredential
	}
	kind := pluralCredential
	rest, ok := strings.CutSuffix(lower, "keys")
	if !ok {
		kind, rest = singularCredential, lower
		for _, data := range []string{"-data", "_data", "data"} {
			if trimmed, cut := strings.CutSuffix(rest, data); cut {
				rest = trimmed
				break
			}
		}
		if rest, ok = strings.CutSuffix(rest, "key"); !ok {
			return notCredential
		}
	}
	if n := len(rest); n > 0 && (rest[n-1] == ' ' || rest[n-1] == '_' || rest[n-1] == '-') {
		rest = rest[:n-1]
	}
	for _, qualifier := range keyQualifiers {
		if strings.HasSuffix(rest, qualifier) {
			return kind
		}
	}
	return notCredential
}

// passCounters are the words before _pass that make it a pass over something
// (first_pass, since_pass, selected_pass) and not a password. A word a
// password is also qualified by (current, last, next, previous, new, old,
// main, no, ...) is not one: current_pass is a password field.
var passCounters = map[string]bool{
	"first": true, "second": true, "third": true, "since": true, "per": true, "num": true, "max": true, "min": true, "total": true,
	"count": true, "retry": true, "single": true, "multi": true, "micro": true, "compact": true, "each": true, "every": true,
	"selected": true, "sort": true, "scan": true, "parse": true, "warm": true, "cold": true, "dry": true, "fast": true, "slow": true,
}

// countsPasses reports a name whose last word, before _pass, makes it one that
// counts or selects a pass.
func countsPasses(before string) bool {
	i := len(before)
	for i > 0 && ((before[i-1] >= 'a' && before[i-1] <= 'z') || (before[i-1] >= '0' && before[i-1] <= '9')) {
		i--
	}
	return passCounters[before[i:]]
}

// trailingName is the identifier text ends with.
func trailingName(lower string) string {
	start := len(lower)
	for start > 0 && isIdentifierByte(lower[start-1]) {
		start--
	}
	return lower[start:]
}

// squashName is the end of a name without its separators.
func squashName(lower string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', ' ', '.':
			return -1
		}
		return r
	}, lower[max(0, len(lower)-64):])
}

func isPaginationName(lower string) bool {
	if !strings.HasSuffix(lower, "token") {
		return false
	}
	normalized := squashName(lower)
	for _, ending := range paginationEndings {
		if strings.HasSuffix(normalized, ending) {
			return true
		}
	}
	return false
}

// credentialStems find where a credential word may end without scanning the
// text with the whole grammar: every word holds one of them.
var credentialStems = []string{"key", "token", "secret", "passw", "passphrase", "_pass", "-pass", "credential", "authorization", "signature", "auth"}

// A value runs to whitespace, a quote, ',' or '}'. Inside JSON text its
// quotes arrive escaped (\"), twice for JSON inside a JSON string (\\\"); they
// delimit the value like plain quotes, and the value never ends inside an
// escape sequence: consuming only the backslash of \" left an unescaped quote
// that broke the whole document.
const credentialValue = `(?:[^\s"',}\\]|\\+[^\s"',}\\])+`

// quoteMark is what opens a literal: ' " or a backtick, doubled or tripled (a
// shell's ”value”, Python's """value"""), and escaped once or twice inside
// JSON text.
const quoteMark = `\\*(?:"{3}|'{3}|` + "`{3}" + `|"{1,2}|'{1,2}|` + "`{1,2}" + `)`

// fieldBetween is a field that lies between a credential's name and the field
// called value that holds its value: a member of a JSON object, after a comma
// and perhaps a line break ("slotSetting": false), or an attribute of an
// element (other="y"). Scalars only: a field that holds an object ends the
// search.
const fieldBetween = `(?:[ \t]*,[ \t]*(?:\r?\n[ \t]*)?"[^"\\\n]{1,40}"[ \t]*:[ \t]*(?:"(?:\\.|[^"\\\n])*"|[^\s,{}\[\]"]{1,40})|\s+[A-Za-z_][\w:.-]{0,30}=(?:"[^"\n]*"|'[^'\n]*'))`

// yamlTag is a scalar's type property, not part of its value. Reuse it in
// assignments, name/value fields and streamed block openers so those surfaces
// cannot disagree about where the scalar begins.
const yamlTag = `!(?:<[^<>\s]+>|(?:[\w.-]*!)?[\w:/.-]*)`

// fieldBefore is the same field between a value field and the name's field
// that follows it.
const fieldBefore = `(?:(?:"[^"\\\n]{1,40}"\s*:\s*(?:"(?:\\.|[^"\\\n])*"|[^\s,{}\[\]"]{1,40})|[A-Za-z_][\w:.-]{0,30}=(?:"[^"\n]*"|'[^'\n]*'))\s*,?\s*)`

// authSchemes are the HTTP authorization schemes whose name precedes the
// credentials in an Authorization value.
const authSchemes = `(?i:basic|bearer|digest|negotiate|ntlm|token|oauth|apikey|key|jwt|dpop|mac|hoba|mutual|vapid|aws4-hmac-sha256|scram-sha-1|scram-sha-256)`

var (
	// "name": "value", the value being a whole JSON string.
	jsonValueAfterName = regexp.MustCompile(`^"\s*:\s*"((?:\\.|[^"\\])*)"`)
	// name=value, name: value, name := value, name == value, name => "value",
	// name["x"] = value, "name": value inside JSON text; the groups are what
	// comes before the operator (a closing quote, a ], blanks), the operator,
	// what comes after it, an opening quote and the value.
	valueAfterName = regexp.MustCompile(`^((?:\\*["'])?\]?\s*)(:=|=>|!==?|={1,3}|:)(\s*)((?:` + quoteMark + `)?)(` + credentialValue + `)`)
	// name: !!str value, a YAML tag before the value; the groups are the same.
	valueAfterTag = regexp.MustCompile(`^((?:\\*["'])?\s*)(:)(\s*` + yamlTag + `[ \t]+)((?:` + quoteMark + `)?)(` + credentialValue + `)`)
	yamlTagPrefix = regexp.MustCompile(`^` + yamlTag + `[ \t]+`)
	// The indicator of a YAML block scalar: | or >, with a chomping sign and an
	// indentation digit.
	blockScalarIndicator = regexp.MustCompile(`^[|>][-+0-9]*$`)
	// --name value; the value is not another flag. The groups are an opening
	// quote and the value.
	valueAfterFlag = regexp.MustCompile(`^[ \t]+((?:` + quoteMark + `)?)((?:[^\s"',}\\-]|\\+[^\s"',}\\])(?:[^\s"',}\\]|\\+[^\s"',}\\])*)`)
	// ("NAME", "literal"): the closing quote of a name that is a call's first
	// argument, a comma, and the opening quote of the second.
	argumentLiteralStart = regexp.MustCompile(`^["'][ \t]*,[ \t]*(?:(default|value|const|initial|fallback|val)[ \t]*=[ \t]*)?(` + quoteMark + `)`)
	// name: &str = "literal", name: Optional[str] = 'literal': a type and the
	// opening quote of the literal it is given.
	// A constructor around the literal (Some("x"), String::from("x")) is read
	// through.
	typedAfterColon = regexp.MustCompile(`^[ \t]*:[ \t]*(?:&(?:'[A-Za-z_]+[ \t]+)?|\*)?[A-Za-z_][\w.]*(?:::[A-Za-z_][\w.]*)*(?:<[^<>=\n]{0,40}>|\[[^\[\]=\n]{0,40}\])?\??[ \t]*=[ \t]*(?:[A-Z]\w*(?:::\w+)*\([ \t]*)?(` + quoteMark + `)`)
	// name string = "literal": Go declares the type after the name.
	typedAfterSpace = regexp.MustCompile(`^[ \t]+(?:\*|&)?(?:string|String|str|\[\]byte|\[\]string)[ \t]*=[ \t]*(` + quoteMark + `)`)
	// What follows a credential's name when its value is a field called value
	// beside it: , "value": "x" (JSON), value: x on the next line (YAML), and
	// value="x" (an XML attribute). The groups are an opening quote and the
	// value.
	valueFieldAfterName = regexp.MustCompile(`^["']?(?:` + fieldBetween + `){0,6}[ \t]*[:,]?[ \t]*(?:\r?\n[ \t]*)?["']?(?i:value)["']?[ \t]*[:=][ \t]*(?:` + yamlTag + `[ \t]+)?((?:` + quoteMark + `)?)(` + credentialValue + `)`)
	// name = """ and name: ` with the text on the lines after the quote, or
	// after blanks that follow it (name = ''' x ''').
	blockLiteralStart = regexp.MustCompile(`^(?:\\*["'])?\]?\s*(?::=|=>|={1,3}|:)\s*(\\*(?:"{3}|'{3}|` + "`" + `))(?:[ \t]*\r?\n\s*|[ \t]+)`)
	// >text</ after an element's name: <password>x</password>.
	elementContent = regexp.MustCompile(`^>\s*([^<>]{1,512}?)\s*</`)
	// >token at the end of what is read: <password>x with its end tag to come.
	elementUnclosed = regexp.MustCompile(`^>\s*([^<>\s"'{}()\[\],;]{1,512})\s*$`)
	// .netrc's password, after a space.
	valueAfterSpace = regexp.MustCompile(`^[ \t]+(\S+)[ \t]*(?:\r?\n|$)`)
	// Authorization: <scheme> <credentials>; only the credentials are masked.
	credentialsAfterScheme = regexp.MustCompile(`^(?:\\*["'])?\s*[:=]\s*(?:\\*["'])?` + authSchemes + `[ \t]+(` + credentialValue + `)`)
	schemeInJSONString     = regexp.MustCompile(`^` + authSchemes + `[ \t]+`)
	authSchemeOnly         = regexp.MustCompile(`^` + authSchemes + `$`)
	bearerCredentials      = regexp.MustCompile(`(?i)^bearer\s+([A-Za-z0-9._~+/=-]{12,})`)
	basicCredentials       = regexp.MustCompile(`(?i)^basic\s+([A-Za-z0-9+/]{8,}={0,2})`)
	// variableName is a camelCase or snake_case identifier made of letters
	// alone; a credential has digits or symbols.
	variableName = regexp.MustCompile(`^(?:[a-z]{2,}(?:[A-Z][a-z]{2,})+|[A-Za-z]{2,}(?:_[A-Za-z]{2,})+);?$`)
	// The next YAML key, when a name's own line holds no value.
	nextKey = regexp.MustCompile(`^[A-Za-z_][\w-]*:$`)
	// A connection string's next "Name=" ends a value.
	nextSetting = regexp.MustCompile(`;[A-Za-z][A-Za-z0-9_]*=`)
)

// credentialSpans finds credentials by name, by shape and by format.
// Detection runs on the text as a reader sees it (see detectionView), and
// every value it masks is reported as written in the original text.
func credentialSpans(text string, view *textView, matched *[]string) []maskSpan {
	detect := text
	if view != nil {
		detect = view.text
	}
	lower := asciiLower(detect)
	spans := namedCredentialSpans(detect, lower)
	spans = append(spans, shapedCredentialSpans(detect)...)
	spans = append(spans, formattedCredentialSpans(detect, lower)...)
	for i, span := range spans {
		if view != nil {
			span = view.original(span)
			spans[i] = span
		}
		*matched = append(*matched, text[span.start:span.end])
	}
	return spans
}

// namedCredentialSpans masks the value a credential's name assigns, and the
// credentials that follow the Bearer and Basic schemes.
func namedCredentialSpans(text, lower string) []maskSpan {
	type candidate struct {
		at   int
		stem string
	}
	var candidates []candidate
	for _, stem := range credentialStems {
		for at := 0; ; {
			i := strings.Index(lower[at:], stem)
			if i < 0 {
				break
			}
			candidates = append(candidates, candidate{at + i, stem})
			at += i + 1
		}
	}
	// Left to right, a name inside a value already masked is part of that
	// value; skipping it keeps the pass linear when values hold names.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].at < candidates[j].at })
	var spans []maskSpan
	consumed := 0
	for _, c := range candidates {
		if c.at < consumed {
			continue
		}
		end, kind := credentialWordEnd(lower, c.at, c.stem)
		// A name that ends the text may still be the name of a field whose
		// value came before it.
		if kind == notCredential || (end < len(text) && !strings.ContainsRune(`"'\:=>]! `+"\t\r\n", rune(text[end]))) {
			continue
		}
		if span, ok := assignedValue(text, lower, c.at, end, kind, c.stem == "authorization"); ok {
			spans = append(spans, span)
			consumed = span.end
		}
	}
	for _, scheme := range []struct {
		word    string
		pattern *regexp.Regexp
		accept  func(string) bool
	}{
		{"bearer", bearerCredentials, isBearerCredentials},
		{"basic", basicCredentials, isBasicCredentials},
	} {
		for at := 0; ; {
			i := strings.Index(lower[at:], scheme.word)
			if i < 0 {
				break
			}
			i += at
			at = i + 1
			if !wordStart(text, i) {
				continue
			}
			match := scheme.pattern.FindStringSubmatchIndex(text[i:])
			if match == nil || !scheme.accept(text[i+match[2]:i+match[3]]) {
				continue
			}
			spans = append(spans, maskSpan{i + match[2], i + match[3]})
			at = i + match[3]
		}
	}
	return spans
}

// credentialWordEnd reports where the credential word holding the stem found
// at i ends, and what kind of name it completes.
func credentialWordEnd(lower string, i int, stem string) (int, credentialKind) {
	end := i + len(stem)
	switch stem {
	case "passw":
		switch {
		case strings.HasPrefix(lower[end:], "ord"):
			end += 3
		case strings.HasPrefix(lower[end:], "d"):
			end++
		default:
			return 0, notCredential
		}
	case "auth":
		// Only a whole name: auth or _auth, not authToken or oauth.
		if end < len(lower) && isIdentifierByte(lower[end]) {
			return 0, notCredential
		}
		return end, credentialNameKind(lower[max(0, end-64):end])
	case "token", "authorization", "signature":
		return end, credentialNameKind(lower[max(0, end-64):end])
	}
	if end < len(lower) && lower[end] == 's' {
		end++
	} else if stem == "key" {
		for _, data := range []string{"-data", "_data", "data"} {
			if strings.HasPrefix(lower[end:], data) {
				end += len(data)
				break
			}
		}
	}
	return end, credentialNameKind(lower[max(0, end-64):end])
}

// isAuthScheme reports a value that names an authorization scheme with no
// credentials after it.
func isAuthScheme(value string) bool {
	return authSchemeOnly.MatchString(value)
}

// proseAfterBearer are the words that follow "Bearer" in documentation of the
// scheme rather than in a request: no token is one of them.
var proseAfterBearer = map[string]bool{
	"authentication": true, "authorization": true, "authenticated": true, "credentials": true,
	"tokens": true, "scheme": true, "schemes": true, "security": true, "mechanism": true,
	"protocol": true, "headers": true, "required": true, "supported": true,
}

// isBearerCredentials reports whether what follows "Bearer" is a token and
// not a word of prose.
func isBearerCredentials(token string) bool {
	return !proseAfterBearer[asciiLower(token)]
}

// isBasicCredentials accepts Basic scheme credentials only when they decode
// to printable user:password text, so "Basic authentication" is prose.
func isBasicCredentials(encoded string) bool {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	for _, c := range decoded {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return strings.IndexByte(string(decoded), ':') > 0
}

// wordStart reports whether a word may start at i: the byte before it is not
// part of a word, or ends a JSON escape such as the \n before a token at the
// start of an encoded line.
func wordStart(text string, i int) bool {
	return i == 0 || !isWordByte(text[i-1]) || escapeBefore(text[:i])
}

func isWordByte(c byte) bool {
	return isIdentifierByte(c) || c >= 0x80
}

// Reference shapes. A leading $ or a %...% marks an environment variable only
// when the rest is the variable's whole name: ALL_CAPS (digits allowed after
// the first letter) or lowercase letters; $Xk92mQzpR7, %Xk92mQzpR7% and
// $3cr3tP@ss are literals. A <placeholder> has no digit: <your-token-here> is
// one, <Xk92mQzpR7> is not.
var (
	environmentReference = regexp.MustCompile(`^\$(?:[A-Z_][A-Z0-9_]*(?:/[A-Za-z0-9_./-]+)?|[a-z_]+)$`)
	percentReference     = regexp.MustCompile(`^%(?:[A-Z_][A-Z0-9_]*|[a-z_]+)%$`)
	placeholderReference = regexp.MustCompile(`^<[^<>\d]{1,64}>$`)
)

// isCredentialReference reports an unquoted value that points at a credential
// instead of holding one: a shell or template expansion, an environment
// variable's name or a <placeholder>. A quoted value is a literal, whatever it
// starts with; callers do not ask about one.
func isCredentialReference(value string) bool {
	value = strings.TrimSpace(value)
	switch {
	case carriesLiteral(value):
		return false
	case strings.HasPrefix(value, "${"), strings.HasPrefix(value, "$("), strings.HasPrefix(value, "`"):
		return true
	case strings.HasPrefix(value, "$"):
		return environmentReference.MatchString(value)
	case strings.HasPrefix(value, "%"):
		return percentReference.MatchString(value)
	case strings.HasPrefix(value, "<"):
		return placeholderReference.MatchString(value)
	}
	return false
}

// wholeReference matches a value that is nothing but an expansion or a
// template: ${NAME}, $(command), {{ .Values.x }}, a command in backticks.
var wholeReference = regexp.MustCompile("^(?:\\$\\{[^}\\s]{1,100}\\}|\\$\\([^)]{1,200}\\)|\\{\\{[^{}]{1,200}\\}\\}|`[^`]{1,200}`)$")

// defaultExpansion is a parameter expansion with a default that is not another
// expansion: ${A:-x}, ${A-x}, ${A:=x}, ${A=x}, ${A:+x}. ${A:-}, ${A:-$B} and
// ${A:?unset} hold no literal.
var defaultExpansion = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*:?[-=+][^$}]`)

// quotedInTemplate is a quoted string inside a template.
var quotedInTemplate = regexp.MustCompile(`["']([^"']{1,100})["']`)

// carriesLiteral reports a reference that holds a literal itself: a default
// value, a command that prints a word ($(echo x), `printf %s x`), a template
// that quotes one that is no name ({{ 'x' }}, default "x"). Such a value is
// the credential, not a pointer to it.
func carriesLiteral(value string) bool {
	switch {
	case strings.HasPrefix(value, "${"):
		return defaultExpansion.MatchString(value)
	case strings.HasPrefix(value, "$("):
		return printsLiteral(strings.TrimSuffix(value[2:], ")"))
	case strings.HasPrefix(value, "`"):
		return printsLiteral(strings.Trim(value, "`"))
	case strings.HasPrefix(value, "{{"):
		for _, quoted := range quotedInTemplate.FindAllStringSubmatch(value, -1) {
			if !isNameShaped(quoted[1]) {
				return true
			}
		}
	}
	return false
}

// printsLiteral reports a command that echoes or prints a word of its own
// (echo hunter2, printf %s hunter2), and not a variable, a flag or a format
// (echo $TOKEN, echo -n, printf '%s' "$TOKEN"). What follows a pipe, a
// redirect or a separator is another command.
func printsLiteral(command string) bool {
	command = strings.TrimSpace(command)
	word, args, _ := strings.Cut(command, " ")
	if word != "echo" && word != "printf" {
		return false
	}
	if i := strings.IndexAny(args, "|;&<>)`"); i >= 0 {
		args = args[:i]
	}
	for _, token := range strings.Fields(args) {
		token = strings.Trim(token, `"'`)
		if token != "" && strings.IndexByte("$-%", token[0]) < 0 {
			return true
		}
	}
	return false
}

// isWholeReference reports a quoted value that is a reference by its whole
// shape. A quote is evidence of a literal, so a quoted value is a reference
// only when nothing else is in it: "${DB_PASSWORD}" and "{{ .Values.password }}"
// are references, "${DB_PASSWORD}x9" and "$3cr3tP@ss" are literals.
func isWholeReference(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || carriesLiteral(value) {
		return false
	}
	switch value[0] {
	case '$':
		return wholeReference.MatchString(value) || environmentReference.MatchString(value)
	case '{', '`':
		return wholeReference.MatchString(value)
	case '%':
		return percentReference.MatchString(value)
	case '<':
		return placeholderReference.MatchString(value)
	}
	return false
}
