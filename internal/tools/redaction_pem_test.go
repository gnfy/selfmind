package tools

import (
	"fmt"
	"strings"
	"testing"
)

// pemBodyLines are n lines of invented key material, each marked.
func pemBodyLines(n int) []string {
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("r3PemBodyLine%02dAbCdEfGhIjKlMnOpQrStUvWxYz0123456789+/AbCdEfGh", i))
	}
	return lines
}

func prefixed(prefix func(i int) string, lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = prefix(i) + line
	}
	return out
}

// A private key's body is masked whatever shares its lines: an indent, the
// marker of a block scalar or a table, a log line's timestamp, or nothing but
// a space between its chunks on one line. The armor stays, and so do the
// lines around the key.
func TestPrivateKeyBodiesAreMaskedWithAPrefix(t *testing.T) {
	body := pemBodyLines(6)
	for _, tc := range []struct {
		name  string
		lines []string
		keep  []string
	}{
		{
			"yaml block scalar", append(append([]string{"key: |", "  -----BEGIN RSA PRIVATE KEY-----"}, prefixed(func(int) string { return "  " }, body)...),
				"  -----END RSA PRIVATE KEY-----", "next: value"),
			[]string{"key: |", "  -----BEGIN RSA PRIVATE KEY-----", "  -----END RSA PRIVATE KEY-----", "next: value"},
		},
		{
			"deep indent", append(append([]string{"      -----BEGIN PRIVATE KEY-----"}, prefixed(func(int) string { return "      " }, body)...),
				"      -----END PRIVATE KEY-----", "done"),
			[]string{"-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----", "done"},
		},
		{
			"tabs", append(append([]string{"\t-----BEGIN EC PRIVATE KEY-----"}, prefixed(func(int) string { return "\t\t" }, body)...),
				"\t-----END EC PRIVATE KEY-----", "done"),
			[]string{"-----BEGIN EC PRIVATE KEY-----", "-----END EC PRIVATE KEY-----", "done"},
		},
		{
			"table bars", append(append([]string{"| -----BEGIN PRIVATE KEY-----"}, prefixed(func(int) string { return "| " }, body)...),
				"| -----END PRIVATE KEY-----", "| other row"),
			[]string{"| -----BEGIN PRIVATE KEY-----", "| -----END PRIVATE KEY-----", "| other row"},
		},
		{
			"comment marker", append(append([]string{"# -----BEGIN PRIVATE KEY-----"}, prefixed(func(int) string { return "# " }, body)...),
				"# -----END PRIVATE KEY-----", "done"),
			[]string{"# -----BEGIN PRIVATE KEY-----", "# -----END PRIVATE KEY-----", "done"},
		},
		{
			"log timestamps", append(append([]string{"2026-10-08T10:00:00.100Z INFO -----BEGIN PRIVATE KEY-----"},
				prefixed(func(i int) string { return fmt.Sprintf("2026-10-08T10:00:00.%03dZ INFO ", 101+i) }, body)...),
				"2026-10-08T10:00:00.200Z INFO -----END PRIVATE KEY-----", "2026-10-08T10:00:01.000Z INFO done"),
			[]string{"INFO -----BEGIN PRIVATE KEY-----", "INFO -----END PRIVATE KEY-----", "2026-10-08T10:00:01.000Z INFO done"},
		},
		{
			"pod log prefix", append(append([]string{"[pod/api-7d9f] -----BEGIN OPENSSH PRIVATE KEY-----"},
				prefixed(func(int) string { return "[pod/api-7d9f] " }, body)...),
				"[pod/api-7d9f] -----END OPENSSH PRIVATE KEY-----", "[pod/api-7d9f] ready"),
			[]string{"-----BEGIN OPENSSH PRIVATE KEY-----", "-----END OPENSSH PRIVATE KEY-----", "[pod/api-7d9f] ready"},
		},
		{
			"key with no END line, then a log line", append(append([]string{"2026-10-08T10:00:00Z INFO -----BEGIN PRIVATE KEY-----"},
				prefixed(func(i int) string { return fmt.Sprintf("2026-10-08T10:00:%02dZ INFO ", i) }, body)...),
				"2026-10-08T10:00:30Z INFO Connection closed by remote host."),
			[]string{"INFO -----BEGIN PRIVATE KEY-----", "Connection closed by remote host."},
		},
		{
			"CRLF", append(append([]string{"  -----BEGIN PRIVATE KEY-----\r"}, prefixed(func(int) string { return "  " }, joinEach(body, "\r"))...),
				"  -----END PRIVATE KEY-----\r"),
			[]string{"-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----"},
		},
	} {
		output := strings.Join(tc.lines, "\n")
		assertChainHides(t, tc.name, output, "r3PemBodyLine")
		assertChainKeeps(t, tc.name+" around the key", output, tc.keep...)
	}
}

func joinEach(lines []string, suffix string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = line + suffix
	}
	return out
}

// A key written on one line, its chunks apart by spaces, as environment
// variables and CI secrets hold it.
func TestPrivateKeyOnOneLineIsMasked(t *testing.T) {
	body := pemBodyLines(4)
	chunks := strings.Join(body, " ")
	for _, tc := range []struct{ name, output string }{
		{"quoted env", `TLS_MATERIAL="-----BEGIN RSA PRIVATE KEY----- ` + chunks + ` -----END RSA PRIVATE KEY-----"`},
		{"export", `export TLS_MATERIAL='-----BEGIN PRIVATE KEY----- ` + chunks + ` -----END PRIVATE KEY-----'`},
		{"single chunk", `-----BEGIN PRIVATE KEY----- ` + strings.Join(body, "") + ` -----END PRIVATE KEY-----`},
		{"in a log line", `2026-10-08T10:00:00Z INFO loaded key -----BEGIN EC PRIVATE KEY----- ` + chunks + ` -----END EC PRIVATE KEY----- ok`},
		{"in JSON", `{"key":"-----BEGIN PRIVATE KEY----- ` + chunks + ` -----END PRIVATE KEY-----","n":1}`},
		{"no END", `TLS_MATERIAL=-----BEGIN PRIVATE KEY----- ` + chunks},
	} {
		assertChainHides(t, tc.name, tc.output, "r3PemBodyLine")
		assertChainKeeps(t, tc.name+" armor", tc.output, "-----BEGIN ")
	}
	// Prose that mentions the armor on one line masks nothing.
	for _, prose := range []string{
		"put -----BEGIN PRIVATE KEY----- here and then the key itself on the next line",
		"The file starts with -----BEGIN PRIVATE KEY----- and ends with -----END PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY----- MIIE",
		"-----BEGIN PRIVATE KEY-----",
	} {
		assertChainShows(t, prose, prose)
	}
}

// Certificates and public keys are never masked, with any prefix.
func TestPublicPEMBodiesAreNeverMasked(t *testing.T) {
	body := pemBodyLines(3)
	cert := "  -----BEGIN CERTIFICATE-----\n  " + strings.Join(body, "\n  ") + "\n  -----END CERTIFICATE-----"
	assertChainShows(t, "indented certificate", cert)
	oneLine := "CERT=\"-----BEGIN CERTIFICATE----- " + strings.Join(body, " ") + " -----END CERTIFICATE-----\""
	assertChainShows(t, "one-line certificate", oneLine)
}

// A body line of one prefix does not carry the mask over to a line of another.
func TestPrivateKeyBodyEndsWhereItsPrefixDoes(t *testing.T) {
	body := pemBodyLines(3)
	output := "  -----BEGIN PRIVATE KEY-----\n  " + strings.Join(body, "\n  ") + "\nunindented AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 line"
	assertChainHides(t, "the key", output, "r3PemBodyLine")
	assertChainKeeps(t, "the line after it", output, "unindented AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 line")
}
