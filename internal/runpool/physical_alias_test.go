package runpool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPhysicalCaseAliasConflictsInPool(t *testing.T) {
	root := filepath.Join(t.TempDir(), "MixedCaseRoot")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(root), "mixedcaseroot")
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(rootInfo, aliasInfo) {
		t.Skip("filesystem does not expose case aliases")
	}
	child := filepath.Join(alias, "missing", "child")
	if !PathsConflict([]string{root}, []string{alias}) || !PathsConflict([]string{root}, []string{child}) {
		t.Error("case alias bypassed physical-root conflict")
	}
	pool := New(2)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- pool.RunObservedPaths(context.Background(), []string{root}, nil, func() error { close(started); <-release; return nil })
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	states := make(chan State, 1)
	second := make(chan error, 1)
	go func() {
		second <- pool.RunObservedPaths(ctx, []string{child}, func(state State) { states <- state }, func() error { return nil })
	}()
	select {
	case state := <-states:
		if state != StateWaitingResource {
			t.Errorf("state=%s", state)
		}
	case err := <-second:
		t.Errorf("case alias entered a second physical writer: %v", err)
	case <-time.After(time.Second):
		t.Error("pool did not report a resource wait")
	}
	cancel()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
