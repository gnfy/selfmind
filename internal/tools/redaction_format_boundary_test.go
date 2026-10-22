package tools

import (
	"encoding/xml"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestMultilineXMLPairOrderMasksBothSurfaces(t *testing.T) {
	for _, fields := range []string{
		"  key=\"DbPassword\"\n  other=\"ordinary\"\n  value=\"" + r5Secret + "\"",
		"  value=\"" + r5Secret + "\"\n  other=\"ordinary\"\n  key=\"DbPassword\"",
	} {
		in := "<add\n" + fields + "/>"
		var decoded struct {
			Value string `xml:"value,attr"`
			Key   string `xml:"key,attr"`
		}
		if err := xml.Unmarshal([]byte(in), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Value != r5Secret || decoded.Key != "DbPassword" {
			t.Fatal("fixture did not decode to the credential pair")
		}
		checkBothSurfaces(t, "XML field order", in, r5Secret)
		for surface, out := range map[string]string{"result": RedactSensitive(in), "stream": streamRedact(in)} {
			if !strings.Contains(out, `other="ordinary"`) {
				t.Errorf("%s masked the ordinary attribute", surface)
			}
		}
	}
}

func TestPairedYAMLBlockValuesMaskBothSurfaces(t *testing.T) {
	for _, tag := range []string{"", "!!str ", "!secret ", "!<tag:yaml.org,2002:str> "} {
		for _, fields := range []string{
			"- name: DB_PASSWORD\n  value: " + tag + "|\n    " + r5Secret,
			"- value: " + tag + "|\n    " + r5Secret + "\n  name: DB_PASSWORD",
			"- name: DB_PASSWORD\n  other: ordinary\n  value: " + tag + ">-\n    " + strings.Repeat("padding", 80) + "\n    " + r5Secret,
		} {
			in := fields + "\n- name: DB_HOST\n  value: ordinary-host"
			checkBothSurfaces(t, "paired block", in, r5Secret)
			for surface, out := range map[string]string{"result": RedactSensitive(in), "stream": streamRedact(in)} {
				if !strings.Contains(out, "ordinary-host") {
					t.Errorf("%s masked the ordinary sibling", surface)
				}
			}
		}
	}
	assertChainShows(t, "- name: DB_HOST\n  value: !!str |\n    ordinary-host", "- name: DB_HOST\n  value: !!str |\n    ordinary-host")
}

func TestUnknownValueFirstFieldsFailClosedAtTheStreamBound(t *testing.T) {
	for _, in := range []string{
		"<add\n  value=\"" + r5Secret + "\"\n  other=\"" + strings.Repeat("x", currentRedactor().secrets.maxHeld+streamHoldSlack+10) + "\"\n  key=\"DbPassword\"/>",
		"<add\n  value=\"" + r5Secret + "\"\n" + strings.Repeat("  other=\"x\"\n", maxHeldFieldLines+1) + "  key=\"DbPassword\"/>",
		"- value: " + r5Secret + "\n" + strings.Repeat("  # note\n", maxHeldFieldLines+1) + "  name: DB_PASSWORD",
	} {
		stream := newLineStreamRedactor()
		var shown []string
		for _, line := range strings.Split(in, "\n") {
			shown = append(shown, stream.push(line)...)
			if stream.heldBytes() > stream.hold.snapshot.maxHeld+streamHoldSlack {
				t.Fatal("unknown value retained output beyond the stream budget")
			}
		}
		shown = append(shown, stream.flush()...)
		if out := strings.Join(shown, "\n"); strings.Contains(out, r5Secret) {
			t.Fatal("unresolved value escaped when the stream stopped holding it")
		}
	}
	for _, in := range []string{
		"<add\n  value=\"ordinary\"\n  key=\"Timeout\"/>",
		"- value: ordinary\n  name: DB_HOST",
	} {
		assertChainShows(t, in, in)
	}
}

func TestHeldXMLValueUsesTheResolvedNameBeyondDetectionLookback(t *testing.T) {
	for _, other := range []string{"  other=\"" + strings.Repeat("x", 500) + "\"", "other=\"ordinary\""} {
		in := "<add\n  value=\"" + r5Secret + "\"\n" + other + "\nkey=\"DbPassword\"/>"
		if out := streamRedact(in); strings.Contains(out, r5Secret) {
			t.Fatal("resolved XML name did not mask the held value")
		}
	}
	for _, name := range []string{"DbPassword", "Timeout"} {
		value := r5Secret
		if name == "Timeout" {
			value = "ordinary"
		}
		in := "<add value=\"" + value + "\" other=\"" + strings.Repeat("x", 500) + "\" key=\"" + name + "\"/>"
		out := streamRedact(in)
		if name == "DbPassword" && strings.Contains(out, value) {
			t.Fatal("inline XML name did not mask the held value")
		}
		if name == "Timeout" && out != in {
			t.Fatal("inline ordinary XML value was masked")
		}
	}
	in := "<add\n  value=\"ordinary\"\n  other=\"" + strings.Repeat("x", 500) + "\"\n  key=\"Timeout\"/>"
	assertChainShows(t, in, in)
}

func TestPairedBlockNameAllowsCommentsAndTags(t *testing.T) {
	for _, name := range []string{"DB_PASSWORD # note", "!!str DB_PASSWORD # note", "\"DB_PASSWORD\" # note"} {
		in := "- name: " + name + "\n  value: !!str |\n    " + strings.Repeat("x", 560) + "\n    " + r5Secret
		checkBothSurfaces(t, "block name syntax", in, r5Secret)
	}
}

func TestFieldExpiryPreservesIndependentRegisteredSecretHold(t *testing.T) {
	secret := "value: synthetic-registry-start\n" + strings.Repeat("  # synthetic-middle\n", maxHeldFieldLines+1) + "registry-end"
	RegisterSensitiveValue(secret)
	stream := newLineStreamRedactor()
	lines := strings.Split(secret, "\n")
	for _, line := range lines[:len(lines)-1] {
		if out := stream.push(line); len(out) != 0 {
			t.Fatal("expired field released a still-matching registered secret")
		}
	}
	out := strings.Join(append(stream.push(lines[len(lines)-1]), stream.flush()...), "\n")
	if strings.Contains(out, "synthetic-registry-start") || strings.Contains(out, "synthetic-middle") || strings.Contains(out, "registry-end") {
		t.Fatal("registered secret was not masked when its final line arrived")
	}
}

func TestTaggedYAMLValuesUseTheSameGrammarAtBothSurfaces(t *testing.T) {
	for _, tag := range []string{"!!str", "!secret", "!<tag:yaml.org,2002:str>"} {
		t.Run(tag, func(t *testing.T) {
			for _, in := range []string{
				"client_secret: " + tag + " " + r5Secret,
				"- name: DB_PASSWORD\n  other: 1\n  value: " + tag + " " + r5Secret,
				"- value: " + tag + " '" + r5Secret + "'\n  name: DB_PASSWORD",
			} {
				checkBothSurfaces(t, "tagged scalar", in, r5Secret)
			}
			for _, in := range []string{
				"password: " + tag + " ${DB_PASSWORD}",
				"- name: DB_PASSWORD\n  value: " + tag + " ${DB_PASSWORD}",
				"description: " + tag + " ordinary-value",
			} {
				assertChainShows(t, in, in)
			}
		})
	}
}

func TestTaggedBlockStateOutlivesTheStreamLookback(t *testing.T) {
	for _, tag := range []string{"", "!!str ", "!secret ", "!<tag:yaml.org,2002:str> "} {
		for _, indicator := range []string{"|", ">-"} {
			in := "password: " + tag + indicator + "\n  " + strings.Repeat("ordinary-data", 40) + "\n  " + r5Secret + "\nnext: visible"
			checkBothSurfaces(t, "tagged block", in, r5Secret)
			stream := newLineStreamRedactor()
			var shown []string
			for _, line := range strings.Split(in, "\n") {
				shown = append(shown, stream.push(line)...)
				if stream.heldBytes() != 0 || stream.lookbackLen() > streamLookbackBytes+64 {
					t.Fatalf("block scalar retained its content: held=%d lookback=%d", stream.heldBytes(), stream.lookbackLen())
				}
			}
			shown = append(shown, stream.flush()...)
			if !strings.Contains(strings.Join(shown, "\n"), "next: visible") {
				t.Fatal("block state masked the sibling key")
			}
		}
	}
}

func TestBlockScalarHashIsLiteralContent(t *testing.T) {
	const in = "password: |\n  # synthetic-password\nnext: visible"
	var decoded map[string]string
	if err := yaml.Unmarshal([]byte(in), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["password"] != "# synthetic-password\n" {
		t.Fatalf("fixture did not decode to literal content: %q", decoded["password"])
	}
	checkBothSurfaces(t, "block content", in, "synthetic-password")
	assertChainShows(t, "password: | # header comment\nnext: visible", "password: | # header comment\nnext: visible")
}

func TestBlockScalarProtectionDoesNotExpireWithItsSize(t *testing.T) {
	// This scalar needs no lookahead: indentation determines its end. Its
	// protection must not expire at the bound for searching a closing quote.
	in := "password: !!str |\n" + strings.Repeat("  "+strings.Repeat("x", 128)+"\n", maxQuotedLiteral/128+1) + "  " + r5Secret + "\nnext: visible"
	for surface, out := range map[string]string{"result": RedactSensitive(in), "stream": streamRedact(in)} {
		if strings.Contains(out, r5Secret) {
			t.Errorf("%s stopped masking inside the scalar", surface)
		}
		if !strings.Contains(out, "next: visible") {
			t.Errorf("%s did not stop at the sibling", surface)
		}
	}
}
