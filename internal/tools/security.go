package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"index/suffixarray"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"selfmind/internal/kernel"
)

// SecretRegistry holds exact runtime secret values that cannot be recognized
// reliably by shape alone (for example an opaque gateway token). It is shared
// by write-time and read-time redaction: callers register credentials when
// configuration or an execution environment is materialized, while every
// existing RedactSensitive call automatically gains exact-value masking.
//
// Values intentionally remain registered for the life of the process. That
// keeps historical output safe after credential rotation and avoids a window
// where an old value becomes visible again.
type SecretRegistry struct {
	mu     sync.RWMutex
	values map[string]secretRule
	// cached is the immutable view redaction reads; Register drops it.
	cached *secretSnapshot
}

type secretRule struct {
	identifierLike bool
	// forms are the secret as written and as JSON strings carry it; raw
	// marks the forms written as is.
	forms []secretForm
	// compact is a secret that is a JSON object or array without its
	// whitespace, matched whatever layout the output gives it.
	compact string
}

type secretForm struct {
	text string
	raw  bool
}

type registeredSecret struct {
	value string
	rule  secretRule
}

func NewSecretRegistry() *SecretRegistry {
	return &SecretRegistry{values: map[string]secretRule{}}
}

// Register adds an opaque secret value. Very short values are ignored because
// replacing common fragments such as "yes" or "1234" would destroy ordinary
// diagnostics without adding meaningful protection.
func (r *SecretRegistry) Register(value string) {
	if r == nil {
		return
	}
	value = strings.TrimSpace(value)
	if len(value) < 6 {
		return
	}
	r.mu.RLock()
	_, known := r.values[value]
	r.mu.RUnlock()
	if known {
		// A value's rule follows from the value alone, so registering it
		// again changes nothing and keeps the snapshot.
		return
	}
	rule := newSecretRule(value)
	r.mu.Lock()
	if _, known := r.values[value]; !known {
		r.values[value] = rule
		r.cached = nil
	}
	r.mu.Unlock()
}

func newSecretRule(value string) secretRule {
	rule := secretRule{identifierLike: isIdentifierLikeSecret(value)}
	// A multi-line secret is known with either line ending: output read
	// line by line loses the carriage return of a CRLF line ending, and a
	// file read whole keeps whichever ending it was written with.
	variants := []string{value}
	if strings.Contains(value, "\n") {
		lf := strings.ReplaceAll(value, "\r\n", "\n")
		for _, variant := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
			if !slices.Contains(variants, variant) {
				variants = append(variants, variant)
			}
		}
	}
	for _, variant := range variants {
		for _, form := range jsonEscapedForms(variant) {
			if !slices.ContainsFunc(rule.forms, func(known secretForm) bool { return known.text == form }) {
				rule.forms = append(rule.forms, secretForm{text: form, raw: form == variant})
			}
		}
	}
	if strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[") {
		if decoded, ok := decodeJSON(value); ok {
			switch decoded.(type) {
			case map[string]interface{}, []interface{}:
				rule.compact = withoutWhitespace(value)
			}
		}
	}
	return rule
}

func (r *SecretRegistry) Redact(value string) string {
	if r == nil || value == "" {
		return value
	}
	spans, _, _ := r.snapshot().spans(value)
	return applyMasks(value, spans)
}

// snapshot returns the registered secrets as one immutable view, built once
// per change rather than once per redaction.
func (r *SecretRegistry) snapshot() *secretSnapshot {
	if r == nil {
		return &secretSnapshot{}
	}
	r.mu.RLock()
	cached := r.cached
	r.mu.RUnlock()
	if cached != nil {
		return cached
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached == nil {
		r.cached = newSecretSnapshot(r.values)
	}
	return r.cached
}

// jsonEscapedForms are a secret as written and in the JSON string bodies the
// registry knows: encoding/json's with and without HTML escaping, the same
// with every control character as \u00xx, and each of those with uppercase
// hex digits. Longer forms come first, so a form inside another cannot leave
// part of that one behind.
func jsonEscapedForms(value string) []string {
	forms := []string{value}
	for _, form := range []string{jsonHTMLStringBody(value), jsonStringBody(value), controlEscaped(value)} {
		for _, variant := range []string{form, upperHexEscapes(form)} {
			if variant != "" && !slices.Contains(forms, variant) {
				forms = append(forms, variant)
			}
		}
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

// upperHexEscapes writes the \uXXXX escapes of a JSON string body with
// uppercase hex digits, as some encoders do.
func upperHexEscapes(form string) string {
	out := []byte(form)
	for i := 0; i+1 < len(out); i++ {
		if out[i] != '\\' {
			continue
		}
		if out[i+1] == 'u' && i+6 <= len(out) {
			copy(out[i+2:i+6], bytes.ToUpper(out[i+2:i+6]))
			i += 5
			continue
		}
		i++ // the escaped character
	}
	return string(out)
}

// jsonHTMLStringBody is value as encoding/json writes it by default, with <,
// > and & escaped, without the quotes.
func jsonHTMLStringBody(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(encoded), `"`), `"`)
}

// controlEscaped is value as a JSON string body that writes every control
// character as \u00XX instead of \n or \t.
func controlEscaped(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch {
		case r < 0x20:
			fmt.Fprintf(&out, `\u%04x`, r)
		case r == '"' || r == '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func isIdentifierLikeSecret(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// escapeBefore reports whether text ends with a JSON escape sequence whose
// last character is a letter or digit, such as the \n before a token at the
// start of an encoded line. The escape stands for another character, so it is
// a word boundary; a \uXXXX escape counts too, which can only mask more.
func escapeBefore(text string) bool {
	n, start := len(text), -1
	switch {
	case n >= 2 && text[n-2] == '\\' && strings.IndexByte("bfnrt", text[n-1]) >= 0:
		start = n - 2
	case n >= 6 && text[n-6] == '\\' && text[n-5] == 'u' && strings.Trim(text[n-4:], "0123456789abcdefABCDEF") == "":
		start = n - 6
	default:
		return false
	}
	// The backslash starts an escape only when it is not itself escaped.
	backslashes := 0
	for i := start; i >= 0 && text[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func isIdentifierByte(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9') || value == '_' || value == '-'
}

var defaultSecretRegistry = NewSecretRegistry()

// RegisterSensitiveValue makes an exact credential value available to the
// process-wide redactor. The value itself is never persisted by this API.
func RegisterSensitiveValue(value string) {
	defaultSecretRegistry.Register(value)
}

func RedactSensitive(value string) string {
	out, _ := redactSensitiveReport(value)
	return out
}

// redactSensitiveReport also reports when masking had to win over JSON
// validity. A JSON tool result must stay JSON: the plan gate, the model and
// every formatter parse it. Text patterns cannot see its structure, so when
// they break a valid document it is redacted value by value instead, unless
// that form would reveal anything the text pass masked.
func redactSensitiveReport(value string) (string, bool) {
	return currentRedactor().report(value, 0)
}

// redactSensitiveText masks credentials in text and reports every value it
// masked.
func redactSensitiveText(value string) (string, maskedValues) {
	return currentRedactor().text(value)
}

// redactor applies one snapshot of the registered secrets to a value and to
// everything nested in it.
type redactor struct {
	secrets *secretSnapshot
}

func currentRedactor() *redactor {
	return &redactor{secrets: defaultSecretRegistry.snapshot()}
}

// maxNestedJSON bounds how many levels of JSON inside JSON strings are
// decoded; deeper strings get the text pass only.
const maxNestedJSON = 8

// report redacts value and says whether the result had to stay unparseable.
// The text pass masks registered secrets and credentials on the text as
// written. A valid JSON document is then also matched string by string on
// its decoded values and keys, so any escaping its producer chose is covered:
// a document the text pass left valid is rewritten in place where a decoded
// string holds more, and one it broke is masked value by value (see
// redactBrokenJSON).
//
// When the text mask is kept, it also covers every string whose decoded value
// the value-by-value form masks, so that form's matches are never lost.
func (r *redactor) report(value string, depth int) (string, bool) {
	if strings.TrimSpace(value) == "" {
		return value, false
	}
	spans, masked := r.spans(value)
	out := applyMasks(value, spans)
	start, end, ok := jsonDocumentBounds(value)
	if !ok {
		return out, false
	}
	// The whitespace and byte order mark around a document are kept as
	// they are; only the document between them is walked.
	prefix, suffix := value[:start], value[end:]
	if !strings.HasPrefix(out, prefix) || !strings.HasSuffix(out[len(prefix):], suffix) {
		return out, false
	}
	document, body := value[start:end], out[len(prefix):len(out)-len(suffix)]
	if json.Valid([]byte(body)) {
		rewritten, _ := r.rewriteJSON(body, false, depth)
		return prefix + rewritten + suffix, false
	}
	var decoded []maskSpan
	structured, unparseable := redactBrokenJSON(document, body, masked, func(document string) (string, bool) {
		var rewritten string
		rewritten, decoded = r.rewriteJSON(document, true, depth)
		return rewritten, true
	})
	if !unparseable {
		return prefix + structured + suffix, false
	}
	if decoded == nil {
		_, decoded = r.rewriteJSON(document, true, depth)
	}
	for _, span := range decoded {
		spans = append(spans, maskSpan{span.start + start, span.end + start})
	}
	return applyMasks(value, spans), true
}

// text masks the union of every registered secret and credential found on
// the original text.
func (r *redactor) text(value string) (string, maskedValues) {
	spans, masked := r.spans(value)
	return applyMasks(value, spans), masked
}

func (r *redactor) spans(value string) ([]maskSpan, maskedValues) {
	return r.detect(value, true)
}

// detect finds what to mask in value. Registered secrets are looked for in
// the text as written and as a reader sees it (see detectionView), so color
// codes or invisible characters inside one do not hide it. containers says
// whether to follow credential containers from the start of value; a stream
// follows them across its lines itself.
func (r *redactor) detect(value string, containers bool) ([]maskSpan, maskedValues) {
	view := detectionView(value)
	spans, raw, escaped := r.secrets.spans(value)
	if view != nil {
		viewSpans, viewRaw, viewEscaped := r.secrets.spans(view.text)
		for _, span := range viewSpans {
			spans = append(spans, view.original(span))
		}
		raw = append(raw, viewRaw...)
		for _, secret := range viewEscaped {
			if !slices.Contains(raw, secret) && !slices.Contains(escaped, secret) {
				escaped = append(escaped, secret)
			}
		}
		escaped = slices.DeleteFunc(escaped, func(secret string) bool { return slices.Contains(raw, secret) })
	}
	masked := maskedValues{registered: raw, escaped: escaped}
	spans = append(spans, credentialSpans(value, view, &masked.matched)...)
	if containers {
		// The scalars of a credential container are masked by the
		// value-by-value form of a document under the same rules (valueMode),
		// and are looked for in it like any other masked value.
		detected := value
		if view != nil {
			detected = view.text
		}
		for _, span := range containerCredentialSpans(detected) {
			if view != nil {
				span = view.original(span)
			}
			masked.matched = append(masked.matched, value[span.start:span.end])
			spans = append(spans, span)
		}
	}
	return spans, masked
}

// nested redacts a decoded JSON string.
func (r *redactor) nested(value string, depth int) string {
	if depth >= maxNestedJSON {
		out, _ := r.text(value)
		return out
	}
	out, _ := r.report(value, depth+1)
	return out
}

// redactBrokenJSON chooses between the value-by-value form of a document and
// the text-masked form the text pass made invalid. Security wins over
// validity: a value-by-value form that reveals any masked value is never used.
//
// A registered secret holding JSON structure or whitespace may have matched
// across tokens, and the value-by-value form can split it into leaves that a
// pattern masks only in part, so a document its raw form matched in is not
// redacted value by value at all. An escaped form holds escapes, which only
// a JSON string can, so its match stays inside one string and does not count.
func redactBrokenJSON(document, textMasked string, masked maskedValues, valueByValue func(string) (string, bool)) (string, bool) {
	for _, secret := range masked.registered {
		if strings.ContainsAny(secret, jsonStructure) || strings.IndexFunc(secret, unicode.IsSpace) >= 0 {
			return textMasked, true
		}
	}
	if structured, ok := valueByValue(document); ok && !revealsMasked(structured, masked) {
		return structured, false
	}
	return textMasked, true
}

// jsonStructure holds the characters a match needs to span JSON tokens.
const jsonStructure = `[]{}":,`

// revealsMasked reports whether document still shows anything the text pass
// masked, as written or as a JSON string writes it.
//
// A registered secret is exact and always looked for. One holding a JSON
// structure character may have matched across tokens, which the
// value-by-value form re-encodes: a secret that is JSON is looked for by its
// compact re-encoding and every key, string and number as well, and any other
// keeps the text mask. redactBrokenJSON keeps the text mask for such a secret
// before asking; this stays as a second net.
//
// A secret that matched only in an escaped form lay inside one JSON string,
// so it is looked for as written and as a JSON string writes it.
//
// A matched value is a run after a credential's name. A run of nothing but
// JSON brackets, literals and masks opens a container under that name, whose
// strings and numbers the value-by-value form masks, so it hides nothing.
func revealsMasked(document string, masked maskedValues) bool {
	needles := map[string]bool{}
	look := func(value string) {
		for _, form := range []string{value, jsonStringBody(value)} {
			if form != "" {
				needles[form] = true
			}
		}
	}
	for _, secret := range masked.registered {
		look(secret)
		if !strings.ContainsAny(secret, jsonStructure) {
			continue
		}
		decoded, ok := decodeJSON(secret)
		if !ok {
			return true
		}
		if compact, ok := encodeJSON(decoded); ok {
			look(compact)
		}
		jsonParts(decoded, look)
	}
	for _, secret := range masked.escaped {
		look(secret)
	}
	for _, value := range masked.matched {
		if containerOpening(value) {
			continue
		}
		look(value)
		// Inside a JSON string the run is escaped; the value-by-value form
		// writes it decoded and re-encoded.
		var decoded string
		if json.Unmarshal([]byte(`"`+value+`"`), &decoded) == nil && decoded != value {
			look(decoded)
		}
	}
	return showsAny(document, needles)
}

// containerOpening reports whether a matched run is only JSON brackets,
// literals and masks.
func containerOpening(value string) bool {
	switch strings.Trim(strings.ReplaceAll(value, "[REDACTED]", ""), "[]{}") {
	case "", "null", "true", "false":
		return true
	}
	return false
}

// jsonParts visits every key, string and number in a decoded JSON value.
func jsonParts(value interface{}, visit func(string)) {
	switch typed := value.(type) {
	case string:
		visit(typed)
	case json.Number:
		visit(typed.String())
	case []interface{}:
		for _, item := range typed {
			jsonParts(item, visit)
		}
	case map[string]interface{}:
		for key, item := range typed {
			visit(key)
			jsonParts(item, visit)
		}
	}
}

// showsAny reports whether document contains any needle. The document is
// indexed once, so the cost follows the document and the needles' combined
// length rather than their product.
func showsAny(document string, needles map[string]bool) bool {
	if len(needles) == 0 {
		return false
	}
	index := suffixarray.New([]byte(document))
	for needle := range needles {
		if len(index.Lookup([]byte(needle), 1)) > 0 {
			return true
		}
	}
	return false
}

// jsonStringBody is value as a JSON string writes it, without the quotes.
func jsonStringBody(value string) string {
	encoded, ok := encodeJSON(value)
	if !ok {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(encoded, `"`), `"`)
}

// decodeJSON decodes exactly one JSON value, keeping numbers as written.
func decodeJSON(value string) (interface{}, bool) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var decoded interface{}
	if decoder.Decode(&decoded) != nil {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return decoded, true
}

// encodeJSON writes a value the way the value-by-value form does: compact,
// keys sorted, HTML characters as written.
func encodeJSON(value interface{}) (string, bool) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return "", false
	}
	return strings.TrimSuffix(out.String(), "\n"), true
}

// maskedValues are what the text pass masked: exact registered secrets whose
// raw form matched, those that matched only in a JSON-escaped form, and the
// values credential patterns matched.
type maskedValues struct {
	registered []string
	escaped    []string
	matched    []string
}

// jsonDocumentBounds finds a JSON object or array that is all of value but
// for whitespace around it and a leading byte order mark. Whitespace JSON
// does not allow (a form feed, a no-break space) still surrounds a document a
// reader would call JSON, so its strings are matched decoded all the same.
func jsonDocumentBounds(value string) (int, int, bool) {
	start := 0
	for start < len(value) {
		r, size := utf8.DecodeRuneInString(value[start:])
		if !unicode.IsSpace(r) && r != 0xFEFF {
			break
		}
		start += size
	}
	end := len(value)
	for end > start {
		r, size := utf8.DecodeLastRuneInString(value[:end])
		if !unicode.IsSpace(r) {
			break
		}
		end -= size
	}
	body := value[start:end]
	return start, end, body != "" && (body[0] == '{' || body[0] == '[') && json.Valid([]byte(body))
}

// RedactionMiddleware is the single tool-result exit boundary. The redacted
// value is what the model, event preview, artifact spool, and downstream
// formatters receive. A result that masking had to leave unparseable says so.
func RedactionMiddleware() ResultMiddleware {
	return func(next ResultExecutor) ResultExecutor {
		return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
			result, err := next(args)
			result.Output, result.RedactionUnparseable = redactSensitiveReport(result.Output)
			return result, redactToolFailure(err)
		}
	}
}
