package zone

// THE `-quietspawn` FLAG, PINNED BY FOUR TESTS (card t_1d63a658).
//
// The flag is a LOG switch, not a speed one - measured: 858 lines cost 3-8 ms of a
// ~178 ms spawner on map 4063, and the numbers are in lanes\build.md. What these
// tests hold is the OTHER half of the card's pass condition, which is the part a
// flag can get wrong: the information must still be there.
//
//	(1) OFF is byte for byte what the zone wrote before: two lines per body.
//	(2) ON writes NO per-body line for a body whose tactics row RESOLVED, and ONE
//	    line per distinct decision naming class, tactics, AIType, attack, seek, the
//	    body count and the cells - so the mapping survives as a table, not a stream.
//	(3) A body whose tactics row does NOT resolve keeps BOTH lines even ON: that
//	    unresolved decision (`tactics "" AIType -`) is the failure the line was used
//	    to find, and a flag that hid it would be a diagnostic deleted.
//	(4) The silencing is scoped to the STARTUP BURST: a `SpawnMonsterObject` called
//	    after `StartSpawner` returns prints both lines with the flag ON, because one
//	    body is never a volume problem.
//
// FIXTURES ARE HAND-BUILT TABLES, not the shipped XML: the question here is which
// LINES are written, and a fixture that needs the client's data would skip on a
// machine that cannot answer it. The mesh is a stub, and the ring is off
// (`SightOff`), so nothing touches a file.

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// The three shapes of body the flag must tell apart.
const (
	quietAggressiveClass = uint32(2001) // its tactics row RESOLVES, it attacks
	quietPassiveClass    = uint32(2002) // its tactics row RESOLVES, it does not attack
	quietUnknownClass    = uint32(2003) // NO tactics row: the diagnostic the flag must keep
)

// quietMonsters is the Monster table those three ids name, plus the AI rows two of
// them resolve through. `MON_NOT_A_ROW` is deliberately absent: that is how a body
// with no tactics row is spelled.
func quietMonsters() *MonsterTable {
	return &MonsterTable{
		byID: map[uint32]MonsterRow{
			quietAggressiveClass: {
				Name: "Monster_QuietAggressive", Level: 50, Exp: 120,
				Tactics: "MON_AGGRESSIVE",
				AIType:  "AGGRESSIVE", AIKnown: true, AIAttack: true, SeekRange: 2500,
				MSPD: 143,
			},
			quietPassiveClass: {
				Name: "Monster_QuietPassive", Level: 1, Exp: 0,
				Tactics: "MON_MISSION_84",
				AIType:  "None", AIKnown: true, AIAttack: false, SeekRange: 0,
				MSPD: 100,
			},
			quietUnknownClass: {
				Name: "Monster_QuietUnknown", Level: 10, Exp: 5,
				Tactics: "MON_NOT_A_ROW",
				// AIKnown false and every AI field zero: the loader's own answer
				// for a tactics name `datatable_monsterai.xml` does not hold.
				MSPD: 100,
			},
		},
		aiRows: map[string]MonsterAIRow{
			"MON_AGGRESSIVE": {AIType: "AGGRESSIVE", Attack: true, SeekRange: 2500},
			"MON_MISSION_84": {AIType: "None", Attack: false, SeekRange: 0},
		},
	}
}

// quietZone is a zone that can place a body through the REAL SpawnMonsterObject with
// no file, no script and no path engine: a mesh that answers a cell derived from the
// point's own x, so the aggregate's cell count is a number this test can predict.
func quietZone(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	h := &Handler{
		Log:      log.New(logs, "", 0),
		Monsters: quietMonsters(),
		MapID:    4063,
		MapName:  "btl_ktv03",
		SightOff: true, // no ring, no map_table read: this test is about lines
	}
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		if hRange != spawnHRange || vRange != spawnVRange {
			return [3]int32{}, 0, false
		}
		return [3]int32{pt[0], pt[1], int32(29 + pt[0]/100)}, int32(29 + pt[0]/100), true
	}
	return h, logs
}

// quietSpawnerTable is three type rows over three flag rows in ONE division, two
// bodies each: 2 aggressive, 2 passive, 2 with no tactics row.
func quietSpawnerTable() *SpawnerTable {
	types := []SpawnerType{}
	for _, class := range []uint32{quietAggressiveClass, quietPassiveClass, quietUnknownClass} {
		tactics := ""
		switch class {
		case quietAggressiveClass:
			tactics = "MON_AGGRESSIVE"
		case quietPassiveClass:
			tactics = "MON_MISSION_84"
		case quietUnknownClass:
			tactics = "MON_NOT_A_ROW"
		}
		types = append(types, SpawnerType{
			ClassID: 1, Division: 1, MonsterID: class, Level: 1, Tactics: tactics,
			MinPop: 2, MaxPop: 2,
		})
	}
	return &SpawnerTable{
		Map: "btl_ktv03",
		Flags: []SpawnPoint{
			{ClassID: 1, Division: 1, X: 0, Y: 0, Z: 29},
			{ClassID: 1, Division: 1, X: 100, Y: 0, Z: 30},
			{ClassID: 1, Division: 1, X: 200, Y: 0, Z: 31},
		},
		Types: types,
	}
}

// quietLines counts what a run wrote, one counter per line this card is about.
type quietLines struct {
	ai      int // `zone: MONSTER AI class ...`
	create  int // `zone: CREATE_TMONSTER ...`
	aggHead int // the aggregate's own summary line
	aggRow  int // one line per distinct decision
}

func countQuiet(text string) quietLines {
	return quietLines{
		ai:      strings.Count(text, "zone: MONSTER AI class"),
		create:  strings.Count(text, "zone: CREATE_TMONSTER \""),
		aggHead: strings.Count(text, "counted "),
		aggRow:  strings.Count(text, "zone: spawner: -quietspawn class"),
	}
}

// TestQuietSpawnOffWritesBothLinesPerBody is pin 1: OFF is the old behaviour, and it
// is the default - the zero value of the field must be the noisy, unchanged run.
func TestQuietSpawnOffWritesBothLinesPerBody(t *testing.T) {
	h, logs := quietZone(t)
	run := h.StartSpawner(quietSpawnerTable())
	if run.Placed != 6 || run.Refused != 0 {
		t.Fatalf("StartSpawner placed %d and refused %d, want 6/0:\n%s",
			run.Placed, run.Refused, logs.String())
	}
	got := countQuiet(logs.String())
	if got.ai != 6 || got.create != 6 {
		t.Fatalf("%d MONSTER AI and %d CREATE_TMONSTER line(s) for 6 bodies, want 6 and 6 "+
			"- QuietSpawn is false, so this run must be byte for byte the old one:\n%s",
			got.ai, got.create, logs.String())
	}
	if got.aggHead != 0 || got.aggRow != 0 {
		t.Fatalf("a -quietspawn line was written with the flag OFF (%d head, %d row):\n%s",
			got.aggHead, got.aggRow, logs.String())
	}
}

// TestQuietSpawnCountsTheBurstIntoOneLinePerDecision is pin 2: the 4 bodies whose
// tactics row resolved write NOTHING per body, and the mapping they carried is on
// the aggregate - one line per distinct decision, with its count.
func TestQuietSpawnCountsTheBurstIntoOneLinePerDecision(t *testing.T) {
	h, logs := quietZone(t)
	h.QuietSpawn = true
	run := h.StartSpawner(quietSpawnerTable())
	if run.Placed != 6 || run.Refused != 0 {
		t.Fatalf("StartSpawner placed %d and refused %d, want 6/0 - the flag must not "+
			"change what is PLACED:\n%s", run.Placed, run.Refused, logs.String())
	}
	text := logs.String()
	got := countQuiet(text)
	// The two unknown-tactics bodies still print (pin 3); the other four do not.
	if got.ai != 2 || got.create != 2 {
		t.Fatalf("%d MONSTER AI and %d CREATE_TMONSTER line(s), want 2 and 2 - only the "+
			"body whose tactics row does NOT resolve may print:\n%s", got.ai, got.create, text)
	}
	if got.aggHead != 1 {
		t.Fatalf("%d aggregate summary line(s), want 1:\n%s", got.aggHead, text)
	}
	if got.aggRow != 2 {
		t.Fatalf("%d aggregate row(s), want 2 (AGGRESSIVE and None - the unknown body is "+
			"NOT aggregated, it printed):\n%s", got.aggRow, text)
	}
	for _, want := range []string{
		`class 2001 "Monster_QuietAggressive" tactics "MON_AGGRESSIVE" AIType AGGRESSIVE ` +
			`attack yes seek 2500: 2 body(ies)`,
		`class 2002 "Monster_QuietPassive" tactics "MON_MISSION_84" AIType None ` +
			`attack no seek 0: 2 body(ies)`,
		"counted 4 body(ies) into 2 tactics decision(s) and wrote none of their 8 per-body line(s)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the aggregate lost %q - the decision the two per-body lines carried "+
				"must survive the flag:\n%s", want, text)
		}
	}
}

// TestQuietSpawnStillPrintsABodyWithNoTacticsRow is pin 3, the card's own condition:
// the `zone: MONSTER AI class ...` line is how a body with no tactics was found
// before, so a flag that silenced it would have deleted the diagnostic.
func TestQuietSpawnStillPrintsABodyWithNoTacticsRow(t *testing.T) {
	h, logs := quietZone(t)
	h.QuietSpawn = true
	// ONE type row, and it is the class with no tactics row: everything else is out
	// of the way so the two lines can only be this body's.
	table := &SpawnerTable{
		Map:   "btl_ktv03",
		Flags: []SpawnPoint{{ClassID: 1, Division: 1, X: 0, Y: 0, Z: 29}},
		Types: []SpawnerType{{
			ClassID: 1, Division: 1, MonsterID: quietUnknownClass,
			Tactics: "MON_NOT_A_ROW", MinPop: 1, MaxPop: 1,
		}},
	}
	if run := h.StartSpawner(table); run.Placed != 1 {
		t.Fatalf("StartSpawner placed %d, want 1:\n%s", run.Placed, logs.String())
	}
	text := logs.String()
	if !strings.Contains(text, `zone: MONSTER AI class 2003 tactics "MON_NOT_A_ROW" AIType - `) {
		t.Fatalf("the body with NO tactics row lost its MONSTER AI line with -quietspawn "+
			"ON - that line is how an unresolved tactics row is found:\n%s", text)
	}
	if !strings.Contains(text, `zone: CREATE_TMONSTER "Monster_QuietUnknown" (class 2003`) {
		t.Fatalf("the body with NO tactics row lost its CREATE_TMONSTER line:\n%s", text)
	}
	if got := countQuiet(text); got.aggRow != 0 {
		t.Fatalf("%d aggregate row(s) for a body that printed in full, want 0:\n%s",
			got.aggRow, text)
	}
}

// TestQuietSpawnDoesNotSilenceALaterBody is pin 4: the flag is scoped to the burst.
// A script's CreateTmonster (or a probe, or a respawn) after the spawner returns is
// ONE body, and it prints both of its lines even with the flag on.
func TestQuietSpawnDoesNotSilenceALaterBody(t *testing.T) {
	h, logs := quietZone(t)
	h.QuietSpawn = true
	h.StartSpawner(&SpawnerTable{Map: "btl_ktv03"}) // the burst: no rows, nothing placed
	logs.Reset()

	if _, err := h.SpawnMonsterObject(MonsterSpec{
		MonsterID: int32(quietAggressiveClass), X: 0, Y: 0, Z: 29, MHP: 500,
	}); err != nil {
		t.Fatalf("SpawnMonsterObject after the burst: %v", err)
	}
	got := countQuiet(logs.String())
	if got.ai != 1 || got.create != 1 {
		t.Fatalf("a body created AFTER the spawner wrote %d MONSTER AI and %d "+
			"CREATE_TMONSTER line(s) with -quietspawn ON, want 1 and 1 - the burst is the "+
			"only place the volume is, so a single body must never be silenced:\n%s",
			got.ai, got.create, logs.String())
	}
	if got.aggRow != 0 {
		t.Fatalf("a later body was counted into the aggregate instead of printed:\n%s",
			logs.String())
	}
}
