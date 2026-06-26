package tools

import (
	"os/exec"
	"strings"
	"testing"
)

func TestExecuteCodeTripleQuoteIsNotInjectable(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	tool := NewExecuteCodeTool()

	// Code containing triple quotes used to break out of the exec('''...''')
	// wrapper and could inject arbitrary statements. With direct file execution
	// it must run verbatim and simply print the literal text.
	code := "print('start')\nx = '''embedded ''' + 'quotes'\nprint(x)\nprint('end')"
	out, err := tool.Execute(map[string]interface{}{"code": code, "timeout": 30})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"start", "embedded quotes", "end"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q; got: %q", want, out)
		}
	}
}

func TestExecuteCodeReportsErrors(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	tool := NewExecuteCodeTool()
	_, err := tool.Execute(map[string]interface{}{"code": "raise ValueError('boom')", "timeout": 30})
	if err == nil {
		t.Fatal("expected error from failing script")
	}
}
