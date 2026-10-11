package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// redactJSONDocument redacts a valid JSON document value by value, so the
// result is valid JSON (see rewriteJSON).
func redactJSONDocument(value string) (string, bool) {
	if !json.Valid([]byte(value)) {
		return "", false
	}
	out, _ := currentRedactor().rewriteJSON(value, true, 0)
	return out, true
}

// rewriteJSON redacts a valid JSON document token by token and keeps every
// other byte as written, so the result is valid JSON in the producer's own
// layout. Strings are matched decoded, so a registered secret or credential is
// found whatever escaping the producer used, and JSON inside a string is
// redacted as a document of its own. The scalars under a credential name are
// masked as valueMode says: every one under a singular name, the strings of a
// plural one; a key that is itself a secret is masked as text, and two keys
// that mask alike stay distinct.
//
// With every false, the document is one the text pass already masked, and
// only strings holding an escape are decoded again: in any other string the
// text pass saw exactly the decoded characters.
//
// It also returns where the rewritten tokens lie in document, inside their
// quotes, so a text mask can cover them as well.
func (r *redactor) rewriteJSON(document string, every bool, depth int) (string, []maskSpan) {
	w := &jsonRewriter{r: r, doc: document, every: every, depth: depth}
	w.space()
	w.value(ordinaryValue)
	if len(w.rewritten) == 0 {
		return document, nil
	}
	w.out.WriteString(document[w.copied:])
	return w.out.String(), w.rewritten
}

type jsonRewriter struct {
	r         *redactor
	doc       string
	pos       int
	copied    int
	out       strings.Builder
	every     bool
	depth     int
	rewritten []maskSpan
}

// value walks the value at pos, whose scalars mode says how to read.
func (w *jsonRewriter) value(mode credentialMode) {
	switch w.doc[w.pos] {
	case '{':
		w.object(containerMode(mode))
	case '[':
		members := containerMode(mode)
		w.pos++
		for w.space(); w.doc[w.pos] != ']'; w.space() {
			w.value(valueMode(members, "", true))
			if w.space(); w.doc[w.pos] == ',' {
				w.pos++
			}
		}
		w.pos++
	case '"':
		w.stringValue(mode)
	case 't', 'f', 'n':
		for w.pos < len(w.doc) && w.doc[w.pos] >= 'a' && w.doc[w.pos] <= 'z' {
			w.pos++
		}
	default:
		start := w.pos
		for w.pos < len(w.doc) && strings.IndexByte("+-0123456789.eE", w.doc[w.pos]) >= 0 {
			w.pos++
		}
		if mode.masksNumber() {
			w.replace(start, w.pos, `"[REDACTED]"`)
		}
	}
}

func (w *jsonRewriter) object(members credentialMode) {
	w.pos++
	// Masked names are tracked to keep them distinct; ordinary names cannot
	// collide with a mask.
	var masked map[string]bool
	for w.space(); w.doc[w.pos] != '}'; w.space() {
		start := w.pos
		raw := w.doc[start:w.skipString()]
		name := decodeJSONString(raw)
		shown := name
		if w.every || strings.IndexByte(raw, '\\') >= 0 {
			shown = w.r.nested(name, w.depth)
		}
		if strings.Contains(shown, "[REDACTED]") {
			if masked == nil {
				masked = map[string]bool{}
			}
			unique := shown
			for n := 2; masked[unique]; n++ {
				unique = fmt.Sprintf("%s#%d", shown, n)
			}
			masked[unique] = true
			shown = unique
		}
		if shown != name {
			w.replace(start, w.pos, encodeJSONString(shown))
		}
		w.space()
		w.pos++ // ':'
		w.space()
		w.value(valueMode(members, name, false))
		if w.space(); w.doc[w.pos] == ',' {
			w.pos++
		}
	}
	w.pos++
}

func (w *jsonRewriter) stringValue(mode credentialMode) {
	start := w.pos
	raw := w.doc[start:w.skipString()]
	if mode != ordinaryValue && mode.masksString(decodeJSONString(raw)) {
		w.replace(start, w.pos, `"[REDACTED]"`)
		return
	}
	if !w.every && strings.IndexByte(raw, '\\') < 0 {
		return
	}
	value := decodeJSONString(raw)
	if shown := w.r.nested(value, w.depth); shown != value {
		w.replace(start, w.pos, encodeJSONString(shown))
	}
}

// skipString moves past the string literal at pos and returns its end.
func (w *jsonRewriter) skipString() int {
	i := w.pos + 1
	for w.doc[i] != '"' {
		if w.doc[i] == '\\' {
			i++
		}
		i++
	}
	w.pos = i + 1
	return w.pos
}

func (w *jsonRewriter) space() {
	for w.pos < len(w.doc) && isJSONSpace(w.doc[w.pos]) {
		w.pos++
	}
}

func (w *jsonRewriter) replace(start, end int, text string) {
	w.out.WriteString(w.doc[w.copied:start])
	w.out.WriteString(text)
	w.copied = end
	if w.doc[start] == '"' {
		start, end = start+1, end-1
	}
	w.rewritten = append(w.rewritten, maskSpan{start, end})
}

func decodeJSONString(raw string) string {
	if strings.IndexByte(raw, '\\') < 0 {
		return raw[1 : len(raw)-1]
	}
	var decoded string
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return raw[1 : len(raw)-1]
	}
	return decoded
}

func encodeJSONString(value string) string {
	encoded, ok := encodeJSON(value)
	if !ok {
		return `"[REDACTED]"`
	}
	return encoded
}
