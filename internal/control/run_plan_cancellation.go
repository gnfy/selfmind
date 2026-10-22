package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RunPlanCancellationDeferral is a requested cancellation the runtime kept
// open, with the one fact it could not find. Main judges coverage; naming the
// missing fact is what makes the repair exact instead of a generic recipe.
type RunPlanCancellationDeferral struct {
	Step   RunPlanStep `json:"step"`
	Reason string      `json:"reason"`
	// Criterion is the step's established acceptance bar; empty when the
	// step was planned without one.
	Criterion         string       `json:"criterion,omitempty"`
	ReplacementStepID string       `json:"replacement_step_id,omitempty"`
	Replacement       *RunPlanStep `json:"replacement,omitempty"`
}

// Why a requested cancellation remains pending.
const (
	CancellationDeferredNoDisposition       = "no_disposition"
	CancellationDeferredUnfinished          = "unfinished"
	CancellationDeferredNoCoverage          = "no_coverage"
	CancellationDeferredReplacementUnknown  = "replacement_unknown"
	CancellationDeferredReplacementOpen     = "replacement_open"
	CancellationDeferredCriterionChanged    = "criterion_changed"
	CancellationDeferredCriterionUnstated   = "criterion_unstated"
	CancellationDeferredVerificationDropped = "verification_dropped"
	CancellationDeferredOmitted             = "omitted"
)

type planCancellationPreconditionError struct{ message string }

func (e *planCancellationPreconditionError) Error() string                    { return e.message }
func (*planCancellationPreconditionError) PlanCancellationPrecondition() bool { return true }

func assessedCancellation(step RunPlanStepInput) bool {
	return strings.TrimSpace(step.CancellationReason) != "" && (step.CancellationDisposition == "not_required" || (step.CancellationDisposition == "user_takeover" && strings.TrimSpace(step.UserTakeoverQuote) != ""))
}

func assessedCancellationForContract(step RunPlanStepInput, version int) bool {
	if !assessedCancellation(step) {
		return false
	}
	return version < scopeEvidencePlanContractVersion || step.CancellationDisposition != "not_required" ||
		step.ReplacementStepID != "" || step.ScopeChangeQuote != ""
}

func retainOmittedOpenSteps(steps []RunPlanStep, previous *RunPlan) ([]RunPlanStep, []RunPlanStep) {
	if previous == nil {
		return steps, nil
	}
	seen := map[string]bool{}
	for _, step := range steps {
		seen[step.StepID] = true
	}
	var retained []RunPlanStep
	for _, old := range previous.Steps {
		if seen[old.StepID] || old.Status == "completed" || old.Status == "cancelled" {
			continue
		}
		// Retain the exact obligation, without creating a second active step.
		old.Status, old.Sequence = "pending", len(steps)+1
		// The current snapshot owns coarse work-unit boundaries. Retaining a
		// missing obligation is not a second declaration of its previous unit;
		// the stable step id and earlier snapshots preserve that attribution.
		old.WorkUnitID, old.WorkUnit = "", false
		steps = append(steps, old)
		retained = append(retained, old)
	}
	return steps, retained
}

// Main owns necessity and takeover meaning; the runtime owns durable state and
// the provenance of the quoted user instruction. Missing judgment cannot erase
// an obligation. This normalization is part of the existing Plan transaction.
// A quote it validates leaves the identity of the input it matched on the
// step; keepCancellationQuoteSources decides what the snapshot records.
func assessRunPlanCancellationsTx(ctx context.Context, tx *sql.Tx, tenant, run string, version int, steps []RunPlanStep) ([]RunPlanCancellationDeferral, error) {
	if version < assessedPlanCancellationContractVersion {
		return nil, nil
	}
	established, err := establishedPlanCriteriaTx(ctx, tx, tenant, run)
	if err != nil {
		return nil, err
	}
	var deferred []RunPlanCancellationDeferral
	for i := range steps {
		s := &steps[i]
		s.CancellationDisposition = strings.TrimSpace(s.CancellationDisposition)
		s.CancellationReason = strings.TrimSpace(s.CancellationReason)
		s.UserTakeoverQuote = strings.TrimSpace(s.UserTakeoverQuote)
		s.ReplacementStepID = strings.TrimSpace(s.ReplacementStepID)
		s.ScopeChangeQuote = strings.TrimSpace(s.ScopeChangeQuote)
		if s.Status != "cancelled" {
			if s.Status != "pending" || s.CancellationDisposition != "unfinished" {
				s.CancellationDisposition, s.CancellationReason = "", ""
			}
			s.UserTakeoverQuote, s.ReplacementStepID, s.ScopeChangeQuote = "", "", ""
			continue
		}
		switch s.CancellationDisposition {
		case "", "unfinished":
			reason := CancellationDeferredUnfinished
			if s.CancellationDisposition == "" {
				reason = CancellationDeferredNoDisposition
			}
			s.Status = "pending"
			s.CancellationDisposition = "unfinished"
			deferred = append(deferred, RunPlanCancellationDeferral{Step: *s, Reason: reason, Criterion: establishedCriterion(*s, established)})
		case "not_required", "user_takeover":
			if !assessedCancellation(s.RunPlanStepInput) {
				return nil, &planCancellationPreconditionError{fmt.Sprintf("step %s cancellation requires a reason and user_takeover requires an exact user quote", s.StepID)}
			}
			if s.CancellationDisposition == "not_required" && version >= scopeEvidencePlanContractVersion {
				gap, source, err := cancellationCoverageTx(ctx, tx, tenant, run, *s, steps, established)
				if err != nil {
					return nil, err
				}
				s.CancellationQuoteSource = source
				if gap.Reason != "" {
					s.Status, s.CancellationDisposition = "pending", "unfinished"
					s.ReplacementStepID, s.ScopeChangeQuote = "", ""
					gap.Step = *s
					deferred = append(deferred, gap)
				}
			}
			if s.CancellationDisposition == "user_takeover" {
				source, err := requireLineageQuoteTx(ctx, tx, tenant, run, s.StepID, "user_takeover_quote", s.UserTakeoverQuote)
				if err != nil {
					return nil, err
				}
				s.CancellationQuoteSource = source
			}
		default:
			return nil, &planCancellationPreconditionError{fmt.Sprintf("step %s has an invalid cancellation_disposition", s.StepID)}
		}
	}
	return deferred, nil
}

// cancellationCoverageTx returns the deferral for an uncovered not_required
// cancellation, or one with an empty Reason when the evidence it cites holds,
// and the identity of the input a scope_change_quote matched.
// Main explains whether that evidence covers the goal; this transaction checks
// the referenced state, the established criterion, and actual user-input
// provenance. No command name or wording of a failure decides necessity.
//
// A replacement keeps the step's established criterion and its required
// verification. A step planned without a criterion has no bar to repeat, so a
// completed replacement that states its own criterion covers it.
func cancellationCoverageTx(ctx context.Context, tx *sql.Tx, tenant, run string, step RunPlanStep, steps []RunPlanStep, established map[string]string) (RunPlanCancellationDeferral, string, error) {
	if step.ScopeChangeQuote != "" {
		source, err := requireLineageQuoteTx(ctx, tx, tenant, run, step.StepID, "scope_change_quote", step.ScopeChangeQuote)
		return RunPlanCancellationDeferral{}, source, err
	}
	gap := RunPlanCancellationDeferral{Criterion: establishedCriterion(step, established), ReplacementStepID: step.ReplacementStepID}
	if step.ReplacementStepID == "" {
		gap.Reason = CancellationDeferredNoCoverage
		return gap, "", nil
	}
	for i := range steps {
		if steps[i].StepID == step.ReplacementStepID && steps[i].StepID != step.StepID {
			replacement := steps[i]
			gap.Replacement = &replacement
			break
		}
	}
	switch replacement := gap.Replacement; {
	case replacement == nil:
		gap.Reason = CancellationDeferredReplacementUnknown
	case replacement.Status != "completed":
		gap.Reason = CancellationDeferredReplacementOpen
	case gap.Criterion == "" && strings.TrimSpace(replacement.SuccessCriteria) == "":
		gap.Reason = CancellationDeferredCriterionUnstated
	case gap.Criterion != "" && normalizeRunPlanText(gap.Criterion) != normalizeRunPlanText(replacement.SuccessCriteria):
		gap.Reason = CancellationDeferredCriterionChanged
	case step.VerificationRequired && !replacement.VerificationRequired:
		gap.Reason = CancellationDeferredVerificationDropped
	}
	return gap, "", nil
}

// establishedCriterion is the bar a step's replacement must keep. A step never
// stored with a criterion is held to what this snapshot declares, if anything.
func establishedCriterion(step RunPlanStep, established map[string]string) string {
	if criterion := established[step.StepID]; criterion != "" {
		return criterion
	}
	return strings.TrimSpace(step.SuccessCriteria)
}

// requireLineageQuoteTx checks the provenance of a quoted user instruction, for
// a takeover and a scope change alike. The quote is copied exactly from ONE
// actual user input on this exact work lineage: that whole input, one whole
// sentence of it, or a partial span long enough not to occur by coincidence. A
// complete instruction is often short ("不用了", "I'll do it"), while "the" cut
// from "do the work" carries none. Main still judges the meaning. It returns the
// identity of the input the quote matched.
func requireLineageQuoteTx(ctx context.Context, tx *sql.Tx, tenant, run, stepID, field, quote string) (string, error) {
	source, err := lineageInputQuotesTx(ctx, tx, tenant, run, quote)
	if err != nil {
		return "", err
	}
	if source == "" {
		return "", &planCancellationPreconditionError{fmt.Sprintf("step %s %s must copy exactly one whole user message, one whole sentence of it, or a word-aligned span of at least two words and eight letters (a CJK character counts twice), from one actual user input in this work lineage", stepID, field)}
	}
	return source, nil
}

// quotedInput reports whether quote is evidence taken from one user input. A
// whole message or sentence may be short; only a partial span must clear the
// length floor. Main decides whether the sourced text changes the goal;
// punctuation does not decide that meaning. Code offers nothing whole.
// Text that may have been cut offers no whole message and no last sentence,
// and a partial span may not end where it was cut.
func quotedInput(content, quote string, cut bool) bool {
	segments := segmentInput(content)
	if whole := trimClosing(quote); whole != "" {
		for i, segment := range segments {
			if segment.code || (cut && i == len(segments)-1) {
				continue
			}
			if trimClosing(content[segment.start:segment.end]) == whole {
				return true
			}
		}
		if candidate, ok := wholeQuotable(content, segments); ok && !cut && candidate == whole {
			return true
		}
	}
	return meaningfulQuote(quote) && quotedSpan(content, quote, cut)
}

// closingPunctuation may surround a whole message or sentence, so "No need."
// and "不用了……" quote as "No need" and "不用了". These are source-shape
// rules; Main decides whether a polite question is an instruction.
const closingPunctuation = ".!?;。！？；…~～\"'“”‘’「」『』"

func trimClosing(text string) string {
	return strings.TrimFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(closingPunctuation, r)
	})
}

// wholeQuotable is the text a whole message offers as a short quote. A
// message without prose offers none.
func wholeQuotable(content string, segments []inputSegment) (string, bool) {
	prose := false
	for _, segment := range segments {
		prose = prose || !segment.code
	}
	trimmed := trimClosing(content)
	return trimmed, prose && trimmed != ""
}

// closingMarks may follow a sentence's own end mark: brackets, quotes,
// emphasis, code and a trailing ellipsis, as in (why?), "skip it?", **stop?**,
// _stop?_, `stop?`, <stop?>, stop?… and 「要停吗？」.
const closingMarks = ")]}\"'*_`>…”’」』》）】"

// queryMark reports whether the "?" at runes[i] opens a URL query: inside a
// word, it precedes a key=value pair (job?id=5), or a letter or digit follows
// it in a word that holds "://" or starts with "www." (https://x.com/s?q).
// One that ends a word still asks.
func queryMark(runes []rune, i int) bool {
	if i == 0 || unicode.IsSpace(runes[i-1]) || i+1 == len(runes) {
		return false
	}
	start := i
	for start > 0 && !unicode.IsSpace(runes[start-1]) {
		start--
	}
	word, next := string(runes[start:i]), runes[i+1]
	if (strings.Contains(word, "://") || strings.HasPrefix(word, "www.")) && (unicode.IsLetter(next) || unicode.IsDigit(next)) {
		return true
	}
	key := i + 1
	for key < len(runes) && (unicode.IsLetter(runes[key]) || unicode.IsDigit(runes[key]) || strings.ContainsRune("_.-%[]", runes[key])) {
		key++
	}
	return key > i+1 && key < len(runes) && runes[key] == '='
}

// inputSegment is one sentence or code line of a user input, as byte offsets.
type inputSegment struct {
	start, end int
	code       bool
}

// segmentInput splits one input into sentences and code lines. Sentences end
// at line breaks and sentence punctuation, after any closing marks that follow
// it; an ASCII mark ends one only before space or the end, so "v1.2" stays one
// word and "job?id=5" one sentence, except that a question mark also ends one
// before unspaced CJK text unless it opens a URL query. A comma ends a clause,
// not a sentence. Fenced (``` or ~~~) and indented lines are code.
func segmentInput(content string) []inputSegment {
	var segments []inputSegment
	fenced := false
	for lineStart := 0; lineStart < len(content); {
		lineEnd := strings.IndexAny(content[lineStart:], "\r\n")
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += lineStart
		}
		line := content[lineStart:lineEnd]
		trimmed := strings.TrimSpace(line)
		fence := strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
		indented := strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
		switch {
		case trimmed == "":
		case fence || fenced || indented:
			segments = append(segments, inputSegment{start: lineStart, end: lineEnd, code: true})
		default:
			segments = appendLineSentences(segments, content, lineStart, lineEnd)
		}
		if fence {
			fenced = !fenced
		}
		lineStart = lineEnd + 1
	}
	return segments
}

func appendLineSentences(segments []inputSegment, content string, start, end int) []inputSegment {
	line := content[start:end]
	var runes []rune
	var offsets []int
	for offset, r := range line {
		runes = append(runes, r)
		offsets = append(offsets, offset)
	}
	offsets = append(offsets, len(line))
	from := 0
	add := func(to int) {
		text := content[start+offsets[from] : start+offsets[to]]
		if trimmed := trimClosing(text); trimmed != "" {
			segments = append(segments, inputSegment{start: start + offsets[from], end: start + offsets[to]})
		}
		from = to
	}
	for i := 0; i < len(runes); i++ {
		if end, ok := sentenceEnd(runes, i); ok {
			add(end)
			i = end - 1
		}
	}
	add(len(runes))
	return segments
}

// sentenceEnd reports whether a sentence ends at runes[i], and where: after
// the closing marks that follow its end mark.
func sentenceEnd(runes []rune, i int) (int, bool) {
	end := i + 1
	for end < len(runes) && strings.ContainsRune(closingMarks, runes[end]) {
		end++
	}
	free := end == len(runes) || unicode.IsSpace(runes[end])
	switch runes[i] {
	case '。', '！', '？', '；':
		return end, true
	case '!', ';':
		return end, free
	case '?':
		// CJK text has no spaces between sentences.
		return end, free || (cjkRune(runes[end]) && !queryMark(runes, i))
	case '.':
		return end, free && !abbreviationBefore(runes[:i])
	}
	return 0, false
}

var sentenceAbbreviations = map[string]bool{
	"etc": true, "vs": true, "mr": true, "mrs": true, "ms": true, "dr": true,
	"st": true, "jr": true, "sr": true, "cf": true, "approx": true,
}

// abbreviationBefore reports whether the word ending at a period is a number
// ("1."), a single letter or dotted form ("e.g."), or a common abbreviation;
// none of them ends a sentence.
func abbreviationBefore(runes []rune) bool {
	start := len(runes)
	for start > 0 && !unicode.IsSpace(runes[start-1]) {
		start--
	}
	word := strings.ToLower(string(runes[start:]))
	if word == "" {
		return false
	}
	if strings.ContainsRune(word, '.') || utf8.RuneCountInString(word) == 1 || sentenceAbbreviations[word] {
		return true
	}
	for _, r := range word {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

const minQuoteWeight = 8

// meaningfulQuote is the floor for a partial span: two words and a rune-length
// floor. Unspaced CJK text is dense, so each CJK rune is a word and counts
// twice toward the floor.
func meaningfulQuote(quote string) bool {
	words, weight, inWord := 0, 0, false
	for _, r := range quote {
		switch {
		case cjkRune(r):
			words, weight, inWord = words+1, weight+2, false
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if !inWord {
				words++
			}
			weight, inWord = weight+1, true
		default:
			inWord = false
		}
	}
	return words >= 2 && weight >= minQuoteWeight
}

func cjkRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// wordRune continues a spaced word. A CJK rune is a word edge by itself.
func wordRune(r rune) bool {
	return !cjkRune(r) && (unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || r == '_')
}

// quotedSpan reports whether quote occurs in content without continuing a
// word at either edge and, in cut text, before the cut.
func quotedSpan(content, quote string, cut bool) bool {
	first, _ := utf8.DecodeRuneInString(quote)
	last, _ := utf8.DecodeLastRuneInString(quote)
	for offset := 0; offset < len(content); {
		index := strings.Index(content[offset:], quote)
		if index < 0 {
			return false
		}
		start, end := offset+index, offset+index+len(quote)
		before, _ := utf8.DecodeLastRuneInString(content[:start])
		after, _ := utf8.DecodeRuneInString(content[end:])
		aligned := !(wordRune(first) && wordRune(before)) && !(wordRune(last) && wordRune(after))
		if aligned && !(cut && end == len(content)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(content[start:])
		offset = start + size
	}
	return false
}

// lineageInputQuotesTx returns the identity of the input quote was copied
// from, as a cancellation quote source, or "" when no input holds it. When
// several inputs hold it, the earliest on the lineage wins: an earlier Run's
// inputs before a later Run's, and within one Run its request before the
// guidance it consumed, guidance in arrival order. A request recorded twice is
// read from the start-event text the person sent before the Run's bounded
// input summary.
func lineageInputQuotesTx(ctx context.Context, tx *sql.Tx, tenant, run, quote string) (string, error) {
	// Each row is one user input. Traverse only exact continuation edges for
	// the same person, never a Thread label, another open task, tool prose, or
	// an unconsumed mailbox row.
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE lineage(id,person_id,input_summary,depth) AS (
		SELECT id,person_id,input_summary,0 FROM runs WHERE tenant_id=? AND id=?
		UNION ALL SELECT p.id,p.person_id,p.input_summary,l.depth+1 FROM lineage l
		JOIN runs c ON c.id=l.id JOIN runs p ON p.id=c.resumes_run_id
		WHERE p.tenant_id=? AND p.person_id=l.person_id AND l.depth<32
	)
	SELECT kind, source_id, content, incomplete FROM (
		SELECT 'run_input' AS kind, l.id AS source_id, l.input_summary AS content, 0 AS incomplete,
			l.depth AS depth, 1 AS precedence, 0 AS arrived, 0 AS sequence
		FROM lineage l WHERE NOT EXISTS (
			SELECT 1 FROM task_events e WHERE e.run_id=l.id AND e.type='run.started'
			AND COALESCE(NULLIF(json_extract(e.payload_json,'$.origin'),''),'user')<>'user')
		UNION ALL SELECT 'run_started', l.id, json_extract(e.payload_json,'$.approval_intent.snapshot.raw_user_text'),
			COALESCE(json_extract(e.payload_json,'$.approval_intent.snapshot.authorization_evidence_incomplete'),0),
			l.depth, 0, 0, e.cursor
		FROM task_events e JOIN lineage l ON l.id=e.run_id WHERE e.type='run.started'
			AND COALESCE(NULLIF(json_extract(e.payload_json,'$.origin'),''),'user')='user'
			AND COALESCE(json_extract(e.payload_json,'$.approval_intent.snapshot.source'),'') IN ('','direct','continuation')
			AND json_extract(e.payload_json,'$.approval_intent.snapshot.raw_user_text') IS NOT NULL
		UNION ALL SELECT 'steering', m.id, m.content, 0, l.depth, 2, m.created_at, m.rowid
		FROM steering_mailbox m JOIN lineage l ON l.id=m.run_id
		WHERE m.tenant_id=? AND m.person_id=l.person_id AND m.status='consumed'
	) ORDER BY depth DESC, precedence, arrived, sequence`, tenant, run, tenant, tenant)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id, content string
		var incomplete bool
		if err := rows.Scan(&kind, &id, &content, &incomplete); err != nil {
			return "", err
		}
		// Either source may be cut without the person's sentence ending there:
		// a Run summary at a byte limit, marked "...", and start-event text at
		// its evidence bound, marked only by incomplete evidence.
		cut := incomplete
		if kind == CancellationQuoteFromRunInput && strings.HasSuffix(content, "...") {
			content, cut = strings.TrimSuffix(content, "..."), true
		}
		if quotedInput(content, quote, cut) {
			return kind + ":" + id, nil
		}
	}
	return "", rows.Err()
}
