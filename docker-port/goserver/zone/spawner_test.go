package zone

// THE MAP SPAWNER'S TESTS (zone/spawner.go).
//
// What is pinned here, and nothing else:
//
//  1. A fixture `mongenflag` of three rows plus a two-row `mongentype` produces
//     THREE bodies, one per point, of the CLASS and Lv the type rows name, each
//     carrying its own type row's RespawnTime - the card's pass condition, in one
//     place. The flag rows do NOT name a class (their ClassID is a row number), so
//     the class assertion is what proves the type rows are the source.
//  2. A body a spawner placed comes back after ITS spawner's delay (5 s for the
//     normal row), and the corpse line says which clock it used.
//  3. A map with no spawner data places nothing and says so ONCE; no interval is
//     named, because there is no fallback any more (zone/respawn.go) - the tests
//     that pin "no row means no return" live in respawn_no_fallback_test.go.
//  4. Map 1331's own shipped data: 54 flag rows, NO type table. That is the fact
//     behind the "no spawner type data" line on the owner's test field, and it is
//     pinned so a later client patch that adds the type table is noticed instead of
//     silently changing what the field spawns.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSpawnerXML writes one fixture table into dir.
func writeSpawnerXML(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// spawnerFixture is a three-row flag set and two spawner types: 1 body of class
// 11600 (normal, 5 s) and 2 of class 10594 (elite, 900 s), so three points hold
// three bodies and both classes and both delays are on the field at once.
func spawnerFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeSpawnerXML(t, dir, "mongenflag_testmap.xml", `<?xml version="1.0" encoding="UTF-8"?>
<idspace id="MonGenFlag_testmap">
	<Class ClassID="1" Division="0" PosX="100" PosY="200" PosZ="5" />
	<Class ClassID="2" Division="0" PosX="300" PosY="400" PosZ="6" />
	<Class ClassID="3" Division="0" PosX="500" PosY="600" PosZ="7" />
</idspace>`)
	writeSpawnerXML(t, dir, "mongentype_testmap.xml", `<?xml version="1.0" encoding="UTF-8"?>
<idspace id="MonGenType_testmap">
	<Class ClassID="1" DirectRespawn="TRUE" Division="0" Lv="12" MaxPop="2" MinPop="1" MonsterID="11600" MonsterName="Monster_Hersu01" Range="500" RespawnTime="5000" Tactics="None" />
	<Class ClassID="2" DirectRespawn="TRUE" Division="0" Lv="65" MaxPop="2" MinPop="2" MonsterID="10594" MonsterName="Monster_AmokTortoise" Range="500" RespawnTime="900000" Tactics="None" />
</idspace>`)
	return dir
}

// flatMesh makes the fake world's mesh answer every request with the point it was
// given and its z as the ground height. That is what lets a position assertion name
// a FLAG row rather than the stub's one fixed cell (newFakeWorld returns the same
// `resolved` cell for every call, which would hide a wrong point).
func flatMesh(w *fakeWorld) {
	w.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return pt, pt[2], true
	}
}

// TestSpawnerPlacesItsTypesQuotaOnTheDivisionsFlags is the card's first pass
// condition: the tables' own bodies, at the tables' own points, of the types'
// classes, each with the type's RespawnTime.
func TestSpawnerPlacesItsTypesQuotaOnTheDivisionsFlags(t *testing.T) {
	w := newFakeWorld(t)
	w.Monsters = newMonsters(map[uint32]MonsterRow{
		11600: {ID: 11600, Name: "Monster_Hersu01", Level: 130, Tactics: "MON_MISSION_84"},
		10594: {ID: 10594, Name: "Monster_AmokTortoise", Level: 70, Tactics: "MON_AGGRESSIVE"},
	})
	flatMesh(w)

	table := LoadSpawner(spawnerFixture(t), "testmap")
	if len(table.Flags) != 3 || len(table.Types) != 2 {
		t.Fatalf("fixture read: %d flag row(s), %d type row(s), want 3 and 2 (%v / %v)",
			len(table.Flags), len(table.Types), table.FlagErr, table.TypeErr)
	}

	run := w.StartSpawner(table)
	if run.Placed != 3 {
		t.Fatalf("placed %d body(ies), want 3 (refused %d, no-point types %d):\n%s",
			run.Placed, run.Refused, run.NoPointTypes, w.logText())
	}
	if got := len(w.runtimeMonsters()); got != 3 {
		t.Fatalf("roster holds %d body(ies), want 3", got)
	}

	// THE ORDER: type 1 takes the first point, type 2 the next two - the cursor is
	// shared by the division's types in file order, so the classes interleave
	// across the flags instead of one species owning a block.
	wantClass := []uint32{11600, 10594, 10594}
	wantX := []int32{100, 300, 500}
	wantY := []int32{200, 400, 600}
	wantDelay := []time.Duration{5 * time.Second, 900 * time.Second, 900 * time.Second}
	for i, m := range w.runtimeMonsters() {
		if m.ClassID != wantClass[i] {
			t.Errorf("body %d is class %d, want %d - the class comes from the TYPE row, "+
				"never from the flag's ClassID", i, m.ClassID, wantClass[i])
		}
		if m.X != wantX[i] || m.Y != wantY[i] {
			t.Errorf("body %d stands at (%d, %d), want (%d, %d) - its own flag row",
				i, m.X, m.Y, wantX[i], wantY[i])
		}
		if m.SpawnerDelay != wantDelay[i] {
			t.Errorf("body %d carries spawner delay %s, want %s", i, m.SpawnerDelay, wantDelay[i])
		}
	}
	// THE TYPE ROW'S Lv WINS over the monster row's, which is the reference's own
	// `if (0 != m_monsterLv) pIesObj->Set(Lv, m_monsterLv)` (MonsterGen.cpp:293).
	if got := w.runtimeMonsters()[0].Level; got != 12 {
		t.Errorf("body 0 is Lv %d, want the type row's 12 (the monster row says 130)", got)
	}

	if log := w.logText(); !strings.Contains(log,
		"zone: spawner: testmap 3 flag row(s), 2 type row(s), 3 monsters placed") {
		t.Errorf("the start-up summary line is missing or not the card's shape:\n%s", log)
	}
}

// TestSpawnerPlacesOneBodyForAMinPopZeroQuestProp pins the quota floor: a type with
// MinPop 0 and MaxPop 1 (a quest prop, e.g. fld_jaq's Trigger_Largestone) still
// appears once - the reference's own first 500 ms tick makes that body.
func TestSpawnerPlacesOneBodyForAMinPopZeroQuestProp(t *testing.T) {
	w := newFakeWorld(t)
	w.Monsters = newMonsters(map[uint32]MonsterRow{
		60120: {ID: 60120, Name: "Trigger_Largestone", Level: 85},
	})
	flatMesh(w)

	dir := t.TempDir()
	writeSpawnerXML(t, dir, "mongenflag_propmap.xml", `<?xml version="1.0" encoding="UTF-8"?>
<idspace id="MonGenFlag_propmap">
	<Class ClassID="1" Division="0" PosX="10" PosY="20" PosZ="3" />
</idspace>`)
	writeSpawnerXML(t, dir, "mongentype_propmap.xml", `<?xml version="1.0" encoding="UTF-8"?>
<idspace id="MonGenType_propmap">
	<Class ClassID="4" Division="0" Lv="85" MaxPop="1" MinPop="0" MonsterID="60120" MonsterName="Trigger_Largestone" Range="500" RespawnTime="60000" Tactics="None" />
</idspace>`)
	run := w.StartSpawner(LoadSpawner(dir, "propmap"))
	if run.Placed != 1 {
		t.Fatalf("a MinPop 0 / MaxPop 1 prop placed %d body(ies), want 1:\n%s",
			run.Placed, w.logText())
	}
	if got := w.runtimeMonsters()[0]; got.ClassID != 60120 || got.SpawnerDelay != 60*time.Second {
		t.Errorf("the prop came out as class %d / delay %s, want 60120 / 60s",
			got.ClassID, got.SpawnerDelay)
	}
}

// TestASpawnerBodyReturnsOnItsOwnRespawnTime is the card's second pass condition:
// the normal row's 5,000 ms - not 23.568 s - and a line that names the spawner.
func TestASpawnerBodyReturnsOnItsOwnRespawnTime(t *testing.T) {
	w := newFakeWorld(t)
	w.Monsters = newMonsters(map[uint32]MonsterRow{
		11600: {ID: 11600, Name: "Monster_Hersu01", Level: 130},
	})
	flatMesh(w)

	dir := t.TempDir()
	writeSpawnerXML(t, dir, "mongenflag_testmap.xml", `<?xml version="1.0" encoding="UTF-8"?>
<idspace id="MonGenFlag_testmap">
	<Class ClassID="1" Division="0" PosX="100" PosY="200" PosZ="5" />
</idspace>`)
	writeSpawnerXML(t, dir, "mongentype_testmap.xml", `<?xml version="1.0" encoding="UTF-8"?>
<idspace id="MonGenType_testmap">
	<Class ClassID="1" Division="0" Lv="0" MaxPop="1" MinPop="1" MonsterID="11600" MonsterName="Monster_Hersu01" Range="500" RespawnTime="5000" Tactics="None" />
</idspace>`)
	if run := w.StartSpawner(LoadSpawner(dir, "testmap")); run.Placed != 1 {
		t.Fatalf("placed %d, want 1:\n%s", run.Placed, w.logText())
	}
	m := w.runtimeMonsters()[0]

	killed := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	w.spawnMu.Lock()
	// THE ATTACK BRANCH, because this body's death is a hit's: the script path's 5 s
	// (CorpseLeaveIdle) would land on the 5 s return and this test could not stand
	// between the leave and the return at all.
	armed := w.armCorpseAt(m, killed, 0, 0, true)
	w.spawnMu.Unlock()
	if !armed {
		t.Fatal("armCorpseAt refused a body that is in the roster")
	}
	c := corpseFor(t, w.Handler, m.Handle)
	if got := c.backAt.Sub(killed); got != 5*time.Second {
		t.Fatalf("the body is due back in %s, want the spawner row's 5s", got)
	}
	if got := c.leaveAt.Sub(killed); got != CorpseLeaveAttack {
		t.Fatalf("the corpse leaves %s after the kill, want the %s attack delay - the "+
			"fixture must leave before it returns", got, CorpseLeaveAttack)
	}

	w.corpseTick(c.leaveAt) // the corpse leaves first; the body is still due later
	if got := len(w.runtimeMonsters()); got != 0 {
		t.Fatalf("%d body(ies) in the roster after the leave, want 0", got)
	}
	w.corpseTick(killed.Add(5*time.Second - time.Millisecond))
	if got := len(w.runtimeMonsters()); got != 0 {
		t.Fatalf("a body came back 1 ms BEFORE its spawner delay: %d in the roster", got)
	}
	w.corpseTick(killed.Add(5 * time.Second))
	back := w.runtimeMonsters()
	if len(back) != 1 {
		t.Fatalf("%d body(ies) came back at the spawner delay, want 1:\n%s",
			len(back), w.logText())
	}
	if back[0].ClassID != 11600 || back[0].SpawnerDelay != 5*time.Second {
		t.Errorf("the body that came back is class %d with delay %s, want class 11600 "+
			"with its spawner's 5s", back[0].ClassID, back[0].SpawnerDelay)
	}

	log := w.logText()
	if !strings.Contains(log, "SPAWNER row") {
		t.Errorf("the corpse line does not name the spawner row as the delay's source:\n%s", log)
	}
	if strings.Contains(log, "FALLBACK") {
		t.Errorf("a body WITH a spawner row was described as using the fallback:\n%s", log)
	}
}

// TestAMapWithNoSpawnerDataPlacesNothingAndIsGivenNoClock is the card's third condition
// under the owner's no-fallback ruling: no files, no bodies, ONE line saying the map has
// no type data, and NO interval named or answered for a body the spawner did not make -
// 23.568 s is deleted (zone/respawn.go).
func TestAMapWithNoSpawnerDataPlacesNothingAndIsGivenNoClock(t *testing.T) {
	w := newFakeWorld(t)
	table := LoadSpawner(t.TempDir(), "nowhere")

	run := w.StartSpawner(table)
	if run.Placed != 0 || len(w.runtimeMonsters()) != 0 {
		t.Fatalf("a map with no spawner data placed %d body(ies), want 0", run.Placed)
	}
	log := w.logText()
	if !strings.Contains(log, "zone: spawner: nowhere 0 flag row(s), 0 type row(s), 0 monsters placed") {
		t.Errorf("the start-up summary line is missing or not the card's shape:\n%s", log)
	}
	if got := strings.Count(log, "has NO spawner type data"); got != 1 {
		t.Errorf("the missing type table is named %d time(s), want exactly 1:\n%s", got, log)
	}
	if strings.Contains(log, "FALLBACK") || strings.Contains(log, "23.568") {
		t.Errorf("the start-up line still names a fallback clock:\n%s", log)
	}

	// NO NUMBER FOR A BODY THE SPAWNER DID NOT MAKE: a probe, a script's spawn, the
	// map's missing data - no row means no return (0, false), never a default.
	if got, ok := respawnDelayFor(Monster{}); ok || got != 0 {
		t.Errorf("a body with no spawner row answered (%s, %v), want (0, false) - it does "+
			"not come back", got, ok)
	}
}

// TestATypeWhoseDivisionHasNoFlagPlacesNothing keeps the join honest: a type row
// whose Division no flag row carries is STATED, not silently dropped.
func TestATypeWhoseDivisionHasNoFlagPlacesNothing(t *testing.T) {
	w := newFakeWorld(t)
	w.Monsters = newMonsters(map[uint32]MonsterRow{
		11600: {ID: 11600, Name: "Monster_Hersu01", Level: 130},
	})
	flatMesh(w)

	dir := t.TempDir()
	writeSpawnerXML(t, dir, "mongenflag_divmap.xml", `<?xml version="1.0" encoding="UTF-8"?>
<idspace id="MonGenFlag_divmap">
	<Class ClassID="1" Division="0" PosX="10" PosY="20" PosZ="3" />
</idspace>`)
	writeSpawnerXML(t, dir, "mongentype_divmap.xml", `<?xml version="1.0" encoding="UTF-8"?>
<idspace id="MonGenType_divmap">
	<Class ClassID="9" Division="7" Lv="12" MaxPop="1" MinPop="1" MonsterID="11600" MonsterName="Monster_Hersu01" Range="500" RespawnTime="5000" Tactics="None" />
</idspace>`)
	run := w.StartSpawner(LoadSpawner(dir, "divmap"))
	if run.Placed != 0 || run.NoPointTypes != 1 {
		t.Fatalf("placed %d, no-point types %d, want 0 and 1:\n%s",
			run.Placed, run.NoPointTypes, w.logText())
	}
	if !strings.Contains(w.logText(), "Division no flag row has") {
		t.Errorf("the type with no points was not stated in the log:\n%s", w.logText())
	}
}

// TestRespawnFlagStillWinsOverTheSpawnerRow keeps the DEV flag's meaning: it exists to
// shorten a probe, so it must override a spawner's own row too (and a body with no row,
// which respawn_no_fallback_test.go covers). Precedence is spelled out in
// respawnDelayFor.
func TestRespawnFlagStillWinsOverTheSpawnerRow(t *testing.T) {
	t.Cleanup(func() { SetRespawnDelay(0) })
	SetRespawnDelay(1500 * time.Millisecond)

	if got, ok := respawnDelayFor(Monster{HasSpawner: true, SpawnerDelay: 5 * time.Second}); !ok ||
		got != 1500*time.Millisecond {
		t.Errorf("with -respawn 1500ms a 5s spawner row is armed at (%s, %v) - the DEV "+
			"flag must win", got, ok)
	}
	SetRespawnDelay(0)
	if got, ok := respawnDelayFor(Monster{HasSpawner: true, SpawnerDelay: 5 * time.Second}); !ok ||
		got != 5*time.Second {
		t.Errorf("with the flag off the spawner row is armed at (%s, %v), want (5s, true)",
			got, ok)
	}
}

// TestMap1331ShipsFlagPointsButNoSpawnerTypes pins the owner's test field's own data.
//
// It is a DATA test, so it reads the client's folder and skips when the client is not
// installed (the same rule the shape test follows). 54 flag rows and no type file is
// not a bug in this server: the client's own `ge\ies10000.ipf` has no
// `mongentype_fld_auch`, so no row names a monster for those points. When a future
// client patch adds the table, this test fails on purpose - the field would then
// start spawning and the change must be re-read, not discovered in game.
func TestMap1331ShipsFlagPointsButNoSpawnerTypes(t *testing.T) {
	dir := XMLDir()
	if _, err := os.Stat(filepath.Join(dir, "mongenflag_fld_auch.xml")); err != nil {
		t.Skipf("client XML not installed at %s: %v", dir, err)
	}
	table := LoadSpawner(dir, "fld_auch")
	if len(table.Flags) != 54 {
		t.Errorf("mongenflag_fld_auch has %d readable flag row(s), want the shipped 54",
			len(table.Flags))
	}
	if len(table.Types) != 0 {
		t.Errorf("mongentype_fld_auch now has %d row(s) - the type table exists after all, "+
			"so the fallback is no longer the whole story; re-read the map's spawner",
			len(table.Types))
	}
	if table.TypeErr == nil {
		t.Error("the missing type table was not recorded as a file error, so the start-up " +
			"line cannot say WHY the map placed nothing")
	}
}
