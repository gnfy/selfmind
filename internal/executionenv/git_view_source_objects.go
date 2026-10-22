package executionenv

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"time"
)

// Delivered or protected work must be rebuildable from the source alone. An
// object's id names content; it does not show what the source holds under it.
// A Run that can write the source can put the work's real commit and trees
// there and an empty file or another object at each blob's path, and a check
// that the object files exist would pass. The proof therefore reads every
// object of the work back from the source and hashes it.

var (
	// errGitSourceIncomplete marks work some of whose objects the source does
	// not hold in readable form.
	errGitSourceIncomplete = errors.New("the source repository does not hold every object of the work")
	// errGitSourceCorrupt marks an object the source holds with content its id
	// does not name.
	errGitSourceCorrupt = errors.New("the source repository holds other content under an object of the work")
)

// errGitObjectCutShort marks an object whose content ended before its
// declared size, which cat-file reports by stopping mid-object.
var errGitObjectCutShort = errors.New("object content ended early")

// gitSourceLacksWork reports a proof that failed on the source's objects
// themselves, as opposed to a source that could not be read or trusted.
func gitSourceLacksWork(err error) bool {
	return errors.Is(err, errGitSourceIncomplete) || errors.Is(err, errGitSourceCorrupt)
}

// proveGitWorkInSource proves the source holds, with matching content, every
// object the work's head adds over base, as the view or its archive lists
// them. Objects reachable from base are the source's own history and are not
// proven again.
//
// The listing is the daemon's own walk over the view's or archive's objects
// (listGitWorkObjects), each commit and tree accepted only when it hashes to
// its id: no pack, index, loose object, graft, commit-graph, or replace ref
// the Run writes can change which objects count as the baseline's. A writer of
// the source does not reach it either, except through objects the work shares
// with the source that the view never stored itself; those are read the same
// way, so a substitute the source holds for one fails the walk or the hash
// below instead of changing what passes.
func proveGitWorkInSource(ctx context.Context, refs *gitSourceRefDir, viewPath, head, base string) error {
	if head == base {
		return nil
	}
	ids, err := listGitViewWork(ctx, viewPath, head, base)
	if err != nil {
		return err
	}
	return refs.holdsObjects(ctx, ids)
}

// listGitViewWork lists the objects head adds over base, walking the objects
// of the view or archive at viewPath.
func listGitViewWork(ctx context.Context, viewPath, head, base string) ([]string, error) {
	reader, err := newGitObjectReader(ctx, viewPath, gitViewEnv(viewPath), errGitViewIncomplete, errGitViewCorrupt)
	if err != nil {
		return nil, err
	}
	defer reader.close()
	return listGitWorkObjects(reader, head, base)
}

// proveGitPrunedWorkInSource proves the same for a pruned view. Its archive is
// gone, so the walk reads the source's own objects, each verified the same way;
// an object it cannot read makes the work incomplete.
func proveGitPrunedWorkInSource(ctx context.Context, scratch string, baseline GitBaseline, head string) error {
	refs, err := newGitSourceRefDir(ctx, scratch, baseline)
	if err != nil {
		return err
	}
	defer refs.close()
	reader, err := newGitObjectReader(ctx, refs.dir, refs.env(), errGitSourceIncomplete, errGitSourceCorrupt)
	if err != nil {
		return err
	}
	ids, err := listGitWorkObjects(reader, head, baseline.Commit)
	reader.close()
	if err != nil {
		return err
	}
	return refs.holdsObjects(ctx, ids)
}

// holdsObjects proves the source holds every object in ids with the content
// its id names: cat-file reads each one back and the daemon hashes it, so a
// placeholder or another object at an object's path does not count, and every
// other copy the source holds of an id must hash the same way
// (holdsEveryCopy). Replace refs are ignored, and objects borrowed through
// alternates are refused before and after the read.
func (r *gitSourceRefDir) holdsObjects(ctx context.Context, ids []string) error {
	return r.readObjects(ctx, ids, false)
}

// holdsNoSubstitutes checks, the same way, every object in ids the source
// already holds, and passes over the ones it lacks.
func (r *gitSourceRefDir) holdsNoSubstitutes(ctx context.Context, ids []string) error {
	return r.readObjects(ctx, ids, true)
}

func (r *gitSourceRefDir) readObjects(ctx context.Context, ids []string, allowMissing bool) error {
	if err := gitSourceObjectIndirection(r.baseline.CommonDir); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if err := r.readResolved(ctx, r.env(), []string{"cat-file", "--batch"}, ids, allowMissing); err != nil {
		return err
	}
	if err := r.holdsEveryCopy(ctx, ids, allowMissing); err != nil {
		return err
	}
	return gitSourceObjectIndirection(r.baseline.CommonDir)
}

// readResolved reads ids with cat-file, run with env and args, and hashes each
// object Git resolves.
func (r *gitSourceRefDir) readResolved(ctx context.Context, env, args, ids []string, allowMissing bool) error {
	gitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := hostGitCommand(gitCtx, r.dir, env, args...)
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	var stderr gitStderr
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: git cat-file: %w", ErrGitViewUnavailable, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: git cat-file: %w", ErrGitViewUnavailable, err)
	}
	if err := readGitObjectProof(bufio.NewReader(stdout), ids, allowMissing); err != nil {
		ctxErr := gitCtx.Err()
		cancel()
		_ = cmd.Wait()
		switch {
		case gitSourceLacksWork(err):
			// Read from a complete answer, so no interruption explains it.
			return err
		case ctxErr != nil:
			return fmt.Errorf("%w: git cat-file did not finish: %w", ErrGitViewUnavailable, ctxErr)
		case errors.Is(err, errGitObjectCutShort):
			// cat-file stops partway through an object the source cannot
			// produce whole, such as one whose header claims a larger size.
			return fmt.Errorf("%w: %w: %w: %s", ErrGitViewUnavailable, errGitSourceCorrupt, err, strings.TrimSpace(stderr.String()))
		default:
			return fmt.Errorf("%w: git cat-file: %w: %s", ErrGitViewUnavailable, err, strings.TrimSpace(stderr.String()))
		}
	}
	if err := cmd.Wait(); err != nil {
		return gitCommandError(gitCtx, args, err, &stderr)
	}
	return nil
}

// readGitObjectProof reads cat-file --batch output for ids, in order, and
// checks that each object's content hashes to its id; with allowMissing, an
// object the source lacks is passed over. Content is hashed as it streams
// past, through one buffer and one hasher per id length.
func readGitObjectProof(out *bufio.Reader, ids []string, allowMissing bool) error {
	buffer := make([]byte, 64<<10)
	digests := map[int]hash.Hash{}
	for _, id := range ids {
		header, err := out.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read object %s: %w", id, err)
		}
		fields := strings.Fields(header)
		if len(fields) == 2 && fields[0] == id && fields[1] == "missing" {
			if allowMissing {
				continue
			}
			return fmt.Errorf("%w: %w: %s is missing or unreadable", ErrGitViewUnavailable, errGitSourceIncomplete, id)
		}
		var size int64
		if len(fields) == 3 && fields[0] == id {
			size, err = strconv.ParseInt(fields[2], 10, 64)
		}
		if len(fields) != 3 || fields[0] != id || err != nil || size < 0 {
			return fmt.Errorf("unexpected object header %q", strings.TrimSpace(header))
		}
		digest := digests[len(id)]
		if digest == nil {
			digest = gitObjectHash(id)
			digests[len(id)] = digest
		}
		digest.Reset()
		fmt.Fprintf(digest, "%s %d\x00", fields[1], size)
		if copied, err := io.CopyBuffer(digest, io.LimitReader(out, size), buffer); err != nil || copied != size {
			return fmt.Errorf("read object %s: %w", id, errors.Join(errGitObjectCutShort, err))
		}
		if end, err := out.ReadByte(); err != nil || end != '\n' {
			return fmt.Errorf("read object %s: %w", id, errGitObjectCutShort)
		}
		if hex.EncodeToString(digest.Sum(nil)) != id {
			return fmt.Errorf("%w: %w: %s", ErrGitViewUnavailable, errGitSourceCorrupt, id)
		}
	}
	return nil
}

// gitObjectHash is the hash an object id of this length was computed with.
func gitObjectHash(id string) hash.Hash {
	if len(id) == 64 {
		return sha256.New()
	}
	return sha1.New()
}
