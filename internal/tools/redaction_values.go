package tools

import (
	"regexp"
	"strings"
)

// valueWindow bounds how much of the text after a credential's name is read
// to find its value. A value that fills it is long, and then is a credential
// whatever it looks like: code and references are short. Reading a bounded
// window keeps the work per name constant, so many names each followed by a
// long unspaced run cost no more than one.
const valueWindow = 512

func isBlank(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// windowEnd is where the window over rest ends: after valueWindow bytes that
// are not blanks, so that a long run of blanks between a name and its value
// does not push the value out of reach; a value is a value however far it lies.
func windowEnd(rest string) int {
	budget := valueWindow
	for i := 0; i < len(rest); i++ {
		if !isBlank(rest[i]) {
			if budget--; budget == 0 {
				return i + 1
			}
		}
	}
	return len(rest)
}

// firstWindow is how much of the text after a name is read first. A pattern
// that matches there and ends before the window does, or that cannot match
// whatever the text goes on with (the window ends in a value, not before
// one), needs no more; the full window of windowEnd is
// read only for a long value or a long run of blanks, so that the usual name
// costs a short match and not a long scan.
const firstWindow = 96

// matchValue matches re, whose last group is the value, against the start of
// rest. A value that reaches the end of the window is extended to its true
// end, and reported long.
func matchValue(re *regexp.Regexp, rest string) (match []int, long bool) {
	match, window := findWindowed(re, rest)
	if match == nil {
		return nil, false
	}
	last := len(match) - 1
	if match[last] == len(window) && len(window) < len(rest) {
		match[last] = valueEnd(rest, match[last])
		long = true
	}
	return match, long
}

// onlyStructure reports a window of nothing but what comes between a name and
// its value: blanks, quotes, backslashes, brackets and operators.
func onlyStructure(window string) bool {
	for i := 0; i < len(window); i++ {
		if strings.IndexByte(" \t\r\n\f\v=:]'\"`!>\\", window[i]) < 0 {
			return false
		}
	}
	return true
}

// findWindowed matches re against the start of rest in the first window when
// that settles it, and in the full window of windowEnd when it does not, and
// returns the window it matched in.
func findWindowed(re *regexp.Regexp, rest string) ([]int, string) {
	small := rest[:min(len(rest), firstWindow)]
	match := re.FindStringSubmatchIndex(small)
	if len(small) == len(rest) || (match != nil && match[1] < len(small)) {
		return match, small
	}
	// No match in the window, or one that reaches its end. A pattern is
	// anchored at the name's end and its value starts with the first byte that
	// is no blank, quote or operator; more text can give a match only to a
	// window that is nothing else, which ends before any value.
	if match == nil && !onlyStructure(small) {
		return nil, small
	}
	window := rest[:windowEnd(rest)]
	return re.FindStringSubmatchIndex(window), window
}

// valueEnd is where a value that runs past i ends: at whitespace, a quote, a
// comma or a closing brace, and never inside an escape sequence.
func valueEnd(text string, i int) int {
	for i < len(text) {
		c := text[i]
		switch {
		case c == '\\':
			j := i
			for j < len(text) && text[j] == '\\' {
				j++
			}
			if j >= len(text) || !valueByte(text[j]) {
				return i
			}
			i = j + 1
		case valueByte(c):
			i++
		default:
			return i
		}
	}
	return i
}

func valueByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\f', '\r', '"', '\'', ',', '}', '\\':
		return false
	}
	return true
}

// quotedEnd is where the quoted value that starts at from ends: at the next
// quote like the one that opened it on the same line, or -1 when there is none
// within reach. A plain quote is escaped by a backslash; a quote that is
// itself escaped (\") closes with the same two bytes; two single quotes inside
// single quotes are one (YAML's 'it”s'). A tripled quote and a backtick end
// where they are closed, on any line.
func quotedEnd(text string, from int, quote string) int {
	return closingMark(text, from, quote, false)
}

// spansLines reports a quote whose literal may run over lines: a tripled quote
// and a backtick (Python's """, JavaScript's and Go's raw strings).
func spansLines(quote string) bool {
	core := strings.TrimLeft(quote, `\`)
	return len(core) >= 3 || strings.HasPrefix(core, "`")
}

// maxQuotedLiteral bounds how far a closing quote is looked for: a quoted
// value of this many bytes is masked whole, and none longer than a log line is
// searched for.
const maxQuotedLiteral = 64 << 10

// closingMark finds the quote that closes the literal started by quote at
// from, on the same line, or on another when lines is true (within
// valueWindow bytes) or the quote spans lines (a tripled quote or a backtick);
// a plain quote's literal may also go on with up to two following lines that
// start with blanks, as a YAML scalar folds. -1 when none does.
func closingMark(text string, from int, quote string, lines bool) int {
	multi := spansLines(quote)
	limit := min(len(text), from+maxQuotedLiteral)
	if lines && !multi {
		limit = min(len(text), from+valueWindow)
	}
	folds := 0
	for i := from; i < limit; i++ {
		c := text[i]
		switch {
		case c == '\n' && !lines && !multi:
			if folds >= 2 || i-from >= valueWindow || i+1 >= len(text) || (text[i+1] != ' ' && text[i+1] != '\t') {
				return -1
			}
			folds++
			continue
		case len(quote) == 1 && c == '\\':
			i++
			continue
		case strings.HasPrefix(text[i:limit], quote):
			// A doubled quote inside is one: YAML's 'it''s', CSV's "a""b".
			if (quote == "'" || quote == `"`) && i+1 < limit && text[i+1] == quote[0] {
				i++
				continue
			}
			return i
		}
	}
	return -1
}

// literalEnd is where the literal that quote opened at from ends: at its
// closing quote, or, when none is within reach, at the end of its line or of
// its unspaced run, whichever is later. A literal that is not closed is
// masked as far as its value goes; it is never left visible for lack of a
// quote. lines lets a literal run over lines to a closing quote on another.
func literalEnd(text string, from int, quote string, lines bool) int {
	if closing := closingMark(text, from, quote, false); closing >= 0 {
		return closing
	}
	if lines {
		if closing := closingMark(text, from, quote, true); closing >= 0 {
			return closing
		}
	}
	end, multi := from, spansLines(quote)
	for limit := min(len(text), from+maxQuotedLiteral); end < limit && (multi || text[end] != '\n') && end-from < valueWindow; end++ {
	}
	// A line longer than the window goes on to its end.
	if !multi {
		for limit := min(len(text), from+maxQuotedLiteral); end < limit && text[end] != '\n'; end++ {
		}
	}
	if end > from && text[end-1] == '\r' {
		end--
	}
	return max(end, valueEnd(text, from))
}

// valueBranch is what one way of assigning a value found: span is what to
// mask when ok, and handled says the text was of this shape, so no other way
// is tried.
type valueBranch struct {
	span    maskSpan
	ok      bool
	handled bool
}

var notHandled = valueBranch{}

// exempt is a decision that a value of this shape holds no credential.
var exempt = valueBranch{handled: true}

// assignedValue finds the value a credential name between start and end
// assigns: a JSON member's string, a call's literal argument, a declaration,
// an assignment, the value field beside a name, an XML element's text, a
// command-line flag's argument, or .netrc's password.
func assignedValue(text, lower string, start, end int, kind credentialKind, authorization bool) (maskSpan, bool) {
	rest := text[end:]
	nameStart := identifierStart(text, start)
	// The first byte after the name tells which shapes can follow it; the
	// patterns of the others are not tried.
	var c, f byte
	if rest != "" {
		c = rest[0]
		// f is the first byte that is not a blank, where an operator, a
		// quote or a type word begins.
		for i := 0; i < len(rest); i++ {
			if !isBlank(rest[i]) {
				f = rest[i]
				break
			}
		}
	}
	if c == '"' {
		if found := jsonMemberValue(text, rest, end, kind, authorization); found.handled {
			return found.span, found.ok
		}
	}
	if found := schemeCredentials(text, rest, end, authorization); found.handled {
		return found.span, found.ok
	}
	if c == '"' || c == '\'' {
		if found := argumentLiteral(text, rest, nameStart, end, kind, authorization); found.handled {
			return found.span, found.ok
		}
	}
	if f == ':' || ((c == ' ' || c == '\t') && strings.IndexByte("*&[sS", f) >= 0) {
		if found := typedLiteral(text, rest, end, kind, authorization); found.handled {
			return found.span, found.ok
		}
	}
	if found := valueFieldBeside(text, rest, nameStart, end, f, kind, authorization); found.handled {
		return found.span, found.ok
	}
	if f != 0 && strings.IndexByte("\"'\\]:=!", f) >= 0 {
		if found := assignmentValue(text, lower, start, end, rest, kind, authorization); found.handled {
			return found.span, found.ok
		}
		if found := blockLiteral(text, rest, end, kind, authorization); found.handled {
			return found.span, found.ok
		}
	}
	if found := elementText(text, rest, nameStart, end, kind, authorization); found.handled {
		return found.span, found.ok
	}
	if found := flagValue(text, lower, end, rest, kind); found.handled {
		return found.span, found.ok
	}
	if found := netrcValue(text, lower, start, end, rest); found.handled {
		return found.span, found.ok
	}
	return maskSpan{}, false
}

// identifierStart is where the identifier that holds offset begins.
func identifierStart(text string, offset int) int {
	start := offset
	for start > 0 && offset-start < contextWindow && isIdentifierByte(text[start-1]) {
		start--
	}
	return start
}

// exemptValue reports a value that its name's kind says is no credential: a
// scheme word alone after Authorization, base64 that is not user:password
// under auth, a count under a plural name, a word under a name such as
// credentials that is usually structure.
func exemptValue(value string, quoted bool, kind credentialKind, authorization bool) bool {
	value = strings.TrimSpace(value)
	switch {
	case authorization && isAuthScheme(value):
		return true
	case kind == basicAuthCredential && !isBasicCredentials(value):
		return true
	case kind == pluralCredential && (isCount(value) || (!quoted && !looksLikeListedCredential(value))):
		return true
	case kind == weakCredential && isSettingWord(value):
		return true
	case kind == counterCredential && isCounterValue(value):
		return true
	case quoted && isWholeReference(value):
		return true
	}
	return false
}

// isCounterValue reports what a counter's name (first_pass) holds: a count, a
// boolean or null, a setting word or a word, with the punctuation of the
// statement around it ignored.
func isCounterValue(value string) bool {
	core := strings.Trim(strings.TrimSpace(value), valuePrefixes+valueTrailers+"`'\".")
	return isCount(core) || isLiteralKeyword(core) || isSettingWord(core) || isWordLike(core)
}

// jsonMemberValue is "name": "value" in JSON text. A string is a literal.
func jsonMemberValue(text, rest string, end int, kind credentialKind, authorization bool) valueBranch {
	match := jsonValueAfterName.FindStringSubmatchIndex(rest)
	if match == nil {
		return notHandled
	}
	from, to := end+match[2], end+match[3]
	if exemptValue(text[from:to], true, kind, authorization) {
		return exempt
	}
	if authorization {
		if scheme := schemeInJSONString.FindStringIndex(text[from:to]); scheme != nil {
			from += scheme[1]
		}
	}
	return valueBranch{span: maskSpan{from, to}, ok: from < to, handled: true}
}

// schemeCredentials is Authorization: <scheme> <credentials>.
func schemeCredentials(text, rest string, end int, authorization bool) valueBranch {
	if !authorization {
		return notHandled
	}
	match, _ := matchValue(credentialsAfterScheme, rest)
	if match == nil {
		return notHandled
	}
	return spanBranch(text, end+match[2], end+match[3], false)
}

// argumentLiteral is a call that names the credential in a quoted string and
// gives a quoted literal after it: os.Setenv("API_TOKEN", "x"),
// viper.Set("api_key", "x"), define('DB_PASSWORD', 'x'),
// req.Header.Add("X-Api-Key", "x"), os.environ.get("API_KEY", "x") and the
// default of args.add_argument("--token", default="x"). The name is the whole of
// its string, so a message that ends in a credential's name is not one.
//
// A call that sets or reads a value masks any literal; any other call, and a
// tuple, masks one that looks like a credential, since a list of names pairs
// names with other names (_first_env("A_TOKEN", "B_TOKEN")).
func argumentLiteral(text, rest string, nameStart, end int, kind credentialKind, authorization bool) valueBranch {
	match := argumentLiteralStart.FindStringSubmatchIndex(rest)
	if match == nil || nameStart == 0 || text[nameStart-1] != rest[0] {
		return notHandled
	}
	call := callContext(text, nameStart)
	keyword := match[2] >= 0
	if call == notACall || (text[nameStart] == '-' && !keyword) {
		return notHandled
	}
	quote := rest[match[4]:match[5]]
	from := end + match[5]
	if emptyMultiQuote(text, from, quote, true) {
		// "" is an empty string, not the opening of one.
		return exempt
	}
	to := literalEnd(text, from, quote, true)
	// "NAME","x": is a JSON object's next key, not an argument.
	if closed := closingMark(text, from, quote, true) == to; closed {
		if after := strings.TrimLeft(text[to+len(quote):min(len(text), to+len(quote)+64)], " \t"); strings.HasPrefix(after, ":") {
			return notHandled
		}
	}
	value := text[from:to]
	switch {
	case isLiteralKeyword(value), strings.HasPrefix(value, "-") && !strings.ContainsAny(value, " \t"), exemptValue(value, true, kind, authorization),
		isTemplateLiteral(quote, value), isEncodingName(value):
		return exempt
	case keyword || call == valueCall:
		if isNameShaped(value) {
			return exempt
		}
	case !secretShaped(value):
		return exempt
	}
	return spanBranch(text, from, to, true)
}

// callKind is what a quoted name that follows a ( belongs to.
type callKind uint8

const (
	notACall callKind = iota
	// valueCall sets or reads a value by its name: Setenv, set, put, define,
	// add, append, get, getenv.
	valueCall
	// otherCall is another call, or a tuple, which a list of names also is.
	otherCall
)

// callContext reads what comes before the quote that opens the name at
// nameStart: a ( that opens a call or a tuple, with at most one argument
// before the name (config.set("section", "password", "x")), and the call's name.
func callContext(text string, nameStart int) callKind {
	p := nameStart - 2
	skipBlanks := func() {
		for p >= 0 && (text[p] == '\\' || text[p] == ' ' || text[p] == '\t') {
			p--
		}
	}
	skipBlanks()
	if p >= 0 && text[p] == ',' {
		// One argument before the name: a string or a variable.
		p--
		skipBlanks()
		if p >= 0 && (text[p] == '"' || text[p] == '\'') {
			quote := text[p]
			p--
			for p >= 0 && text[p] != quote && nameStart-p < 160 {
				p--
			}
			if p < 0 || text[p] != quote {
				return notACall
			}
			p--
		} else {
			for p >= 0 && (isIdentifierByte(text[p]) || text[p] == '.') && nameStart-p < 160 {
				p--
			}
		}
		skipBlanks()
	}
	if p < 0 || text[p] != '(' {
		return notACall
	}
	q := p
	for q > 0 && p-q < nameWindow && isIdentifierByte(text[q-1]) && text[q-1] != '-' {
		q--
	}
	// The call's name, without the underscores a private helper starts with,
	// begins with a word that sets or reads a value: Setenv, setdefault,
	// SetItem, define, putenv, Add, append, get, getenv.
	callee := strings.TrimLeft(asciiLower(text[q:p]), "_")
	for _, word := range []string{"set", "put", "define", "export", "assign", "store", "save", "write", "add", "append", "get", "insert", "push", "header"} {
		if strings.HasPrefix(callee, word) {
			return valueCall
		}
	}
	return otherCall
}

// isEncodingName reports the name of a text encoding, the usual last argument
// of a call that writes a file or decodes a buffer.
func isEncodingName(value string) bool {
	switch asciiLower(strings.TrimSpace(value)) {
	case "utf8", "utf-8", "utf16le", "ucs2", "ascii", "base64", "base64url", "hex", "binary", "latin1":
		return true
	}
	return false
}

// nameWord is a word, or words joined by _ . : / or -, that a name or a
// setting is made of: api_mode, ANTHROPIC_TOKEN, us-west-1, --config.
var nameWord = regexp.MustCompile(`^[-_]{0,2}[A-Za-z]+(?:[_.:/-][A-Za-z0-9]+)*$`)

// envName is an environment variable's name: two words or more in capitals.
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+$`)

// trailingCount is a count of one or two digits that ends a name after a
// joiner: us-west-1, region_2.
var trailingCount = regexp.MustCompile(`[_.:/-][0-9]{1,2}$`)

// secretShaped reports a literal that looks like a credential and not like a
// name: a symbol among its letters, or a digit that is not a trailing count of
// one or two digits (us-west-1 is a name; hunter_2024 and ab_Xk9mQz2pLw4r are
// not), with no words around them. The name of an environment variable is one
// whatever digits are in it.
func secretShaped(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if !nameWord.MatchString(value) {
		return true
	}
	return strings.ContainsAny(trailingCount.ReplaceAllString(value, ""), "0123456789") && !envName.MatchString(value)
}

// isNameShaped reports a literal that names a credential or a variable and is
// not one: access_token, API_KEY_NAME. A call that sets a value maps a name to
// another name this way (fields.set("token", "access_token")).
func isNameShaped(value string) bool {
	value = strings.TrimSpace(value)
	return envName.MatchString(value) || (nameWord.MatchString(value) && credentialNameKind(asciiLower(value)) != notCredential)
}

// typedLiteral is a declaration with a type between the name and a quoted
// literal: name: &str = "x" (Rust), name: string = "x" (TypeScript, Kotlin,
// Python), var name string = "x" (Go).
func typedLiteral(text, rest string, end int, kind credentialKind, authorization bool) valueBranch {
	match := typedAfterColon.FindStringSubmatchIndex(rest)
	if match == nil {
		if match = typedAfterSpace.FindStringSubmatchIndex(rest); match == nil {
			return notHandled
		}
	}
	quote := rest[match[2]:match[3]]
	from := end + match[3]
	if emptyMultiQuote(text, from, quote, true) {
		return exempt
	}
	to := literalEnd(text, from, quote, true)
	if exemptValue(text[from:to], true, kind, authorization) || isTemplateLiteral(quote, text[from:to]) {
		return exempt
	}
	return spanBranch(text, from, to, true)
}

// valueFieldBeside is a credential's name next to a field called value: the
// name: X_PASSWORD / value: x pairs of a Kubernetes env list, an ECS task
// definition's {"name": …, "value": …}, an XML <add key=… value=…/>, and a
// name whose value is a nested map with a value field. The field follows the
// name or, in a list entry, a JSON object or an element's attributes, comes
// before it; a comment, blank lines and the entry's other fields may lie
// between.
func valueFieldBeside(text, rest string, nameStart, end int, f byte, kind credentialKind, authorization bool) valueBranch {
	// The field called value follows after a quote, a colon, a comma or a line
	// break; after an operator (name=value) it is another thing.
	if strings.IndexByte("\"':,vV", f) >= 0 && hasValueWord(rest) {
		if match, long := matchValue(valueFieldAfterName, rest); match != nil {
			return valueOfField(text, end+match[4], end+match[5], rest[match[2]:match[3]], long, kind, authorization)
		}
	}
	if !nameOfField(text, nameStart) {
		return notHandled
	}
	if from, to, quote, ok := valueFieldOnLaterLine(text, nameStart, end); ok {
		return valueOfField(text, from, to, quote, false, kind, authorization)
	}
	if from, to, quote, ok := valueFieldBefore(text, nameStart); ok {
		return valueOfField(text, from, to, quote, false, kind, authorization)
	}
	return notHandled
}

// valueOfField decides the value of a field beside a name: from..to, opened by
// quote when it is quoted.
func valueOfField(text string, from, to int, quote string, long bool, kind credentialKind, authorization bool) valueBranch {
	quoted := quote != ""
	if !quoted && blockScalarIndicator.MatchString(text[from:to]) {
		start, end := lineAround(text, from)
		if header := blockScalarOpener.FindStringSubmatch(text[start:end]); header != nil {
			return blockScalarBody(text, to, len(header[1]))
		}
	}
	if quoted {
		if emptyMultiQuote(text, from, quote, false) {
			return exempt
		}
		to = literalEnd(text, from, quote, false)
		if isCommandSubstitution(quote, text[from:to]) {
			return exempt
		}
	}
	value := text[from:to]
	if !long && !quoted && (isLiteralKeyword(value) || isCredentialReference(value) || opensContainer(value)) {
		return exempt
	}
	if exemptValue(value, quoted, kind, authorization) {
		return exempt
	}
	return spanBranch(text, from, to, quoted)
}

var (
	// nameFieldBefore ends the text before a name that is the value of a
	// field called name or key.
	nameFieldBefore = regexp.MustCompile(`(?i)(?:^|[\s,{<"'-])(?:name|key)["']?\s*[:=]\s*(?:` + yamlTag + `[ \t]+)?["']?$`)
	// valueBeforeName ends the text before such a name with a field called
	// value: {"value": "x", "name": ...}, <add value="x" key=...>. The value is
	// a double-quoted, a single-quoted or a bare string.
	valueBeforeName = regexp.MustCompile(`(?i)(?:^|[\s,{<"'])value["']?\s*[:=]\s*(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|([^\s,"'<>}]+))\s*,?\s*` + fieldBefore + `{0,6}["']?(?:name|key)["']?\s*[:=]\s*(?:` + yamlTag + `[ \t]+)?["']?$`)
)

// nameOfField reports a credential name that is the value of a field called
// name or key.
func nameOfField(text string, nameStart int) bool {
	tail := text[max(0, nameStart-32):nameStart]

	return nameFieldBefore.MatchString(tail)
}

// hasValueWord reports that the word value is in the first window after a
// name, which a field called value beside it needs.
func hasValueWord(rest string) bool {
	window := rest[:min(len(rest), firstWindow)]
	return strings.Contains(window, "alue") || strings.Contains(window, "ALUE")
}

// yamlLine is a line of a block mapping or list read for its key.
type yamlLine struct {
	col, keyCol int
	dash        bool
	key         string
	valueAt     int
}

// parseYAMLLine reads a line's indent, its list dash, its key and where the
// key's value starts; ok is false for a blank line or a comment.
func parseYAMLLine(line string) (yamlLine, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || trimmed[0] == '#' || trimmed[0] == '\r' {
		return yamlLine{}, false
	}
	l := yamlLine{col: len(line) - len(trimmed)}
	l.keyCol = l.col
	if trimmed[0] == '-' {
		l.dash = true
		l.keyCol = len(line) - len(strings.TrimLeft(trimmed[1:], " \t"))
	}
	j := l.keyCol
	for j < len(line) && (isIdentifierByte(line[j]) || line[j] == '.') {
		j++
	}
	if j > l.keyCol && j < len(line) && line[j] == ':' {
		l.key = asciiLower(line[l.keyCol:j])
		l.valueAt = j + 1
		for l.valueAt < len(line) && (line[l.valueAt] == ' ' || line[l.valueAt] == '\t') {
			l.valueAt++
		}
	}
	return l, true
}

// maxFieldLines bounds how many lines are read for the value field of a name.
const maxFieldLines = 8

// lineAround is the line that holds offset, within contextWindow bytes of it.
func lineAround(text string, offset int) (start, end int) {
	start = max(0, offset-contextWindow)
	start += strings.LastIndexByte(text[start:offset], '\n') + 1
	end = len(text)
	if nl := strings.IndexByte(text[offset:min(len(text), offset+contextWindow)], '\n'); nl >= 0 {
		end = offset + nl
	} else {
		end = min(end, offset+contextWindow)
	}
	return start, end
}

// fieldValue is the value of a line that holds a key's value, bare or quoted;
// quote is the opening quote of a quoted one, and from is where its text starts.
func fieldValue(text string, lineStart int, line string, l yamlLine) (from, to int, quote string, ok bool) {
	value := line[l.valueAt:]
	from = lineStart + l.valueAt
	if tag := yamlTagPrefix.FindStringIndex(value); tag != nil {
		value = value[tag[1]:]
		from += tag[1]
	}
	if value == "" || value[0] == '&' || value[0] == '*' || value[0] == '#' {
		return 0, 0, "", false
	}
	if value[0] == '"' || value[0] == '\'' {
		return from + 1, from + 1, value[:1], true
	}
	if i := strings.Index(value, " #"); i >= 0 {
		value = value[:i]
	}
	value = strings.TrimRight(value, " \t\r")
	return from, from + len(value), "", true
}

// valueFieldOnLaterLine finds the value field of the list entry or mapping that
// a name ends the line of, on one of the lines after it: blank lines, comments
// and the entry's other keys (and what is under them) may lie between.
func valueFieldOnLaterLine(text string, nameStart, end int) (from, to int, quote string, ok bool) {
	lineStart, lineEnd := lineAround(text, nameStart)
	if lineEnd >= len(text) || lineEnd < end {
		return 0, 0, "", false
	}
	rest := strings.TrimSpace(text[end:lineEnd])
	if strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, `'`) {
		rest = strings.TrimSpace(rest[1:])
	}
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return 0, 0, "", false
	}
	head, ok := parseYAMLLine(text[lineStart:lineEnd])
	if !ok {
		return 0, 0, "", false
	}
	pos, seen := lineEnd+1, 0
	for pos < len(text) && seen < maxFieldLines && pos-lineEnd < contextWindow {
		next := len(text)
		if nl := strings.IndexByte(text[pos:], '\n'); nl >= 0 {
			next = pos + nl
		}
		line := text[pos:next]
		start := pos
		pos = next + 1
		seen++
		l, isLine := parseYAMLLine(line)
		switch {
		case !isLine:
			continue
		case l.dash && l.col < head.keyCol, !l.dash && l.col < head.keyCol:
			return 0, 0, "", false
		case l.col == head.keyCol && !l.dash && l.key == "value":
			return fieldValue(text, start, line, l)
		}
	}
	return 0, 0, "", false
}

// valueFieldBefore finds the value field that comes before a name in the same
// entry: a field of an object or an element's attributes on the line, or an
// earlier line of the same list entry.
func valueFieldBefore(text string, nameStart int) (from, to int, quote string, ok bool) {
	tailStart := max(0, nameStart-400)
	if match := valueBeforeName.FindStringSubmatchIndex(text[tailStart:nameStart]); match != nil {
		for g, mark := range []string{`"`, `'`, ""} {
			if a, b := match[2+2*g], match[3+2*g]; a >= 0 {
				return tailStart + a, tailStart + b, mark, true
			}
		}
	}
	lineStart, lineEnd := lineAround(text, nameStart)
	head, isLine := parseYAMLLine(text[lineStart:lineEnd])
	if !isLine || head.dash {
		return 0, 0, "", false
	}
	for seen, at := 0, lineStart-1; at > 0 && seen < maxFieldLines && lineStart-at < contextWindow; seen++ {
		begin := max(0, at-contextWindow)
		begin += strings.LastIndexByte(text[begin:at], '\n') + 1
		line := text[begin:at]
		at = begin - 1
		l, isLine := parseYAMLLine(line)
		switch {
		case !isLine:
			continue
		case l.dash && l.keyCol == head.keyCol:
			// The entry's first line.
			if l.key == "value" {
				return fieldValue(text, begin, line, l)
			}
			return 0, 0, "", false
		case l.col < head.keyCol:
			return 0, 0, "", false
		case !l.dash && l.col == head.keyCol && l.key == "value":
			return fieldValue(text, begin, line, l)
		}
	}
	return 0, 0, "", false
}

// blockScalar is a YAML block scalar (password: |, key: >-) whose lines are the
// credential: the lines after the indicator that are indented deeper than the
// key, blank lines among them. Indentation determines the end, so unlike a
// quoted literal it needs no bounded search for a closing delimiter. The
// indicator stays; with no such line in the text yet, there is nothing to mask
// and the indicator is no value either.
func blockScalar(text string, stemStart, to int) valueBranch {
	nameStart := identifierStart(text, stemStart)
	windowStart := max(0, nameStart-lineWindow)
	keyCol := nameStart - (windowStart + strings.LastIndexByte(text[windowStart:nameStart], '\n') + 1)
	if nameStart > 0 && (text[nameStart-1] == '"' || text[nameStart-1] == '\'') {
		keyCol--
	}
	return blockScalarBody(text, to, keyCol)
}

// blockScalarBody reads the scalar opened at to using the actual mapping key's
// column. A name/value pair's key is value, not the earlier credential name.
func blockScalarBody(text string, to, keyCol int) valueBranch {
	lineEnd := strings.IndexByte(text[to:], '\n')
	if lineEnd < 0 {
		return exempt
	}
	if tail := strings.TrimSpace(text[to : to+lineEnd]); tail != "" && tail[0] != '#' {
		return notHandled
	}
	first, last := -1, -1
	for pos := to + lineEnd + 1; pos < len(text); {
		next := len(text)
		if nl := strings.IndexByte(text[pos:], '\n'); nl >= 0 {
			next = pos + nl
		}
		line := strings.TrimRight(text[pos:next], " \t\r")
		if line != "" {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if indent <= keyCol {
				break
			}
			if first < 0 {
				first = pos + indent
			}
			last = pos + len(line)
		}
		pos = next + 1
	}
	if first < 0 {
		return exempt
	}
	return valueBranch{span: maskSpan{first, last}, ok: true, handled: true}
}

// commandEnd is the end of the $( ) that starts at from, on its own line and
// within 240 bytes, or from when it has none.
func commandEnd(text string, from int) int {
	depth := 0
	for i := from; i < len(text) && i < from+240 && text[i] != '\n'; i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return from
}

// assignmentValue is name=value, name: value, name := value, name == value,
// name => "value" and name["x"] = value: a quoted value is a literal; a bare
// one is code or a literal by the evidence of its line (see readsAsCode).
func assignmentValue(text, lower string, start, end int, rest string, kind credentialKind, authorization bool) valueBranch {
	match, long := matchValue(valueAfterName, rest)
	if match == nil {
		return notHandled
	}
	if rest[match[4]:match[5]] == ":" && match[8] == match[9] && rest[match[10]] == '!' {
		// name: !!str value: the tag is not the value.
		if tagged, tagLong := matchValue(valueAfterTag, rest); tagged != nil {
			match, long = tagged, tagLong
		}
	}
	before := rest[match[2]:match[3]]
	operator := rest[match[4]:match[5]]
	gap := rest[match[6]:match[7]]
	quote := rest[match[8]:match[9]]
	quoted := quote != ""
	from, to := end+match[10], end+match[11]
	if operator == ":" && !quoted && !strings.Contains(gap, "\n") && blockScalarIndicator.MatchString(text[from:to]) {
		if found := blockScalar(text, start, to); found.handled {
			return found
		}
	}
	// NAME= command runs the command with NAME empty; the name touches the
	// operator and whitespace follows it. A hash rocket is a hash's, and a
	// bare value after it is code.
	if operator == "=" && match[3] == match[2] && match[7] > match[6] {
		return exempt
	}
	// A string literal's prefix (f"...", rb'...') is not its value.
	if !quoted && to-from <= 2 && to < len(text) && (text[to] == '"' || text[to] == '\'') && isStringPrefix(text[from:to]) {
		quote, quoted = text[to:to+1], true
		from = to + 1
		to = from
	}
	if quoted {
		if emptyMultiQuote(text, from, quote, false) {
			// '' followed by text that is not a value: an empty string.
			return notHandled
		}
		if strings.Contains(gap, "\n") && spansLines(quote) {
			// A docstring on the line after the name: class Token:, def f() -> Token:.
			return exempt
		}
		to = literalEnd(text, from, quote, false)
		if isCommandSubstitution(quote, text[from:to]) || isTemplateLiteral(quote, text[from:to]) {
			return exempt
		}
		if isComparison(operator) && !hasAlphanumeric(text[from:to]) {
			// token != "--": a test against punctuation, not a password.
			return exempt
		}
		if isComparison(operator) && isTypeName(text[from:to]) {
			// typeof password !== "string"
			return exempt
		}
	}
	if kind == weakCredential && !quoted {
		if ((operator == ":" && gap == "") || wordsFollow(text, to)) && isWordLike(text[from:to]) {
			// credential:read is a name and a word, not a mapping, and
			// "credentials: try the next one" is a sentence. A value with a
			// digit or a symbol among its letters is neither.
			return exempt
		}
	}
	if !quoted && strings.HasPrefix(text[from:to], "$(") && !strings.Contains(text[from:to], ")") {
		// A command substitution has blanks in it: it ends at its own ).
		to = max(to, commandEnd(text, from))
	}
	value := text[from:to]
	if !quoted && strings.HasPrefix(before, "]") && !strings.ContainsAny(before, "\"'") {
		// x[password] = value: the name is a variable used as a key, and the
		// bare value is another.
		return exempt
	}
	if !quoted && !long {
		core := strings.TrimLeft(value, valuePrefixes)
		site := assignment{operator: operator, spaced: strings.ContainsAny(before, " \t"), keyQuote: strings.ContainsAny(before, "\"'"), gap: gap}
		switch {
		case strings.HasPrefix(value, "="), value == "-", isLiteralKeyword(value), opensContainer(value), isCredentialReference(value):
			// What follows == with nothing to compare, a list marker, a
			// boolean, the opening of a container, a reference.
			return exempt
		case operator == ":" && isTypeName(value):
			return exempt
		case operator == "=>" && plausibleCode(core, strings.TrimRight(core, valueTrailers), isCallShape(core, lineAfter(text, to))):
			// A hash rocket's bare value is a variable or a call, not a word with digits.
			return exempt
		case arrayRepeat.MatchString(value):
			// [0u8; 32]
			return exempt
		case isInterpolation(value, text[to:]):
			// {name} in a format string or a Go value list.
			return exempt
		case operator == ":" && strings.Contains(gap, "\n") && nextKey.MatchString(value):
			// The name ends its line and the next line is another key.
			return exempt
		case readsAsCode(text, start, end, site, from, to):
			return exempt
		}
	}
	if !long && exemptValue(value, quoted, kind, authorization) {
		return exempt
	}
	if !quoted && !long {
		if next := nextSetting.FindStringIndex(value); next != nil {
			to = from + next[0]
		}
	}
	return spanBranch(text, from, to, quoted)
}

// blockLiteral is a literal that starts on the line after its opening quote:
// password = """, then the text, then """ (Python), or a backtick string.
func blockLiteral(text, rest string, end int, kind credentialKind, authorization bool) valueBranch {
	if rest == "" || strings.IndexByte("\"'`:=> \t]\\", rest[0]) < 0 {
		return notHandled
	}
	match, _ := findWindowed(blockLiteralStart, rest)
	if match == nil {
		return notHandled
	}
	quote := rest[match[2]:match[3]]
	from := end + match[1]
	to := literalEnd(text, from, quote, true)
	if exemptValue(text[from:to], true, kind, authorization) {
		return exempt
	}
	return spanBranch(text, from, to, true)
}

// elementText is <password>x</password>.
func elementText(text, rest string, nameStart, end int, kind credentialKind, authorization bool) valueBranch {
	if nameStart == 0 || text[nameStart-1] != '<' {
		return notHandled
	}
	if rest == "" || rest[0] != '>' {
		return notHandled
	}
	match := elementContent.FindStringSubmatchIndex(rest[:windowEnd(rest)])
	if match == nil {
		// An element whose end tag is not in sight (the next line of a stream):
		// one token is its text, when the text ends there.
		if len(rest) > valueWindow {
			return notHandled
		}
		if match = elementUnclosed.FindStringSubmatchIndex(rest); match == nil {
			return notHandled
		}
	}
	from, to := end+match[2], end+match[3]
	value := text[from:to]
	if isCredentialReference(value) || isLiteralKeyword(value) || opensContainer(value) || exemptValue(value, false, kind, authorization) {
		return exempt
	}
	return spanBranch(text, from, to, false)
}

// flagValue is a command-line flag's argument: --api-key value, --password
// "two words". The help text of a flag (--token string, --api-key API_KEY,
// --token <TOKEN>) and a redirection (--with-token < token.txt) hold none.
func flagValue(text, lower string, end int, rest string, kind credentialKind) valueBranch {
	if kind == basicAuthCredential || !isFlagName(lower, end) {
		return notHandled
	}
	match, long := matchValue(valueAfterFlag, rest)
	if match == nil {
		return notHandled
	}
	quote := rest[match[2]:match[3]]
	from, to := end+match[4], end+match[5]
	quoted := quote != ""
	if quoted {
		if emptyMultiQuote(text, from, quote, false) {
			return notHandled
		}
		to = literalEnd(text, from, quote, false)
		if isCommandSubstitution(quote, text[from:to]) {
			return exempt
		}
	}
	value := text[from:to]
	if !quoted && !long {
		switch {
		case isLiteralKeyword(value), isTypeName(value), isCredentialReference(value), opensContainer(value),
			strings.ContainsRune("<>|&", rune(value[0])), echoesFlag(value, lower[max(0, end-nameWindow):end]):
			return exempt
		}
	}
	if !long && exemptValue(value, quoted, kind, false) {
		return exempt
	}
	return spanBranch(text, from, to, quoted)
}

// netrcValue is .netrc's password value.
func netrcValue(text, lower string, start, end int, rest string) valueBranch {
	if !isNetrcPassword(lower, start, end) {
		return notHandled
	}
	// A .netrc password is short and ends its line.
	match, long := matchValue(valueAfterSpace, rest)
	if match == nil || long {
		return notHandled
	}
	return spanBranch(text, end+match[2], end+match[3], false)
}

// spanBranch masks text[start:end]. A value made only of closing punctuation
// is the end of a string or a call that happens to follow a name
// (input("Password: ")), not a secret; neither is a mask already there. A
// bare reference holds none; a quoted one is a literal like any other.
func spanBranch(text string, start, end int, quoted bool) valueBranch {
	value := text[start:end]
	if start >= end || value == "[REDACTED]" || strings.Trim(value, ")];>") == "" || (!quoted && isCredentialReference(value)) {
		return exempt
	}
	return valueBranch{span: maskSpan{start, end}, ok: true, handled: true}
}

// echoesName reports a bare value that is the name it is assigned to
// (this.token = token), which passes a variable along.
func echoesName(value, name string) bool {
	return strings.EqualFold(strings.TrimRight(value, ";)"), trailingName(name))
}

// echoesFlag reports a value that is the flag's own name in capitals, the
// meta-variable of a usage line: --api-key API_KEY, [--password PASSWORD].
func echoesFlag(value, flag string) bool {
	return squashName(asciiLower(strings.Trim(value, "[]<>.,;:()"))) == squashName(trailingName(flag))
}

// isLiteralKeyword reports a value that is a boolean or a null, in any case:
// automountServiceAccountToken: false, password: null.
func isLiteralKeyword(value string) bool {
	switch asciiLower(strings.TrimRight(value, ");,")) {
	case "null", "nil", "none", "undefined", "true", "false", "yes", "no", "on", "off", "~":
		return true
	}
	return false
}

// isTypeName reports a primitive type name, which follows a name in a type
// annotation or a usage line, with the & or * of a reference or pointer.
func isTypeName(value string) bool {
	switch asciiLower(strings.TrimLeft(strings.TrimRight(value, ";,)"), "&*")) {
	case "string", "str", "number", "int", "integer", "float", "double", "bool", "boolean",
		"bytes", "byte", "any", "unknown", "void", "object", "never", "secretstring":
		return true
	}
	return typeExpression.MatchString(value)
}

// typeExpression is a generic type: Optional[str], List[Secret], Option<String>.
// Its arguments are words, never digits, which a credential has.
var typeExpression = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*(?:\[[A-Za-z_][A-Za-z_.,\[\] |]*\]|<[A-Za-z_][A-Za-z_.,<> |&?]*>)\??;?$`)

// opensContainer reports a bare value that opens a JSON container, a flow
// collection or a template, whose contents are read on their own: nothing but
// brackets, or brackets and a key that ends in a colon. A value that merely
// starts with a bracket ({abc123) is a literal.
func opensContainer(value string) bool {
	if value == "" || (value[0] != '{' && value[0] != '[') {
		return false
	}
	rest := strings.TrimLeft(value, "{[")
	// A container opened in prose ends in a backtick or an ellipsis.
	return strings.Trim(rest, "]}`.") == "" || strings.HasSuffix(rest, ":")
}

// arrayRepeat is the start of an array of a repeated value: [0u8; 32].
var arrayRepeat = regexp.MustCompile(`^\[\w+;$`)

// wordsAfterBrace is what a {name} interpolation holds: a variable or a
// member path, not a credential, which has digits or symbols.
var wordsAfterBrace = regexp.MustCompile(`^[A-Za-z_][A-Za-z_.]*\[?$`)

// isInterpolation reports a value that opens with { or [ and is a variable's
// name that the text closes or quotes right after: {err}, {refreshToken},
// {credentials['key']}, in a format string or a Go value list.
func isInterpolation(value, after string) bool {
	return len(value) > 1 && value[0] == '{' && wordsAfterBrace.MatchString(value[1:]) &&
		after != "" && strings.IndexByte("}'\"", after[0]) >= 0
}

// isStringPrefix reports the letters that prefix a string literal in Python
// and Rust: f"...", rb'...', b"...".
func isStringPrefix(value string) bool {
	switch asciiLower(value) {
	case "f", "r", "b", "u", "fr", "rf", "br", "rb", "ur":
		return true
	}
	return false
}

// looksLikeListedCredential reports whether a bare value a plural name
// assigns looks like a credential: not a count, a list marker or a
// container whose members are read on their own.
func looksLikeListedCredential(value string) bool {
	return !isCount(strings.TrimLeft(value, "[{")) && value != "-" && !opensContainer(value)
}

// backtickCommand is a command and its arguments: a lowercase word, then a blank.
var backtickCommand = regexp.MustCompile(`^[a-z][a-z0-9_./-]*[ \t][^\n]*$`)

// isCommandSubstitution reports a backtick string that is a shell command
// (`cat token.txt`), which a literal on one word or on several lines is not.
func isCommandSubstitution(quote, value string) bool {
	return strings.TrimLeft(quote, `\`) == "`" && backtickCommand.MatchString(value) && !printsLiteral(value)
}

// templateStart is the literal text of a template string before its first
// ${...}: words, as in `Bearer ${token}`.
var templateStart = regexp.MustCompile("^[A-Za-z _:/.-]*\\$\\{")

// isWordLike reports a value that reads as a word of a sentence or a name:
// letters joined by _ . : / or -, with the punctuation of the sentence or the
// code span around it ignored, or no letters at all (#). A digit anywhere in
// it makes it no word.
func isWordLike(value string) bool {
	if strings.ContainsAny(value, "0123456789") {
		// A digit anywhere, past the first non-ASCII byte too.
		return false
	}
	if i := strings.IndexFunc(value, func(r rune) bool { return r >= 0x80 }); i >= 0 {
		// The punctuation or the words of another script end the value.
		value = value[:i]
	}
	core := strings.TrimRight(strings.TrimLeft(value, valuePrefixes+"`'\""), valueTrailers+"`'\".")
	return !hasAlphanumeric(core) || !secretShaped(core)
}

// wordsFollow reports a value followed on its line by another word: prose,
// where a bare value is a word of a sentence.
func wordsFollow(text string, to int) bool {
	i := to
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	return i > to && i < len(text) && ((text[i] >= 'a' && text[i] <= 'z') || (text[i] >= 'A' && text[i] <= 'Z'))
}

// isTemplateLiteral reports a backtick string that is a template with an
// interpolation after its words (`Bearer ${token}`).
func isTemplateLiteral(quote, value string) bool {
	return strings.TrimLeft(quote, `\`) == "`" && templateStart.MatchString(value)
}

// isComparison reports an operator that tests a value instead of giving one.
func isComparison(operator string) bool {
	switch operator {
	case "==", "===", "!=", "!==":
		return true
	}
	return false
}

func hasAlphanumeric(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return true
		}
	}
	return false
}

// emptyMultiQuote reports a quote that is no opening of a literal. A quote
// written twice opens one (”value”) only when a value starts right after it
// and the same quote closes it; "" is an empty string, and so is ” before text
// that is no value (--token=”: help). A tripled quote opens one whenever text
// follows it on its line, closed or not. A backtick opens one only before a
// letter or a digit, since it more often closes a code span in prose
// (`?token=`).
func emptyMultiQuote(text string, from int, quote string, lines bool) bool {
	core := strings.TrimLeft(quote, `\`)
	if strings.HasPrefix(core, "`") && (from >= len(text) || !isAlphanumeric(text[from])) {
		return true
	}
	if len(core) < 2 {
		return false
	}
	if len(core) >= 3 && core[0] != '`' {
		// A tripled quote is never prose: it opens a literal, closed far away or
		// not at all (""" X """, """X, ''' X); only the end of the line leaves
		// it without text after it.
		return from >= len(text) || text[from] == '\n' || text[from] == '\r'
	}
	if from >= len(text) || strings.IndexByte(")]};:,. /*\t\r\n", text[from]) >= 0 {
		return true
	}
	return closingMark(text, from, quote, lines) < 0
}

func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isSettingWord reports a word that sets a mode under a name such as
// credentials, which usually names a setting and not a credential: fetch's
// credentials: include, omit, same-origin; a CI step's persist-credentials.
func isSettingWord(value string) bool {
	switch asciiLower(strings.TrimSpace(value)) {
	case "include", "omit", "same-origin", "default", "none", "auto", "env", "inherit", "required", "optional",
		"enabled", "disabled", "manual", "file", "store", "helper", "keychain", "cache", "true", "false", "null", "yes", "no", "on", "off",
		"prod", "production", "dev", "development", "staging", "test", "ci", "local":
		return true
	}
	return false
}

func isCount(value string) bool {
	return value != "" && strings.Trim(value, "0123456789") == ""
}

// nameWindow bounds how far back from a name the text around it is read.
const nameWindow = 64

// isFlagName reports whether the name ending at end is a command-line flag:
// the word it ends starts with '-'.
func isFlagName(lower string, end int) bool {
	start := end
	for start > 0 && end-start < nameWindow && isIdentifierByte(lower[start-1]) {
		start--
	}
	return start < end && lower[start] == '-'
}

// isNetrcPassword reports .netrc's "password <value>": the word "password"
// after "machine" or "login" on its line, or alone at the start of a line
// that continues such an entry. A line that merely starts with the word
// (Password string, password reset) is not one.
func isNetrcPassword(lower string, start, end int) bool {
	if lower[start:end] != "password" || (start > 0 && isIdentifierByte(lower[start-1])) {
		return false
	}
	line := lineBefore(lower, start)
	trimmed := strings.TrimSpace(line)
	if trimmed != "" {
		return strings.HasPrefix(trimmed, "machine ") || strings.HasPrefix(trimmed, "default ") || strings.Contains(line, "login ")
	}
	return continuesNetrcEntry(lower, start-len(line))
}

// continuesNetrcEntry reports that the line before lineStart is a line of a
// .netrc entry: its first word is one the format starts a line with.
func continuesNetrcEntry(lower string, lineStart int) bool {
	window := strings.TrimRight(lower[max(0, lineStart-contextWindow):lineStart], " \t\r\n")
	if i := strings.LastIndexByte(window, '\n'); i >= 0 {
		window = window[i+1:]
	}
	fields := strings.Fields(window)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "machine", "login", "default", "account", "macdef":
		return true
	}
	return false
}

// lineBefore is the text of the line before offset, at most 160 bytes of it.
func lineBefore(text string, offset int) string {
	line := text[max(0, offset-160):offset]
	if i := strings.LastIndexByte(line, '\n'); i >= 0 {
		return line[i+1:]
	}
	return line
}
