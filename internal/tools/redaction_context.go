package tools

import (
	"regexp"
	"strings"
)

// Code or config? A credential's name that is assigned a bare (unquoted)
// value may be code naming a credential without holding one (token := tok,
// apiKey: someVar,) or a config line whose value is a literal (PASSWORD=ab_cd,
// password: correct_horse). The text of the line decides, by positive evidence
// only: a bare value is read as code when the line shows it is code, and as a
// literal otherwise. A quoted value never comes here; it is always a literal.
//
// Value shapes (after a leading ( ! * & - + ~):
//
//	call     path( ...        index   path[ ...       member  words.words
//	ident    any identifier   variable camelCase or snake_case of letters, or
//	the name itself
//
// Evidence on the line that holds the name:
//
//	strong     a declaration keyword or type before the name; := with a name
//	           that is not ALL_CAPS
//	statement  x.name = (member access, the = with whitespace around it); ==
//	           or != with whitespace before it; a value that ends its
//	           statement with ;; a name that closes a quote and ends its line
//	           with : (case "x_token":), or a line that starts if, while,
//	           def, ...
//	medium     an unclosed ( [ { before the name on its line, or a previous
//	           line ending with one or with a comma (call arguments, an object
//	           or struct literal); a value followed by ) } or a trailing comma
//
// Evidence is about the line, never about the value: a line can be written to
// look like code (a trailing ;, a declaration word, x. before the name, :=, a
// label with the value below), and a credential looks the same on it. A value
// with a digit among its letters, a symbol, or a dotted path with digits is a
// literal whatever the line says, unless it is a call or an index. Strong
// evidence is a construct only code has (and a .properties line does not), so
// it exempts any identifier without a digit. Statement evidence can be shown
// by a line of configuration (db.password = secret), so it exempts a call, an
// index, a dotted path of letters, a camelCase or snake_case variable and a
// statement keyword, and not a single word. Medium evidence exempts the same
// and the name itself. With no evidence a call, index or letters-only member
// path is exempt on a spaced = (Python, Go, HCL), never on an = touching the
// name (dotenv, shell, --flag=, ?query=) and never on the : of a name: value
// line (YAML, HTTP headers). Anything else is a literal.
type assignment struct {
	operator string
	// spaced: whitespace before the operator; keyQuote: a quote closes the
	// name; gap: what lies between the operator and the value.
	spaced, keyQuote bool
	gap              string
}

// touching reports an = (or ==) that touches the name: dotenv, shell, a flag
// or a query string, where a bare value is a literal.
func (a assignment) touching() bool {
	return !a.spaced && (a.operator == "=" || a.operator == "==" || a.operator == "===" || a.operator == "!=" || a.operator == "!==")
}

var (
	// callHead is a path followed by (, and callArguments are what a call's
	// arguments are made of: words, dots, commas, = : / - and brackets. A
	// password has other symbols (! @ # % ^ + ~ ; ...).
	callHead      = regexp.MustCompile(`^[A-Za-z_$][\w$]*(?:(?:\.|::|->)[A-Za-z_$][\w$]*)*\(`)
	callArguments = regexp.MustCompile(`^[\w$.,=()\[\]{}:/*&|<>?-]*$`)
	callIdentity  = regexp.MustCompile(`[A-Za-z_$][\w$]*`)
	// indexShape is a path followed by [ and a simple index.
	indexShape = regexp.MustCompile(`^[A-Za-z_$][\w$]{0,31}(?:\??\.[A-Za-z_$][\w$]{0,31})*\??\.?\[(?:[\w$.:-]{0,63}\];?)?$`)
	// wordPath is member access written in words alone: no digits, so a dotted
	// token (a Discord or legacy Vault token, a kubeadm bootstrap token) is
	// not one.
	wordPath  = regexp.MustCompile(`^[A-Za-z_$]{1,32}(?:\.[A-Za-z_$]{1,32})+;?$`)
	identPath = regexp.MustCompile(`^[A-Za-z_$][\w$]*;?$`)
	// controlLine begins a line that opens a block: if not token: ...
	controlLine = regexp.MustCompile(`^\s*(?:if|elif|else|while|for|with|def|class|case|switch|except|match|when|default)\b`)
	// startsAsCode is the beginning of an expression: an identifier.
	startsAsCode = regexp.MustCompile(`^[A-Za-z_$]`)
	declarations = map[string]bool{"const": true, "let": true, "var": true, "val": true, "final": true, "static": true, "readonly": true, "public": true, "private": true, "protected": true, "internal": true, "string": true, "int": true, "long": true, "bool": true, "boolean": true, "char": true, "float": true, "double": true, "auto": true}
)

const (
	valueTrailers = ";,)]}:?"
	valuePrefixes = "(!*&-+~"
	// contextWindow bounds how far back from a name its line is read.
	contextWindow = 512
	// lineWindow bounds how much of its own line is read before a name.
	lineWindow = 256
)

// isCallShape reports a call as code writes it: a path, then ( and arguments of
// words and punctuation of code. The call is closed (f(a, g(b))) with nothing
// after its parenthesis but ; or ?, or it goes on: a token ends at a blank, a
// comma or a quote, so an unclosed call is code when the line goes on after
// the token with more of it (f(, f(a, "x"), str(g(a, b)) and a word with a
// parenthesis when the line ends there (Sum(mer, abc(def;). A number of three
// digits or more among the arguments (Hunter(2024)) is not code's.
func isCallShape(core, after string) bool {
	head := callHead.FindString(core)
	if head == "" {
		return false
	}
	rest := core[len(head):]
	trimmed := strings.TrimRight(rest, ";?")
	if !callArguments.MatchString(trimmed) || longNumber.MatchString(callIdentity.ReplaceAllString(trimmed, "")) || numberInWord.MatchString(trimmed) {
		return false
	}
	depth := 1
	for i := 0; i < len(trimmed); i++ {
		switch trimmed[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 && i != len(trimmed)-1 {
				return false
			}
		}
	}
	if depth == 0 {
		return true
	}
	return rest == trimmed && (trimmed == "" || strings.HasSuffix(trimmed, ",") || strings.HasSuffix(trimmed, "(") || callContinues(after))
}

var (
	// longNumber is a number of three digits or more.
	longNumber = regexp.MustCompile(`[0-9]{3,}`)
	// numberInWord is a digit before a letter: 2mQz is no number and no name.
	numberInWord = regexp.MustCompile(`[0-9][A-Za-z_$]`)
)

// callContinues reports text after a token that goes on with a call's
// arguments: a comma, a quote, a closing bracket or an operator, and not the
// end of the line, a word or a statement's end.
func callContinues(after string) bool {
	after = strings.TrimLeft(after, " \t")
	return after != "" && strings.IndexByte(",\"'`)]}+.|&?:=<>*/-%!", after[0]) >= 0
}

// lineAfter is the text after to on its line, at most 64 bytes of it.
func lineAfter(text string, to int) string {
	after := text[to:min(len(text), to+64)]
	if i := strings.IndexByte(after, '\n'); i >= 0 {
		after = after[:i]
	}
	return after
}

type evidenceLevel uint8

const (
	noEvidence evidenceLevel = iota
	mediumEvidence
	// statementEvidence is a line that ends its statement, compares, or is a
	// block label: a line of configuration can be written the same way.
	statementEvidence
	// strongEvidence is a construct only code has: a declaration, :=, member
	// access with a spaced =.
	strongEvidence
)

func readsAsCode(text string, start, end int, site assignment, from, to int) bool {
	value := text[from:to]
	// Name::path (Rust, C++, PHP): the operator took the first colon.
	if site.operator == ":" && site.gap == "" && strings.HasPrefix(value, ":") {
		return true
	}
	if value == "(" {
		return true
	}
	core := strings.TrimLeft(value, valuePrefixes)
	bare := strings.TrimRight(core, valueTrailers)
	touching := site.touching()
	yamlStyle := site.operator == ":"
	level := codeEvidence(text, start, end, site, from, to)
	call := isCallShape(core, lineAfter(text, to))
	switch {
	case level == strongEvidence && startsAsCode.MatchString(core) && plausibleCode(core, bare, call):
		return true
	case level == statementEvidence && startsAsCode.MatchString(core) && statementCode(core, bare, call):
		return true
	case level >= mediumEvidence && !touching && (strings.HasPrefix(value, "(") || call || wordPath.MatchString(bare)):
		return true
	case indexShape.MatchString(core):
		return level >= mediumEvidence || (!touching && !yamlStyle)
	case call, wordPath.MatchString(bare):
		return level >= mediumEvidence || (!touching && !yamlStyle)
	case identPath.MatchString(bare):
		return (level == mediumEvidence || level == statementEvidence) && (variableName.MatchString(bare) || echoesName(bare, asciiLower(text[max(0, start-nameWindow):end])))
	}
	return false
}

// strongMaxWord is the longest word, without any mark of a variable, that
// strong evidence still reads as one (tok, pw, hunter, changeme).
const strongMaxWord = 8

// codeSymbols are the symbols a credential has and an identifier does not.
const codeSymbols = "!@#%^&+~|\\<>=/-$()[]{}\"'`"

// pathSymbols are what an identifier may have that looks like a symbol: the
// arrow of a member access, the scope operator and optional chaining.
var pathSymbols = strings.NewReplacer("->", "", "::", "", "?.", "")

// plausibleCode narrows what strong evidence exempts to what code is made of: a
// call whose arguments are words and punctuation of code, an index, or an
// identifier that is made like a variable: no digit and no symbol, and
// camelCase or snake_case, a dotted path of letters, a :: path, a statement
// keyword, or a word of up to strongMaxWord letters (tok, pw, hunter). A
// value with a digit and a letter (Xk9ab12Cd, hunter2), a symbol (Summer!) or
// a long single word (correcthorsebatterystaple) is how a credential looks,
// whatever the line around it says; a line spoofed with a trailing ;, a
// declaration word or x. before the name must not change that.
func plausibleCode(core, bare string, call bool) bool {
	if call || indexShape.MatchString(core) {
		return true
	}
	if strings.ContainsAny(bare, "0123456789") || strings.ContainsAny(pathSymbols.Replace(strings.TrimRight(bare, "*")), codeSymbols) {
		return false
	}
	return variableName.MatchString(bare) || wordPath.MatchString(bare) || strings.Contains(bare, "::") || strings.Contains(bare, "?.") ||
		statementKeywords[asciiLower(bare)] || len(bare) <= strongMaxWord
}

// statementKeywords are the words that begin the statement a block label's
// value line holds, or end a comparison: if not token: return None.
var statementKeywords = map[string]bool{
	"return": true, "raise": true, "continue": true, "break": true, "pass": true, "if": true, "else": true, "elif": true,
	"from": true, "import": true, "yield": true, "await": true, "throw": true, "and": true, "or": true, "not": true,
	"is": true, "in": true, "then": true, "do": true, "end": true, "fi": true, "null": true, "nil": true, "none": true,
	"undefined": true, "true": true, "false": true, "self": true, "this": true, "unsafe": true, "match": true, "while": true,
}

// statementCode is what a line that ends its statement, compares or labels a
// block may give a name: a call, an index, a path or a variable of several
// words, a keyword. A single word (hunter) is as likely a literal on such a
// line, which a file of configuration can be written as.
func statementCode(core, bare string, call bool) bool {
	if call || indexShape.MatchString(core) {
		return true
	}
	if strings.ContainsAny(bare, "0123456789") {
		return false
	}
	return wordPath.MatchString(bare) || variableName.MatchString(bare) || statementKeywords[asciiLower(bare)]
}

func codeEvidence(text string, start, end int, site assignment, from, to int) evidenceLevel {
	nameStart := start
	for nameStart > 0 && start-nameStart < contextWindow && isIdentifierByte(text[nameStart-1]) {
		nameStart--
	}
	windowStart := max(0, nameStart-lineWindow)
	lineStart := windowStart + strings.LastIndexByte(text[windowStart:nameStart], '\n') + 1
	prefix := text[lineStart:nameStart]
	touching := site.touching()
	switch {
	case declaredBy(prefix):
		return strongEvidence
	case site.operator == ":=" && !touching && !isAllCaps(text[nameStart:end]):
		return strongEvidence
	case !touching && (site.operator == "==" || site.operator == "===" || site.operator == "!=" || site.operator == "!=="):
		return statementEvidence
	case !touching && site.spaced && site.operator == "=" && memberAccessBefore(text, nameStart):
		return statementEvidence
	case !touching && endsStatement(text, from, to) == ';':
		return statementEvidence
	case (insideBrackets(prefix) || previousLineOpens(text, lineStart)) && !(touching && isAllCaps(text[nameStart:end])):
		// NAME=value with an ALL_CAPS name is an environment assignment (in a
		// subshell, a group or an argument list), never a keyword argument.
		return mediumEvidence
	case site.operator == ":" && strings.Contains(site.gap, "\n") && (site.keyQuote || controlLine.MatchString(text[lineStart:end])):
		// A name that ends a line with : and whose value is on the next line is
		// a case label or a block opener, not a YAML key.
		return statementEvidence
	case !touching && endsStatement(text, from, to) != 0:
		return mediumEvidence
	}
	return noEvidence
}

func isAllCaps(name string) bool {
	letters := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			return false
		}
		letters = letters || (c >= 'A' && c <= 'Z')
	}
	return letters
}

// memberAccessBefore reports a name that ends a member access: x.name, x().name
// or x->name.
func memberAccessBefore(text string, nameStart int) bool {
	if nameStart >= 2 && text[nameStart-1] == '.' {
		c := text[nameStart-2]
		return isIdentifierByte(c) || c == ')' || c == ']'
	}
	return nameStart >= 3 && text[nameStart-2:nameStart] == "->" && isIdentifierByte(text[nameStart-3])
}

// declaredBy reports a declaration keyword or type as the word before a name.
func declaredBy(prefix string) bool {
	end := len(prefix)
	for end > 0 && (prefix[end-1] == ' ' || prefix[end-1] == '\t') {
		end--
	}
	if end == len(prefix) {
		// No blank between the word and the name: it is part of the name.
		return false
	}
	begin := end
	for begin > 0 && (isIdentifierByte(prefix[begin-1]) || prefix[begin-1] == '*' || prefix[begin-1] == '&') && end-begin < 16 {
		begin--
	}
	return declarations[asciiLower(strings.TrimRight(prefix[begin:end], "*&"))]
}

// insideBrackets reports an unclosed ( [ or { in the text before a name.
func insideBrackets(prefix string) bool {
	// Only the last 256 bytes are read: a bracket opened further back on a
	// line is no evidence for the name that far from it.
	if len(prefix) > 256 {
		prefix = prefix[len(prefix)-256:]
	}
	depth := 0
	var quote byte
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case (c == ')' || c == ']' || c == '}') && depth > 0:
			depth--
		}
	}
	return depth > 0
}

// previousLineOpens reports that the line before this one ends with an opening
// bracket or a comma, so this line continues a call or literal. YAML and INI
// lines end with neither.
func previousLineOpens(text string, lineStart int) bool {
	window := text[max(0, lineStart-contextWindow):lineStart]
	for end := len(window) - 1; end > 0; {
		begin := strings.LastIndexByte(window[:end], '\n') + 1
		line := strings.TrimRight(window[begin:end], " \t\r")
		if line == "" {
			end = begin - 1
			continue
		}
		switch line[len(line)-1] {
		case '{', '(', '[', ',':
			return true
		}
		return false
	}
	return false
}

// endsStatement reports how a value ends: ';' for a statement, ')' for a
// closed call or list, 0 when nothing of the kind follows it.
func endsStatement(text string, from, to int) byte {
	if strings.HasSuffix(text[from:to], ";") {
		return ';'
	}
	limit := min(len(text), to+contextWindow)
	lineEnd := limit
	if i := strings.IndexByte(text[to:limit], '\n'); i >= 0 {
		lineEnd = to + i
	}
	after := strings.TrimSpace(text[to:lineEnd])
	if after == "" {
		return 0
	}
	switch after[0] {
	case ';':
		return ';'
	case '}', ')':
		return ')'
	case ',':
		if strings.TrimSpace(after[1:]) == "" {
			return ')'
		}
	}
	return 0
}
