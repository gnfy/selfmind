package executionenv

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Git does not hash what it reads, and need not read one object from one copy:
// when a pack (the newest or most recently used) and a loose file both hold an
// id, Git 2.54 announces a blob's size from the pack copy but streams its
// content from the loose copy, and reads a commit or tree whole from the pack
// copy. With a multi-pack index, which the person's own Git reads by default,
// that index chooses the pack copy. A source can hold more than one copy of an
// object: the import itself adds a pack each time, and a writer of the source
// can add a pack whose index names one of the work's ids for other content,
// placed so that the genuine copy is read today. Pack order or a repack could
// make Git read that other copy later, so every copy of the work's objects
// must hash to its id, not only the one Git resolves now.

// holdsEveryCopy checks the copies of ids that Git did not resolve: a loose
// object beside a packed one, the copy in each pack whose index names the id,
// and, when the source has a multi-pack index, the copy that index resolves.
// An id with a single copy was checked by the resolved read already. With
// allowMissing, an id the source lacks has no copy to check.
func (r *gitSourceRefDir) holdsEveryCopy(ctx context.Context, ids []string, allowMissing bool) error {
	objects := filepath.Join(r.baseline.CommonDir, "objects")
	copies := make(map[string]int, len(ids))
	var loose []string
	for _, id := range ids {
		info, err := os.Lstat(filepath.Join(objects, id[:2], id[2:]))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil || !info.Mode().IsRegular():
			return fmt.Errorf("%w: %w: the loose copy of %s is not a plain file", ErrGitViewUnavailable, errGitSourceCorrupt, id)
		default:
			loose = append(loose, id)
			copies[id]++
		}
	}
	indexes, err := filepath.Glob(filepath.Join(objects, "pack", "*.idx"))
	if err != nil {
		return fmt.Errorf("%w: list source packs: %v", ErrGitViewUnavailable, err)
	}
	named := map[string][]string{}
	for _, index := range indexes {
		held, err := gitPackIndexNames(index, ids)
		if err != nil {
			return fmt.Errorf("%w: %w: pack index %s cannot be checked: %v", ErrGitViewUnavailable, errGitSourceCorrupt, filepath.Base(index), err)
		}
		named[index] = held
		for _, id := range held {
			copies[id]++
		}
	}
	for _, id := range ids {
		if copies[id] == 0 && !allowMissing {
			return fmt.Errorf("%w: %w: Git reads %s from a copy the daemon cannot locate", ErrGitViewUnavailable, errGitSourceCorrupt, id)
		}
	}
	for _, id := range loose {
		if copies[id] > 1 {
			if err := verifyLooseGitObject(filepath.Join(objects, id[:2], id[2:]), id); err != nil {
				return err
			}
		}
	}
	sort.Strings(indexes)
	for _, index := range indexes {
		var shared []string
		for _, id := range named[index] {
			if copies[id] > 1 {
				shared = append(shared, id)
			}
		}
		if len(shared) == 0 {
			continue
		}
		if err := r.readPackCopies(ctx, index, shared); err != nil {
			return err
		}
	}
	for _, midx := range []string{"multi-pack-index", "multi-pack-index.d"} {
		if _, err := os.Lstat(filepath.Join(objects, "pack", midx)); err == nil {
			// A multi-pack index chooses the copy Git reads when it is on.
			return r.readResolved(ctx, r.env(), []string{"-c", "core.multiPackIndex=true", "cat-file", "--batch"}, ids, allowMissing)
		}
	}
	return nil
}

// readPackCopies reads ids through an object directory that holds only the
// pack whose index is at index, so each id resolves to that pack's copy.
func (r *gitSourceRefDir) readPackCopies(ctx context.Context, index string, ids []string) error {
	objects, err := os.MkdirTemp(r.dir, "copy-")
	if err != nil {
		return fmt.Errorf("%w: prepare pack check: %v", ErrGitViewUnavailable, err)
	}
	defer os.RemoveAll(objects)
	base := index[:len(index)-len(".idx")]
	if err := os.Mkdir(filepath.Join(objects, "pack"), 0o700); err != nil {
		return fmt.Errorf("%w: prepare pack check: %v", ErrGitViewUnavailable, err)
	}
	for _, ext := range []string{".pack", ".idx"} {
		if err := os.Symlink(base+ext, filepath.Join(objects, "pack", filepath.Base(base)+ext)); err != nil {
			return fmt.Errorf("%w: prepare pack check: %v", ErrGitViewUnavailable, err)
		}
	}
	env := append(cleanGitProbeEnv(os.Environ()),
		"GIT_DIR="+r.dir, "GIT_COMMON_DIR="+r.dir, "GIT_OBJECT_DIRECTORY="+objects, "GIT_NO_REPLACE_OBJECTS=1")
	return r.readResolved(ctx, env, []string{"cat-file", "--batch"}, ids, false)
}

// verifyLooseGitObject inflates the loose object at path and hashes it.
func verifyLooseGitObject(path, id string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %w: read the loose copy of %s: %v", ErrGitViewUnavailable, errGitSourceCorrupt, id, err)
	}
	defer file.Close()
	inflated, err := zlib.NewReader(bufio.NewReader(file))
	if err != nil {
		return fmt.Errorf("%w: %w: the loose copy of %s is not an object", ErrGitViewUnavailable, errGitSourceCorrupt, id)
	}
	digest := gitObjectHash(id)
	if _, err := io.Copy(digest, inflated); err != nil || hex.EncodeToString(digest.Sum(nil)) != id {
		return fmt.Errorf("%w: %w: the loose copy of %s holds other content", ErrGitViewUnavailable, errGitSourceCorrupt, id)
	}
	return nil
}

// maxGitPackIndexTable bounds the object-name table read whole into memory;
// a larger one is searched in place.
const maxGitPackIndexTable = 64 << 20

// gitPackIndexNames returns the ids, of all the same length, that the pack
// index at path names. Version 1 and version 2 indexes are read; any other is
// an error, since its copies could not be checked.
func gitPackIndexNames(path string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	hashLen := int64(len(ids[0]) / 2)
	var header [8]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return nil, err
	}
	fanoutAt, tableAt, stride := int64(0), int64(1024+4), 4+hashLen
	if bytes.Equal(header[:4], []byte{0xff, 't', 'O', 'c'}) {
		if version := binary.BigEndian.Uint32(header[4:]); version != 2 {
			return nil, fmt.Errorf("pack index version %d", version)
		}
		fanoutAt, tableAt, stride = 8, 8+1024, hashLen
	}
	fanout := make([]byte, 1024)
	if _, err := file.ReadAt(fanout, fanoutAt); err != nil {
		return nil, err
	}
	count := int64(binary.BigEndian.Uint32(fanout[1020:]))
	if tableAt+count*stride > info.Size() {
		return nil, fmt.Errorf("pack index names more objects than it holds")
	}
	var table []byte
	if count*stride <= maxGitPackIndexTable {
		table = make([]byte, count*stride)
		if _, err := file.ReadAt(table, tableAt); err != nil {
			return nil, err
		}
	}
	name := make([]byte, hashLen)
	nameAt := func(i int64) ([]byte, error) {
		if table != nil {
			return table[i*stride : i*stride+hashLen], nil
		}
		_, err := file.ReadAt(name, tableAt+i*stride)
		return name, err
	}
	var held []string
	for _, id := range ids {
		raw, err := hex.DecodeString(id)
		if err != nil || int64(len(raw)) != hashLen {
			return nil, fmt.Errorf("object id %q", id)
		}
		low := int64(0)
		if raw[0] > 0 {
			low = int64(binary.BigEndian.Uint32(fanout[(int(raw[0])-1)*4:]))
		}
		high := int64(binary.BigEndian.Uint32(fanout[int(raw[0])*4:]))
		if low > high || high > count {
			return nil, fmt.Errorf("pack index fan-out is not ordered")
		}
		for low < high {
			middle := low + (high-low)/2
			candidate, err := nameAt(middle)
			if err != nil {
				return nil, err
			}
			switch compared := bytes.Compare(candidate, raw); {
			case compared == 0:
				held = append(held, id)
				low = high
			case compared < 0:
				low = middle + 1
			default:
				high = middle
			}
		}
	}
	return held, nil
}
