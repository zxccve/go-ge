package main

// ONE START PER MAP (measured in the container, 2026-11-10).
//
// At boot two callers want the same warm map: startWarm brings the warm set up while run()'s
// first pass calls reconcileWarm, and reconcileWarm sees the map "down" because it only enters
// m.live once its process has actually started. Map 10 came up TWICE in the image - :7010 and
// :7012 - and the registry then held two presence files for one map. The claim in start() is
// what stops the second copy; this test pins it, and pins that the claim is RELEASED so a
// later start (after a failure) is still allowed.

import (
	"path/filepath"
	"testing"
)

// A start already in flight is not started again: the caller gets the answer the winner would
// give, and the winner's claim is left alone.
func TestAMapBeingStartedIsNotStartedTwice(t *testing.T) {
	dir := t.TempDir()
	m := idleManager(dir, []int{10}, map[int]*managed{})
	m.cfg.bin = filepath.Join(dir, "no-such-zone-binary")
	m.starting = map[int]bool{10: true}

	if ok := m.start(10); !ok {
		t.Fatal("start() answered false for a map another call is already starting")
	}
	if _, live := m.live[10]; live {
		t.Fatal("the second start added a live entry for a map that is still loading")
	}
	if !m.starting[10] {
		t.Fatal("the second start released a claim it did not take")
	}
}

// The claim is released when the start returns, so the retry floor can try again: with no
// claim left, a start whose binary cannot run fails (false) instead of being answered "already
// in flight", and it leaves no claim behind.
func TestTheStartClaimIsReleasedWhenTheStartReturns(t *testing.T) {
	dir := t.TempDir()
	m := idleManager(dir, []int{10}, map[int]*managed{})
	m.cfg.bin = filepath.Join(dir, "no-such-zone-binary")
	m.starting = map[int]bool{}
	m.cfg.logdir = dir

	if ok := m.start(10); ok {
		t.Fatal("start() answered true although the zone binary cannot run")
	}
	if _, held := m.starting[10]; held {
		t.Fatal("the claim survived a failed start - the map could never be retried")
	}
}
