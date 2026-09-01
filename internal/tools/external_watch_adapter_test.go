package tools

import "testing"

func TestStatusJSONExternalWatchAdapter(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  ExternalWatchObservationState
	}{
		{`{"status":"pending","detail":"still running"}`, ExternalWatchObservationPending},
		{`{"status":"succeeded"}`, ExternalWatchObservationSucceeded},
		{`{"status":"failed"}`, ExternalWatchObservationFailed},
	} {
		got, err := ClassifyExternalWatchObservation(ExternalWatchAdapterStatusJSON, tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("input=%s got=%q err=%v", tc.input, got, err)
		}
	}
	for _, input := range []string{`{"status":"unknown"}`, `not-json`} {
		if _, err := ClassifyExternalWatchObservation(ExternalWatchAdapterStatusJSON, input); err == nil {
			t.Fatalf("invalid adapter output accepted: %s", input)
		}
	}
}
