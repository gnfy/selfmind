package app

import "testing"

func TestWorkerCountParsesEnv(t *testing.T) {
	cases := map[string]int{
		"":    1,
		"1":   1,
		"4":   4,
		"16":  16,
		"99":  16, // capped
		"0":   1,  // invalid → default
		"-3":  1,
		"abc": 1,
		" 3 ": 3,
	}
	for in, want := range cases {
		t.Setenv("SELFMIND_WORKERS", in)
		if got := workerCount(); got != want {
			t.Fatalf("workerCount(%q) = %d, want %d", in, got, want)
		}
	}
}
