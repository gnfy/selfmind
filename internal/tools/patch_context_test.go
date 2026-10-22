package tools

import "testing"

func TestPatchBlankContextPreservesCanonicalAndImplicitHunks(t *testing.T) {
	for _, context := range []string{" alpha\n \n beta", "alpha\n\nbeta"} {
		ops, err := parseV4APatch("*** Begin Patch\n*** Update File: file.txt\n@@\n" + context + "\n-old\n+new\n*** End Patch")
		if err != nil || len(ops) != 1 || len(ops[0].Hunks) != 1 || len(ops[0].Hunks[0].Lines) != 5 {
			t.Fatalf("blank context was lost: %+v %v", ops, err)
		}
		line := ops[0].Hunks[0].Lines[1]
		if line.Content != "" {
			t.Fatalf("blank context changed: %+v", line)
		}
	}
}
