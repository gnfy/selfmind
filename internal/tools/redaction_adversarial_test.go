package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

type adversarialCase struct{ group, in, secret string }

// The adversarial list of the review of round 3: lines written to look like
// code, to look like a reference, or to hide a literal behind a long value, an
// unterminated quote, a call that reads, or a name and a value in the wrong
// order. A case of the group visible is one whose value stays: a reference by
// its shape, PWD (the working directory), and the one documented residual of
// the code rule. Every other value must be hidden, in the result and in the
// stream, directly and through the terminal tool.
func reviewerAdversarialCases() []adversarialCase {
	const s = "Xk9ab12Cd"
	const h = "hunter2"
	long := strings.Repeat("A1b2C3d4", 80) // 640 bytes
	cases := []adversarialCase{
		// 1. trailing statement end on non-touching operators
		{"semicolon", "password: " + s + ";", s},
		{"semicolon", "password = " + s + ";", s},
		{"semicolon", "secret: " + h + ";", h},
		{"semicolon", "X-Api-Key: " + s + ";", s},
		{"semicolon", "token = " + s + ";", s},
		{"semicolon", "PASSWORD=" + s + ";", s},
		{"semicolon", "db:\n  password: " + s + ";\n", s},
		{"semicolon", "[pypi]\npassword = " + s + ";", s},
		{"semicolon", "Authorization: Bearer " + s + "abc123;", s},
		// 2. declaration keyword or type before the name
		{"declaration", "const password: " + s, s},
		{"declaration", "var password = " + s, s},
		{"declaration", "let SECRET=" + s, s},
		{"declaration", "string password: " + s, s},
		{"declaration", "int token=" + s, s},
		{"declaration", "static secret = " + s, s},
		{"declaration", "public password: " + s, s},
		{"declaration", "final String api_key = " + s, s},
		{"declaration", "readonly token: " + h, h},
		{"declaration", "auto password: " + s, s},
		{"declaration", "- const password: " + s, s},
		{"declaration", "char *secret = " + s, s},
		// 3. member access before the name
		{"member", "x.password = " + s, s},
		{"member", "db.password = " + s, s},
		{"member", "spring.datasource.password = " + s, s},
		{"member", "this.token = " + s, s},
		{"member", "cfg->secret = " + s, s},
		{"member", "a().password = " + s, s},
		{"member", "app.config.api_key = " + h, h},
		// 4. := and comparison
		{"operator", "password := " + s, s},
		{"operator", "api_key := " + h, h},
		{"operator", "password == " + s, s},
		{"operator", "password === " + s, s},
		{"operator", "secret != " + s, s},
		// 5. block openers and labels with the value on the next line
		{"label", "if password:\n    " + s, s},
		{"label", "while token:\n  " + s, s},
		{"label", "case \"password\":\n  " + s, s},
		{"label", "\"password\":\n  " + s, s},
		{"label", "else secret:\n  " + h, h},
		{"label", "def password:\n  " + s, s},
		{"label", "for token:\n" + s, s},
		// 6. medium evidence: brackets, previous line, trailers
		{"medium", "(password: s.rGSC2pPbVeuK31A2OTQEhaB5", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"medium", "{password: s.rGSC2pPbVeuK31A2OTQEhaB5}", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"medium", "a,\npassword: s.rGSC2pPbVeuK31A2OTQEhaB5", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"medium", "x = {\npassword: s.rGSC2pPbVeuK31A2OTQEhaB5", "rGSC2pPbVeuK31A2OTQEhaB5"},
		{"medium", "[\n  password: Xk9(2mQz!pR7\n]", "Xk9(2mQz!pR7"},
		{"medium", "foo(\n  password: Xk9(2mQz!pR7,", "Xk9(2mQz!pR7"},
		{"visible", "{\n  password: correct_horse_battery\n}", "correct_horse_battery"},
		{"medium", "(PASSWORD=Xk9(2mQz!pR7 ./run)", "Xk9(2mQz!pR7"},
		{"medium", "password: Xk9ab12Cd,", s},
		{"medium", "password: Xk9ab12Cd)", s},
		{"medium", "password: " + s + " }", s},
		{"medium", "{ password: " + s + " }", s},
		{"medium", "f(password=" + s + ")", s},
		{"medium", "f(a, secret=" + h + ")", h},
		{"medium", "password: first.last2", "first.last2"},
		{"medium", "{\npassword: Summer2024.Winter", "Summer2024.Winter"},
		// 7. references and containers
		{"visible", "password: $hunter", "$hunter"},
		{"visible", "password: ${Xk9ab12Cd}", s},
		{"visible", "password: $(cat /run/secrets/Xk9ab12Cd)", s},
		{"reference", "password: `Xk9ab12Cd`", s},
		{"visible", "password: <hunter>", "hunter"},
		{"visible", "password: %HUNTER%", "HUNTER"},
		{"visible", "password: $HUNTER", "HUNTER"},
		{"reference", "password: {" + s + "}", s},
		{"visible", "password: {hunter}", "hunter"},
		{"reference", "password: [" + s + "]", s},
		{"visible", "password: {{ " + s + " }}", s},
		{"visible", "password: ${" + s + "}", s},
		{"visible", "password: \"${" + s + "}\"", s},
		{"visible", "password: '$(" + s + ")'", s},
		{"visible", "password: yes", "yes"},
		// 8. long and unterminated literals in the new literal forms
		{"literal", `os.Setenv("API_TOKEN", "` + long + `")`, long[:40]},
		{"literal", `var password string = "` + long + `"`, long[:40]},
		{"literal", `const API_TOKEN: &str = "` + long + `";`, long[:40]},
		{"literal", `os.Setenv("API_TOKEN", "` + s, s},
		{"literal", `var password string = "` + s, s},
		{"literal", `password = "` + s, s},
		{"literal", `password: "` + s + "\nnext\"", s},
		{"literal", `os.Setenv("API_TOKEN", "line1` + "\n" + `line2")`, "line2"},
		{"literal", "define('DB_PASSWORD', 'a\\'" + s + "')", s},
		{"literal", `settings.put("token", "` + s + `")`, s},
		{"literal", `config.set("auth", "password", "` + s + `")`, s},
		{"literal", `store("API_KEY", "` + s + `")`, s},
		{"literal", `vault.read("API_KEY", "` + s + `")`, s},
		{"literal", `getenv("API_KEY", "` + s + `")`, s},
		{"literal", `os.environ.get("API_KEY", "` + s + `")`, s},
		{"literal", `args.add_argument("--token", default="` + s + `")`, s},
		{"literal", `headers.append("Authorization", "` + s + `")`, s},
		{"literal", `req.Header.Add("X-Api-Key", "` + s + `")`, s},
		{"literal", `req.Header.Set("Authorization", "Token ` + s + `")`, s},
		{"literal", `x["password"] = "` + s + `"`, s},
		{"literal", `x["password"] = ` + s, s},
		{"literal", `x[password] = "` + s + `"`, s},
		{"literal", `password => ` + s, s},
		{"literal", `'password' => '` + s + `'`, s},
		{"literal", `<password>` + s + `</password>`, s},
		{"literal", `<password>` + s + "\n</password>", s},
		{"literal", `<add key="DbPassword" value="` + s + `"/>`, s},
		{"literal", `<add value="` + s + `" key="DbPassword"/>`, s},
		{"literal", "- value: " + s + "\n  name: DB_PASSWORD", s},
		{"literal", "- name: DB_PASSWORD\n  # note\n  value: " + s, s},
		{"literal", "- name: DB_PASSWORD\n  value: " + s, s},
		{"literal", "- name: DB_PASSWORD\n\n  value: " + s, s},
		{"literal", `{"value":"` + s + `","name":"DB_PASSWORD"}`, s},
		{"literal", `{"name":"DB_PASSWORD","value":"` + s + `"}`, s},
		{"literal", "name: DB_PASSWORD\nvalueFrom: x\nvalue: " + s, s},
		{"literal", "SMTP_PASS=" + s, s},
		{"visible", "PWD=" + s, s},
		{"literal", "APP_KEY=base64:" + s, s},
		{"literal", "MASTER_KEY=" + s, s},
		{"literal", "https://x.test/blob?sig=" + s + "%3D", s},
		{"literal", "X-Amz-Signature=" + strings.Repeat("ab12", 16), "ab12ab12ab12"},
		{"literal", "https://hooks.slack.com/services/T000/B000/" + s + "ZzYy", s},
		// 9. blank-line gaps
		{"gap", "password:" + strings.Repeat("\n", 5000) + s, s},
		{"gap", "password:" + strings.Repeat(" ", 5000) + s, s},
		{"gap", "password:\n" + strings.Repeat("\n", 3000) + s, s},
		{"gap", "password:" + strings.Repeat("\n", 100) + s, s},
		{"gap", "password = " + strings.Repeat("\t", 4000) + s, s},
	}
	return cases
}

func TestReviewerAdversarialList(t *testing.T) {
	cases := reviewerAdversarialCases()
	if len(cases) < 110 {
		t.Fatalf("the list holds %d cases", len(cases))
	}
	for _, c := range cases {
		result, stream := RedactSensitive(c.in), streamRedact(c.in)
		if c.group == "visible" {
			if !strings.Contains(result, c.secret) || !strings.Contains(stream, c.secret) {
				t.Errorf("[visible] %.60q was masked:\n  result: %.80q\n  stream: %.80q", c.in, result, stream)
			}
			continue
		}
		for surface, out := range map[string]string{"result": result, "stream": stream} {
			if strings.Contains(out, c.secret) {
				t.Errorf("[%s] the %s leaked %.40q:\n  in:  %.100q\n  out: %.100q", c.group, surface, c.secret, c.in, out)
			}
		}
		// Through the terminal tool, which prints the text with cat. A text
		// of thousands of lines needs its events drained as they come.
		toolResult, toolStream := terminalSurfacesDraining(t, c.in)
		if strings.Contains(toolResult, c.secret) || strings.Contains(toolStream, c.secret) {
			t.Errorf("[%s] the terminal tool leaked %.40q:\n  in: %.100q", c.group, c.secret, c.in)
		}
	}
}

// terminalSurfacesDraining is terminalSurfaces with a reader of the events, so
// that an output of many lines does not wait on a full channel.
func terminalSurfacesDraining(t *testing.T, output string) (result, stream string) {
	t.Helper()
	kernel.SetAgentEventRedactor(RedactSensitive)
	t.Cleanup(func() { kernel.SetAgentEventRedactor(nil) })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	registry.Register(NewExecuteCommandTool())
	registry.UseResultMiddleware(RedactionMiddleware())
	registry.UseResultMiddleware(EvidenceMiddleware())
	events := make(chan string, 256)
	var streamed []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for raw := range events {
			if event, ok := kernel.DecodeAgentEvent(raw); ok && event.Type == "tool.output" {
				line, _ := event.Payload["line"].(string)
				streamed = append(streamed, line)
			}
		}
	}()
	ctx := kernel.WithEventChannel(context.Background(), events)
	res, err := registry.DispatchResult("terminal", map[string]interface{}{
		"command": "cat output.txt", "cwd": dir, "timeout": "60", "_tool_call_id": "print", "_context": ctx})
	close(events)
	<-done
	if err != nil {
		t.Fatalf("printing failed: %v", err)
	}
	return res.Output, strings.Join(streamed, "\n")
}
