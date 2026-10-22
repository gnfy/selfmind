package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzRedactNoPanic: any text redacts without panicking, a valid JSON
// document stays JSON unless the report says it could not, the mask never
// grows the text without bound, and neither the result nor the stream takes
// long. The seeds run with the ordinary tests.
func FuzzRedactNoPanic(f *testing.F) {
	seeds := []string{
		`{"password":"abc","token":[1,2,{"x":"y"}],"credentials":{"a":["b"]}}`,
		"token=(\n", "\x1b[31mtoken\x1b[0m=a ", `"password": "pa"`, "-----BEGIN PRIVATE KEY-----\nQUJD\n-----END PRIVATE KEY-----",
		"  -----BEGIN PRIVATE KEY-----\n  QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5\n  -----END PRIVATE KEY-----",
		"KEY=-----BEGIN PRIVATE KEY----- QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5 QUJD -----END PRIVATE KEY-----",
		"Authorization: Bearer abcdefghijklmnop", "postgres://u:p@h/db", "Cookie: a=b; c=d", "mysql -pq", "curl -u a:b http://x",
		`{"authToken":4242424,"a":"token=abc"}`, "\xef\xbb\xbf{\"secret\":\"x\"}", "a\xe2\x80\x8bb\xc2\xadc=d",
		"\\" + "u0070assword=x", `{"p` + "\\" + `u0061ssword":"x"}`,
		"password:\n  \n  x", "api_keys:\n- k1\n- k2", "{{ .Values.password }}", "password = os.environ[\"X\"]",
		"c.token = tok", "const x = new Client({ apiKey: someVar, timeout: 3 })", "token := os.Getenv(\"T\")",
		`os.Setenv("API_TOKEN", "x")`, `const T: &str = "x";`, `<password>x</password>`, "- name: X_PASSWORD\n  value: x",
		"https://h.test/p?sig=abcdefgh12&se=1", "https://hooks.slack.com/services/T0/B0/abcdefgh12", `f"Bearer {token}"`,
		`{"api_key":[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[["x"]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 1<<16 {
			t.Skip()
		}
		began := time.Now()
		out, unparseable := redactSensitiveReport(input)
		if took := time.Since(began); took > 300*time.Millisecond {
			t.Errorf("slow result redaction (%d bytes took %s): %q", len(input), took, input)
		}
		if len(out) > 4*len(input)+1024 {
			t.Fatalf("output grew: %d -> %d", len(input), len(out))
		}
		if utf8.ValidString(input) && !utf8.ValidString(out) {
			t.Fatalf("output is not valid UTF-8: %q -> %q", input, out)
		}
		trimmed := strings.TrimSpace(input)
		if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) && !unparseable {
			if !json.Valid([]byte(strings.TrimSpace(out))) {
				t.Fatalf("valid JSON became invalid without the unparseable flag: %q -> %q", input, out)
			}
		}
		stream := newLineStreamRedactor()
		lines := strings.Split(input, "\n")
		began = time.Now()
		for _, line := range lines {
			stream.push(line)
		}
		stream.flush()
		if took := time.Since(began); took > 300*time.Millisecond+time.Duration(len(lines))*500*time.Microsecond {
			t.Errorf("slow stream redaction (%d bytes, %d lines took %s): %q", len(input), len(lines), took, input)
		}
	})
}
