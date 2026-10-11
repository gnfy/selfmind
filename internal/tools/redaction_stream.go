package tools

import (
	"regexp"
	"sort"
	"strings"
)

// streamLookbackBytes bounds the already streamed text a stream keeps beyond
// what a pending JSON secret needs, enough to recognize a credential whose
// name ended the previous line.
const streamLookbackBytes = 256

// streamHoldSlack is allowed on top of the longest registered secret a stream
// may be printing before it stops holding lines for it.
const streamHoldSlack = 1024

// lineStreamRedactor masks output that reaches clients one line at a time,
// before the whole result exists for the result middleware to mask.
//
// Each line is masked together with a bounded lookback of the lines before
// it, so a value on the line after its credential's name is masked as it is
// in the full result. A registered secret spanning lines is never in one line
// whole: while the text so far ends with the beginning of such a secret, the
// lines from where it began are held back until it completes, when every one
// of its lines is masked, or diverges, when they are released as they are.
// What is held is bounded by the longest such secret plus a small slack, and
// everything held is released when the stream ends, however it ends.
//
// Two kinds of state follow the stream line by line, because their extent is
// not bounded by a registered value: a private key's body between its armor
// lines, and the scalars inside a container that a credential name opened
// (a pretty-printed "api_keys": [ with its strings on the lines after it).
// They mask a line as soon as it arrives, as the result does.
type lineStreamRedactor struct {
	// buf is the lookback, then each pending line preceded by a line break.
	buf      []byte
	base     int
	pending  []streamLine
	seq      int
	firstSeq int
	// recent are the last lines before the newest, for secrets that open with
	// punctuation lines.
	recent []string

	hold    streamHold
	tracker containerTracker
	plain   int
	pem     pemStream
	block   blockScalarStream
	// field is the value-field line, if any, that is held until the name it
	// may belong to has come.
	field fieldHold
}

// fieldHold is a line of a mapping whose key is value, which stays pending
// while the lines of its entry follow: the name it belongs to may come after
// other fields (- value: x, other: 1, name: DB_PASSWORD).
type fieldHold struct {
	seq, col, lines int
	fallback        maskSpan
	quote           string
	xml             bool
}

// maxHeldFieldLines bounds the lines a value field waits for its name: the
// lines the result looks through before a name, and the name's own.
const maxHeldFieldLines = maxFieldLines + 1

// streamLine is a line not yet shown.
type streamLine struct {
	text string
	// masks are the ranges of text that the stream state masks.
	masks []maskSpan
	// start is where text lies in buf.
	start int
}

func newLineStreamRedactor() *lineStreamRedactor {
	return &lineStreamRedactor{}
}

// push takes the next line and returns the lines now safe to show.
func (s *lineStreamRedactor) push(line string) []string {
	r := currentRedactor()
	s.sync(r)
	masks := s.stateMasks(line)
	s.buf = append(s.buf, '\n')
	s.pending = append(s.pending, streamLine{text: line, masks: masks, start: len(s.buf)})
	s.buf = append(s.buf, line...)
	n := s.seq
	s.seq++
	return s.release(r, s.holdFrom(line, n))
}

// flush returns every held line once the stream has ended.
func (s *lineStreamRedactor) flush() []string {
	if len(s.pending) == 0 {
		return nil
	}
	r := currentRedactor()
	s.sync(r)
	return s.release(r, len(s.pending))
}

// heldBytes is how much text the stream holds back.
func (s *lineStreamRedactor) heldBytes() int { return heldBytesOf(s.pending) }

func heldBytesOf(lines []streamLine) int {
	total := 0
	for _, line := range lines {
		total += len(line.text) + 1
	}
	return total
}

// lookbackLen is how much shown text the stream keeps.
func (s *lineStreamRedactor) lookbackLen() int { return s.base }

// keep is how much shown text the stream may keep: the lookback and what a
// pending JSON secret in any layout needs to be found whole.
func (s *lineStreamRedactor) keep() int {
	keep := streamLookbackBytes
	if s.hold.snapshot != nil {
		for _, compact := range s.hold.snapshot.compact {
			keep = max(keep, streamLookbackBytes+compact.limit)
		}
	}
	return keep
}

// sync follows the registry: a secret registered while the stream runs is
// recognized from its next line on, and what was in progress is dropped.
func (s *lineStreamRedactor) sync(r *redactor) {
	if s.hold.snapshot == r.secrets {
		return
	}
	s.hold = streamHold{snapshot: r.secrets, kmp: make([]int, len(r.secrets.compact))}
}

// stateMasks follows the stream state over the next line and returns what it
// masks in it.
func (s *lineStreamRedactor) stateMasks(line string) []maskSpan {
	var masks []maskSpan
	if span, ok := s.pem.body(line); ok {
		masks = append(masks, span)
	}
	if span, ok := s.block.body(line); ok {
		masks = append(masks, span)
	}
	return append(masks, s.containerMasks(line)...)
}

// containerMasks follows credential containers across lines. A stream that
// stops looking like JSON for two lines ends the tracking, so an unbalanced
// bracket in ordinary text cannot mask what follows it for good.
func (s *lineStreamRedactor) containerMasks(line string) []maskSpan {
	t := &s.tracker
	idle := len(t.frames) == 0 && t.overflow == 0 && !t.keyed && !t.hasCandidate
	if idle && strings.IndexByte(line, '{') < 0 && strings.IndexByte(line, '[') < 0 {
		return nil
	}
	if !idle {
		switch jsonishLine(line) {
		case lineJSON:
			s.plain = 0
		case linePlain:
			s.plain++
			if s.plain >= 2 {
				*t, s.plain = containerTracker{}, 0
				return nil
			}
		}
	}
	return t.scan(line)
}

type lineKind uint8

const (
	lineBlank lineKind = iota
	lineJSON
	linePlain
)

// jsonishLine tells a line a JSON document can hold from one it cannot: it
// starts with a quote, a bracket, a comma, a number or a literal.
func jsonishLine(line string) lineKind {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return lineBlank
	}
	switch c := trimmed[0]; {
	case strings.IndexByte(`"{}[],-`, c) >= 0, c >= '0' && c <= '9':
		return lineJSON
	}
	switch strings.TrimRight(trimmed, ",") {
	case "true", "false", "null":
		return lineJSON
	}
	return linePlain
}

// pemStream follows a private key's body from its BEGIN line to its END line.
type pemStream struct {
	active bool
	bytes  int
	state  pemBodyState
}

var pemBeginAtLineEnd = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----\s*$`)

// body reports the part of line that is a private key's body. The armor lines
// stay visible: the BEGIN line starts the state, the END line, or any line a
// key's body does not hold, ends it.
func (p *pemStream) body(line string) (maskSpan, bool) {
	if !p.active {
		if strings.Contains(line, "-----BEGIN ") && pemBeginAtLineEnd.MatchString(line) {
			p.active, p.bytes, p.state = true, 0, pemBodyState{}
		}
		return maskSpan{}, false
	}
	from, to, kind := p.state.next(strings.TrimRight(line, "\r"), false)
	switch {
	case kind == pemBlank:
		return maskSpan{}, false
	case kind == pemEnd || p.bytes+len(line) > maxPEMBody:
		p.active = false
		return maskSpan{}, false
	}
	p.bytes += len(line) + 1
	return maskSpan{from, to}, true
}

// blockScalarStream follows a YAML block scalar under a credential's name
// (password: |) from its indicator line to the first line that is not indented
// deeper than the key, and masks each of its lines as it arrives.
type blockScalarStream struct {
	active bool
	keyCol int
	// A name/key field can name the value block on a later line of its entry.
	nameCol, nameLines int
	nameKind           credentialKind
}

var blockScalarOpener = regexp.MustCompile(`^(\s*(?:-\s+)*)["']?([A-Za-z_][\w.-]*)["']?\s*:\s*(?:` + yamlTag + `[ \t]+)?[|>][-+0-9]*\s*(?:#.*)?$`)

// body reports the part of line that is a credential's block scalar text.
func (b *blockScalarStream) body(line string) (maskSpan, bool) {
	if b.active {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "" {
			return maskSpan{}, false
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " \t"))
		if indent > b.keyCol {
			return maskSpan{indent, len(trimmed)}, true
		}
		b.active = false
	}
	if b.nameLines > 0 {
		b.nameLines--
	}
	if l, ok := parseYAMLLine(line); ok {
		if l.col < b.nameCol || (l.dash && l.keyCol <= b.nameCol) {
			b.nameLines = 0
		}
		if l.key == "name" || l.key == "key" {
			b.nameCol, b.nameLines, b.nameKind = l.keyCol, maxFieldLines+1, fieldNameKind(line[l.valueAt:])
		}
	}
	if strings.LastIndexAny(line, "|>") < 0 {
		return maskSpan{}, false
	}
	if m := blockScalarOpener.FindStringSubmatch(line); m != nil {
		kind := credentialNameKind(asciiLower(m[2]))
		if asciiLower(m[2]) == "value" {
			// A value-first block's name may arrive after an arbitrarily long
			// body. Keep streaming bounded by masking it until its scope ends;
			// a known ordinary name before the value does not need that fallback.
			kind = singularCredential
			if b.nameLines > 0 && b.nameCol == len(m[1]) {
				kind = b.nameKind
			}
		}
		if kind != notCredential && kind != counterCredential {
			b.active, b.keyCol = true, len(m[1])
		}
	}
	return maskSpan{}, false
}

// streamHold follows the registered secrets that span lines.
type streamHold struct {
	snapshot *secretSnapshot
	// live are the multi-line secrets whose lines have matched so far.
	live []liveMatch
	// kmp is, per JSON secret, how much of it the stream so far ends with,
	// ignoring whitespace.
	kmp []int
}

// liveMatch is a multi-line secret that began on line seq and has matched
// its lines up to next.
type liveMatch struct {
	form int
	seq  int
	next int
}

// advance feeds the next line to the multi-line secrets. A secret's first line
// ends the line it begins on; the lines after it match whole; its last line
// matches as a beginning, and what follows it on that line is not the secret.
// recent are the lines before this one, newest last. A secret that opens with
// punctuation lines is looked for from its first line with more in it.
func (h *streamHold) advance(forms []multilineSecret, line string, seq int, recent []string) {
	kept := h.live[:0]
	for _, m := range h.live {
		lines := forms[m.form].lines
		switch {
		case m.next == len(lines)-1:
			// Completed: the whole secret lies in the window and is masked
			// when its lines are released.
		case line == lines[m.next]:
			m.next++
			kept = append(kept, m)
		}
	}
	h.live = kept
	for i, form := range forms {
		if !startsMultiline(form, line, recent) {
			continue
		}
		h.live = append(h.live, liveMatch{form: i, seq: seq, next: form.skip + 1})
	}
}

// startsMultiline reports whether line, after the recent lines, begins a
// match of form: its skipped lines came just before, the first as the end of a
// line, and this line is the first with more in it.
func startsMultiline(form multilineSecret, line string, recent []string) bool {
	if form.skip == 0 {
		return strings.HasSuffix(line, form.lines[0])
	}
	if len(recent) < form.skip || line != form.lines[form.skip] {
		return false
	}
	before := recent[len(recent)-form.skip:]
	if !strings.HasSuffix(before[0], form.lines[0]) {
		return false
	}
	for i := 1; i < form.skip; i++ {
		if before[i] != form.lines[i] {
			return false
		}
	}
	return true
}

// feed advances the JSON secrets over the next line, ignoring whitespace.
func (h *streamHold) feed(secrets []compactSecret, line string) {
	for k, secret := range secrets {
		state := h.kmp[k]
		for i := 0; i < len(line); i++ {
			b := line[i]
			if isJSONSpace(b) {
				continue
			}
			for state > 0 && b != secret.text[state] {
				state = secret.border[state-1]
			}
			if b == secret.text[state] {
				state++
			}
			if state == len(secret.text) {
				// A complete secret is not in progress; look for the next.
				state = secret.border[state-1]
			}
		}
		h.kmp[k] = state
	}
}

// valueFieldLine is a line of a mapping whose key is value, in YAML or in
// pretty-printed JSON; the group is its indentation and list dash.
var valueFieldLine = regexp.MustCompile(`(?i)^([ \t]*(?:-[ \t]+)?)(?:<[\w:.-]+[ \t]+)?["']?value["']?[ \t]*([:=])[ \t]*(?:` + yamlTag + `[ \t]+)?((?:` + quoteMark + `)?)(` + credentialValue + `)`)

var pairedNameFieldLine = regexp.MustCompile(`(?i)^([ \t]*(?:-[ \t]+)?)["']?(?:name|key)["']?[ \t]*[:=][ \t]*\S`)

// An XML name can also finish the value's own line after other attributes.
// Match from the scalar's closing quote so a name inside its text cannot bind.
var xmlNameAfterValue = regexp.MustCompile(`(?i)^["']\s*` + fieldBefore + `*["']?(?:name|key)["']?\s*=\s*(["']?[^\s/>]+)`)

// fieldAt starts bounded lookahead for a scalar value whose name may follow it.
// A block body is already protected by constant-size indentation state.
func fieldAt(line string, seq int) (fieldHold, bool) {
	m := valueFieldLine.FindStringSubmatchIndex(line)
	if m == nil {
		return fieldHold{}, false
	}
	quote := line[m[6]:m[7]]
	from, to := m[8], m[9]
	if quote == "" && blockScalarIndicator.MatchString(line[from:to]) {
		return fieldHold{}, false
	}
	value := valueOfField(line, from, to, quote, false, singularCredential, false)
	return fieldHold{seq: seq, col: m[3] - m[2], lines: maxHeldFieldLines,
		fallback: value.span, quote: quote, xml: line[m[4]:m[5]] == "="}, true
}

// fieldNameKind reads the scalar name of a name/value pair. Tags and trailing
// YAML comments describe the scalar but are not part of its identifier.
func fieldNameKind(value string) credentialKind {
	value = strings.TrimSpace(value)
	if view := detectionView(value); view != nil {
		value = view.text
	}
	if tag := yamlTagPrefix.FindStringIndex(value); tag != nil {
		value = value[tag[1]:]
	}
	if len(value) > 0 && (value[0] == '"' || value[0] == '\'') {
		quote := value[:1]
		value = value[1:literalEnd(value, 1, quote, false)]
	} else if end := strings.IndexAny(value, " \t\r,#/>}"); end >= 0 {
		value = value[:end]
	}
	return credentialNameKind(asciiLower(value))
}

// resolveField binds the held scalar to the name just observed. The whole
// result's lookback can be shorter than the stream's hold budget, so releasing
// the line and relying on that recognizer alone could expose its value.
func (s *lineStreamRedactor) resolveField(kind credentialKind) {
	i := s.field.seq - s.firstSeq
	span := s.field.fallback
	if i >= 0 && i < len(s.pending) && span.end > span.start && kind != notCredential && kind != counterCredential {
		value := valueOfField(s.pending[i].text, span.start, span.end, s.field.quote, false, kind, false)
		if value.ok {
			s.pending[i].masks = append(s.pending[i].masks, value.span)
		}
	}
	s.field = fieldHold{}
}

// maskUnresolvedField fails closed when the stream's byte or line budget ends
// before a field's name is known. It masks only the held scalar, not the other
// fields; retaining output forever or releasing its possible credential would
// both violate the streaming boundary.
func (s *lineStreamRedactor) maskUnresolvedField() {
	i := s.field.seq - s.firstSeq
	if i >= 0 && i < len(s.pending) && s.field.fallback.end > s.field.fallback.start {
		s.pending[i].masks = append(s.pending[i].masks, s.field.fallback)
	}
	s.field = fieldHold{}
}

// holdFrom feeds line, the n-th of the stream, to the recognizers and returns
// the index of the first pending line to keep holding; len(pending) when
// none is.
func (s *lineStreamRedactor) holdFrom(line string, n int) int {
	snapshot := s.hold.snapshot
	held := len(s.pending)
	if len(snapshot.multiline) > 0 {
		s.hold.advance(snapshot.multiline, line, n, s.recent)
		for _, m := range s.hold.live {
			held = min(held, m.seq-s.firstSeq)
		}
		if snapshot.maxSkip > 0 {
			s.recent = append(s.recent, line)
			if len(s.recent) > snapshot.maxSkip {
				s.recent = s.recent[len(s.recent)-snapshot.maxSkip:]
			}
		}
	}
	if len(snapshot.compact) > 0 {
		s.hold.feed(snapshot.compact, line)
		for k, secret := range snapshot.compact {
			state := s.hold.kmp[k]
			if state <= secret.lead {
				continue
			}
			start := liveStart(s.buf, state)
			if start < 0 || len(s.buf)-start > secret.limit {
				// No layout of the secret takes this much room.
				s.hold.kmp[k] = 0
				continue
			}
			held = min(held, s.lineAt(start))
		}
	}
	independentHold := held
	// A line that is a value field (- value: x) may be followed by the name it
	// belongs to (name: DB_PASSWORD), which then shows it to be a credential:
	// it waits for the next line.
	if s.field.lines > 0 && n > s.field.seq {
		// The lines of its entry, up to the first that is less indented, which
		// belongs to another entry or ends the object.
		trimmed := strings.TrimLeft(line, " \t")
		boundary := !s.field.xml && trimmed != "" && len(line)-len(trimmed) < s.field.col
		name := pairedNameFieldLine.FindStringSubmatchIndex(line)
		resolved := name != nil && (s.field.xml || name[3]-name[2] == s.field.col)
		s.field.lines--
		switch {
		case resolved:
			s.resolveField(fieldNameKind(line[name[1]-1:]))
		case boundary:
			s.field = fieldHold{}
		case s.field.lines == 0:
			s.maskUnresolvedField()
		}
	}
	if field, ok := fieldAt(line, n); ok {
		if s.field.lines > 0 {
			s.maskUnresolvedField()
		}
		s.field = field
		if field.xml && field.fallback.end > field.fallback.start {
			if name := xmlNameAfterValue.FindStringSubmatch(line[field.fallback.end:]); name != nil {
				s.resolveField(fieldNameKind(name[1]))
			}
		}
	}
	if s.field.lines > 0 {
		held = min(held, s.field.seq-s.firstSeq)
	}
	// What is held stays within what a registered secret can take.
	limit := snapshot.maxHeld + streamHoldSlack
	for held < len(s.pending)-1 && len(s.buf)-s.pending[held].start > limit {
		held++
	}
	if s.field.lines > 0 && (s.field.seq-s.firstSeq < held || heldBytesOf(s.pending[s.field.seq-s.firstSeq:]) > limit) {
		s.maskUnresolvedField()
		held = independentHold
		for held < len(s.pending)-1 && len(s.buf)-s.pending[held].start > limit {
			held++
		}
	}
	s.hold.dropBefore(s.firstSeq + held)
	return max(held, 0)
}

// dropBefore forgets the multi-line secrets that began before line seq.
func (h *streamHold) dropBefore(seq int) {
	kept := h.live[:0]
	for _, m := range h.live {
		if m.seq >= seq {
			kept = append(kept, m)
		}
	}
	h.live = kept
}

// lineAt is the index of the pending line that holds offset of buf; offsets
// in the lookback belong to the first.
func (s *lineStreamRedactor) lineAt(offset int) int {
	i := sort.Search(len(s.pending), func(i int) bool { return s.pending[i].start > offset }) - 1
	return max(i, 0)
}

// liveStart is the offset in buf of the state-th last byte that is not
// whitespace, or -1 when buf does not reach back that far.
func liveStart(buf []byte, state int) int {
	seen := 0
	for i := len(buf) - 1; i >= 0; i-- {
		if isJSONSpace(buf[i]) {
			continue
		}
		if seen++; seen == state {
			return i
		}
	}
	return -1
}

// explainedByLine reports whether every mask the window put on a line is one
// the line gets on its own.
func explainedByLine(r *redactor, line string, masks []maskSpan) bool {
	spans, _ := r.spans(line)
	own := mergeSpans(spans)
	for _, mask := range masks {
		i := sort.Search(len(own), func(i int) bool { return own[i].end >= mask.end })
		if i == len(own) || own[i].start > mask.start {
			return false
		}
	}
	return true
}

// maxLookbackBlanks bounds the blanks a lookback keeps on top of its keep
// bytes: blank lines between a name and its value do not use up the lookback,
// up to this much, so a value is found however many of them lie between.
const maxLookbackBlanks = 64 << 10

// lookbackStart is where, in the shown text, the lookback begins: it keeps
// keep bytes that are not blanks, with the blanks between them, and at most
// maxLookbackBlanks bytes of blanks more.
func lookbackStart(shown []byte, keep int) int {
	if len(shown) <= keep {
		return 0
	}
	budget, blanks := keep, 0
	for i := len(shown) - 1; i >= 0; i-- {
		if isBlank(shown[i]) {
			if blanks++; blanks > maxLookbackBlanks {
				return i + 1
			}
			continue
		}
		if budget--; budget == 0 {
			return i
		}
	}
	return 0
}

// release masks the first n pending lines with what the whole window shows
// and what the stream state masked, and drops them from the window.
func (s *lineStreamRedactor) release(r *redactor, n int) []string {
	if n == 0 {
		return nil
	}
	lines := make([]string, n)
	blank := true
	for i := 0; i < n && blank; i++ {
		blank = strings.TrimSpace(s.pending[i].text) == ""
	}
	if blank {
		// Nothing in blank lines is a secret. They are not looked at, and
		// they push the lookback no further than its blank limit.
		for i := 0; i < n; i++ {
			lines[i] = s.pending[i].text
		}
		return s.drop(n, blankLookbackStart(s.pending[n-1].start+len(s.pending[n-1].text), s.keep()), lines)
	}
	spans, _ := r.detect(string(s.buf), false)
	merged := mergeSpans(spans)
	next := 0
	for i := 0; i < n; i++ {
		pending := s.pending[i]
		start, end := pending.start, pending.start+len(pending.text)
		for next < len(merged) && merged[next].end <= start {
			next++
		}
		var clipped []maskSpan
		for j := next; j < len(merged) && merged[j].start < end; j++ {
			clipped = append(clipped, maskSpan{max(merged[j].start, start) - start, min(merged[j].end, end) - start})
		}
		masks := append(append([]maskSpan(nil), pending.masks...), clipped...)
		if _, _, document := jsonDocumentBounds(pending.text); !document {
			// What the window masks is what the full result masks. A line
			// read alone has lost the lines whose evidence says that a value
			// is code, so it is never read alone.
			lines[i] = applyMasks(pending.text, masks)
			continue
		}
		// A document is read whole and stays JSON: its masks are the ones its
		// own detection finds, and only a mask that this does not explain
		// came from the lines around it.
		if explainedByLine(r, pending.text, masks) {
			masks = nil
		}
		lines[i], _ = r.report(applyMasks(pending.text, masks), 0)
	}
	return s.drop(n, lookbackStart(s.buf[:s.pending[n-1].start+len(s.pending[n-1].text)], s.keep()), lines)
}

// blankLookbackStart keeps what was kept before blank lines were shown, and
// no more blanks than a lookback may hold, in shown text that ends at end.
func blankLookbackStart(end, keep int) int {
	return max(0, end-(keep+maxLookbackBlanks))
}

// drop makes the text released so far, from offset from on, the lookback, and
// leaves the lines after the n-th pending. The buffer is cut from the front
// and not rebuilt, so that a long lookback of blank lines costs nothing per
// line; appending to it later moves what is left when its room runs out.
func (s *lineStreamRedactor) drop(n, from int, lines []string) []string {
	end := s.pending[n-1].start + len(s.pending[n-1].text)
	rest := s.pending[n:]
	for i := range rest {
		rest[i].start -= from
	}
	s.buf, s.base, s.pending = s.buf[from:], end-from, rest
	s.firstSeq += n
	return lines
}
