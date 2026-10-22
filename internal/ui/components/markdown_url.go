package components

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// A line that holds a URL is not broken at hyphens: Codex wraps such a line only
// at spaces so a terminal and a person still see one token (wrapping.rs,
// adaptive_wrap_line). Whether a token is URL-like follows its is_url_like_token.

// urlTokenCutset is stripped from both ends of a token before it is judged, so a
// URL in parentheses or at the end of a sentence still counts.
const urlTokenCutset = "()[]{}<>,.;:!'\""

// lineHasURLLikeToken reports whether any token of the line, split at ASCII
// whitespace, looks like a URL.
func lineHasURLLikeToken(line string) bool {
	tokens := strings.FieldsFunc(ansi.Strip(line), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f'
	})
	for _, token := range tokens {
		if isURLLikeToken(token) {
			return true
		}
	}
	return false
}

// isURLLikeToken recognizes an absolute URL (scheme://host...) or a bare one
// (host[:port] followed by a path, query or fragment, or starting with www.).
// File paths such as src/main.rs are not URLs: the host must be localhost, an
// IPv4 address or a domain name.
func isURLLikeToken(raw string) bool {
	token := strings.Trim(raw, urlTokenCutset)
	return isAbsoluteURLLike(token) || isBareURLLike(token)
}

func isAbsoluteURLLike(token string) bool {
	if !strings.Contains(token, "://") {
		return false
	}
	if parsed, err := url.Parse(token); err == nil {
		switch parsed.Scheme {
		case "http", "https", "ftp", "ftps", "ws", "wss":
			return parsed.Host != ""
		}
		return true
	}
	return hasValidSchemePrefix(token)
}

// hasValidSchemePrefix accepts a custom scheme://rest that the URL parser rejects.
func hasValidSchemePrefix(token string) bool {
	scheme, _, found := strings.Cut(token, "://")
	if !found {
		return false
	}
	for i, r := range scheme {
		switch {
		case r < 128 && isASCIILetter(byte(r)):
		case i > 0 && ((r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

func isBareURLLike(token string) bool {
	hostPort, hasTrailer := token, false
	if i := strings.IndexAny(token, "/?#"); i >= 0 {
		hostPort, hasTrailer = token[:i], true
	}
	if !hasTrailer && !strings.HasPrefix(strings.ToLower(hostPort), "www.") {
		return false
	}
	host, port := splitHostAndPort(hostPort)
	if port != "" && !isValidPort(port) {
		return false
	}
	return strings.EqualFold(host, "localhost") || isIPv4(host) || isDomainName(host)
}

// splitHostAndPort separates a trailing :digits. A bracketed IPv6 host never
// passes as a domain, an address or localhost, so it is not URL-like.
func splitHostAndPort(hostPort string) (string, string) {
	if i := strings.LastIndex(hostPort, ":"); i > 0 && isDigits(hostPort[i+1:]) {
		return hostPort[:i], hostPort[i+1:]
	}
	return hostPort, ""
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func isValidPort(port string) bool {
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

func isIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if _, err := strconv.ParseUint(part, 10, 8); err != nil {
			return false
		}
	}
	return true
}

func isDomainName(host string) bool {
	if !strings.Contains(host, ".") {
		return false
	}
	labels := strings.Split(host, ".")
	tld := labels[len(labels)-1]
	if len(tld) < 2 || len(tld) > 63 {
		return false
	}
	for i := 0; i < len(tld); i++ {
		if !isASCIILetter(tld[i]) {
			return false
		}
	}
	for _, label := range labels[:len(labels)-1] {
		if !isDomainLabel(label) {
			return false
		}
	}
	return true
}

func isDomainLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	isAlnum := func(b byte) bool { return isASCIILetter(b) || (b >= '0' && b <= '9') }
	if !isAlnum(label[0]) || !isAlnum(label[len(label)-1]) {
		return false
	}
	for i := 0; i < len(label); i++ {
		if !isAlnum(label[i]) && label[i] != '-' {
			return false
		}
	}
	return true
}
