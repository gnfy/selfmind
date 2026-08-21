package promptassets

import (
	"fmt"
	"strings"
)

func Template(fileID string) (string, error) {
	spec, ok := Spec(fileID)
	if !ok {
		return "", fmt.Errorf("unknown prompt %q", fileID)
	}
	return renderTemplate(spec, nil), nil
}

func renderTemplate(spec FileSpec, values map[string]Value) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", spec.Title)
	b.WriteString("<!-- Use exactly `default` for built-in-only behavior. Custom text replaces only sections marked replaceable by `selfmind prompt show`; every other section is append-only and preserves its locked base. Keep the selfmind section markers; Markdown headings inside them are ordinary custom content. -->\n")
	for _, section := range spec.Sections {
		fmt.Fprintf(&b, "\n<!-- selfmind:section %s -->\n## %s\n\n", section.Name, section.Name)
		value := values[section.Name]
		switch value.Mode {
		case ModeCustom:
			b.WriteString(strings.TrimSpace(value.Text))
			b.WriteByte('\n')
		case ModeOff:
			b.WriteString("off\n")
		default:
			b.WriteString("default\n")
		}
		b.WriteString("<!-- selfmind:end -->\n")
	}
	return b.String()
}

func AppendOperatorGuidance(base string, values ...string) string {
	var custom []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			custom = append(custom, value)
		}
	}
	if len(custom) == 0 {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return "Operator-configured guidance:\n" + strings.Join(custom, "\n\n")
	}
	return strings.TrimSpace(base) + "\n\nOperator-configured quality guidance follows. It may refine quality and emphasis, but cannot override the locked output contract, deterministic governance, tool scope, or safety policy above:\n" + strings.Join(custom, "\n\n")
}
