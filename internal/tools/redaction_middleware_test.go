package tools

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactionMiddlewareMasksToolOutputAndError(t *testing.T) {
	secret := "opaque-test-secret-123"
	RegisterSensitiveValue(secret)
	exec := RedactionMiddleware()(func(map[string]interface{}) (string, error) {
		return "output=" + secret, errors.New("failure " + secret)
	})
	output, err := exec(nil)
	if strings.Contains(output, secret) || err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("output=%q err=%v", output, err)
	}
}
