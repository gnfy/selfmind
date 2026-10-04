package kernel

import (
	"fmt"
	"strings"
	"time"
)

// RuntimeObservation is selected once at Run start. It identifies observed
// runtime state, distinct from remembered prose and from the workspace's code.
// A later work_select receipt may supersede the initial continuation edge.
type RuntimeObservation struct {
	ObservedAt        time.Time
	BuildFingerprint  string
	MaxActiveWorkRuns int
	ResumesRunID      string
}

func (o *RuntimeObservation) Prompt() string {
	if o == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Runtime observation at this Run's start\n")
	if !o.ObservedAt.IsZero() {
		fmt.Fprintf(&b, "observed_at: %s\n", o.ObservedAt.UTC().Format(time.RFC3339))
	}
	writeKV(&b, "running_build_fingerprint", o.BuildFingerprint)
	if o.MaxActiveWorkRuns > 0 {
		fmt.Fprintf(&b, "max_active_work_runs: %d\n", o.MaxActiveWorkRuns)
	}
	if o.ResumesRunID == "" {
		b.WriteString("initial_continuation: none\n")
	} else {
		writeKV(&b, "initial_continuation_parent", o.ResumesRunID)
	}
	b.WriteString("These runtime facts do not establish the workspace's current code or external state. Prior answers describe their recorded turn, not current observations. Claim an action or continuation only from this Run's actual tool/control receipts; otherwise identify it as historical or unverified. Main decides which additional evidence the user's request needs.\n")
	return b.String()
}
