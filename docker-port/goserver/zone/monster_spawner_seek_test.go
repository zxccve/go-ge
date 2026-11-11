package zone

// THE SPAWNER'S BODY, THE SEEK, AND THE ONE 3550.
//
// THE CARD'S MEASURED SYMPTOM (btl_auch01, map 1361, `-monai script`, 2026-11-09):
// every body's tactics loop was asking `GetObjectLocaly(mon=..., rng=2500)` once a
// second, and the log held ZERO `MONSTER APPROACH` / `MONSTER RUN` lines and
// `walks=0` in every METRICS window. The question was whether the seek answers nil
// because nothing is in range (correct) or because a SPAWNER-created body never
// reaches the selector at all.
//
// THE TWO FACTS THE LIVE LOG NOW PINS, AND THE TESTS HERE:
//
//	(1) a body `StartSpawner` placed IS in `runtimeMonsters` - the spawner's every
//	    body goes through `SpawnMonsterObject`, which appends to `spawnedMonster`
//	    (zone\create.go:451) and `runtimeMonsters` copies that roster
//	    (zone\create.go:128-137). So the AI pass and `monsterApproachTick` both see it.
//	(2) a player standing inside the body's own SeekRange is ACQUIRED and the order
//	    becomes ONE `ZC_MOVE_MONSTER` (3550) - pinned by handle and by the stand point
//	    the plan named, not by a log line.
//
// THE FIXTURES ARE THE REAL ONES: the shipped Monster table (`realMonsters`) and the
// shipped `datatable_monsterai.xml` behind it, a `SpawnerTable` shaped like
// `mongenflag_`/`mongentype_` rows, and the real translated corpus for the script
// case. The mesh is a stub that answers every ask as ground cell 29 - these tests
// measure the WIRING and the selector, not the path engine.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"geserver/script"
)

// spawnerSeekClass is Monster_AmokTortoise: Tactics="MON_AGGRESSIVERUN" in the shipped
// Monster row, AIType AGGRESSIVE, Attack TRUE, SeekRange 1500 (the live btl_auch01 log
// line `tactics "MON_AGGRESSIVERUN" AIType AGGRESSIVE attack yes seek 1500`).
const spawnerSeekClass = uint32(10594)

// spawnerSeekTable is one mongenflag_ row and one mongentype_ row that meet through
// Division 1 - the same join StartSpawner makes for a live map. `z` is the flag row's
// own PosZ, i.e. the ground height at the point.
func spawnerSeekTable(z int32) *SpawnerTable {
	return &SpawnerTable{
		Map:   "btl_auch01",
		Flags: []SpawnPoint{{ClassID: 1, Division: 1, X: 0, Y: 0, Z: z}},
		Types: []SpawnerType{{
			ClassID: 1, Division: 1,
			MonsterID: spawnerSeekClass, MonsterName: "Monster_AmokTortoise",
			Level: 65, Tactics: "MON_AGGRESSIVERUN",
			RespawnTime: 5 * time.Second, MinPop: 1, MaxPop: 1,
		}},
	}
}

// spawnerSeekZone builds a zone that can place a real AmokTortoise through the REAL
// StartSpawner and then walk it: the shipped tables, a mesh that answers cell 29, the
// game's own MAX-HP script (a spawned body's HP is SCR_Get_MON_MHP), and a stub plan.
func spawnerSeekZone(t *testing.T) (*Handler, *bytes.Buffer, *script.Host) {
	t.Helper()
	requireStatus(t)
	h, logs := castZone(t)
	sightUseTable(t, map[int]SightRow{swingMapID: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = swingMapID
	h.Monsters = realMonsters(t)
	h.MaxHP = testMonsterHPScript(t)
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		if hRange != spawnHRange || vRange != spawnVRange {
			return [3]int32{}, 0, false
		}
		return [3]int32{pt[0], pt[1], 29}, 29, true
	}
	h.Plan = directPlan

	host := script.New()
	t.Cleanup(host.Close)
	host.Log = h.Log.Printf
	host.Hate = h
	host.Monster = h
	host.Order = h
	host.Seek = h
	h.MonsterBrain = host
	return h, logs, host
}

// spawnerSeekBody places the table's one body and answers it, failing on any refusal.
// `z` is the flag row's PosZ (the ground height at the point).
func spawnerSeekBody(t *testing.T, h *Handler, logs *bytes.Buffer, z int32) Monster {
	t.Helper()
	run := h.StartSpawner(spawnerSeekTable(z))
	if run.Placed != 1 || run.Refused != 0 {
		t.Fatalf("StartSpawner placed %d and refused %d, want 1/0:\n%s",
			run.Placed, run.Refused, logs.String())
	}
	made := h.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d body(ies) in runtimeMonsters after the spawner, want 1 - a spawner "+
			"body must be in the roster the AI pass walks (zone/create.go:451, :128-137)",
			len(made))
	}
	m := made[0]
	if hp, ok := h.monsterLiveHP(m.Handle); !ok || hp <= 0 {
		t.Fatalf("the spawner body has no live HP (ok=%v, hp=%g): no tactics pass will "+
			"ever think for it:\n%s", ok, hp, logs.String())
	}
	return m
}

// TestSpawnerBodyIsInTheAIPass is question 2 answered without a script: the roster the
// think pass and the approach pass read is the spawner's own roster.
func TestSpawnerBodyIsInTheAIPass(t *testing.T) {
	h, logs, _ := spawnerSeekZone(t)
	m := spawnerSeekBody(t, h, logs, 29)

	if m.Tactics != "MON_AGGRESSIVERUN" {
		t.Fatalf("the spawner body runs tactics %q, want the type row's MON_AGGRESSIVERUN",
			m.Tactics)
	}
	if m.AssaultRange != 1500 {
		t.Fatalf("the spawner body's seek radius is %g, want the tactics row's SeekRange 1500",
			m.AssaultRange)
	}
	if m.AINoAttack {
		t.Fatalf("the tactics row says Attack=TRUE but the body was marked as never swinging")
	}
	// THE ROSTER THE TWO PASSES READ, named at their definitions.
	if got := h.runtimeMonsters(); len(got) != 1 || got[0].Handle != m.Handle {
		t.Fatalf("runtimeMonsters() = %d body(ies), want the spawner's own handle 0x%08x - "+
			"monsterAITick (zone/monster_ai.go:80) and monsterApproachTick "+
			"(zone/monster_order.go:544) both read this list",
			len(got), m.Handle)
	}
}

// TestSpawnerBodySeeksAPlayerInsideItsSeekRange is the CARD'S PASS CONDITION: a
// SPAWNER-created body (not a `-spawndummy` probe), a PC standing inside its own
// SeekRange, and ONE move order on the wire, pinned on the 3550's handle and goal.
//
// THE SEEK IS THE SCRIPT'S OWN SELECTOR (`SeekTarget`, the world behind
// `GetObjectLocaly`); the order is the one the BATTLE entry places (`StartAttack`);
// the walk is the approach's own clock (`monsterApproachTick`), which is the step that
// turns an order into a 3550.
func TestSpawnerBodySeeksAPlayerInsideItsSeekRange(t *testing.T) {
	h, logs, _ := spawnerSeekZone(t)
	body := spawnerSeekBody(t, h, logs, 29)

	// INSIDE SeekRange 1500 and OUTSIDE the row's reach, so the answer is a WALK, not a
	// swing: the case the live field never reached.
	victim := aiVictim(t, h, [3]int32{600, 0, 29})

	target, found := h.SeekTarget(body.Handle, body.AssaultRange, "")
	if !found {
		t.Fatalf("SeekTarget answered nil for a spawner body with the PC 600 units away "+
			"and SeekRange %g:\n%s", body.AssaultRange, logs.String())
	}
	if target.Handle != victim.sess.Team[0].Handle {
		t.Fatalf("the seek acquired 0x%08x, want the player 0x%08x",
			target.Handle, victim.sess.Team[0].Handle)
	}

	h.MonsterAIScript = true
	h.StartMonsterAttack(body.Handle, target.Handle)
	h.monsterApproachTick(time.Now())

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 for one order on a spawner body, want exactly 1 - the order "+
			"path is what drops a script's Move if it is broken:\n%s", len(moves), logs.String())
	}
	handle, goal, speed := decodeMonsterMove(t, moves[0])
	if handle != body.Handle {
		t.Fatalf("the 3550 names handle 0x%08x, want the spawner body 0x%08x", handle, body.Handle)
	}
	if d := flatDist([3]int32{body.X, body.Y, body.Cell}, goal); d < 1 {
		t.Fatalf("the 3550 walks to the body's own point %v - an order that does not move "+
			"is not an approach", goal)
	}
	if speed <= 0 {
		t.Fatalf("the 3550 carries %g u/s - SCR_Get_MON_MSPD answered nothing for a "+
			"spawner body", speed)
	}

	// ONE ORDER, NOT ONE PER TICK: the same still target must not re-send.
	for i := 0; i < 3; i++ {
		h.monsterApproachTick(time.Now())
	}
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 after four approach ticks for one still target, want 1 - the "+
			"leg in flight is kept (FieldChar.cpp:946-949):\n%s", n, logs.String())
	}
}

// TestSpawnerBodySeekLineIsWrittenOncePerBody is the diagnostic half: the seek's
// answer is printed for the body ONCE, however many scans it makes, and a nil names
// the nearest PC, its distance, and the range that was asked.
func TestSpawnerBodySeekLineIsWrittenOncePerBody(t *testing.T) {
	h, logs, _ := spawnerSeekZone(t)
	body := spawnerSeekBody(t, h, logs, 29)

	// FIRST: the player is 5000 away - outside the tactics row's 1500 circle. The line
	// must name the PC, the distance and the radius, not just "nobody".
	victim := aiVictim(t, h, [3]int32{5000, 0, 29})
	if _, found := h.SeekTarget(body.Handle, 1500, ""); found {
		t.Fatalf("the seek answered a target 5000 units away from a 1500 SeekRange")
	}
	wantOut := fmt.Sprintf("rng=1500 -> nobody; the nearest PC 0x%08x is 5000 units away, "+
		"OUTSIDE the circle", victim.sess.Team[0].Handle)
	if !strings.Contains(logs.String(), wantOut) {
		t.Fatalf("the seek line does not name the nearest PC, its distance and the radius; "+
			"want a line containing\n  %s\ngot:\n%s", wantOut, logs.String())
	}

	// MANY MORE SCANS: same body, same question, once a second for two minutes.
	for i := 0; i < 120; i++ {
		h.SeekTarget(body.Handle, 1500, "")
	}
	if n := strings.Count(logs.String(), "MONSTER AI SEEK 0x"); n != 1 {
		t.Fatalf("%d seek line(s) for one body after 121 nil scans, want 1 - a repeat of "+
			"the same answer writes nothing:\n%s", n, logs.String())
	}

	// THE SECOND LINE IS THE ACQUIRE: the player walks into the circle and the body's
	// answer changes. Exactly one more line, then silence again.
	cullPlace(h, victim, [3]int32{400, 0, 29})
	if _, found := h.SeekTarget(body.Handle, 1500, ""); !found {
		t.Fatalf("the seek answered nil for a PC 400 units inside SeekRange 1500:\n%s",
			logs.String())
	}
	for i := 0; i < 120; i++ {
		h.SeekTarget(body.Handle, 1500, "")
	}
	if n := strings.Count(logs.String(), "MONSTER AI SEEK 0x"); n != 2 {
		t.Fatalf("%d seek line(s) after a nil->found transition, want 2 (the nil and the "+
			"acquire), not one per scan:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "-> FOUND PC 0x") {
		t.Fatalf("the acquire line is missing - the transition from nil to found must be "+
			"visible:\n%s", logs.String())
	}

	// AND THE FOUND ANSWER IS THE OTHER SHAPE: one line, with the handle and distance.
	h2, logs2, _ := spawnerSeekZone(t)
	body2 := spawnerSeekBody(t, h2, logs2, 29)
	v2 := aiVictim(t, h2, [3]int32{400, 0, 29})
	if _, found := h2.SeekTarget(body2.Handle, body2.AssaultRange, ""); !found {
		t.Fatalf("the seek answered nil for a PC 400 units inside SeekRange %g:\n%s",
			body2.AssaultRange, logs2.String())
	}
	wantFound := fmt.Sprintf("rng=%g -> FOUND PC 0x%08x at 400 units",
		body2.AssaultRange, v2.sess.Team[0].Handle)
	if !strings.Contains(logs2.String(), wantFound) {
		t.Fatalf("the found seek line does not name the handle and distance; want\n  %s\ngot:\n%s",
			wantFound, logs2.String())
	}
	if n := strings.Count(logs2.String(), "MONSTER AI SEEK 0x"); n != 1 {
		t.Fatalf("%d seek line(s) for a found body, want 1:\n%s", n, logs2.String())
	}
}

// deepGroundZone is the btl_auch01 SHAPE the live log measured: the walkable ground sits
// 6,158 units BELOW z=0, and a mesh query whose z hint is outside the reference's
// 300-unit vertical band finds nothing - the same `NavNear` a tok mesh answers.
func deepGroundZone(t *testing.T, groundZ int32) (*Handler, *bytes.Buffer, *script.Host) {
	t.Helper()
	requireStatus(t)
	h, logs := castZone(t)
	sightUseTable(t, map[int]SightRow{swingMapID: {ViewRange: 6000, ViewMiddleRange: 4000}})
	h.MapID = swingMapID
	h.Monsters = realMonsters(t)
	h.MaxHP = testMonsterHPScript(t)
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		if hRange != spawnHRange || vRange != spawnVRange {
			return [3]int32{}, 0, false
		}
		if d := float64(pt[2] - groundZ); d > float64(vRange) || d < -float64(vRange) {
			return [3]int32{}, 0, false
		}
		return [3]int32{pt[0], pt[1], 29}, groundZ, true
	}
	h.Plan = directPlan

	host := script.New()
	t.Cleanup(host.Close)
	host.Log = h.Log.Printf
	host.Hate = h
	host.Monster = h
	host.Order = h
	host.Seek = h
	h.MonsterBrain = host
	return h, logs, host
}

// TestScriptWalkCarriesTheBodysOwnGroundHeight is the FIX for the 93 refused walks in the
// measured run: a spawner body's home point must carry the height its spawn resolved
// (Monster.GroundZ), and the script's own RandomMove must then put ONE 3550 on the wire.
//
// RED WITHOUT THE FIX: `monsterHomeAt` asked the mesh at z=0, this fixture's NavNear
// refused it (ground is 6,158 away, the band is 300), so Home.Z was 0 and RandomMove's
// `Move(mon, x, y, 0)` was refused with `MONSTER MOVE ... no ground within 1000 units`.
func TestScriptWalkCarriesTheBodysOwnGroundHeight(t *testing.T) {
	const groundZ = int32(-6158)
	h, logs, host := deepGroundZone(t, groundZ)
	body := spawnerSeekBody(t, h, logs, groundZ)
	if body.GroundZ != groundZ {
		t.Fatalf("the spawner body carries GroundZ %d, want the mesh height %d the spawn "+
			"resolved - a body that loses it cannot ask the mesh for its own ground:\n%s",
			body.GroundZ, groundZ, logs.String())
	}
	home, known := h.Home(body.Handle)
	if !known {
		t.Fatalf("the body has no home point after the spawn:\n%s", logs.String())
	}
	if home.Z != groundZ {
		t.Fatalf("the body's home Z is %d, want the ground height %d - this is the z every "+
			"later script walk passes to the mesh (btl_auch01: 93 refusals at z=0):\n%s",
			home.Z, groundZ, logs.String())
	}

	victim := aiVictim(t, h, [3]int32{600, 0, 29})
	src := fmt.Sprintf("local mon = { Handle = %d, X = %d, Y = %d }\nRandomMove(mon, 100, 500)",
		body.Handle, body.X, body.Y)
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\n%s", err, logs.String())
	}
	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 from the script's own RandomMove on a spawner body, want 1 - "+
			"the point is on the body's own ground:\n%s", len(moves), logs.String())
	}
	if strings.Contains(logs.String(), "the map's mesh has no ground") {
		t.Fatalf("the walk was refused on a body standing on known ground:\n%s", logs.String())
	}
	handle, goal, _ := decodeMonsterMove(t, moves[0])
	if handle != body.Handle {
		t.Fatalf("the 3550 names 0x%08x, want the spawner body 0x%08x", handle, body.Handle)
	}
	if d := flatDist([3]int32{body.X, body.Y, body.Cell}, goal); d < 100 || d > 500 {
		t.Fatalf("the walk goes %g units from the body, want the script's [100, 500] band "+
			"(the goal is %v)", d, goal)
	}
}

// TestSpawnerBodyScriptAcquiresAndWalks is the same pass condition through the GAME'S
// OWN SCRIPT instead of the zone's selector called directly: the NONE entry's
// GetObjectLocaly -> InsertHate -> ChangeTactics(TS_BATTLE), the BATTLE entry's
// StartAttack, and the approach's 3550. It SKIPS (never passes) when the translated
// corpus is absent, because a green test that ran no script would be worse than none.
func TestSpawnerBodyScriptAcquiresAndWalks(t *testing.T) {
	h, logs, host := spawnerSeekZone(t)
	body := spawnerSeekBody(t, h, logs, 29)
	if _, err := host.Index(monaiCorpus); err != nil {
		t.Skipf("the translated corpus is not at %s (%v) - rebuild it with: "+
			"cd src && python scp_to_lua.py --dump ../out/lua_corpus", monaiCorpus, err)
	}
	if err := host.LoadLibraries(); err != nil {
		t.Logf("script libraries did not load (%v) - the AI entries do not need them", err)
	}
	h.MonsterAIScript = true

	victim := aiVictim(t, h, [3]int32{600, 0, 29})

	// THE NONE ENTRY, THEN THE BATTLE ENTRY, on the order `tickIfDue` runs them
	// (zone\chase.go:771-776): the think pass, then the approach's own clock, for 3 s
	// of the script's time.
	t0 := time.Unix(1_700_000_000, 0)
	atBattle := false
	for i := 0; i < 60; i++ {
		at := t0.Add(time.Duration(i) * 100 * time.Millisecond)
		h.monsterAITick(at)
		h.monsterApproachTick(time.Now())
		if h.MonsterTactic(body.Handle) == "TS_BATTLE" {
			atBattle = true
		}
	}
	if !atBattle {
		t.Fatalf("the script never changed the spawner body to TS_BATTLE with a PC 600 "+
			"units inside its SeekRange 1500 - the NONE entry's acquire did not happen "+
			"(script log is the zone log here):\n%s", logs.String())
	}

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 after the game's own script acquired the player, want 1:\n%s",
			len(moves), logs.String())
	}
	handle, _, _ := decodeMonsterMove(t, moves[0])
	if handle != body.Handle {
		t.Fatalf("the script's 3550 names 0x%08x, want the spawner body 0x%08x",
			handle, body.Handle)
	}
	if !strings.Contains(logs.String(), "zone: TACTICS 0x") ||
		!strings.Contains(logs.String(), "-> TS_BATTLE") {
		t.Fatalf("no TACTICS line for the script's own state change:\n%s", logs.String())
	}
}
