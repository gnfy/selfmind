package tools

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ValidateExternalWatchScalar prevents a pattern over a document from confusing
// a nested result or log message with the model's selected observation target.
// The model owns field selection and business meaning; this is format validation.
func ValidateExternalWatchScalar(output string) error {
	output = strings.TrimSpace(output)
	if output == "" || len(output) > 4096 || !utf8.ValidString(output) || strings.ContainsAny(output, "\r\n") || strings.HasPrefix(output, "{") || strings.HasPrefix(output, "[") {
		return fmt.Errorf("observation must be one bounded scalar, not a document or log; select the exact object's field in the read-only command, or use the typed observation adapter")
	}
	return nil
}

// MatchExternalWatchScalar matches the entire selected value, so SUCCEEDED does
// not accidentally match NOT_SUCCEEDED. Historical receipts keep their matcher.
func MatchExternalWatchScalar(pattern, output string) bool {
	if pattern == "" || ValidateExternalWatchScalar(output) != nil {
		return false
	}
	re, err := regexp.Compile(`\A(?:` + pattern + `)\z`)
	return err == nil && re.MatchString(strings.TrimSpace(output))
}
