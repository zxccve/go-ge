// THE MAP'S OWN SPAWNER: the tables that place a field's monsters, and the delay
// each one comes back on.
//
// WHAT THIS REPLACES. Until now every body in a field came from a probe on the
// command line (`-spawnmonster`, `-spawndummy`) and every one of them came back on
// ONE interval - the 23.568 s of a single recording (`zone/respawn.go`). A field
// therefore looked like a test dummy: the same body in the same place, and nothing
// the map itself asked for.
//
// THE REFERENCE (source2, re-read for this card rather than taken on trust):
//
//   - `CWorld::InitMonsterGenType` (World.cpp:951-1010) reads `mongentype_<world>`
//     and builds one generator per row from `ClassID`, `MonsterID`, `Lv`,
//     `Tactics`, `RespawnTime`, `MaxPop`, `MinPop`, `Range` and `Division`;
//   - `CWorld::InitMonsterGenFlag` (World.cpp:1115-1180) reads `mongenflag_<world>`
//     and files each `{ClassID, Division, PosX, PosY, PosZ}` point under its
//     `Division`;
//   - `CMonsterGen::CreateMonster` (MonsterGen.cpp:262-320) picks a flag of the
//     generator's own Division and creates the type row's `MonsterID` at that
//     point - the monster CLASS comes from the TYPE row, never from the flag row;
//   - the return interval is the type row's `RespawnTime` in MILLISECONDS
//     (`CMonsterGen::IsAllowCreateMonster`, MonsterGen.cpp:158-215), not a
//     property of the species and not one zone-wide constant.
//
// THE FLAG ROW'S `ClassID` IS A ROW NUMBER, NOT A MONSTER CLASS - and that is the
// one thing this card's briefing had wrong, so it is proved here rather than
// argued: `mongenflag_fld_jaq` has 29 flag rows numbered 1..29 and
// `mongentype_fld_jaq` has 8 spawner rows numbered 1..8, and no Monster table row
// carries 1..29 at all (the shipped Monster ids start at 10, and that row is
// GM_GameQueen). So a flag cannot name its own class; the TYPE rows name it, and
// they reach the points through `Division`. In the shipped data that is 29 flags
// and 8 types on fld_jaq, 123 and 15 on btl_auch01, 408 and 13 on fld_alt01.
//
// WHAT THIS SLICE PLACES, then: for every TYPE row, `MinPop` bodies of that row's
// `MonsterID` (and if `MaxPop` is above zero but `MinPop` is zero, ONE - the
// reference creates that body within its first 500 ms tick, so a quest prop with
// MinPop 0 must not be missing from a fresh field). Each body goes to a flag of the
// type's own Division, and the walk over the flag list is SHARED by the types of
// that division in file order, so the classes interleave instead of the first
// species owning the first N points. `CMonsterGen` picks its point at RANDOM
// (MonsterGen.cpp:275-282); a fixed walk is the same picture with two differences
// that are worth it here: the owner can walk to a point a log named, and a test can
// assert which class stands where. `MaxPop` is read and kept for the refill that
// would fill a field to its ceiling; it does not place anything yet.
//
// THE DELAY IS THE TYPE ROW'S OWN `RespawnTime`, carried on the body from the spawn
// through its corpse (Monster.SpawnerDelay, zone/respawn.go). The shipped rows show
// the shape: 5,000 ms is the most common value, and 900,000 and 1,800,000 are
// present too (1,082 / 85 / 93 rows) - the normal/elite/boss trio the other
// project's v32 note names is in our tables, and it is a ROW, so the code reads the
// row and never a species.
//
// A MAP WITH NO TYPE TABLE PLACES NOTHING AND SAYS SO ONCE. Map 1331 (fld_auch,
// Thueringen Lakeside - the owner's test field) ships 54 flag rows and NO
// `mongentype_fld_auch`: the type table is absent from the client's own
// `ge\ies10000.ipf` as well as from `ge\xml`, so nothing names a monster for those
// 54 points. That is not a decoding failure and it is not patched over with a class
// from another map, and NO CLOCK is invented for it either: the map places nothing,
// the start-up line says how many rows each side had, and a body the zone makes
// another way (a probe) has no spawner row and does not come back (zone/respawn.go).
// A map that DOES carry both tables (most fields and battle maps do) is populated
// from them.
package zone

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The two file names the reference builds with `GetWorldName()`
// (World.cpp:955 and :1119), point for point - both are read from the same `-xml`
// folder as every other datatable.
const (
	spawnerFlagPrefix = "mongenflag_"
	spawnerTypePrefix = "mongentype_"
)

// SpawnPoint is one `mongenflag_<map>` row: a place a monster may stand. ClassID is
// the row's own number, kept only so a diagnostic can name the row; it is NOT a
// monster class (see the header).
type SpawnPoint struct {
	ClassID  uint32
	Division int
	X, Y, Z  int32
}

// SpawnerType is one `mongentype_<map>` row: which monster a spawner makes, at what
// level, with what tactics, and how long after a death it comes back.
//
// RespawnTime is milliseconds straight from the row. Zero is kept as zero and is a
// REAL answer - the row says the body returns at once - never a reading of "no row":
// StartSpawner passes `HasSpawner` with it, and that flag is what says the row exists.
type SpawnerType struct {
	ClassID     uint32
	Division    int
	MonsterID   uint32
	MonsterName string
	Level       int
	Tactics     string
	RespawnTime time.Duration
	MinPop      int
	MaxPop      int
	Range       float64
}

// SpawnerTable is a map's spawner data: the points and the types, both in FILE
// ORDER. They are not joined row to row (a flag's ClassID is not a type's); they
// meet through `Division`, exactly as the reference's two tables do.
type SpawnerTable struct {
	Map string

	Flags []SpawnPoint
	Types []SpawnerType

	// FlagErr / TypeErr are the file-level problems (a missing file, an XML that
	// cannot be read). Neither is fatal to the zone: a map without spawner data
	// still serves players, and a body the zone makes another way has no spawner row
	// and does not come back - no interval is invented for it.
	FlagErr error
	TypeErr error
}

// LoadSpawner reads `mongenflag_<map>.xml` and `mongentype_<map>.xml` from dir.
//
// It never returns nil for a named map: the counts and the per-file errors are the
// answer, and the caller states them. `mapName` empty (a run that could not name its
// map) answers an empty table with a sentence, not a panic.
func LoadSpawner(dir, mapName string) *SpawnerTable {
	t := &SpawnerTable{Map: mapName}
	if strings.TrimSpace(mapName) == "" {
		t.FlagErr = fmt.Errorf("this run has no map class name, so there is no spawner table to name")
		t.TypeErr = t.FlagErr
		return t
	}

	flagPath := filepath.Join(dir, spawnerFlagPrefix+mapName+".xml")
	n, err := eachClass(flagPath, func(attrs map[string]string) {
		id, ok := atou32(attrs["ClassID"])
		if !ok {
			return
		}
		x, xok := atoi(attrs["PosX"])
		y, yok := atoi(attrs["PosY"])
		z, zok := atoi(attrs["PosZ"])
		if !xok || !yok || !zok {
			return
		}
		division, _ := atoi(attrs["Division"])
		t.Flags = append(t.Flags, SpawnPoint{
			ClassID: id, Division: division,
			X: int32(x), Y: int32(y), Z: int32(z),
		})
	})
	if err != nil {
		t.FlagErr = err
	} else if n != len(t.Flags) {
		t.FlagErr = fmt.Errorf("%s has %d row(s) but %d carried a ClassID and a "+
			"position - the rest are skipped", flagPath, n, len(t.Flags))
	}

	typePath := filepath.Join(dir, spawnerTypePrefix+mapName+".xml")
	_, err = eachClass(typePath, func(attrs map[string]string) {
		id, ok := atou32(attrs["ClassID"])
		if !ok {
			return
		}
		monster, mok := atou32(attrs["MonsterID"])
		if !mok {
			return
		}
		level, _ := atoi(attrs["Lv"])
		division, _ := atoi(attrs["Division"])
		minPop, _ := atoi(attrs["MinPop"])
		maxPop, _ := atoi(attrs["MaxPop"])
		respawn, _ := atoi(attrs["RespawnTime"])
		t.Types = append(t.Types, SpawnerType{
			ClassID: id, Division: division, MonsterID: monster,
			MonsterName: attrs["MonsterName"], Level: level,
			Tactics: attrs["Tactics"], MinPop: minPop, MaxPop: maxPop,
			Range:       atof(attrs["Range"]),
			RespawnTime: time.Duration(respawn) * time.Millisecond,
		})
	})
	if err != nil {
		t.TypeErr = err
	}
	return t
}

// SpawnerRun is what StartSpawner did, for the log and for a test.
type SpawnerRun struct {
	Flags        int // flag rows read
	Types        int // type rows read
	Placed       int // bodies created
	NoPointTypes int // type rows whose Division has no flag row
	Refused      int // bodies the creation path refused (and said why)
}

// spawnerQuota is how many bodies one type row places at boot: its `MinPop`, and
// ONE where `MaxPop` is above zero but `MinPop` is not (the reference's own first
// 500 ms tick would make that body; a quest prop must not be missing from a fresh
// field). A type with `MaxPop` 0 is disabled and places nothing.
func spawnerQuota(t SpawnerType) int {
	if t.MinPop > 0 {
		return t.MinPop
	}
	if t.MaxPop > 0 {
		return 1
	}
	return 0
}

// StartSpawner places each type row's quota on the flags of its Division and STATES
// THE RESULT ONCE.
//
// Every body goes through `Handler.SpawnMonsterObject` - the same creation path a
// probe and a script's `CreateTmonster` use - so a spawned body is the same kind of
// object as any other: same handle block, same HP ledger, same 3527, same corpse
// and respawn chain (which is what carries its `RespawnTime` back, see
// zone/respawn.go). Nothing here builds a Monster by hand.
//
// THE SUMMARY LINE IS THE CARD'S, VERBATIM IN SHAPE:
//
//	zone: spawner: <map> <n> flag row(s), <m> type row(s), <k> monsters placed
//
// A map with no TYPE rows gets one more line saying WHY, once, and no interval is named:
// with no row there is no clock, and the number that used to stand in for one is deleted
// (zone/respawn.go).
func (h *Handler) StartSpawner(t *SpawnerTable) SpawnerRun {
	if t == nil {
		t = &SpawnerTable{}
	}
	run := SpawnerRun{Flags: len(t.Flags), Types: len(t.Types)}

	// THE `-quietspawn` SCOPE (zone\spawn_quiet.go): the flag counts bodies only while
	// this burst runs, so a script's `CreateTmonster` at any other moment keeps both of
	// its lines. Set before the loop and cleared by the defer, whatever the loop does.
	h.spawnerRunning.Store(true)
	defer func() {
		h.spawnerRunning.Store(false)
		h.flushQuietSpawn()
	}()

	// THE POINTS, BY DIVISION - the join the reference uses (InitMonsterGenFlag
	// files each flag under its Division; CreateMonster asks for that division).
	byDivision := map[int][]SpawnPoint{}
	for _, f := range t.Flags {
		byDivision[f.Division] = append(byDivision[f.Division], f)
	}
	// ONE CURSOR PER DIVISION, shared by its types in file order, so the classes
	// interleave across the points instead of the first species owning a block of
	// them. It wraps when the quotas need more bodies than the division has points
	// (fld_jaq: 38 bodies over 29 points - the map's own FlagCount allows it).
	cursor := map[int]int{}

	var refused []string
	for _, typ := range t.Types {
		points := byDivision[typ.Division]
		if len(points) == 0 {
			run.NoPointTypes++
			continue
		}
		for k := 0; k < spawnerQuota(typ); k++ {
			point := points[cursor[typ.Division]%len(points)]
			cursor[typ.Division]++
			spec := MonsterSpec{
				MonsterID: int32(typ.MonsterID),
				X:         float64(point.X),
				Y:         float64(point.Y),
				Z:         float64(point.Z),
				Level:     float64(typ.Level),
				Tactics:   spawnerTactics(typ.Tactics),
				// THE DELAY TRAVELS WITH THE BODY, AND SO DOES THE FACT THAT A ROW
				// EXISTS: the corpse chain reads both at the kill (zone/respawn.go
				// respawnDelayFor), so a body this spawner made returns on ITS
				// spawner's number - a stated 0 included - and a body it did not make
				// is not given a number at all.
				Respawn:        typ.RespawnTime,
				HasSpawner:     true,
				SpawnerClassID: typ.ClassID,
			}
			if _, err := h.SpawnMonsterObject(spec); err != nil {
				run.Refused++
				if len(refused) < 3 {
					refused = append(refused, err.Error())
				}
				continue
			}
			run.Placed++
		}
	}

	meshN, meshD := MeshCost()
	hpN, hpD, defN, defD := ScriptCost()
	h.Log.Printf("zone: spawner: %s %d flag row(s), %d type row(s), %d monsters placed "+
		"(%d mesh quer(ies) %d ms; %d HP script call(s) %d ms; %d DEF script call(s) %d ms)",
		spawnerMapLabel(t.Map), run.Flags, run.Types, run.Placed,
		meshN, meshD.Milliseconds(), hpN, hpD.Milliseconds(), defN, defD.Milliseconds())
	if run.NoPointTypes > 0 {
		h.Log.Printf("zone: spawner: %d type row(s) name a Division no flag row has - "+
			"those spawners place nothing; the points and the types meet through "+
			"Division, never row to row", run.NoPointTypes)
	}
	if run.Refused > 0 {
		h.Log.Printf("zone: spawner: %d body(ies) REFUSED by the creation path - first: %s",
			run.Refused, strings.Join(refused, " | "))
	}
	if run.Types == 0 {
		h.Log.Printf("zone: spawner: %s has NO spawner type data (%s), so it places "+
			"nothing and no body it did not make is given a respawn clock",
			spawnerMapLabel(t.Map), spawnerWhyNoTypes(t))
	}
	return run
}

// spawnerMapLabel names the map in a log line. A run with no map name says so
// instead of printing an empty string.
func spawnerMapLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(no map name)"
	}
	return name
}

// spawnerWhyNoTypes explains an empty type list: a missing file is a different
// fact from a file with no rows, and both are different from a flag file that is
// missing too.
func spawnerWhyNoTypes(t *SpawnerTable) string {
	switch {
	case t.TypeErr != nil:
		return t.TypeErr.Error()
	case t.FlagErr != nil:
		return t.FlagErr.Error()
	default:
		return "the file has 0 rows"
	}
}

// spawnerTactics turns a type row's tactics into the spawn path's override. The
// shipped rows say "None" when the species' own `Tactics` is the one to run, and
// passing "None" through would look for an AI row by that name and leave the body
// passive - so it is the same as an empty override.
func spawnerTactics(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "None") {
		return ""
	}
	return s
}

// atou32 reads an unsigned id attribute. atoi is the table convention but a
// ClassID is unsigned everywhere else in the zone (Monster.ClassID), so the
// conversion is done once here.
func atou32(s string) (uint32, bool) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	return uint32(v), err == nil
}
