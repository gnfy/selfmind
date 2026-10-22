package tools

import (
	"strings"
	"unicode"
)

// credentialMode is how the scalars at a position of a JSON value are read.
// The value-by-value walk of a document and the container tracker of text
// and streams share it, so a stream masks what the result masks.
type credentialMode uint8

const (
	ordinaryValue credentialMode = iota
	// everyScalar is under a singular credential name: every string and
	// number is a credential, at any depth.
	everyScalar
	// credentialStrings is the value of a plural credential name: its
	// strings are credentials, except members under a reference-like key
	// (name, ref, id, ...) of an object it opens.
	credentialStrings
	// listedStrings is an element or member of a credentialStrings
	// container: a string is a credential, a container holds references.
	listedStrings
	// basicAuthString is docker's and npm's auth: base64 of user:password.
	basicAuthString
)

// valueMode is the mode of a value from the mode of its container and the
// key it is under; an array element has no key.
func valueMode(container credentialMode, key string, inArray bool) credentialMode {
	if container == everyScalar {
		return everyScalar
	}
	name := ""
	if !inArray {
		name = credentialNameText(key)
		if holdsCredentialStem(name) {
			switch credentialNameKind(name) {
			case singularCredential, weakCredential:
				return everyScalar
			case pluralCredential:
				return credentialStrings
			case basicAuthCredential:
				return basicAuthString
			}
		}
	}
	if container == credentialStrings && (inArray || !isReferenceName(name)) {
		return listedStrings
	}
	return ordinaryValue
}

// holdsCredentialStem reports whether lowercase text holds a word a
// credential name is made of, which most keys of a document do not.
func holdsCredentialStem(lower string) bool {
	for _, stem := range credentialStems {
		if strings.Contains(lower, stem) {
			return true
		}
	}
	return false
}

// containerMode is the mode of the members of a container at mode. The
// members of an object listed under a plural name read as that name's
// container does: its strings are credentials unless a key says they are
// references.
func containerMode(mode credentialMode) credentialMode {
	switch mode {
	case everyScalar, credentialStrings:
		return mode
	case listedStrings:
		return credentialStrings
	}
	return ordinaryValue
}

// masksString reports whether a decoded string at this mode is masked.
func (m credentialMode) masksString(value string) bool {
	switch m {
	case ordinaryValue:
		return false
	case basicAuthString:
		return isBasicCredentials(value)
	}
	// A string is a literal; it is a reference only when it is one whole:
	// "${TOKEN}" is, "${TOKEN}x9" is not.
	return value != "" && value != "[REDACTED]" && !strings.EqualFold(value, "bearer") && !isWholeReference(value)
}

func (m credentialMode) masksNumber() bool {
	return m == everyScalar
}

// credentialNameText is a decoded key as a reader sees it, lowercase.
func credentialNameText(name string) string {
	if hasFormatCharacter(name) {
		name = strings.Map(func(r rune) rune {
			if r >= 0x80 && unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, name)
	}
	return asciiLower(name)
}

// referenceNameEndings name keys whose values point at a credential instead
// of holding one, such as a Kubernetes secret reference's name and key.
var referenceNameEndings = []string{"name", "names", "ref", "refs", "id", "ids", "kind", "type", "namespace", "namespaces", "path", "file", "uri", "url", "key"}

// isReferenceName takes a key as credentialNameText wrote it.
func isReferenceName(lower string) bool {
	for _, ending := range referenceNameEndings {
		if strings.HasSuffix(lower, ending) {
			return true
		}
	}
	return false
}

// maxContainerDepth bounds the containers a tracker follows; deeper ones
// are counted, not tracked.
const maxContainerDepth = 64

// containerTracker follows JSON-shaped text, also across the lines of a
// stream, and reports the scalars inside containers that a credential name
// opened: pretty-printed `"api_keys": [` with its strings on the lines after
// it, or `{"credentials": {"pass": "…"}}` inside a log line. It reads quoted
// keys and values only; a JSON string never spans lines, so string state
// ends with each line.
type containerTracker struct {
	frames   []containerFrame
	overflow int
	// key is the key a ':' just completed; keyed says the next value is
	// that key's.
	key   string
	keyed bool
	// candidate is the last string, which a following ':' makes a key.
	candidate    string
	hasCandidate bool
}

type containerFrame struct {
	mode   credentialMode
	object bool
}

// here is the mode of a value at the current position, and whether the
// position holds a value at all rather than an object key.
func (c *containerTracker) here() (credentialMode, bool) {
	if c.overflow > 0 {
		// Deeper than the frames go: a scalar keeps the mode of the deepest
		// container that was followed, so what a credential's name opened
		// stays masked however far its scalars are nested.
		if len(c.frames) > 0 {
			switch c.frames[len(c.frames)-1].mode {
			case everyScalar:
				return everyScalar, true
			case credentialStrings, listedStrings:
				return listedStrings, true
			}
		}
		return ordinaryValue, false
	}
	if len(c.frames) == 0 {
		if c.keyed {
			return valueMode(ordinaryValue, c.key, false), true
		}
		return ordinaryValue, false
	}
	frame := c.frames[len(c.frames)-1]
	if frame.object {
		if c.keyed {
			return valueMode(frame.mode, c.key, false), true
		}
		return ordinaryValue, false
	}
	return valueMode(frame.mode, "", true), true
}

func (c *containerTracker) settle() {
	c.keyed, c.hasCandidate = false, false
}

// scan reads the next part of the text and returns the ranges in it that
// hold credential scalars.
func (c *containerTracker) scan(text string) []maskSpan {
	var spans []maskSpan
	for i := 0; i < len(text); {
		switch ch := text[i]; {
		case ch == '"':
			end := closingQuote(text, i)
			if end < 0 {
				// An unterminated quote is not JSON; skip to the next line.
				next := strings.IndexByte(text[i:], '\n')
				if next < 0 {
					return spans
				}
				c.settle()
				i += next + 1
				continue
			}
			mode, isValue := c.here()
			if isValue {
				if mode.masksString(decodeJSONString(text[i : end+1])) {
					spans = append(spans, maskSpan{i + 1, end})
				}
				c.settle()
			} else {
				c.candidate, c.hasCandidate = decodeJSONString(text[i:end+1]), true
			}
			i = end + 1
		case ch == ':':
			if c.hasCandidate {
				c.key, c.keyed, c.hasCandidate = c.candidate, true, false
			}
			i++
		case ch == '{' || ch == '[':
			mode, _ := c.here()
			if c.overflow > 0 || len(c.frames) == maxContainerDepth {
				c.overflow++
			} else {
				c.frames = append(c.frames, containerFrame{mode: containerMode(mode), object: ch == '{'})
			}
			c.settle()
			i++
		case ch == '}' || ch == ']':
			if c.overflow > 0 {
				c.overflow--
			} else if len(c.frames) > 0 {
				c.frames = c.frames[:len(c.frames)-1]
			}
			c.settle()
			i++
		case ch == ',':
			c.settle()
			i++
		case ch == '-' || (ch >= '0' && ch <= '9'):
			end := i + 1
			for end < len(text) && strings.IndexByte("+-0123456789.eE", text[end]) >= 0 {
				end++
			}
			if mode, isValue := c.here(); isValue && mode.masksNumber() && (i == 0 || !isWordByte(text[i-1])) {
				spans = append(spans, maskSpan{i, end})
			}
			c.settle()
			i = end
		default:
			if !isJSONSpace(ch) {
				// Any other word ends a key's wait for its value.
				if c.keyed && ch != '\n' {
					c.settle()
				}
			}
			i++
		}
	}
	return spans
}

// closingQuote is the index of the quote ending the string opened at i, or
// -1 when the line ends first.
func closingQuote(text string, i int) int {
	for j := i + 1; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++
		case '"':
			return j
		case '\n':
			return -1
		}
	}
	return -1
}

// containerCredentialSpans follows the containers of one text from its
// start.
func containerCredentialSpans(text string) []maskSpan {
	if strings.IndexByte(text, '{') < 0 && strings.IndexByte(text, '[') < 0 {
		return nil
	}
	var tracker containerTracker
	return tracker.scan(text)
}
