package main

// THE MANAGER TAKES ITS OWN ORPHANS BACK (owner, 2026-11-10: "i dont really want to restart any
// map myself if thats possible. let the manager do its own thing... the manager should take over
// back as soon as possible").
//
// A manager restart used to leave every on-demand zone it had started running and invisible:
// m.live is empty at boot, a request for an already-served map is dropped without adoption, and
// start() - the only adopter - is reached for WARM maps only. The record's `managed` marker,
// written by the zone from GE_MANAGED=1, is what tells its own orphan from a zone the operator
// started by hand (which is never touched).

import (
	"testing"
	"time"

	"geserver/world"
)

// An orphan this manager's predecessor started is adopted: it leaves the registry as the
// authority for the port and enters m.live, so the idle clock and stopZone can see it.
func TestAManagersOwnOrphanIsAdopted(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 7017, world.Record{MapID: 507, MapName: "fld_ite", Port: 7017, PID: 4242, Managed: true})
	m := idleManager(dir, nil, map[int]*managed{})

	if n := m.adoptManaged(); n != 1 {
		t.Fatalf("adoptManaged() = %d, want 1", n)
	}
	z, ok := m.live[507]
	if !ok {
		t.Fatal("map 507 is not in m.live after adoption")
	}
	if z.port != 7017 {
		t.Fatalf("adopted port = %d, want 7017 (the record is the authority)", z.port)
	}
	if z.cmd != nil {
		t.Fatal("an adopted zone must have no cmd - it is stopped through the record's pid")
	}
	if !m.inUse[7017] {
		t.Fatal("the adopted port is not marked in use; reservePort could hand it to a second zone")
	}
	if _, armed := m.zeroSince[507]; armed {
		t.Fatal("adoption must not arm the idle clock - the first empty sweep does that")
	}
}

// A ZONE THE OPERATOR STARTED BY HAND IS NOT ADOPTED: no `managed` marker, no entry, no stop.
func TestAHandStartedZoneIsNeverAdopted(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 7010, world.Record{MapID: 10, MapName: "reb_basic", Port: 7010, PID: 4000000})
	m := idleManager(dir, nil, map[int]*managed{})

	if n := m.adoptManaged(); n != 0 {
		t.Fatalf("adoptManaged() = %d; a hand-started zone must never be taken over", n)
	}
	if _, ok := m.live[10]; ok {
		t.Fatal("map 10 entered m.live from a record with no managed marker")
	}
	if m.inUse[7010] {
		t.Fatal("a hand-started zone's port was marked in use")
	}
}

// ADOPTION IS NOT DUPLICATION: the first sweep adopts the orphan once, a second sweep adds
// nothing, and a map already in m.live (the warm set started it a moment ago) is left alone.
func TestAdoptionDoesNotDuplicateAnEntry(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 7017, world.Record{MapID: 507, MapName: "fld_ite", Port: 7017, PID: 4242, Managed: true})

	m := idleManager(dir, nil, map[int]*managed{})
	if n := m.adoptManaged(); n != 1 {
		t.Fatalf("first adoptManaged() = %d, want 1", n)
	}
	if n := m.adoptManaged(); n != 0 {
		t.Fatalf("second adoptManaged() = %d, want 0 (already adopted)", n)
	}
	if len(m.live) != 1 {
		t.Fatalf("m.live holds %d entries, want 1", len(m.live))
	}

	held := &managed{mapID: 507, port: 7017, start: time.Now()}
	m2 := idleManager(dir, nil, map[int]*managed{507: held})
	if n := m2.adoptManaged(); n != 0 {
		t.Fatalf("adoptManaged() = %d for a map already in m.live, want 0", n)
	}
	if m2.live[507] != held {
		t.Fatal("adoption replaced the live entry it should have left alone")
	}
}

// THE WHOLE POINT, END TO END IN THE DECISION: an adopted orphan with nobody inside is then
// due for the idle stop, because idleStops only requires "ours", not "we hold its cmd".
func TestAnAdoptedOrphanThenFallsToTheIdleClock(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 7017, world.Record{MapID: 507, MapName: "fld_ite", Port: 7017, PID: 4242, Managed: true, Players: 0})
	m := idleManager(dir, nil, map[int]*managed{})
	m.adoptManaged()

	recs, err := m.reg.Records()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if got := m.idleStops(now, recs, nil); len(got) != 0 {
		t.Fatalf("the first sweep armed %d stop(s); it must only arm", len(got))
	}
	got := m.idleStops(now.Add(11*time.Minute), recs, nil)
	if len(got) != 1 || got[0].mapID != 507 {
		t.Fatalf("due = %+v; an adopted orphan must idle out like any other zone", got)
	}
}
