package control

import "testing"

// The instant the work was admitted is what a later tightening of the person's
// mode is measured against, so a snapshot that carries a nonsensical one is not
// recorded, and one that carries none (zero: unknown) stays valid.
func TestAdmissionTimeIsRecordedOnlyWhenItMeansAMoment(t *testing.T) {
	admission := verifyOnlyCronAdmission()
	admission.AdmittedAt = -1
	if _, _, err := encodeAdmissionSnapshot(admission); err == nil {
		t.Fatal("a negative admission time must not be recorded")
	}
	admission.AdmittedAt = 0
	if _, raw, err := encodeAdmissionSnapshot(admission); err != nil || raw == "" {
		t.Fatalf("an unknown admission time stays valid: %q %v", raw, err)
	}
}
