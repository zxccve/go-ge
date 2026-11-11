package zone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tinyPoints writes a spawn table and loads it. The shape is hand-written so the test
// says what it means: map 10 lands, map 51 has a position but NO cell, and map 999 does
// not exist. Nothing here reads the real 468 KB file.
func tinyPoints(t *testing.T) *ZonePoints {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "zone_points.json")
	body := `{"generated_by":"test","version":1,"built":"2026-10-27",
	  "counts":{"maps":2,"with_spawn":2,"with_town":1,"gates":0},
	  "by_name":{"reb_basic":10},
	  "maps":{
	    "10":{"name":"reb_basic","mesh":"reb_basic","town":1,
	          "spawn":{"x":4110,"y":-130,"cell":146,"source":"townpoint:1"},
	          "points":[{"n":1,"x":4110,"y":-130,"cell":146,"name":"<$>10062500</>"}],
	          "restart":{"warp_id":203,"x":3946,"y":193,"cell":279},
	          "gates":[{"name":"REB-BASIC_OFFICE_REB","to":"office_reb","to_id":6005,
	                    "x":-2901,"y":14643,"cell":46}]},
	    "51":{"name":"dun_arm01_movie01","mesh":"dun_arm01_movie01","town":0,
	          "spawn":{"x":-26,"y":-4461,"cell":null,"source":"map:waypoint"},
	          "points":[],"restart":null,"gates":[]}},
	  "note":"test"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	points, err := LoadZonePoints(path)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	return points
}

// TestSpawnForReturnsTheMapsOwnPoint is the whole point: map 10's x, y and CELL come
// back as the file stores them, with the source, so map 15 can be served the same way.
func TestSpawnForReturnsTheMapsOwnPoint(t *testing.T) {
	points := tinyPoints(t)

	landing, err := points.SpawnFor(10)
	if err != nil {
		t.Fatalf("SpawnFor(10): %v", err)
	}
	if landing.Pos != [3]int32{4110, -130, 146} {
		t.Fatalf("map 10 spawn = %v, want [4110 -130 146]", landing.Pos)
	}
	if landing.Map != "reb_basic" || landing.Mesh != "reb_basic" {
		t.Fatalf("map 10 name/mesh = %q/%q, want reb_basic/reb_basic",
			landing.Map, landing.Mesh)
	}
	if landing.Source != "townpoint:1" {
		t.Fatalf("map 10 source = %q, want townpoint:1", landing.Source)
	}

	// The rows the zone does not read yet must at least survive the load, or the next
	// piece of work would find them silently dropped.
	entry := points.Maps["10"]
	if len(entry.Points) != 1 || entry.Points[0].N != 1 {
		t.Fatalf("map 10 points = %v, want one point numbered 1", entry.Points)
	}
	if entry.Restart == nil || entry.Restart.WarpID != 203 {
		t.Fatalf("map 10 restart = %v, want warp 203", entry.Restart)
	}
	if len(entry.Gates) != 1 || entry.Gates[0].ToID != 6005 {
		t.Fatalf("map 10 gates = %v, want one gate to 6005", entry.Gates)
	}
}

// TestSpawnForRefusesANullCell: a null cell is the emitter saying "no ground here", and
// the zone must refuse the map rather than fall back to map 10's numbers or send the
// height. The message has to name the map, because that is what the operator fixes.
func TestSpawnForRefusesANullCell(t *testing.T) {
	points := tinyPoints(t)

	landing, err := points.SpawnFor(51)
	if err == nil {
		t.Fatalf("SpawnFor(51) accepted a map with no landing cell: %v", landing)
	}
	if !strings.Contains(err.Error(), "51") {
		t.Fatalf("the error does not name the map: %v", err)
	}
	if !strings.Contains(err.Error(), "NO resolved landing cell") {
		t.Fatalf("the error does not say why the map cannot be served: %v", err)
	}
}

// TestSpawnForUnknownMap: an id the file does not carry is a startup failure that says
// so, never a quiet map 10 spawn.
func TestSpawnForUnknownMap(t *testing.T) {
	points := tinyPoints(t)

	if landing, err := points.SpawnFor(999); err == nil {
		t.Fatalf("SpawnFor(999) invented a spawn: %v", landing)
	} else if !strings.Contains(err.Error(), "999") {
		t.Fatalf("the error does not name the map: %v", err)
	}
}

// TestSpawnForRefusesAHeight pins the guard the crash taught: a cell that is really a
// height is rejected with the same wording ParseSpawn uses.
func TestSpawnForRefusesAHeight(t *testing.T) {
	var doc ZonePoints
	doc.Version = ZonePointsVersion
	doc.Maps = map[string]MapEntry{
		"10": {Name: "reb_basic", Spawn: &MapPoint{X: 1, Y: 2, Cell: int32p(-1088)}},
	}
	if _, err := doc.SpawnFor(10); err == nil {
		t.Fatal("SpawnFor accepted a height as a cell")
	} else if !strings.Contains(err.Error(), "HEIGHT") {
		t.Fatalf("the error does not use the shared wording: %v", err)
	}
}

// TestPointsNameIsTheMapClassName: the barrack asks this table for a map's CLASS
// name, which the client feeds straight into SwitchMode. A map the file does not
// carry must say so - the barrack then keeps its own fallback, because an invented
// name is a wrong world.
func TestPointsNameIsTheMapClassName(t *testing.T) {
	points := tinyPoints(t)

	if name, ok := points.Name(10); !ok || name != "reb_basic" {
		t.Fatalf("Name(10) = %q %v, want reb_basic", name, ok)
	}
	if name, ok := points.Name(999); ok {
		t.Fatalf("Name(999) = %q, want unknown", name)
	}
	var none *ZonePoints
	if name, ok := none.Name(15); ok {
		t.Fatalf("a nil table answered %q", name)
	}
}

// formationPoints writes a version-2 spawn table: one map whose spawn and town point
// carry a formation, with the LAST follower slot unresolved (a null cell) - the shape
// the emitter produces beside a leader that itself sits on the edge.
func formationPoints(t *testing.T) *ZonePoints {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "zone_points.json")
	body := `{"generated_by":"test","version":2,"built":"2026-10-27",
	  "counts":{"maps":1,"with_spawn":1,"with_town":1,"gates":0},
	  "by_name":{"reb_basic":10},
	  "maps":{
	    "10":{"name":"reb_basic","mesh":"reb_basic","town":1,
	          "spawn":{"x":4110,"y":-130,"cell":146,"source":"townpoint:1",
	                   "formation":[{"x":4110,"y":-130,"cell":146},
	                                {"x":4220,"y":-70,"cell":146},
	                                {"x":4000,"y":-70,"cell":null}]},
	          "points":[{"n":1,"x":4110,"y":-130,"cell":146,"name":"<$>1</>",
	                     "formation":[{"x":4110,"y":-130,"cell":146},
	                                  {"x":4220,"y":-70,"cell":146},
	                                  {"x":4000,"y":-70,"cell":null}]}],
	          "restart":null,"gates":[]}},
	  "note":"test"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	points, err := LoadZonePoints(path)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	return points
}

// TestFormationSlots pins the arrival formation: slot 0 is the leader on the point,
// slot 1 the first follower at its own offset and cell, and a slot with NO resolved
// cell falls back to the leader's point instead of being sent.
func TestFormationSlots(t *testing.T) {
	points := formationPoints(t)

	landing, err := points.SpawnFor(10)
	if err != nil {
		t.Fatalf("SpawnFor(10): %v", err)
	}
	leader := [3]int32{4110, -130, 146}
	for _, tc := range []struct {
		slot int
		want [3]int32
		why  string
	}{
		{0, leader, "the leader IS the point"},
		{1, [3]int32{4220, -70, 146}, "the first follower keeps its own cell"},
		{2, leader, "a null cell falls back to the leader"},
		{3, leader, "past the slots, everybody stands on the point"},
		{9, leader, "far past the slots, same"},
	} {
		if got := landing.Slot(tc.slot); got != tc.want {
			t.Fatalf("spawn slot %d = %v, want %v (%s)", tc.slot, got, tc.want, tc.why)
		}
	}

	town := points.TownPoints(10)
	if len(town) != 1 {
		t.Fatalf("map 10 carries %d town point(s), want 1", len(town))
	}
	if got := town[0].Slot(1); got != [3]int32{4220, -70, 146} {
		t.Fatalf("town point slot 1 = %v, want the follower's offset", got)
	}
	if got := town[0].Slot(0); got != leader {
		t.Fatalf("town point slot 0 = %v, want the point itself", got)
	}
}

// TestVersionOneFileHasNoFormation: an older file must load and behave exactly as it
// did - nobody on a slot, everybody on the point. Refusing it would turn "the emitter
// has not been re-run" into "the zone will not start".
func TestVersionOneFileHasNoFormation(t *testing.T) {
	points := tinyPoints(t) // version 1, no formation anywhere

	landing, err := points.SpawnFor(10)
	if err != nil {
		t.Fatalf("SpawnFor(10): %v", err)
	}
	if len(landing.Formation) != 0 {
		t.Fatalf("a version-1 file produced %d formation slot(s)", len(landing.Formation))
	}
	for _, slot := range []int{0, 1, 2, 5} {
		if got := landing.Slot(slot); got != [3]int32{4110, -130, 146} {
			t.Fatalf("slot %d of a version-1 file = %v, want the point", slot, got)
		}
	}
	if got := points.TownPoints(10)[0].Slot(2); got != [3]int32{4110, -130, 146} {
		t.Fatalf("town slot 2 of a version-1 file = %v, want the point", got)
	}
}

// TestZonePointsFileMap15 reads the REAL shared table, because the hand-written one
// above cannot catch an emitter change (a renamed key, a cell that moved to text). The
// skip keeps a checkout without the data working, the same as navgraph_test.go.
func TestZonePointsFileMap15(t *testing.T) {
	path := filepath.Join("..", "..", "data", "zone_points.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no spawn table at %s (run: cd src && python emit_zone_points.py)", path)
	}
	points, err := LoadZonePoints(path)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	if len(points.Maps) < 100 {
		t.Fatalf("%s carries %d map(s), that is not the whole table", path, len(points.Maps))
	}

	landing, err := points.SpawnFor(15)
	if err != nil {
		t.Fatalf("SpawnFor(15): %v", err)
	}
	if landing.Pos != [3]int32{2209, 16830, 233} {
		t.Fatalf("map 15 spawn = %v, want [2209 16830 233]", landing.Pos)
	}
	if landing.Map != "coim_basic" {
		t.Fatalf("map 15 name = %q, want coim_basic", landing.Map)
	}
	// The barrack's lookup answers from the same rows, without needing a spawn.
	if name, ok := points.Name(15); !ok || name != "coim_basic" {
		t.Fatalf("Name(15) = %q %v, want coim_basic", name, ok)
	}
	if name, ok := points.Name(999999); ok {
		t.Fatalf("Name(999999) = %q, want unknown", name)
	}
}

// int32p is the pointer the JSON cell needs, so a test can say "a height" without a
// file.
func int32p(v int32) *int32 { return &v }
