package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"geserver/world"
)

// writePresence drops a presence file where the manager will read it.
func writePresence(t *testing.T, dir string, port int, rec world.Record) {
	t.Helper()
	rec.Port = port
	rec.LastSeen = float64(time.Now().UnixNano()) / 1e9
	if rec.IP == "" {
		rec.IP = "127.0.0.1"
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "zone_"+strconv.Itoa(port)+".json")
	if err := os.WriteFile(name, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A PLAYER'S OPEN ZONE IS NEVER EVICTED TO MAKE ROOM. A mission may be killed when its timer
// runs out; an open map has no timer, so somebody standing in a non-warm map for an hour keeps
// it (the owner, 2026-11-09: "zone have no such timer so if player stays for long time, we also
// have to accommodate that").
func TestACapNeverEvictsAZoneWithAPlayerInside(t *testing.T) {
	dir := t.TempDir()
	reg := &world.Registry{Dir: dir}
	writePresence(t, dir, 8000, world.Record{MapID: 1361, Port: 8000, MapName: "btl_auch01", Players: 2})

	m := &manager{
		cfg:       config{zonesDir: dir, maxZones: 1},
		reg:       reg,
		warm:      map[int]bool{},
		live:      map[int]*managed{1361: {mapID: 1361, port: 8000, start: time.Now().Add(-time.Hour)}},
		zeroSince: map[int]time.Time{},
	}

	if got := m.evictable(); got != 0 {
		t.Fatalf("evictable() = %d; the only zone has 2 players and must not be evicted", got)
	}
	if !m.atCap() {
		t.Fatal("atCap() = false with one zone and max 1")
	}

	// The player leaves: the same zone becomes the candidate, oldest first.
	writePresence(t, dir, 8000, world.Record{MapID: 1361, Port: 8000, MapName: "btl_auch01", Players: 0})
	if got := m.evictable(); got != 1361 {
		t.Fatalf("evictable() = %d; with nobody inside, map 1361 is the one to stop", got)
	}
}

// A WARM MAP IS NEVER A CANDIDATE, even with nobody inside: the towns must stay up.
func TestAWarmMapIsNeverEvicted(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8000, world.Record{MapID: 10, Port: 8000, MapName: "reb_basic"})
	m := &manager{
		cfg:       config{zonesDir: dir, maxZones: 1},
		reg:       &world.Registry{Dir: dir},
		warm:      map[int]bool{10: true},
		live:      map[int]*managed{10: {mapID: 10, port: 8000, start: time.Now()}},
		zeroSince: map[int]time.Time{},
	}
	if got := m.evictable(); got != 0 {
		t.Fatalf("evictable() = %d; a warm map is never stopped for the cap", got)
	}
}

// THE OLDEST EMPTY ZONE IS THE ONE THAT GOES, so a map somebody just opened is not the first
// casualty of the next request.
func TestTheOldestEmptyZoneIsEvictedFirst(t *testing.T) {
	dir := t.TempDir()
	writePresence(t, dir, 8000, world.Record{MapID: 1361, MapName: "btl_auch01"})
	writePresence(t, dir, 8001, world.Record{MapID: 6004, MapName: "office_coim"})
	m := &manager{
		cfg:  config{zonesDir: dir, maxZones: 1},
		reg:  &world.Registry{Dir: dir},
		warm: map[int]bool{},
		live: map[int]*managed{
			6004: {mapID: 6004, port: 8001, start: time.Now().Add(-time.Minute)},
			1361: {mapID: 1361, port: 8000, start: time.Now().Add(-time.Hour)},
		},
		zeroSince: map[int]time.Time{},
	}
	if got := m.evictable(); got != 1361 {
		t.Fatalf("evictable() = %d; the zone that has been up longest goes first", got)
	}
}
