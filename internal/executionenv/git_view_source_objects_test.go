package executionenv

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// workObject is one object a view's work adds over the baseline, with the
// loose object file the view stored for it.
type workObject struct {
	id, kind string
	data     []byte
}

// commitPathInGitView commits content at a nested path, so the work adds a
// commit, two trees, and a blob.
func commitPathInGitView(t *testing.T, view GitView, path, content string) string {
	t.Helper()
	full := filepath.Join(view.Path, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", path)
	baselineGit(t, view.Path, "commit", "-qm", "work: "+path)
	return baselineGit(t, view.Path, "rev-parse", "HEAD")
}

// readWorkObjects reads the loose file of every object head adds over base in
// the repository at dir.
func readWorkObjects(t *testing.T, dir, head, base string) []workObject {
	t.Helper()
	var objects []workObject
	for _, id := range strings.Fields(baselineGit(t, dir, "rev-list", "--objects", "--no-object-names", head, "--not", base)) {
		data, err := os.ReadFile(filepath.Join(dir, ".git", "objects", id[:2], id[2:]))
		if err != nil {
			t.Fatalf("object %s is not loose in %s: %v", id, dir, err)
		}
		objects = append(objects, workObject{id: id, kind: baselineGit(t, dir, "cat-file", "-t", id), data: data})
	}
	return objects
}

func looseObjectData(t *testing.T, root, id string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".git", "objects", id[:2], id[2:]))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sourcePlantKinds are what a Run that can write the source and read the view
// may put in the source under the work's ids, next to the real commits and
// trees. Each passes a check that the object files exist; none holds the work.
var sourcePlantKinds = []string{
	"empty blob files",
	"another blob under each blob id",
	"another tree under each tree id",
	"bytes that are not zlib under each blob id",
	"a tree header under each blob id",
	"a wrong size under each blob id",
	"another blob packed and indexed under each blob id",
	"another blob packed under a multi-pack-index",
}

// plantInSource writes the work's real commits and trees into the source and,
// at each blob or tree id, what kind describes.
func plantInSource(t *testing.T, root, kind string, objects []workObject) {
	t.Helper()
	gitDir := filepath.Join(root, ".git")
	otherBlob := baselineGit(t, root, "rev-parse", "HEAD:file.txt")
	otherTree := baselineGit(t, root, "rev-parse", "HEAD^{tree}")
	var packed []string
	for _, object := range objects {
		data := object.data
		if object.kind == "tree" && kind == "another tree under each tree id" {
			data = looseObjectData(t, root, otherTree)
		}
		if object.kind == "blob" {
			switch kind {
			case "empty blob files":
				data = []byte{}
			case "another blob under each blob id":
				data = looseObjectData(t, root, otherBlob)
			case "bytes that are not zlib under each blob id":
				data = []byte("not a zlib stream\n")
			case "a tree header under each blob id":
				data = reheaderLooseObject(t, object.data, "tree", 0)
			case "a wrong size under each blob id":
				data = reheaderLooseObject(t, object.data, "blob", 1)
			case "another blob packed and indexed under each blob id", "another blob packed under a multi-pack-index":
				packed = append(packed, object.id)
				continue
			default:
				continue
			}
		}
		writeLooseGitObject(t, gitDir, object.id, data)
	}
	if len(packed) != 0 {
		plantPackedSubstitute(t, root, otherBlob, packed)
		if kind == "another blob packed under a multi-pack-index" {
			baselineGit(t, root, "multi-pack-index", "write")
		}
	}
}

// reheaderLooseObject rewrites a loose object with another type, or with its
// declared size off by grow, around the same content: valid zlib that does not
// hold the object its path names.
func reheaderLooseObject(t *testing.T, data []byte, kind string, grow int) []byte {
	t.Helper()
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_, content, ok := bytes.Cut(raw, []byte{0})
	if !ok {
		t.Fatal("loose object has no header")
	}
	var out bytes.Buffer
	writer := zlib.NewWriter(&out)
	fmt.Fprintf(writer, "%s %d\x00", kind, len(content)+grow)
	writer.Write(content)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// plantPackedSubstitute packs the substitute object and writes the pack's
// index so that it names each of ids instead: the source then reads the
// substitute's content for those ids from a well-formed pack.
func plantPackedSubstitute(t *testing.T, root, substitute string, ids []string) {
	t.Helper()
	plantPackedObject(t, filepath.Join(root, ".git", "objects", "pack"), root, substitute, ids)
}

// plantPackedObject packs object from the repository at repo and writes the
// pack into packDir with an index that names each of ids instead, as a writer
// of that object store could: whoever reads those ids there gets object's
// content from a well-formed pack, newer than any pack already there. It
// returns the pack's path without its extension.
func plantPackedObject(t *testing.T, packDir, repo, object string, ids []string) string {
	t.Helper()
	var entries []packedAs
	for _, id := range ids {
		entries = append(entries, packedAs{object: object, as: id})
	}
	return plantPackedObjects(t, packDir, repo, entries)
}

// packedAs names one object of a planted pack: the object repo holds, and the
// id the planted index gives it.
type packedAs struct{ object, as string }

// plantPackedObjects is plantPackedObject for several objects at once, each
// indexed under the id entries give it.
func plantPackedObjects(t *testing.T, packDir, repo string, entries []packedAs) string {
	t.Helper()
	dir := t.TempDir()
	var list strings.Builder
	for _, entry := range entries {
		list.WriteString(entry.object + "\n")
	}
	name := "pack-" + gitWithStdin(t, repo, list.String(), "pack-objects", "-q", filepath.Join(dir, "pack"))
	index, err := os.ReadFile(filepath.Join(dir, name+".idx"))
	if err != nil {
		t.Fatal(err)
	}
	const hashLen = 20
	count := int(binary.BigEndian.Uint32(index[8+255*4:]))
	oids, crcs := 8+256*4, 8+256*4+count*hashLen
	offsets := crcs + count*4
	type indexEntry struct{ id, crc, offset []byte }
	packed := map[string]indexEntry{}
	for i := 0; i < count; i++ {
		packed[hex.EncodeToString(index[oids+i*hashLen:oids+(i+1)*hashLen])] =
			indexEntry{crc: index[crcs+i*4 : crcs+(i+1)*4], offset: index[offsets+i*4 : offsets+(i+1)*4]}
	}
	var rows []indexEntry
	for _, entry := range entries {
		row, ok := packed[entry.object]
		if !ok {
			t.Fatalf("object %s is not in the planted pack", entry.object)
		}
		row.id, _ = hex.DecodeString(entry.as)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i].id, rows[j].id) < 0 })
	packSum := index[len(index)-2*hashLen : len(index)-hashLen]
	var forged bytes.Buffer
	forged.Write(index[:8])
	for b := 0; b < 256; b++ {
		below := 0
		for _, row := range rows {
			if int(row.id[0]) <= b {
				below++
			}
		}
		binary.Write(&forged, binary.BigEndian, uint32(below))
	}
	for _, row := range rows {
		forged.Write(row.id)
	}
	for _, row := range rows {
		forged.Write(row.crc)
	}
	for _, row := range rows {
		forged.Write(row.offset)
	}
	forged.Write(packSum)
	sum := sha1.Sum(forged.Bytes())
	forged.Write(sum[:])
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pack, err := os.ReadFile(filepath.Join(dir, name+".pack"))
	if err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string][]byte{name + ".pack": pack, name + ".idx": forged.Bytes()} {
		if err := os.WriteFile(filepath.Join(packDir, file), data, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(packDir, name)
}

// gitWithStdin is baselineGit with input on stdin.
func gitWithStdin(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanGitProbeEnv(os.Environ())
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// An id says what content it names, not what the source holds under it.
// Planted objects that only look like the work make /views report it
// undelivered and /apply fail rather than report a delivery.
func TestPlantedObjectsNeverCountAsDeliveredWork(t *testing.T) {
	for format, fixture := range sourceRefFormats {
		for _, name := range sourcePlantKinds {
			t.Run(format+"/"+name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				root := fixture(t)
				baseline, err := InspectCleanGitBaseline(ctx, root)
				if err != nil {
					t.Fatal(err)
				}
				views := filepath.Join(t.TempDir(), "views")
				view, err := EnsureGitView(ctx, views, "planted", baseline)
				if err != nil {
					t.Fatal(err)
				}
				head := commitPathInGitView(t, view, "a/b.txt", "planted work\n")
				plantInSource(t, root, name, readWorkObjects(t, view.Path, head, baseline.Commit))
				baselineGit(t, root, "update-ref", "refs/heads/selfmind/"+view.ID, head)
				if info, err := InspectGitViewDelivery(ctx, views, view.ID, baseline); err != nil || info.DeliveredBranch != "" {
					t.Fatalf("planted objects counted as delivered work: %+v %v", info, err)
				}
				if delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err == nil || delivered.DeliveredBranch != "" {
					t.Fatalf("/apply reported planted objects as delivered: %+v %v", delivered, err)
				}
			})
		}
	}
}

// Prune deletes the archive, so the same planted objects must keep it, both at
// the archive's own inspection and at the import that protects the work.
func TestPlantedObjectsNeverLetPruneDeleteTheArchive(t *testing.T) {
	for format, fixture := range sourceRefFormats {
		for _, name := range sourcePlantKinds {
			t.Run(format+"/"+name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				root := fixture(t)
				baseline, err := InspectCleanGitBaseline(ctx, root)
				if err != nil {
					t.Fatal(err)
				}
				views := filepath.Join(t.TempDir(), "views")
				view, err := EnsureGitView(ctx, views, "planted", baseline)
				if err != nil {
					t.Fatal(err)
				}
				head := commitPathInGitView(t, view, "a/b.txt", "archived work\n")
				objects := readWorkObjects(t, view.Path, head, baseline.Commit)
				if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
					t.Fatal(err)
				}
				retired, err := RetireGitView(ctx, views, view.ID, baseline, nil)
				if err != nil {
					t.Fatal(err)
				}
				removeGitPacks(t, filepath.Join(root, ".git"))
				plantInSource(t, root, name, objects)
				if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err == nil {
					t.Fatal("prune deleted the archive while the source held planted objects")
				}
				if _, err := os.Stat(filepath.Join(retired.Path, "a", "b.txt")); err != nil {
					t.Fatalf("archive was not kept: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(views, ".retired", ".pruned", view.ID+".json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a prune record was written: %v", err)
				}
				archive, err := retiredGitViewDir(views)
				if err != nil {
					t.Fatal(err)
				}
				info := GitViewDelivery{ViewID: view.ID, Path: retired.Path, HeadCommit: head}
				// The import alone already refuses: index-pack compares each
				// object it receives with the copy the source holds.
				if err := importGitViewCommit(ctx, baseline, info); err == nil {
					t.Fatal("index-pack accepted the work over planted objects")
				}
				record := PrunedGitView{Version: 1, SourceRoot: baseline.Root, BaselineCommit: baseline.Commit,
					HeadCommit: head, RetainedRef: retainedGitViewRef(view.ID)}
				if err := retainGitViewCommit(ctx, archive, baseline, record, info); err == nil {
					t.Fatal("the work was protected while the source held planted objects")
				}
				if refs := baselineGit(t, root, "for-each-ref", "--format=%(refname)", record.RetainedRef); refs != "" {
					t.Fatalf("a protected ref was created: %s", refs)
				}
			})
		}
	}
}

// After prune the source is the only copy, so a pruned view whose work the
// source holds only as planted objects is not reported as protected.
func TestPlantedObjectsAreNotReportedAsProtectedWork(t *testing.T) {
	for format, fixture := range sourceRefFormats {
		for _, name := range sourcePlantKinds {
			t.Run(format+"/"+name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				root := fixture(t)
				baseline, err := InspectCleanGitBaseline(ctx, root)
				if err != nil {
					t.Fatal(err)
				}
				views := filepath.Join(t.TempDir(), "views")
				view, err := EnsureGitView(ctx, views, "planted", baseline)
				if err != nil {
					t.Fatal(err)
				}
				head := commitPathInGitView(t, view, "a/b.txt", "pruned work\n")
				objects := readWorkObjects(t, view.Path, head, baseline.Commit)
				if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
					t.Fatal(err)
				}
				if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err != nil {
					t.Fatal(err)
				}
				removeGitPacks(t, filepath.Join(root, ".git"))
				plantInSource(t, root, name, objects)
				if record, err := InspectPrunedGitView(ctx, views, view.ID, baseline); err == nil || !strings.Contains(err.Error(), "no longer complete") {
					t.Fatalf("planted objects were reported as protected work: %+v %v", record, err)
				}
			})
		}
	}
}

// A proof that could not run is no evidence about the work: cancellation is
// reported as such, and a failed Git command carries Git's own message.
func TestSourceWorkProofKeepsFailureMeaning(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	refs, err := newGitSourceRefDir(ctx, f.views, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer refs.close()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := refs.holdsObjects(cancelled, []string{f.baseline.Commit}); !errors.Is(err, context.Canceled) || gitSourceLacksWork(err) {
		t.Fatalf("cancelled proof = %v", err)
	}
	head := baselineGit(t, f.pending.Path, "rev-parse", "HEAD")
	unknown := strings.Repeat("ab", 20)
	if err := proveGitWorkInSource(ctx, refs, f.pending.Path, head, unknown); err == nil || gitSourceLacksWork(err) || !strings.Contains(err.Error(), unknown) {
		t.Fatalf("a failed listing lost Git's message: %v", err)
	}
}

// A proof stopped while Git is running reports that it did not finish, never
// that the work is missing. A stand-in git that never answers plays the slow
// listing (the walk's reads of the view) or the slow read-back of the source;
// the proof is cancelled once that has started, or runs into a deadline.
func TestSourceWorkProofReportsInterruptionMidRun(t *testing.T) {
	phases := map[string]func(ctx context.Context, f sourceAttackFixture, refs *gitSourceRefDir, head string, ids []string) error{
		"listing": func(ctx context.Context, f sourceAttackFixture, refs *gitSourceRefDir, head string, ids []string) error {
			_, err := listGitViewWork(ctx, f.pending.Path, head, f.baseline.Commit)
			return err
		},
		"read-back": func(ctx context.Context, f sourceAttackFixture, refs *gitSourceRefDir, head string, ids []string) error {
			return refs.holdsObjects(ctx, ids)
		},
	}
	for phase, run := range phases {
		for _, deadline := range []bool{false, true} {
			name := phase + "/cancelled"
			if deadline {
				name = phase + "/timed out"
			}
			t.Run(name, func(t *testing.T) {
				f := newSourceAttackFixture(t)
				refs, err := newGitSourceRefDir(context.Background(), f.views, f.baseline)
				if err != nil {
					t.Fatal(err)
				}
				defer refs.close()
				head := baselineGit(t, f.pending.Path, "rev-parse", "HEAD")
				ids, err := listGitViewWork(context.Background(), f.pending.Path, head, f.baseline.Commit)
				if err != nil {
					t.Fatal(err)
				}
				ctx, startedFile := stuckGit(t, "cat-file")
				want := error(context.Canceled)
				if deadline {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(context.Background(), 2*time.Second)
					defer stop()
					want = context.DeadlineExceeded
				}
				started := time.Now()
				err = run(ctx, f, refs, head, ids)
				if !errors.Is(err, want) || !strings.Contains(err.Error(), "git cat-file did not finish") || gitSourceLacksWork(err) {
					t.Fatalf("interrupted proof = %v", err)
				}
				if _, statErr := os.Stat(startedFile); statErr != nil {
					t.Fatalf("the stuck cat-file never started: %v", statErr)
				}
				if elapsed := time.Since(started); elapsed > 10*time.Second {
					t.Fatalf("the proof waited %v for a stopped Git", elapsed)
				}
			})
		}
	}
}

// The reachability check of prune streams rev-list; stopped, it too reports
// that it did not finish, and the archive stays.
func TestPruneReachabilityCheckReportsInterruption(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	stuck, startedFile := stuckGit(t, "rev-list")
	if _, err := PruneRetiredGitView(stuck, f.views, f.archived.ID, f.baseline, nil); !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "git rev-list did not finish") {
		t.Fatalf("interrupted prune = %v", err)
	}
	if _, statErr := os.Stat(startedFile); statErr != nil {
		t.Fatalf("the stuck rev-list never started: %v", statErr)
	}
	archive, err := retiredGitViewDir(f.views)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectGitViewDelivery(ctx, archive, f.archived.ID, f.baseline); err != nil {
		t.Fatalf("archive is not intact after an interrupted prune: %v", err)
	}
}

// An alternates entry that appears while the objects are read back could have
// supplied them, so the proof refuses it even though every object hashed.
func TestSourceWorkProofRefusesAlternatesAddedWhileItRuns(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	info, err := InspectGitViewDelivery(ctx, f.views, f.pending.ID, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := importGitViewCommit(ctx, f.baseline, info); err != nil {
		t.Fatal(err)
	}
	refs, err := newGitSourceRefDir(ctx, f.views, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer refs.close()
	ids, err := listGitViewWork(ctx, f.pending.Path, info.HeadCommit, f.baseline.Commit)
	if err != nil {
		t.Fatal(err)
	}
	gitStandIn(t, plantAlternatesAction(t, f.gitDir), "cat-file")
	if err := refs.holdsObjects(ctx, ids); err == nil || !strings.Contains(err.Error(), "alternates") || gitSourceLacksWork(err) {
		t.Fatalf("a proof that ran while the source gained alternates = %v", err)
	}
}

// The proof hashes each object as it streams past, through one buffer, so the
// garbage it leaves does not grow with the size of the work.
func TestObjectProofReusesOneBuffer(t *testing.T) {
	var batch bytes.Buffer
	var ids []string
	body := bytes.Repeat([]byte("x"), 48<<10)
	for i := 0; i < 256; i++ {
		content := append([]byte(fmt.Sprintf("%d\n", i)), body...)
		sum := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(content))), content...))
		id := hex.EncodeToString(sum[:])
		ids = append(ids, id)
		fmt.Fprintf(&batch, "%s blob %d\n", id, len(content))
		batch.Write(content)
		batch.WriteByte('\n')
	}
	stream := bytes.NewReader(batch.Bytes())
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := readGitObjectProof(bufio.NewReader(stream), ids, false)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("proving %d objects of %d bytes allocated %d bytes", len(ids), batch.Len(), allocated)
	}
}

// A source whose format its own Git no longer accepts, or reads differently,
// holds no delivered or protected work: an archive does not count as
// delivered, prune keeps it, and /apply fails.
func TestSourceFormatChangesFailClosed(t *testing.T) {
	variants := map[string][][2]string{
		"unsupported format version": {{"core.repositoryformatversion", "2"}},
		"unknown extension":          {{"core.repositoryformatversion", "1"}, {"extensions.futureformat", "true"}},
		"object format changed":      {{"core.repositoryformatversion", "1"}, {"extensions.objectformat", "sha256"}},
		"ref storage changed":        {{"core.repositoryformatversion", "1"}, {"extensions.refstorage", "reftable"}},
	}
	for name, settings := range variants {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newSourceAttackFixture(t)
			for _, setting := range settings {
				baselineGit(t, f.root, "config", "--file", filepath.Join(f.gitDir, "config"), setting[0], setting[1])
			}
			archive, err := retiredGitViewDir(f.views)
			if err != nil {
				t.Fatal(err)
			}
			if info, err := InspectGitViewDelivery(ctx, archive, f.archived.ID, f.baseline); err == nil && info.DeliveredBranch != "" {
				t.Fatalf("an archive counted as delivered to a source its own Git cannot read: %+v", info)
			}
			if _, err := PruneRetiredGitView(ctx, f.views, f.archived.ID, f.baseline, nil); err == nil {
				t.Fatal("prune deleted an archive whose source its own Git cannot read")
			}
			if _, err := os.Stat(filepath.Join(archive, f.archived.ID, "file.txt")); err != nil {
				t.Fatalf("archive was not kept: %v", err)
			}
			if delivered, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline); err == nil || delivered.DeliveredBranch != "" {
				t.Fatalf("/apply delivered into a source its own Git cannot read: %+v %v", delivered, err)
			}
		})
	}
}

// A source whose object directory is a symbolic link is read through it, as
// its own Git reads it: real work counts there, and planted objects fail.
func TestSymlinkedSourceObjectsAreProvenLikeAnyOther(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	store := filepath.Join(t.TempDir(), "objects")
	if err := os.Rename(filepath.Join(root, ".git", "objects"), store); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, filepath.Join(root, ".git", "objects")); err != nil {
		t.Fatal(err)
	}
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	kept, err := EnsureGitView(ctx, views, "kept", baseline)
	if err != nil {
		t.Fatal(err)
	}
	head := commitPathInGitView(t, kept, "a/b.txt", "real work\n")
	if _, err := DeliverGitViewBranch(ctx, views, kept.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireGitView(ctx, views, kept.ID, baseline, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneRetiredGitView(ctx, views, kept.ID, baseline, nil); err != nil {
		t.Fatalf("prune through a linked object directory: %v", err)
	}
	if got := baselineGit(t, root, "cat-file", "-p", head+":a/b.txt"); got != "real work" {
		t.Fatalf("protected content = %q", got)
	}
	planted, err := EnsureGitView(ctx, views, "planted", baseline)
	if err != nil {
		t.Fatal(err)
	}
	plantedHead := commitPathInGitView(t, planted, "c/d.txt", "planted work\n")
	plantInSource(t, root, "empty blob files", readWorkObjects(t, planted.Path, plantedHead, baseline.Commit))
	baselineGit(t, root, "update-ref", "refs/heads/selfmind/"+planted.ID, plantedHead)
	if info, err := InspectGitViewDelivery(ctx, views, planted.ID, baseline); err != nil || info.DeliveredBranch != "" {
		t.Fatalf("planted objects behind a linked object directory counted as delivered: %+v %v", info, err)
	}
	if delivered, err := DeliverGitViewBranch(ctx, views, planted.ID, baseline); err == nil || delivered.DeliveredBranch != "" {
		t.Fatalf("/apply reported planted objects as delivered: %+v %v", delivered, err)
	}
}
