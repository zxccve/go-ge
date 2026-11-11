package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"geserver/world"
)

// idleManager builds the smallest manager the idle decision needs: a registry on a temp dir,
// a warm set, the live map, and the three clocks.
func idleManager(dir string, warm []int, live map[int]*managed) *manager {
	w := map[int]bool{}
	for _, id := range warm {
		w[id] = true
	}
	return &manager{
		cfg:       config{idle: 10 * time.Minute, linger: 2 * time.Hour, retain: 24 * time.Hour},
		reg:       &world.Registry{Dir: dir},
		warm:      w,
		live:      live,
		inUse:     map[int]bool{},
		ports:     map[int]int{},
		failed:    map[int]time.Time{},
		tries:     map[int]int{},
		zeroSince: map[int]time.Time{},
		recent:    map[int]time.Time{},
	}
}

// A NON-WARM MAP NOBODY EVER ENTERED IS STOPPED AFTER -idle. The owner's own test: "i just need
// to see if our on demand map actually stop when there are no activity." The first sweep arms the
// clock; one second before the grace nothing is due; one second after it, one zone is.
func TestANeverEnteredZoneIsDueAfterIdle(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8001, world.Record{MapID: 6004, MapName: "office_coim", Players: 0})
	m := idleManager(dir, nil, map[int]*managed{6004: {mapID: 6004, port: 8001}})

	now := time.Now()
	recs, err := m.reg.Records()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.idleStops(now, recs, map[int]time.Time{}); len(got) != 0 {
		t.Fatalf("the first sweep armed %d stop(s); a zone seen empty for the first time is not due", len(got))
	}
	if got := m.idleStops(now.Add(10*time.Minute-time.Second), recs, map[int]time.Time{}); len(got) != 0 {
		t.Fatalf("%d stop(s) one second before -idle; a never-used zone gets the full 10m", len(got))
	}
	got := m.idleStops(now.Add(10*time.Minute+time.Second), recs, map[int]time.Time{})
	if len(got) != 1 || got[0].mapID != 6004 || got[0].grace != 10*time.Minute {
		t.Fatalf("due = %+v; a map nobody entered must be due after -idle with grace 10m", got)
	}
}

// A MAP A PLAYER USED IS SOMEBODY'S ROAD: it gets -linger (2 h), not -idle. The 24 h activity
// record (session_count_zone) is what says "used".
func TestAUsedZoneGetsLingerNotIdle(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8001, world.Record{MapID: 204, MapName: "fld_ptv", Players: 0})
	m := idleManager(dir, nil, map[int]*managed{204: {mapID: 204, port: 8001}})

	now := time.Now()
	recs, err := m.reg.Records()
	if err != nil {
		t.Fatal(err)
	}
	recent := map[int]time.Time{204: now.Add(-time.Hour)}
	m.idleStops(now, recs, recent)
	if got := m.idleStops(now.Add(11*time.Minute), recs, recent); len(got) != 0 {
		t.Fatalf("%d stop(s) at 11m; a map used an hour ago is lingered for 2h", len(got))
	}
	got := m.idleStops(now.Add(2*time.Hour+time.Minute), recs, recent)
	if len(got) != 1 || got[0].grace != 2*time.Hour {
		t.Fatalf("due = %+v; a used map is due after -linger with grace 2h", got)
	}
}

// A WARM MAP IS NEVER DUE, however long it sits empty - the towns are the world.
func TestAWarmZoneIsNeverDue(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8001, world.Record{MapID: 10, MapName: "reb_basic", Players: 0})
	m := idleManager(dir, []int{10}, map[int]*managed{10: {mapID: 10, port: 8001}})

	recs, err := m.reg.Records()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m.idleStops(now, recs, nil)
	if got := m.idleStops(now.Add(48*time.Hour), recs, nil); len(got) != 0 {
		t.Fatalf("%d stop(s) for a warm map after 48h empty; a town never idle-stops", len(got))
	}
	if _, armed := m.zeroSince[10]; armed {
		t.Fatal("a warm map's idle clock was armed; it must never even be a candidate")
	}
}

// A MAP WITH A PLAYER INSIDE IS NOT DUE, and the clock is cleared so a later emptiness starts
// from zero rather than from the last time it was seen empty.
func TestAZoneWithAPlayerInsideIsNotDue(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8001, world.Record{MapID: 6004, MapName: "office_coim", Players: 2})
	m := idleManager(dir, nil, map[int]*managed{6004: {mapID: 6004, port: 8001}})

	now := time.Now()
	recs, err := m.reg.Records()
	if err != nil {
		t.Fatal(err)
	}
	m.idleStops(now, recs, nil)
	if got := m.idleStops(now.Add(5*time.Hour), recs, nil); len(got) != 0 {
		t.Fatalf("%d stop(s) with 2 players inside; an open zone has no timer", len(got))
	}
}

// AN ADOPTED ZONE IS STILL DUE. After a manager restart every running zone is adopted: the entry
// in m.live has no cmd, only the port, and the process is reached through the pid in the presence
// record. The old code required z.cmd.Process and skipped exactly these, so a restarted manager
// never shrank the fleet again.
func TestAnAdoptedZoneIsDue(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8001, world.Record{MapID: 6004, MapName: "office_coim", Players: 0, PID: 4000000})
	adopted := &managed{mapID: 6004, port: 8001} // cmd is nil: this manager did not start it
	m := idleManager(dir, nil, map[int]*managed{6004: adopted})

	now := time.Now()
	recs, err := m.reg.Records()
	if err != nil {
		t.Fatal(err)
	}
	m.idleStops(now, recs, nil)
	got := m.idleStops(now.Add(11*time.Minute), recs, nil)
	if len(got) != 1 || got[0].mapID != 6004 {
		t.Fatalf("due = %+v; an adopted zone (no cmd) must idle out like any other", got)
	}
}

// A ZONE THIS MANAGER NEVER OWNED IS NOT ITS TO STOP: a hand-started zone is left alone, and no
// clock is even reported for it.
func TestAHandStartedZoneIsNotDue(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8002, world.Record{MapID: 6005, MapName: "solo_probe", Players: 0, PID: 4000001})
	m := idleManager(dir, nil, map[int]*managed{})

	now := time.Now()
	recs, err := m.reg.Records()
	if err != nil {
		t.Fatal(err)
	}
	m.idleStops(now, recs, nil)
	if got := m.idleStops(now.Add(5*time.Hour), recs, nil); len(got) != 0 {
		t.Fatalf("%d stop(s) for a zone this manager did not start; it must never be killed", len(got))
	}
}

// THE STOP GOES THROUGH stopZone, so an adopted zone's presence file is withdrawn and its slot is
// freed even though there is no cmd to kill. A pid that does not exist is the test's stand-in for
// "the kill cannot succeed here"; the cleanup must still happen and the map must leave m.live.
func TestAnAdoptedZoneIsStoppedThroughItsPresenceRecord(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8001, world.Record{MapID: 6004, MapName: "office_coim", Players: 0, PID: 4000000})
	m := idleManager(dir, nil, map[int]*managed{6004: {mapID: 6004, port: 8001}})
	m.cfg.zonesDir = dir

	m.zeroSince[6004] = time.Now().Add(-11 * time.Minute)
	m.stopIdle()

	if _, live := m.live[6004]; live {
		t.Fatal("map 6004 is still in m.live after the idle stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "zone_"+strconv.Itoa(8001)+".json")); !os.IsNotExist(err) {
		t.Fatalf("the presence file survived the idle stop (stat err = %v)", err)
	}
}
