package tools

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// nearestParameterLimit bounds the hint: when more declared names than this
// look close, the unknown name points at nothing in particular.
const nearestParameterLimit = 3

// unknownParameterError refuses a name the schema does not declare. When the
// name is close to declared ones it asks whether one of them was meant. It
// never renames the argument for the model: near names can differ in meaning
// (reason is not cancellation_reason), and only the caller knows which it
// intended.
func unknownParameterError(path, name string, properties map[string]PropertyDef) error {
	hint := ""
	if near := nearestParameterNames(name, properties); len(near) > 0 {
		hint = "did you mean " + joinChoices(near) + "? "
	}
	return fmt.Errorf("unknown parameter: %s — %sthis object accepts only %s", joinParameterPath(path, name), hint, acceptedParameters(properties))
}

// nearestParameterNames returns the declared names an unknown name most likely
// meant, sorted. Three tiers, most specific first: the same name apart from
// case and separators; names whose words line up one for one with the unknown
// name's, allowing a typo or plural (step_ids, step_id); names that contain a
// near match for every significant word of it (criterion, success_criteria).
// It is lexical only; synonyms get no hint, and more matches than the limit
// means the name points at nothing in particular.
func nearestParameterNames(unknown string, properties map[string]PropertyDef) []string {
	words := parameterWords(unknown)
	if len(words) == 0 || utf8.RuneCountInString(unknown) > 48 {
		return nil
	}
	flat := strings.Join(words, "")
	var aligned, containing []string
	for name := range properties {
		other := parameterWords(name)
		if strings.Join(other, "") == flat {
			return []string{name}
		}
		switch {
		case wordsAligned(words, other):
			aligned = append(aligned, name)
		case wordsNear(words, other):
			containing = append(containing, name)
		}
	}
	near := containing
	if len(aligned) > 0 {
		near = aligned
	}
	if len(near) > nearestParameterLimit {
		return nil
	}
	sort.Strings(near)
	return near
}

// parameterWords splits a parameter name into lower-case words at separators
// and at camelCase boundaries.
func parameterWords(name string) []string {
	var words []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	var previous rune
	for _, r := range name {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(previous):
			flush()
			current = append(current, r)
		default:
			current = append(current, r)
		}
		previous = r
	}
	flush()
	return words
}

// wordsNear reports that every significant (four or more letters) word of the
// unknown name has a near word in the candidate.
func wordsNear(unknown, candidate []string) bool {
	significant := 0
	for _, word := range unknown {
		if utf8.RuneCountInString(word) < 4 {
			continue
		}
		significant++
		found := false
		for _, other := range candidate {
			if nearWord(word, other) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return significant > 0
}

// wordsAligned reports that both names have the same number of words and each
// pair is equal, a typo or a plural of the other.
func wordsAligned(unknown, candidate []string) bool {
	if len(unknown) != len(candidate) {
		return false
	}
	for i := range unknown {
		if unknown[i] != candidate[i] && !nearWord(unknown[i], candidate[i]) && unknown[i]+"s" != candidate[i] && candidate[i]+"s" != unknown[i] {
			return false
		}
	}
	return true
}

// nearWord is a typo or plural distance: equal, a prefix of a word of at least
// five letters (reason, reasoning), or one edit (two for words of seven
// letters or more, as criterion and criteria).
func nearWord(a, b string) bool {
	if a == b {
		return true
	}
	shorter := min(utf8.RuneCountInString(a), utf8.RuneCountInString(b))
	if shorter < 4 {
		return false
	}
	if shorter >= 5 && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a)) {
		return true
	}
	budget := 1
	if shorter >= 7 {
		budget = 2
	}
	return editDistance(a, b) <= budget
}

func editDistance(a, b string) int {
	left, right := []rune(a), []rune(b)
	previous := make([]int, len(right)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(left); i++ {
		current := make([]int, len(right)+1)
		current[0] = i
		for j := 1; j <= len(right); j++ {
			cost := 1
			if left[i-1] == right[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(right)]
}

func joinChoices(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
	}
}
