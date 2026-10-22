package executionenv

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Which objects count as a work's own, and which as already in the baseline,
// is decided by a walk the daemon runs itself. Git reads objects without
// hashing them, so a pack whose index names a commit of the baseline's history
// but holds other content could otherwise make work commits look like part of
// that history; the Run can write such a pack into its own view, and a writer
// of the source into the source. The walk reads each commit and tree through
// gitObjectReader, which accepts an object only when its content hashes to its
// id, and follows parent links only from verified commits. A commit counts as
// baseline only when a verified chain of parents leads to it from the
// baseline, so no object anyone can write widens what is excluded.

var (
	// errGitViewIncomplete and errGitViewCorrupt mark a view or archive that
	// cannot produce its own work: an object of it is missing, or another
	// object stands under its id. Neither says anything about the source.
	errGitViewIncomplete = errors.New("the view does not hold every object of its work")
	errGitViewCorrupt    = errors.New("the view holds other content under an object of its work")
	// errGitWorkTooLong marks a walk that would read more commits than the
	// daemon reads for one work.
	errGitWorkTooLong = errors.New("the work's history is too long to list")
	// errGitWorkTooLarge marks a walk that would keep more in memory, or
	// follow a commit or tree further, than the daemon does for one work.
	errGitWorkTooLarge = errors.New("the work is too large to list")
)

// maxGitWalkBytes bounds what one walk must hold: the ids and commits it
// retains and the trees it is going through, as charged at Go's allocation
// sizes. Measured, the live heap after a walk was never more than 1.03 times
// what it charged, and the peak, garbage included, 1.1 to 2.1 times, or 3.2
// times when the walk read the same 1 MiB trees over and over. Classifying a
// 300,000-commit baseline history charges 123 MiB, and 300 commits changing
// one file each in an 8,000-file directory 66 MiB. It is a variable only so a
// test can lower it.
var maxGitWalkBytes int64 = 512 << 20

// maxGitWalkCommits bounds the commits one walk holds that it does not yet
// know to be the baseline's. It is a variable only so a test can lower it.
var maxGitWalkCommits = 100000

// maxGitWalkObject bounds the size of one commit or tree the walk reads.
const maxGitWalkObject = 64 << 20

// gitObjectReader reads objects through one cat-file --batch process and
// accepts an object only when its content hashes to its id. missing and
// corrupt are the errors it reports for an absent object and for other
// content under an id. It reads no blob: Git streams a blob without checking
// it against the size it announced, and may stream another copy than the one
// it took that size from, so a planted copy could leave the reader waiting for
// bytes that never come.
type gitObjectReader struct {
	ctx      context.Context
	cancel   context.CancelFunc
	cmd      *exec.Cmd
	in       io.WriteCloser
	out      *bufio.Reader
	stderr   gitStderr
	missing  error
	corrupt  error
	digests  map[int]hash.Hash
	finished bool
}

func newGitObjectReader(ctx context.Context, dir string, env []string, missing, corrupt error) (*gitObjectReader, error) {
	readerCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	r := &gitObjectReader{ctx: readerCtx, cancel: cancel, missing: missing, corrupt: corrupt, digests: map[int]hash.Hash{}}
	r.cmd = hostGitCommand(readerCtx, dir, env, "cat-file", "--batch")
	r.cmd.Stderr = &r.stderr
	in, err := r.cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: git cat-file: %w", ErrGitViewUnavailable, err)
	}
	out, err := r.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: git cat-file: %w", ErrGitViewUnavailable, err)
	}
	if err := r.cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%w: git cat-file: %w", ErrGitViewUnavailable, err)
	}
	r.in, r.out = in, bufio.NewReader(out)
	return r, nil
}

// read returns the type and content of id once its content hashes to id.
func (r *gitObjectReader) read(id string) (string, []byte, error) {
	if _, err := io.WriteString(r.in, id+"\n"); err != nil {
		return "", nil, r.failure(err)
	}
	header, err := r.out.ReadString('\n')
	if err != nil {
		return "", nil, r.failure(err)
	}
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[0] == id && fields[1] == "missing" {
		return "", nil, fmt.Errorf("%w: %w: %s is missing or unreadable", ErrGitViewUnavailable, r.missing, id)
	}
	var size int64
	if len(fields) == 3 && fields[0] == id {
		size, err = strconv.ParseInt(fields[2], 10, 64)
	}
	if len(fields) != 3 || fields[0] != id || err != nil || size < 0 {
		return "", nil, r.failure(fmt.Errorf("unexpected object header %q", strings.TrimSpace(header)))
	}
	// Neither of these is read, so the process, which may already be writing
	// it, is stopped.
	if fields[1] == "blob" {
		r.stop()
		return "", nil, fmt.Errorf("%w: %w: %s is a blob, where only commits and trees are read", ErrGitViewUnavailable, r.corrupt, id)
	}
	if size > maxGitWalkObject {
		r.stop()
		return "", nil, fmt.Errorf("%w: %s is a %d-byte %s, larger than a walk reads", ErrGitViewUnavailable, id, size, fields[1])
	}
	content := make([]byte, size)
	if _, err := io.ReadFull(r.out, content); err != nil {
		return "", nil, r.cutShort(id, err)
	}
	if end, err := r.out.ReadByte(); err != nil || end != '\n' {
		return "", nil, r.cutShort(id, err)
	}
	digest := r.digests[len(id)]
	if digest == nil {
		digest = gitObjectHash(id)
		r.digests[len(id)] = digest
	}
	digest.Reset()
	fmt.Fprintf(digest, "%s %d\x00", fields[1], size)
	digest.Write(content)
	if hex.EncodeToString(digest.Sum(nil)) != id {
		return "", nil, fmt.Errorf("%w: %w: %s", ErrGitViewUnavailable, r.corrupt, id)
	}
	return fields[1], content, nil
}

// failure keeps what a failed read means: an interruption is reported as
// such, and anything else carries the start of Git's stderr.
func (r *gitObjectReader) failure(err error) error {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: git cat-file did not finish: %w", ErrGitViewUnavailable, ctxErr)
	}
	r.stop()
	return fmt.Errorf("%w: git cat-file: %w: %s", ErrGitViewUnavailable, err, strings.TrimSpace(r.stderr.String()))
}

// cutShort reports an object whose content ended early: cat-file stops partway
// through an object the store cannot produce whole.
func (r *gitObjectReader) cutShort(id string, err error) error {
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: git cat-file did not finish: %w", ErrGitViewUnavailable, ctxErr)
	}
	r.stop()
	return fmt.Errorf("%w: %w: %s ended early: %w: %s", ErrGitViewUnavailable, r.corrupt, id, errors.Join(errGitObjectCutShort, err),
		strings.TrimSpace(r.stderr.String()))
}

// stop ends the cat-file process and waits for it, so its stderr is complete.
// A process whose output is no longer read could be blocked writing an
// object, so it is killed rather than left to finish; what it wrote to stderr
// before is kept.
func (r *gitObjectReader) stop() {
	if r.finished {
		return
	}
	r.finished = true
	r.in.Close()
	r.cancel()
	_ = r.cmd.Wait()
}

func (r *gitObjectReader) close() { r.stop() }

// gitCommitFacts is what the walk takes from a verified commit.
type gitCommitFacts struct {
	tree    string
	parents []string
	date    int64
}

// maxGitCommitParents bounds the parent lines one commit may have. Git's own
// octopus merges stay at a few dozen branches; a commit naming more than this
// is refused before any parent is read.
const maxGitCommitParents = 1024

// parseGitCommit takes the tree, the distinct parents, and the committer date
// of a commit. The ids are copied out, so nothing keeps the commit's text,
// which can be up to maxGitWalkObject long.
func parseGitCommit(id string, content []byte) (gitCommitFacts, error) {
	return newCommitParser(func(parent []byte) (string, error) { return string(parent), nil }).parse(id, content)
}

// commitParser parses commits one after another, reusing its scratch. Each
// parent id goes through intern, and repeats of a parent are left out.
type commitParser struct {
	intern  func([]byte) (string, error)
	seen    map[string]bool
	parents []string
}

func newCommitParser(intern func([]byte) (string, error)) *commitParser {
	return &commitParser{intern: intern, seen: map[string]bool{}}
}

// parse takes the facts of one commit. Its parents are copied into a slice of
// exactly their number, however many parent lines named them: a commit's
// facts are kept for the whole walk, so anything more is held as many times
// as there are commits.
func (p *commitParser) parse(id string, content []byte) (gitCommitFacts, error) {
	var facts gitCommitFacts
	invalid := func() (gitCommitFacts, error) {
		return gitCommitFacts{}, fmt.Errorf("%w: commit %s is malformed", ErrGitViewUnavailable, id)
	}
	headers := content
	if end := bytes.Index(content, []byte("\n\n")); end >= 0 {
		headers = content[:end]
	}
	clear(p.seen)
	p.parents = p.parents[:0]
	named := 0
	for index := 0; len(headers) > 0; index++ {
		line := headers
		if end := bytes.IndexByte(headers, '\n'); end >= 0 {
			line, headers = headers[:end], headers[end+1:]
		} else {
			headers = nil
		}
		field, value, _ := bytes.Cut(line, []byte(" "))
		switch {
		case index == 0:
			if string(field) != "tree" || !gitObjectID(string(value), len(id)) {
				return invalid()
			}
			facts.tree = string(value)
		case string(field) == "parent" && named == index-1:
			if named++; named > maxGitCommitParents {
				return gitCommitFacts{}, fmt.Errorf("%w: %w: commit %s names more than %d parents",
					ErrGitViewUnavailable, errGitWorkTooLarge, id, maxGitCommitParents)
			}
			if !gitObjectIDBytes(value, len(id)) {
				return invalid()
			}
			parent, err := p.intern(value)
			if err != nil {
				return gitCommitFacts{}, err
			}
			if !p.seen[parent] {
				p.seen[parent] = true
				p.parents = append(p.parents, parent)
			}
		case string(field) == "committer":
			if parts := bytes.Fields(value); len(parts) >= 2 {
				facts.date, _ = strconv.ParseInt(string(parts[len(parts)-2]), 10, 64)
			}
		}
	}
	if facts.tree == "" {
		return invalid()
	}
	if len(p.parents) > 0 {
		facts.parents = make([]string, len(p.parents))
		copy(facts.parents, p.parents)
	}
	return facts, nil
}

// gitTreeEntry is one entry of a verified tree. kind is 't' for a tree, 'b'
// for a blob or symlink, and 'g' for a submodule commit, which belongs to
// another repository.
type gitTreeEntry struct {
	name string
	id   string
	kind byte
}

// parseGitTree reads the entries of a tree. It first measures them and passes
// their count and what keeping them costs, at Go's allocation sizes, to
// measure, so a tree too large to keep is refused before any entry is built.
func parseGitTree(id string, content []byte, measure func(count int, cost int64) error) ([]gitTreeEntry, error) {
	raw := len(id) / 2
	count, cost := 0, int64(0)
	for rest := content; len(rest) > 0; count++ {
		space := bytes.IndexByte(rest, ' ')
		nul := bytes.IndexByte(rest, 0)
		if space <= 0 || nul <= space+1 || len(rest) < nul+1+raw {
			return nil, fmt.Errorf("%w: tree %s is malformed", ErrGitViewUnavailable, id)
		}
		cost += goAllocBytes(nul-space-1) + goAllocBytes(len(id))
		rest = rest[nul+1+raw:]
	}
	if err := measure(count, cost+goPtrAllocBytes(count*gitTreeEntryBytes)); err != nil {
		return nil, err
	}
	entries := make([]gitTreeEntry, 0, count)
	for len(content) > 0 {
		space := bytes.IndexByte(content, ' ')
		nul := bytes.IndexByte(content, 0)
		mode := string(content[:space])
		entry := gitTreeEntry{name: string(content[space+1 : nul]), id: hex.EncodeToString(content[nul+1 : nul+1+raw])}
		switch mode {
		case "40000":
			entry.kind = 't'
		case "160000":
			entry.kind = 'g'
		case "100644", "100755", "100664", "120000":
			entry.kind = 'b'
		default:
			return nil, fmt.Errorf("%w: tree %s has an entry of unknown mode %q", ErrGitViewUnavailable, id, mode)
		}
		entries = append(entries, entry)
		content = content[nul+1+raw:]
	}
	return entries, nil
}

// gitObjectID reports whether value is a hexadecimal object id of length n.
func gitObjectID(value string, n int) bool {
	id, ok := gitObjectIDField(value)
	return ok && id == value && len(value) == n
}

// gitObjectIDBytes is gitObjectID for an id still in an object's bytes.
func gitObjectIDBytes(value []byte, n int) bool {
	if len(value) != n || (n != 40 && n != 64) {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

// maxGitTreeDepth is Git's own default core.maxTreeDepth: the walk follows a
// tree at most that many directories below its commit's root tree.
const maxGitTreeDepth = 2048

// walkCommit is one commit the walk has read.
type walkCommit struct {
	id       string
	facts    gitCommitFacts
	baseline bool
	queued   bool
	expanded bool
	index    int
}

// walkQueue orders commits newest first and, between equal dates, the work's
// before the baseline's, so a walk reads no more baseline history than the
// dates require.
type walkQueue []*walkCommit

func (q walkQueue) Len() int { return len(q) }
func (q walkQueue) Less(i, j int) bool {
	if q[i].facts.date != q[j].facts.date {
		return q[i].facts.date > q[j].facts.date
	}
	if q[i].baseline != q[j].baseline {
		return !q[i].baseline
	}
	return q[i].id < q[j].id
}
func (q walkQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *walkQueue) Push(x any) {
	commit := x.(*walkCommit)
	commit.index = len(*q)
	*q = append(*q, commit)
}
func (q *walkQueue) Pop() any {
	old := *q
	commit := old[len(old)-1]
	*q = old[:len(old)-1]
	return commit
}

// listGitWorkObjects lists the commits head adds over base, and the trees and
// blobs each of those commits introduces against its parents, from objects
// read through reader. Commits are walked newest first from both ends, the way
// rev-list limits a range, until no commit of the work is left unread; one is
// excluded only when a verified chain of parents reaches it from base. An
// object a work commit holds at the same path as one of its parents is that
// parent's, so it is left to the parent's own entry; any other object of the
// work, including one the baseline holds at another path, is listed. Listing
// more than rev-list --objects would is safe: the source must hold it anyway.
//
// Work commits are read parents first, so when a tree turns up again, every
// object under it is already listed or held by a parent read before it or by
// the baseline, and the tree need not be read again; whichever parents it was
// first compared with, those came earlier. The walk is bounded: at most
// maxGitWalkCommits commits not yet known to be the baseline's, at most
// maxGitCommitParents parents a commit, trees at most maxGitTreeDepth
// directories below their root, what it retains within maxGitWalkBytes, trees
// it is done with cached within maxGitWalkCacheBytes, and its own work within
// maxGitWalkSteps; it stops when the reader's context ends.
func listGitWorkObjects(reader *gitObjectReader, head, base string) ([]string, error) {
	return newGitWorkWalk(reader).run(head, base)
}

// maxGitWalkCacheBytes bounds the trees, and their name indexes, that a walk
// keeps after it is done with them; past it the least recently used are
// dropped and, when needed again, read again and hash-verified. It is a
// variable only so a test can lower it.
var maxGitWalkCacheBytes int64 = 64 << 20

// maxGitWalkSteps bounds the work one walk does itself, in steps of about one
// name-index lookup: gitWalkReadSteps and one per gitWalkReadBytesPerStep
// bytes for each object read (including a tree read again after the cache
// dropped it), gitWalkParseSteps for each tree entry parsed, and for each
// entry compared with its parents' trees one per parent, plus one per 64
// bytes of its name. Measured, spending all of it took 4 s to 40 s of CPU:
// about 5 s on comparisons or on reading 1 MiB trees, 16 s on commits with
// 1,024 parent lines, 36 s on a million tiny trees read again. It is a
// variable only so a test can lower it.
var maxGitWalkSteps int64 = 1 << 28

const (
	gitWalkParseSteps = 4
	// gitWalkReadSteps is the fixed cost of reading one object through Git,
	// whatever its size: the round trip to the process.
	gitWalkReadSteps = 256
	// gitWalkReadBytesPerStep is how many bytes of an object cost one more
	// step: they are read, hashed, and copied.
	gitWalkReadBytesPerStep = 16
	// gitWalkCheckEvery is how many entries or commits the walk goes through
	// between looks at its context.
	gitWalkCheckEvery = 256
)

// What a walk holds is charged by these estimates of what Go allocates for
// each item besides its ids and names, which are charged at Go's allocation
// sizes (goAllocBytes). They were measured on honest histories; see
// TestWalkKeepsWithinItsBudget and TestWalkHeapStaysNearItsCharge.
const (
	// gitWalkCommitBytes is a commit read: its record, its entries in the
	// commit, work, and order maps, and its places in the queue and lists.
	gitWalkCommitBytes = 256
	// gitWalkSetBytes is one id in a map the walk keeps: interned, listed,
	// named, introduced, or cached.
	gitWalkSetBytes = 64
	// gitTreeEntryBytes is one entry of a tree read, in the entries slice.
	gitTreeEntryBytes = 40
)

// goSizeClasses are the sizes Go's allocator rounds a small allocation up to.
var goSizeClasses = []int64{8, 16, 24, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240, 256,
	288, 320, 352, 384, 416, 448, 480, 512, 576, 640, 704, 768, 896, 1024, 1152, 1280, 1408, 1536, 1792, 2048,
	2304, 2688, 3072, 3200, 3456, 4096, 4864, 5376, 6144, 6528, 6784, 6912, 8192, 9472, 9728, 10240, 10880,
	12288, 13568, 14336, 16384, 18432, 19072, 20480, 21760, 24576, 27264, 28672, 32768}

// goAllocBytes is what Go allocates for n bytes: a size class, or whole pages.
func goAllocBytes(n int) int64 {
	if n <= 0 {
		return 0
	}
	if size := int64(n); size > goSizeClasses[len(goSizeClasses)-1] {
		return (size + 8191) &^ 8191
	}
	return goSizeClasses[sort.Search(len(goSizeClasses), func(i int) bool { return goSizeClasses[i] >= int64(n) })]
}

// goPtrAllocBytes is goAllocBytes for an allocation that holds pointers: Go
// puts an 8-byte header in front of such an allocation of more than 512 bytes
// (up to the largest size class), so a slice of 1,024 strings, 16 KiB, takes
// the 18 KiB class.
func goPtrAllocBytes(n int) int64 {
	if n > 512 && int64(n) <= goSizeClasses[len(goSizeClasses)-1]-8 {
		return goAllocBytes(n + 8)
	}
	return goAllocBytes(n)
}

// nameIndexBytes is what a name index of n entries takes: Go sizes a map in
// powers of two at most seven-eighths full, each slot a 16-byte name, a
// 40-byte entry, and a control byte, in tables of at most 1,024 slots; a
// table of that size is 57 KiB, which Go rounds up to whole pages.
func nameIndexBytes(n int) int64 {
	slots := int64(8)
	for slots*7/8 < int64(n) {
		slots *= 2
	}
	const tableSlots = 1024
	if slots <= tableSlots {
		return goPtrAllocBytes(int(slots * 57))
	}
	return slots / tableSlots * goPtrAllocBytes(tableSlots*57)
}

// gitWorkWalk is the state of one listGitWorkObjects walk. What it must
// retain (the ids it interned, the commits it read, the listing, and the sets
// of ids it named and introduced) and the trees it is going through, which
// are pinned, are charged against maxGitWalkBytes; the trees it is done with
// stay cached within maxGitWalkCacheBytes.
type gitWorkWalk struct {
	reader       *gitObjectReader
	parser       *commitParser
	interned     map[string]string
	seen         map[string]bool
	commits      map[string]*walkCommit
	unclassified int
	listed       map[string]bool
	ids          []string
	kinds        map[string]byte
	introduced   map[string]bool
	held         []string

	trees          map[string]*walkTree
	recent, oldest *walkTree
	// cacheLimit is the bound on the trees kept after use; a test may change
	// it for one walk.
	cacheLimit int64

	kept, pinned, cached, peak int64
	steps                      int64
	ticks                      int
}

// walkTree is a tree the walk has read, with its name index once it has been
// compared against as a parent. A pinned tree is in use; an unpinned one is
// on the cache's list, most recently used first.
type walkTree struct {
	id         string
	entries    []gitTreeEntry
	byName     map[string]gitTreeEntry
	bytes      int64
	pins       int
	prev, next *walkTree
}

func newGitWorkWalk(reader *gitObjectReader) *gitWorkWalk {
	w := &gitWorkWalk{reader: reader, interned: map[string]string{}, seen: map[string]bool{},
		commits: map[string]*walkCommit{}, listed: map[string]bool{}, kinds: map[string]byte{},
		introduced: map[string]bool{}, trees: map[string]*walkTree{}, cacheLimit: maxGitWalkCacheBytes}
	w.parser = newCommitParser(w.internBytes)
	return w
}

// run lists the work head adds over base.
func (w *gitWorkWalk) run(head, base string) ([]string, error) {
	if head == base {
		return nil, nil
	}
	head, err := w.intern(head)
	if err != nil {
		return nil, err
	}
	if base, err = w.intern(base); err != nil {
		return nil, err
	}
	work, err := w.classify(head, base)
	if err != nil {
		return nil, err
	}
	var parentTrees []string
	for _, commit := range work {
		if err := w.tick(); err != nil {
			return nil, err
		}
		if err := w.list(commit.id); err != nil {
			return nil, err
		}
		if err := w.named(commit.facts.tree, 't'); err != nil {
			return nil, err
		}
		parentTrees = parentTrees[:0]
		for _, parent := range commit.facts.parents {
			parentTrees = append(parentTrees, w.commits[parent].facts.tree)
		}
		if err := w.introduce(commit.facts.tree, w.distinct(parentTrees)); err != nil {
			return nil, err
		}
	}
	return w.ids, nil
}

// peakCharged is the most the walk has held at once by its own estimates.
func (w *gitWorkWalk) peakCharged() int64 { return w.peak }

func (w *gitWorkWalk) tooLarge() error {
	return fmt.Errorf("%w: %w: listing it would hold more than %d MiB", ErrGitViewUnavailable, errGitWorkTooLarge, maxGitWalkBytes>>20)
}

// keep charges n bytes the walk retains to the end.
func (w *gitWorkWalk) keep(n int64) error {
	w.kept += n
	w.notePeak()
	if w.kept+w.pinned > maxGitWalkBytes {
		return w.tooLarge()
	}
	return nil
}

// pin charges n bytes of trees in use.
func (w *gitWorkWalk) pin(n int64) error {
	w.pinned += n
	w.notePeak()
	if w.kept+w.pinned > maxGitWalkBytes {
		return w.tooLarge()
	}
	return nil
}

func (w *gitWorkWalk) notePeak() {
	if held := w.kept + w.pinned + w.cached; held > w.peak {
		w.peak = held
	}
}

// step charges n steps of the walk's own work.
func (w *gitWorkWalk) step(n int64) error {
	if w.steps += n; w.steps > maxGitWalkSteps {
		return fmt.Errorf("%w: %w: listing it would take more than %d steps of reading objects and of parsing and comparing tree entries",
			ErrGitViewUnavailable, errGitWorkTooLarge, maxGitWalkSteps)
	}
	return nil
}

// tick looks at the walk's context every gitWalkCheckEvery calls, so a
// deadline or cancellation ends the walk with its own meaning.
func (w *gitWorkWalk) tick() error {
	if w.ticks++; w.ticks%gitWalkCheckEvery != 0 {
		return nil
	}
	if err := w.reader.ctx.Err(); err != nil {
		return fmt.Errorf("%w: the work walk did not finish: %w", ErrGitViewUnavailable, err)
	}
	return nil
}

// intern returns the walk's one copy of id, so an id named many times is
// held once.
func (w *gitWorkWalk) intern(id string) (string, error) {
	if held, ok := w.interned[id]; ok {
		return held, nil
	}
	if err := w.keep(gitWalkSetBytes + goAllocBytes(len(id))); err != nil {
		return "", err
	}
	w.interned[id] = id
	return id, nil
}

// internBytes is intern for an id still in a commit's text; it allocates only
// for an id not held yet.
func (w *gitWorkWalk) internBytes(id []byte) (string, error) {
	if held, ok := w.interned[string(id)]; ok {
		return held, nil
	}
	return w.intern(string(id))
}

// distinct returns ids without repeats, in their first order, reusing ids.
func (w *gitWorkWalk) distinct(ids []string) []string {
	if len(ids) < 2 {
		return ids
	}
	clear(w.seen)
	out := ids[:0]
	for _, id := range ids {
		if !w.seen[id] {
			w.seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// read reads and verifies one object and charges the read: a fixed cost for
// the round trip, and one step for each gitWalkReadBytesPerStep bytes that were
// read, hashed, and copied, so that reading is bounded by the step budget
// whatever the objects' sizes and however often the cache makes the walk read
// the same one again.
func (w *gitWorkWalk) read(id string) (string, []byte, error) {
	kind, content, err := w.reader.read(id)
	if err != nil {
		return "", nil, err
	}
	if err := w.step(gitWalkReadSteps + int64(len(content))/gitWalkReadBytesPerStep); err != nil {
		return "", nil, err
	}
	return kind, content, nil
}

func (w *gitWorkWalk) load(id string) (*walkCommit, error) {
	if commit := w.commits[id]; commit != nil {
		return commit, nil
	}
	if w.unclassified >= maxGitWalkCommits {
		return nil, fmt.Errorf("%w: %w: more than %d commits", ErrGitViewUnavailable, errGitWorkTooLong, maxGitWalkCommits)
	}
	kind, content, err := w.read(id)
	if err != nil {
		return nil, err
	}
	if kind != "commit" {
		return nil, fmt.Errorf("%w: %w: %s is a %s, not a commit", ErrGitViewUnavailable, w.reader.corrupt, id, kind)
	}
	facts, err := w.parser.parse(id, content)
	if err != nil {
		return nil, err
	}
	if err := w.keep(gitWalkCommitBytes + goAllocBytes(len(facts.tree)) + goPtrAllocBytes(16*len(facts.parents))); err != nil {
		return nil, err
	}
	commit := &walkCommit{id: id, facts: facts}
	w.commits[id] = commit
	w.unclassified++
	return commit, nil
}

// classify walks commits from head and base and returns the work's, parents
// first.
func (w *gitWorkWalk) classify(head, base string) ([]*walkCommit, error) {
	commits, load := w.commits, w.load
	var queue walkQueue
	unread := 0
	push := func(commit *walkCommit) {
		heap.Push(&queue, commit)
		commit.queued = true
		if !commit.baseline {
			unread++
		}
	}
	markBaseline := func(start *walkCommit) {
		for stack := []*walkCommit{start}; len(stack) > 0; {
			commit := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if commit.baseline {
				continue
			}
			commit.baseline = true
			w.unclassified--
			if commit.queued {
				unread--
				heap.Fix(&queue, commit.index)
			}
			if commit.expanded {
				for _, parent := range commit.facts.parents {
					stack = append(stack, commits[parent])
				}
			}
		}
	}
	headCommit, err := load(head)
	if err != nil {
		return nil, err
	}
	baseCommit, err := load(base)
	if err != nil {
		return nil, err
	}
	markBaseline(baseCommit)
	push(headCommit)
	push(baseCommit)
	var expanded []*walkCommit
	for unread > 0 {
		if err := w.tick(); err != nil {
			return nil, err
		}
		commit := heap.Pop(&queue).(*walkCommit)
		commit.queued = false
		if !commit.baseline {
			unread--
		}
		commit.expanded = true
		expanded = append(expanded, commit)
		for _, id := range commit.facts.parents {
			parent, err := load(id)
			if err != nil {
				return nil, err
			}
			if commit.baseline {
				markBaseline(parent)
			}
			if !parent.expanded && !parent.queued {
				push(parent)
			}
		}
	}
	// A commit found later to be reachable from base is the baseline's,
	// however early it was read.
	inWork := map[string]bool{}
	for _, commit := range expanded {
		if !commit.baseline {
			inWork[commit.id] = true
		}
	}
	return parentsFirst(expanded, inWork, commits), nil
}

// list adds id to the listing.
func (w *gitWorkWalk) list(id string) error {
	if w.listed[id] {
		return nil
	}
	id, err := w.intern(id)
	if err != nil {
		return err
	}
	if err := w.keep(gitWalkSetBytes); err != nil {
		return err
	}
	w.listed[id] = true
	w.ids = append(w.ids, id)
	return nil
}

// named records how the work names id. An id named both as a directory and
// as a file is not an object Git could check out either way; it is refused
// rather than listed as one.
func (w *gitWorkWalk) named(id string, kind byte) error {
	if seen, ok := w.kinds[id]; ok {
		if seen != kind {
			return fmt.Errorf("%w: %w: %s is named both as a tree and as a file", ErrGitViewUnavailable, w.reader.corrupt, id)
		}
		return nil
	}
	id, err := w.intern(id)
	if err != nil {
		return err
	}
	if err := w.keep(gitWalkSetBytes); err != nil {
		return err
	}
	w.kinds[id] = kind
	return nil
}

// acquire returns the tree id pinned, from the cache or read and verified.
func (w *gitWorkWalk) acquire(id string) (*walkTree, error) {
	if cached := w.trees[id]; cached != nil {
		if cached.pins == 0 {
			w.unlink(cached)
			w.cached -= cached.bytes
			if err := w.pin(cached.bytes); err != nil {
				return nil, err
			}
		}
		cached.pins++
		return cached, nil
	}
	kind, content, err := w.read(id)
	if err != nil {
		return nil, err
	}
	if kind != "tree" {
		return nil, fmt.Errorf("%w: %w: %s is a %s, not a tree", ErrGitViewUnavailable, w.reader.corrupt, id, kind)
	}
	var cost int64
	entries, err := parseGitTree(id, content, func(count int, bytes int64) error {
		if err := w.step(int64(count) * gitWalkParseSteps); err != nil {
			return err
		}
		cost = bytes + gitWalkSetBytes + goAllocBytes(len(id))
		return w.pin(cost)
	})
	if err != nil {
		return nil, err
	}
	read := &walkTree{id: id, entries: entries, bytes: cost, pins: 1}
	w.trees[id] = read
	return read, nil
}

// index builds the name index of a pinned tree, once while it stays cached.
func (w *gitWorkWalk) index(tree *walkTree) error {
	if tree.byName != nil {
		return nil
	}
	cost := nameIndexBytes(len(tree.entries))
	if err := w.pin(cost); err != nil {
		return err
	}
	tree.bytes += cost
	tree.byName = make(map[string]gitTreeEntry, len(tree.entries))
	for _, entry := range tree.entries {
		tree.byName[entry.name] = entry
	}
	return nil
}

// release unpins a tree; once no frame uses it, it joins the cache, which
// drops its least recently used trees past maxGitWalkCacheBytes.
func (w *gitWorkWalk) release(tree *walkTree) {
	if tree.pins--; tree.pins > 0 {
		return
	}
	w.pinned -= tree.bytes
	w.cached += tree.bytes
	tree.prev, tree.next = nil, w.recent
	if w.recent != nil {
		w.recent.prev = tree
	} else {
		w.oldest = tree
	}
	w.recent = tree
	for w.cached > w.cacheLimit && w.oldest != nil {
		dropped := w.oldest
		w.unlink(dropped)
		w.cached -= dropped.bytes
		delete(w.trees, dropped.id)
	}
}

func (w *gitWorkWalk) unlink(tree *walkTree) {
	if tree.prev != nil {
		tree.prev.next = tree.next
	} else {
		w.recent = tree.next
	}
	if tree.next != nil {
		tree.next.prev = tree.prev
	} else {
		w.oldest = tree.prev
	}
	tree.prev, tree.next = nil, nil
}

// treeFrame is a pinned tree whose entries the walk is going through, with
// the pinned trees its parents hold at the same path.
type treeFrame struct {
	tree    *walkTree
	parents []*walkTree
	depth   int
	next    int
}

// introduce lists root and what it holds that none of parents holds at the
// same path, depth first on an explicit stack. A tree already introduced, or
// held by a parent, is not read again, and an entry already introduced or
// listed is not compared again; a tree more than maxGitTreeDepth directories
// below root is refused.
func (w *gitWorkWalk) introduce(root string, parents []string) error {
	var stack []treeFrame
	enter := func(tree string, parents []string, depth int) error {
		if w.introduced[tree] {
			return nil
		}
		for _, parent := range parents {
			if parent == tree {
				return nil
			}
		}
		if depth > maxGitTreeDepth {
			return fmt.Errorf("%w: %w: tree %s lies more than %d directories below its commit's root",
				ErrGitViewUnavailable, errGitWorkTooLarge, tree, maxGitTreeDepth)
		}
		tree, err := w.intern(tree)
		if err != nil {
			return err
		}
		if err := w.keep(gitWalkSetBytes); err != nil {
			return err
		}
		w.introduced[tree] = true
		if err := w.list(tree); err != nil {
			return err
		}
		read, err := w.acquire(tree)
		if err != nil {
			return err
		}
		frame := treeFrame{tree: read, depth: depth}
		for _, parent := range parents {
			held, err := w.acquire(parent)
			if err != nil {
				return err
			}
			frame.parents = append(frame.parents, held)
			if err := w.index(held); err != nil {
				return err
			}
		}
		stack = append(stack, frame)
		return nil
	}
	if err := enter(root, parents, 0); err != nil {
		return err
	}
	for len(stack) > 0 {
		if err := w.tick(); err != nil {
			return err
		}
		frame := &stack[len(stack)-1]
		if frame.next == len(frame.tree.entries) {
			w.release(frame.tree)
			for _, parent := range frame.parents {
				w.release(parent)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		entry := frame.tree.entries[frame.next]
		frame.next++
		if entry.kind == 'g' {
			continue
		}
		if err := w.named(entry.id, entry.kind); err != nil {
			return err
		}
		// Nothing a parent holds changes what is done with an entry already
		// introduced or listed.
		if entry.kind == 't' && w.introduced[entry.id] || entry.kind != 't' && w.listed[entry.id] {
			continue
		}
		if len(frame.parents) > 0 {
			if err := w.step(int64(len(frame.parents)) * (1 + int64(len(entry.name))/64)); err != nil {
				return err
			}
		}
		same := false
		held := w.held[:0]
		for _, parent := range frame.parents {
			at, ok := parent.byName[entry.name]
			if !ok {
				continue
			}
			if at.id == entry.id && at.kind == entry.kind {
				same = true
				break
			}
			if at.kind == 't' && entry.kind == 't' {
				held = append(held, at.id)
			}
		}
		w.held = held
		switch {
		case same:
		case entry.kind == 't':
			// enter may grow the stack, so frame is not used after it.
			if err := enter(entry.id, w.distinct(held), frame.depth+1); err != nil {
				return err
			}
		default:
			if err := w.list(entry.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// parentsFirst orders the work commits among read so that each comes after
// every parent of it that is also work.
func parentsFirst(read []*walkCommit, inWork map[string]bool, commits map[string]*walkCommit) []*walkCommit {
	type frame struct {
		commit *walkCommit
		next   int
	}
	state := map[string]int{}
	ordered := make([]*walkCommit, 0, len(inWork))
	for _, start := range read {
		if !inWork[start.id] || state[start.id] != 0 {
			continue
		}
		state[start.id] = 1
		stack := []frame{{commit: start}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			if top.next < len(top.commit.facts.parents) {
				parent := top.commit.facts.parents[top.next]
				top.next++
				if inWork[parent] && state[parent] == 0 {
					state[parent] = 1
					stack = append(stack, frame{commit: commits[parent]})
				}
				continue
			}
			state[top.commit.id] = 2
			ordered = append(ordered, top.commit)
			stack = stack[:len(stack)-1]
		}
	}
	return ordered
}
