package llm

import (
	"fmt"
	"regexp"
)

// Claim references follow the same request-bound opaque identity contract as
// Run and artifact references. No recording-time claim selects replay authority.
var vcrEffectClaimIDPattern = regexp.MustCompile(`\beffectclaim_[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

func vcrEffectClaimPlaceholder(index int) string {
	return fmt.Sprintf("{{SELFMIND_VCR_EFFECT_CLAIM_%d}}", index+1)
}
