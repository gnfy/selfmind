package verification

// ClosureFacts is a bounded read projection, never a completion verdict or a
// tool input. Open steps remain Main's decisions; evidence is citeable only at
// this snapshot and acceptance revalidates it against subsequent changes.
type ClosureFacts struct {
	Available        bool               `json:"available"`
	PlanVersion      int                `json:"plan_version,omitempty"`
	OpenStepIDs      []string           `json:"open_step_ids,omitempty"`
	OpenStepsOmitted int                `json:"open_steps_omitted,omitempty"`
	Evidence         []CiteableEvidence `json:"evidence"`
	EvidenceOmitted  int                `json:"evidence_omitted,omitempty"`
}
