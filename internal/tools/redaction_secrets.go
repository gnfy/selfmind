package tools

import (
	"sort"
	"strings"
)

// secretSnapshot is an immutable view of the registered secrets.
type secretSnapshot struct {
	secrets []registeredSecret
	// compact are the secrets that are JSON objects or arrays.
	compact []compactSecret
	// multiline are the raw forms holding a line break, which a stream that
	// delivers one line at a time can only see whole across lines.
	multiline []multilineSecret
	// maxHeld bounds the bytes a stream holds while such a secret may be
	// printing; maxSkip is the most leading punctuation lines of any.
	maxHeld int
	maxSkip int
}

// multilineSecret is a registered secret that spans lines, one string per
// line. A stream sees lines without their line endings, so only the LF form
// takes part.
type multilineSecret struct {
	lines []string
	// skip is how many leading lines hold nothing but JSON punctuation (the
	// { and [ a pretty-printed secret opens with). A stream does not wait
	// for the rest of the secret because of them: a match is looked for from
	// the first line that holds more, with the lines before it as history.
	skip int
}

func newMultilineSecret(form string) multilineSecret {
	secret := multilineSecret{lines: strings.Split(form, "\n")}
	for secret.skip < len(secret.lines)-2 && strings.Trim(secret.lines[secret.skip], jsonStructure+" \t") == "" {
		secret.skip++
	}
	return secret
}

type compactSecret struct {
	index int
	text  string
	// border is the KMP failure function of text: border[i] is the length
	// of the longest proper prefix of text[:i+1] that is also its suffix.
	border []int
	// lead is the length of text's leading JSON punctuation. A stream holds
	// lines only once more than that matches, so a line that merely ends
	// with { does not wait for the next one.
	lead int
	// limit bounds the bytes any layout of the secret can take: its own
	// length and a generous line break and indentation per structural
	// character.
	limit int
}

func newCompactSecret(index int, text string) compactSecret {
	lead := 0
	for lead < len(text) && strings.IndexByte(jsonStructure, text[lead]) >= 0 {
		lead++
	}
	structural := 1
	for i := 0; i < len(text); i++ {
		if strings.IndexByte(jsonStructure, text[i]) >= 0 {
			structural++
		}
	}
	return compactSecret{index: index, text: text, border: borders(text), lead: lead, limit: len(text) + 80*structural}
}

func newSecretSnapshot(values map[string]secretRule) *secretSnapshot {
	snapshot := &secretSnapshot{secrets: make([]registeredSecret, 0, len(values))}
	for value, rule := range values {
		snapshot.secrets = append(snapshot.secrets, registeredSecret{value: value, rule: rule})
	}
	sort.Slice(snapshot.secrets, func(i, j int) bool { return snapshot.secrets[i].value < snapshot.secrets[j].value })
	for i, secret := range snapshot.secrets {
		if secret.rule.compact != "" {
			compact := newCompactSecret(i, secret.rule.compact)
			snapshot.compact = append(snapshot.compact, compact)
			snapshot.maxHeld = max(snapshot.maxHeld, compact.limit)
		}
		for _, form := range secret.rule.forms {
			if form.raw && strings.Contains(form.text, "\n") && !strings.Contains(form.text, "\r") {
				multiline := newMultilineSecret(form.text)
				snapshot.multiline = append(snapshot.multiline, multiline)
				snapshot.maxHeld = max(snapshot.maxHeld, len(form.text))
				snapshot.maxSkip = max(snapshot.maxSkip, multiline.skip)
			}
		}
	}
	return snapshot
}

// spans finds every occurrence of every registered secret in text, and
// reports which secrets matched as written and which only in an escaped form.
//
// A secret made only of letters, digits, '_' and '-' is masked only at
// identifier boundaries, so a short value such as a tool or account name does
// not corrupt a longer resource name that merely contains it. Overlapping or
// touching occurrences of such secrets count as one run, whose ends must be
// boundaries; the edge of another secret's occurrence is a boundary too.
func (s *secretSnapshot) spans(text string) ([]maskSpan, []string, []string) {
	if s == nil || len(s.secrets) == 0 || text == "" {
		return nil, nil, nil
	}
	type identifierSpan struct {
		maskSpan
		secret int
	}
	var spans []maskSpan
	var identifiers []identifierSpan
	matchedRaw := make([]bool, len(s.secrets))
	matchedEscaped := make([]bool, len(s.secrets))
	for i, secret := range s.secrets {
		for _, form := range secret.rule.forms {
			for at := 0; ; {
				j := strings.Index(text[at:], form.text)
				if j < 0 {
					break
				}
				j += at
				at = j + 1
				span := maskSpan{j, j + len(form.text)}
				if secret.rule.identifierLike {
					identifiers = append(identifiers, identifierSpan{span, i})
					continue
				}
				spans = append(spans, span)
				matchedRaw[i] = matchedRaw[i] || form.raw
				matchedEscaped[i] = matchedEscaped[i] || !form.raw
			}
		}
	}
	if len(s.compact) > 0 && strings.ContainsAny(text, "{[") {
		view := whitespaceFreeView(text)
		for _, secret := range s.compact {
			for at := 0; ; {
				j := strings.Index(view.text[at:], secret.text)
				if j < 0 {
					break
				}
				j += at
				at = j + 1
				spans = append(spans, view.original(maskSpan{j, j + len(secret.text)}))
				matchedRaw[secret.index] = true
			}
		}
	}
	if len(identifiers) > 0 {
		hard := mergeSpans(append([]maskSpan(nil), spans...))
		sort.Slice(identifiers, func(i, j int) bool { return identifiers[i].start < identifiers[j].start })
		boundaryBefore := func(at int) bool {
			return at == 0 || !isIdentifierByte(text[at-1]) || escapeBefore(text[:at]) || covered(hard, at-1)
		}
		boundaryAfter := func(at int) bool {
			return at == len(text) || !isIdentifierByte(text[at]) || covered(hard, at)
		}
		for i := 0; i < len(identifiers); {
			run := identifiers[i].maskSpan
			members := []int{identifiers[i].secret}
			for i++; i < len(identifiers) && identifiers[i].start <= run.end; i++ {
				run.end = max(run.end, identifiers[i].end)
				members = append(members, identifiers[i].secret)
			}
			if !boundaryBefore(run.start) || !boundaryAfter(run.end) {
				continue
			}
			spans = append(spans, run)
			for _, member := range members {
				matchedRaw[member] = true
			}
		}
	}
	var raw, escaped []string
	for i, secret := range s.secrets {
		switch {
		case matchedRaw[i]:
			raw = append(raw, secret.value)
		case matchedEscaped[i]:
			escaped = append(escaped, secret.value)
		}
	}
	return spans, raw, escaped
}

func borders(pattern string) []int {
	border := make([]int, len(pattern))
	for i, k := 1, 0; i < len(pattern); i++ {
		for k > 0 && pattern[i] != pattern[k] {
			k = border[k-1]
		}
		if pattern[i] == pattern[k] {
			k++
		}
		border[i] = k
	}
	return border
}
