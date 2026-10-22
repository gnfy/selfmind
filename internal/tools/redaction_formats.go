package tools

import (
	"regexp"
	"strings"
)

// credentialShape is a credential recognized by its own form, with no name
// assigning it. The literal prefix finds candidates quickly; body is anchored
// at the prefix, and what it matches past keep bytes, or its first group when
// it has one, is what is masked.
type credentialShape struct {
	prefix string
	body   *regexp.Regexp
	keep   int
	// boundary requires the prefix not to continue a word.
	boundary bool
}

var credentialShapes = []credentialShape{
	// A provider key; the boundary keeps "task-", "disk-" and "risk-" names
	// ordinary.
	{prefix: "sk-", body: regexp.MustCompile(`^sk-[A-Za-z0-9_-]{16,}`), keep: 3, boundary: true},
	// GitHub classic personal, OAuth, user-to-server, server-to-server and
	// refresh tokens, and fine-grained personal access tokens.
	{prefix: "gh", body: regexp.MustCompile(`^gh[pousr]_[A-Za-z0-9]{36,}`), keep: 4, boundary: true},
	{prefix: "github_pat_", body: regexp.MustCompile(`^github_pat_[A-Za-z0-9_]{22,}`), keep: 11, boundary: true},
	// A JWT: three base64url segments, the header's JSON starting "eyJ".
	{prefix: "eyJ", body: regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`), boundary: true},
	// An AWS access key id is an identifier; the secret access key written
	// next to it is the credential.
	{prefix: "AKIA", body: awsSecretAfterKeyID, boundary: true},
	{prefix: "ASIA", body: awsSecretAfterKeyID, boundary: true},
	// Slack bot, user, app and refresh tokens.
	{prefix: "xox", body: regexp.MustCompile(`^xox[abposr]-[A-Za-z0-9-]{10,}`), keep: 5, boundary: true},
	// Stripe secret and restricted keys.
	{prefix: "sk_", body: regexp.MustCompile(`^sk_(?:live|test)_[A-Za-z0-9]{16,}`), keep: 8, boundary: true},
	{prefix: "rk_", body: regexp.MustCompile(`^rk_(?:live|test)_[A-Za-z0-9]{16,}`), keep: 8, boundary: true},
	// Google API keys.
	{prefix: "AIza", body: regexp.MustCompile(`^AIza[0-9A-Za-z_-]{35}`), keep: 4, boundary: true},
	// GitLab personal access tokens.
	{prefix: "glpat-", body: regexp.MustCompile(`^glpat-[A-Za-z0-9_-]{20,}`), keep: 6, boundary: true},
	// Hugging Face access tokens.
	{prefix: "hf_", body: regexp.MustCompile(`^hf_[A-Za-z0-9]{30,}`), keep: 3, boundary: true},
}

var awsSecretAfterKeyID = regexp.MustCompile(`^A[KS]IA[A-Z0-9]{16}(?:\\*["']|[\s,;:|=]){1,16}([A-Za-z0-9/+]{40})(?:[^A-Za-z0-9/+=]|$)`)

// shapedCredentialSpans finds credentials by their own form.
func shapedCredentialSpans(text string) []maskSpan {
	var spans []maskSpan
	for _, shape := range credentialShapes {
		for at := 0; ; {
			i := strings.Index(text[at:], shape.prefix)
			if i < 0 {
				break
			}
			i += at
			at = i + 1
			if shape.boundary && !wordStart(text, i) {
				continue
			}
			match := shape.body.FindStringSubmatchIndex(text[i:])
			if match == nil {
				continue
			}
			start, end := i+shape.keep, i+match[1]
			if len(match) > 2 {
				start, end = i+match[2], i+match[3]
			}
			spans = append(spans, maskSpan{start, end})
			at = end
		}
	}
	return spans
}

var (
	// scheme://user:password@host; the password is masked.
	urlPassword = regexp.MustCompile(`^://[^\s/@:?#"'<>\\]*:([^\s/@?#"'<>\\]+)@[A-Za-z0-9.\[\]-]`)
	// Cookie: and Set-Cookie: headers, before their name=value pairs.
	cookieHeader = regexp.MustCompile(`^(?:\\*["'])?\s*:\s*(?:\\*["'])?`)
	cookiePair   = regexp.MustCompile(`^[ \t]*([^=;\s"',\\]+)=([^;\s"',\\]*)`)
	// mysql -p<password>, attached to the flag.
	mysqlPassword = regexp.MustCompile(`^-p(?:'([^'\n]+)'|"([^"\n]+)"|([^\s'"-][^\s'"]*))`)
	// curl -u user:password and --user user:password.
	curlUser = regexp.MustCompile(`^(?:-u|--user)(?:[ \t]+|=)["']?[^\s:"']*:([^\s"']+)`)
	// The armor of a private key in PEM: RSA, EC, DSA, OPENSSH, ENCRYPTED.
	pemPrivateKeyBegin = regexp.MustCompile(`^-----BEGIN ((?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?)-----`)
)

// maxPEMBody bounds how far a private key's body is looked for.
const maxPEMBody = 64 << 10

// formattedCredentialSpans finds credentials by the format that holds them:
// a private key's PEM body, a URL's password, cookie values, and a
// password on a mysql or curl command line.
func formattedCredentialSpans(text, lower string) []maskSpan {
	var spans []maskSpan
	spans = append(spans, privateKeySpans(text)...)
	for at := 0; ; {
		i := strings.Index(text[at:], "://")
		if i < 0 {
			break
		}
		i += at
		at = i + 3
		if i == 0 || !isASCIILetter(text[i-1]) {
			continue
		}
		window := text[i:min(len(text), i+valueWindow)]
		if match := urlPassword.FindStringSubmatchIndex(window); match != nil && !isCredentialReference(window[match[2]:match[3]]) {
			spans = append(spans, maskSpan{i + match[2], i + match[3]})
		}
	}
	spans = append(spans, cookieSpans(text, lower)...)
	spans = append(spans, signatureSpans(text, lower)...)
	spans = append(spans, webhookSpans(text, lower)...)
	spans = append(spans, commandLineSpans(text, lower, " -p", "mysql", mysqlPassword)...)
	spans = append(spans, commandLineSpans(text, lower, " -u", "curl", curlUser)...)
	spans = append(spans, commandLineSpans(text, lower, " --user", "curl", curlUser)...)
	return spans
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// privateKeySpans masks the body of every PEM private key and keeps its armor
// lines. The body is the run of lines after BEGIN that a key's body can hold
// (see pemBodyState), so prose that merely mentions the armor, or an END line
// far away, masks nothing it should not. Lines end with a line break, or
// with its JSON escape inside a string. A body may be indented, quoted or
// prefixed by a log line's timestamp, and a key squeezed into one line keeps
// its chunks apart with spaces.
func privateKeySpans(text string) []maskSpan {
	var spans []maskSpan
	for at := 0; ; {
		i := strings.Index(text[at:], "-----BEGIN ")
		if i < 0 {
			break
		}
		i += at
		at = i + 1
		match := pemPrivateKeyBegin.FindStringIndex(text[i:])
		if match == nil {
			continue
		}
		pos := i + match[1]
		after := pos
		for after < len(text) && (text[after] == ' ' || text[after] == '\t') {
			after++
		}
		if n := lineBreakAt(text, after); n > 0 {
			pos = after + n
		} else if from, to, ok := oneLinePEMBody(text, pos); ok {
			spans = append(spans, maskSpan{from, to})
			at = max(at, to)
			continue
		} else {
			continue
		}
		var body pemBodyState
		start, end := pos, pos
		first, continued := true, false
		limit := min(len(text), pos+maxPEMBody)
	lines:
		for pos < limit {
			stop := lineEnd(text, pos, limit)
			from, to, kind := body.next(trimLineEnd(text[pos:stop]), continued)
			switch kind {
			case pemEnd:
				break lines
			case pemBody:
				if first {
					start, first = pos+from, false
				}
				end = pos + to
			}
			if stop >= len(text) {
				break
			}
			// A line longer than a body line can be is read in pieces.
			n := lineBreakAt(text, stop)
			continued = n == 0
			pos = stop + n
		}
		if end > start {
			spans = append(spans, maskSpan{start, end})
		}
		at = max(at, end)
	}
	return spans
}

// minPEMChunk is the shortest piece of a key squeezed into one line that
// another piece follows: an armored body is wrapped at 64 or 76 characters.
const minPEMChunk = 16

// oneLinePEMBody finds the body of a private key written on the line of its
// armor, its chunks apart by spaces, after the BEGIN armor that ends at pos:
// base64 chunks, each but the last long enough to be a wrapped line of a key,
// and enough of them to be a key.
func oneLinePEMBody(text string, pos int) (from, to int, ok bool) {
	limit := min(len(text), pos+maxPEMBody)
	i := pos
	chunks, total, last := 0, 0, 0
	for i < limit {
		for i < limit && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
		j := i
		for j < limit && isBase64Byte(text[j]) {
			j++
		}
		if j == i {
			break
		}
		// The chunk before this one was not the last.
		if chunks > 0 && last < minPEMChunk {
			return 0, 0, false
		}
		if chunks == 0 {
			from = i
		}
		last = j - i
		chunks, total, to = chunks+1, total+last, j
		i = j
	}
	if chunks == 0 || total < 2*minPEMChunk || (i < len(text) && !strings.ContainsRune(" \t\r\n\"'\\,;)}]-<", rune(text[i]))) {
		return 0, 0, false
	}
	return from, to, true
}

// lineBreakAt is the length of the line break at i: CRLF, LF, or their
// JSON escapes.
func lineBreakAt(text string, i int) int {
	for _, br := range []string{"\r\n", "\n", `\r\n`, `\n`} {
		if strings.HasPrefix(text[i:], br) {
			return len(br)
		}
	}
	return 0
}

// maxPEMLine bounds a line of a private key's body; a longer one is not.
const maxPEMLine = 8192

// lineEnd is where the line starting at pos ends: at its line break, or at
// limit, or where a line of a key's body could no longer be.
func lineEnd(text string, pos, limit int) int {
	end := min(limit, pos+maxPEMLine)
	for _, br := range []string{"\n", `\n`} {
		if i := strings.Index(text[pos:end], br); i >= 0 {
			end = pos + i
		}
	}
	return end
}

// trimLineEnd drops the carriage return a line may end with.
func trimLineEnd(line string) string {
	line = strings.TrimSuffix(line, "\r")
	return strings.TrimSuffix(line, `\r`)
}

// pemHeaderLine is a PEM header such as Proc-Type: 4,ENCRYPTED.
var pemHeaderLine = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*: [A-Za-z0-9,+/=:._ -]*$`)

func isBase64Byte(c byte) bool {
	return isASCIILetter(c) || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '='
}

type pemLineKind uint8

const (
	// pemBlank is an empty line, which a body may hold.
	pemBlank pemLineKind = iota
	// pemBody is a line of a key's body.
	pemBody
	// pemEnd is a line a body cannot hold: the END armor, or anything else.
	pemEnd
)

// pemBodyState follows the lines of a private key's body, from the line after
// its BEGIN armor. A body line is base64 text, or a header (Proc-Type,
// Version). Its text may follow a prefix that every line of the body shares
// in shape: an indent, a block scalar's or quote's marker, a log line's
// timestamp, whose digits differ from line to line. The first line sets the
// prefix; a line with another one ends the body, and so does an armor line.
type pemBodyState struct {
	started bool
	shape   string
}

// maxPEMPrefix bounds a body line's prefix.
const maxPEMPrefix = 200

// pemPrefixOnly matches what precedes a header line: blanks and the markers
// of a block scalar, a quote or a comment.
var pemPrefixOnly = regexp.MustCompile(`^[ \t|>#]*`)

// next classifies a line, without its line break, and returns the part of it
// to mask. continued says the line is the rest of a longer one.
func (s *pemBodyState) next(line string, continued bool) (from, to int, kind pemLineKind) {
	if strings.TrimSpace(line) == "" {
		return 0, 0, pemBlank
	}
	if strings.Contains(line, "-----") {
		return 0, 0, pemEnd
	}
	rest := line[len(pemPrefixOnly.FindString(line)):]
	if pemHeaderLine.MatchString(rest) {
		from, to = len(line)-len(rest), len(line)
	} else {
		to = len(strings.TrimRight(line, " \t"))
		from = to
		for from > 0 && isBase64Byte(line[from-1]) {
			from--
		}
		// After a prefix of words, a whole line of base64 begins a body, not
		// a word that ends a sentence.
		wordPrefix := from > len(pemPrefixOnly.FindString(line))
		if from == to || (wordPrefix && !s.started && to-from < 2*minPEMChunk) {
			return 0, 0, pemEnd
		}
	}
	if continued {
		return from, to, pemBody
	}
	shape := pemPrefixShape(line[:from])
	switch {
	case !s.started && from <= maxPEMPrefix:
		s.started, s.shape = true, shape
	case !s.started || shape != s.shape:
		return 0, 0, pemEnd
	}
	return from, to, pemBody
}

// pemPrefixShape is a prefix with its digits made alike, so that two
// timestamps have one shape.
func pemPrefixShape(prefix string) string {
	if strings.IndexFunc(prefix, func(r rune) bool { return r >= '0' && r <= '9' }) < 0 {
		return prefix
	}
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return '0'
		}
		return r
	}, prefix)
}

// cookieSpans masks the values of a Cookie header and the cookie value of a
// Set-Cookie header; Set-Cookie's attributes (Path, Domain, ...) stay.
func cookieSpans(text, lower string) []maskSpan {
	var spans []maskSpan
	for at := 0; ; {
		i := strings.Index(lower[at:], "cookie")
		if i < 0 {
			break
		}
		i += at
		at = i + 6
		start := i
		setCookie := strings.HasSuffix(lower[:i], "set-")
		if setCookie {
			start -= 4
		}
		if (start > 0 && isIdentifierByte(lower[start-1])) || (i+6 < len(lower) && isIdentifierByte(lower[i+6])) {
			continue
		}
		header := cookieHeader.FindStringIndex(text[i+6:])
		if header == nil || header[1] == 0 {
			continue
		}
		pos := i + 6 + header[1]
		for {
			pair := cookiePair.FindStringSubmatchIndex(text[pos:])
			if pair == nil {
				break
			}
			if pair[5] > pair[4] {
				spans = append(spans, maskSpan{pos + pair[4], pos + pair[5]})
			}
			pos += pair[1]
			if setCookie || pos >= len(text) || text[pos] != ';' {
				break
			}
			pos++
		}
		at = max(at, pos)
	}
	return spans
}

// signedURLValue is the value of a signed URL's signature parameter: base64,
// hex or percent-encoded, long enough that sig=1 is not one.
var signedURLValue = regexp.MustCompile(`^=([A-Za-z0-9%+/_=.~-]{8,})`)

// signatureSpans masks the signature of a signed URL: sig= (Azure shared
// access signatures, Logic Apps), Signature= (AWS query authentication,
// CloudFront) and X-Amz-Signature= or X-Goog-Signature= (presigned URLs). A
// signature is the credential in a link that grants access without one. The
// bare words count only as a query parameter; the vendor names anywhere.
func signatureSpans(text, lower string) []maskSpan {
	var spans []maskSpan
	for _, name := range []string{"sig", "signature"} {
		for at := 0; ; {
			i := strings.Index(lower[at:], name+"=")
			if i < 0 {
				break
			}
			i += at
			at = i + len(name)
			vendor := name == "signature" && (strings.HasSuffix(lower[:i], "x-amz-") || strings.HasSuffix(lower[:i], "x-goog-") || strings.HasSuffix(lower[:i], "x-ms-"))
			// AWS signs a request in its Authorization header as well:
			// ..., Signature=<hex>.
			signed := name == "signature" && i > 0 && text[i-1] == ' ' && strings.Contains(lower[max(0, i-valueWindow):i], "aws4-hmac")
			if !vendor && !signed && (i == 0 || strings.IndexByte("?&;", text[i-1]) < 0) {
				continue
			}
			match := signedURLValue.FindStringSubmatchIndex(text[i+len(name) : min(len(text), i+len(name)+valueWindow)])
			if match == nil {
				continue
			}
			from, to := i+len(name)+match[2], i+len(name)+match[3]
			spans = append(spans, maskSpan{from, to})
			at = to
		}
	}
	return spans
}

// webhookSecret is the part of an incoming webhook's address that grants the
// right to post: the last segment of a Slack hook, the token after a Discord
// webhook's id, the segment after IncomingWebhook in a Teams connector's.
var webhookSecret = []struct {
	marker  string
	pattern *regexp.Regexp
}{
	{"hooks.slack.com/", regexp.MustCompile(`(?i)^hooks\.slack\.com/(?:services|workflows|triggers)/(?:[A-Za-z0-9_-]+/){1,4}([A-Za-z0-9_-]{8,})`)},
	{"discord", regexp.MustCompile(`(?i)^discord(?:app)?\.com/api(?:/v\d+)?/webhooks/\d+/([A-Za-z0-9_-]{8,})`)},
	{"/incomingwebhook/", regexp.MustCompile(`(?i)^/incomingwebhook/([A-Za-z0-9_-]{8,})`)},
}

// webhookSpans masks the secret in the address of an incoming webhook.
func webhookSpans(text, lower string) []maskSpan {
	var spans []maskSpan
	for _, hook := range webhookSecret {
		for at := 0; ; {
			i := strings.Index(lower[at:], hook.marker)
			if i < 0 {
				break
			}
			i += at
			at = i + 1
			match := hook.pattern.FindStringSubmatchIndex(text[i:min(len(text), i+valueWindow)])
			if match == nil {
				continue
			}
			spans = append(spans, maskSpan{i + match[2], i + match[3]})
			at = i + match[3]
		}
	}
	return spans
}

// commandLineSpans masks the password that pattern finds after flag on a
// line that runs program.
func commandLineSpans(text, lower, flag, program string, pattern *regexp.Regexp) []maskSpan {
	var spans []maskSpan
	for at := 0; ; {
		i := strings.Index(text[at:], flag)
		if i < 0 {
			break
		}
		i += at + 1
		at = i
		if !strings.Contains(lineBefore(lower, i), program) {
			continue
		}
		window := text[i:min(len(text), i+valueWindow)]
		match := pattern.FindStringSubmatchIndex(window)
		if match == nil {
			continue
		}
		for group := 2; group+1 < len(match); group += 2 {
			if match[group] >= 0 {
				if value := window[match[group]:match[group+1]]; !isCredentialReference(value) {
					spans = append(spans, maskSpan{i + match[group], i + match[group+1]})
				}
				break
			}
		}
	}
	return spans
}
