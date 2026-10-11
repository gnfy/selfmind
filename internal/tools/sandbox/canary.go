package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
)

// canaryForm is the only form a canary takes: NewCanary generates it, and
// nothing else is ever looked up, recorded or acted on as one.
var canaryForm = regexp.MustCompile(`^com\.selfmind\.exec\.[0-9a-f]{24}$`)

// NewCanary is a fresh, unguessable canary for one command's sandbox.
func NewCanary() string {
	suffix := make([]byte, 12)
	_, _ = rand.Read(suffix)
	return "com.selfmind.exec." + hex.EncodeToString(suffix)
}

// ValidCanary reports whether name is a canary NewCanary could have made.
func ValidCanary(name string) bool { return canaryForm.MatchString(name) }

// ErrLeftoverStopUnverified reports a platform where stopping the processes
// left under a canary has not been verified, so it is not attempted.
var ErrLeftoverStopUnverified = errors.New("stopping the processes a command left under its sandbox is not verified on this architecture")
