// THE SCRIPT'S READ-ONLY AND TACTIC CALLS - the tests for zone\monster_engine.go and
// script\monster_engine.go.
//
// One test per rule the file header quotes: the tactics state is STORED and logged
// once per change; a name the state table does not declare is refused and the
// standing state survives; the distance is EDGE TO EDGE and may be negative; the AI
// row is read through the body's `Tactics` NAME (a different string from the state);
// the home is the spawn point and its distance is CENTRE TO CENTRE; the state dies
// with the body. The last test is the card's pass condition in Lua: every bound name
// called with the arity `ge\script\monster\mon_ts_battle.scp` calls it with, and the
// engine state each one touched asserted afterwards.
//
// NOTHING HERE RUNS A SCRIPT OR SENDS A PACKET except the one test that asserts no
// packet is sent.
package zone

import (
	"bytes"
	"strings"
	"testing"

	"geserver/script"
)

// engineZone is the smallest world these five calls answer from: a logger, a mesh
// that answers height 29 for any point (so a home's z is a real number and not a
// zero the test would be blind to), and nothing else.
func engineZone(t *testing.T) (*Handler, *bytes.Buffer) {
	t.Helper()
	h, logs := hateZone(t)
	h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return [3]int32{pt[0], pt[1], 29}, 29, true
	}
	return h, logs
}

// engineBody puts one monster on the field with the tactics NAME its row would carry
// (`MON_AGGRESSIVE` for class 11591). It is hateBody plus the one column these reads
// need, so that file's fixture is not edited for this card.
func engineBody(h *Handler, handle uint32, tactics string, x, y int32) Monster {
	m := Monster{Handle: handle, ClassID: chaseClassID, Level: 111, X: x, Y: y, Cell: 29, Tactics: tactics}
	h.spawnedMonster = append(h.spawnedMonster, m)
	if h.monsterHP == nil {
		h.monsterHP = map[uint32]float64{}
	}
	h.monsterHP[handle] = 100
	return m
}

// enginePlayer joins one player character at an exact point, so a distance case can
// choose its numbers.
func enginePlayer(h *Handler, handle uint32, name string, x, y int32) uint32 {
	p := newPlayer(handle, name, [3]int32{x, y, 1})
	h.Players.Add(p.sess)
	return p.sess.Team[0].Handle
}

// TestChangeTacticsStoresTheNameAndWritesOneLine is the card's first deliverable: the
// name is STORED on the body, the line `zone: TACTICS <mon> -> <TS_X>` is written on
// a CHANGE only, and a name the state table does not declare is refused exactly as
// FieldChar_Message.cpp:609-612 refuses it.
func TestChangeTacticsStoresTheNameAndWritesOneLine(t *testing.T) {
	h, logs := engineZone(t)
	mon := engineBody(h, MonsterHandle(21), "MON_AGGRESSIVE", 0, 0)

	// A fresh body is TS_NONE, exactly as CMonsterNPC::InitFSM leaves it
	// (`SetTacticsState(TS_NONE)`, MonsterNPC.cpp:255).
	if got := h.MonsterTactic(mon.Handle); got != "TS_NONE" {
		t.Fatalf("a body that has never changed tactics answers %q, want TS_NONE "+
			"(MonsterNPC.cpp:255)", got)
	}
	if strings.Contains(logs.String(), "zone: TACTICS ") {
		t.Fatalf("a fresh body wrote a TACTICS line:\n%s", logs.String())
	}

	h.ChangeTactics(mon.Handle, "TS_BATTLE")
	want := `zone: TACTICS 0x20000015 "class 11591" -> TS_BATTLE (was TS_NONE)`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the first change was not written\nwant: %s\ngot:\n%s", want, logs.String())
	}
	if got := h.MonsterTactic(mon.Handle); got != "TS_BATTLE" {
		t.Fatalf("after ChangeTactics(TS_BATTLE) the stored state is %q", got)
	}
	lines := strings.Count(logs.String(), "zone: TACTICS ")

	// THE SAME NAME AGAIN is stored and writes NOTHING: the card asks for the line
	// once, and a script in the no-target branch calls TS_NONE on every pass
	// (mon_ts_battle.scp:158).
	h.ChangeTactics(mon.Handle, "TS_BATTLE")
	if got := strings.Count(logs.String(), "zone: TACTICS "); got != lines {
		t.Fatalf("a repeated ChangeTactics wrote %d extra line(s):\n%s", got-lines, logs.String())
	}

	// TS_RALLY9 IS USED BY A SHIPPED SCRIPT AND DECLARED NOWHERE
	// (monnpc_viron_basic.scp:132; State.h:10-23 has RALLY1..RALLY4 only), so the
	// reference's GetState answers INVALID_STATE and the call RETURNS - the body keeps
	// TS_BATTLE.
	h.ChangeTactics(mon.Handle, "TS_RALLY9")
	if got := h.MonsterTactic(mon.Handle); got != "TS_BATTLE" {
		t.Fatalf("an undeclared tactics name changed the body's state to %q - the "+
			"reference returns on INVALID_STATE (FieldChar_Message.cpp:609-612)", got)
	}
	if !strings.Contains(logs.String(), `zone: TACTICS refused "TS_RALLY9"`) {
		t.Fatalf("an undeclared tactics name was refused WITHOUT a line:\n%s", logs.String())
	}

	// TS_RALLY1 IS declared (MonsterNPC.h:33), so it is accepted.
	h.ChangeTactics(mon.Handle, "TS_RALLY1")
	if got := h.MonsterTactic(mon.Handle); got != "TS_RALLY1" {
		t.Fatalf("ChangeTactics(TS_RALLY1) stored %q, want TS_RALLY1", got)
	}
	if !strings.Contains(logs.String(), "-> TS_RALLY1 (was TS_BATTLE)") {
		t.Fatalf("the second change was not written:\n%s", logs.String())
	}

	// A handle this zone never spawned stores nothing at all, once.
	h.ChangeTactics(MonsterHandle(99), "TS_BATTLE")
	if got := h.MonsterTactic(MonsterHandle(99)); got != "" {
		t.Fatalf("a handle that is not a body answered the state %q", got)
	}
	if n := strings.Count(logs.String(), "TACTICS refused for 0x"); n != 1 {
		t.Fatalf("the unknown-handle refusal was written %d times, want 1:\n%s", n, logs.String())
	}
}

// TestGetDistanceIsEdgeToEdge is the second census correction: the builtin subtracts
// BOTH collision bodies (FieldCharFunc.cpp:254-258), so 1000 units between two bodies
// of radius 50 is 900 - and two overlapping bodies answer a NEGATIVE number, which is
// a distance this side must not clamp.
func TestGetDistanceIsEdgeToEdge(t *testing.T) {
	h, _ := engineZone(t)
	h.Monsters = newMonsters(map[uint32]MonsterRow{chaseClassID: chaseRow()})
	mon := engineBody(h, MonsterHandle(22), "MON_AGGRESSIVE", 0, 0)
	ann := enginePlayer(h, 0x40000101, "Ann", 1000, 0)

	d, ok := h.Distance(mon.Handle, ann)
	if !ok {
		t.Fatalf("two live bodies answered no distance")
	}
	if d != 900 {
		t.Fatalf("1000 units between two radius-50 bodies answered %g, want 900 - the "+
			"builtin is EDGE TO EDGE (FieldCharFunc.cpp:254-258)", d)
	}

	// The monster's own half is its row's body: class 11591 is Size "S" = 50.
	if r := h.monsterBodyRadius(mon); r != 50 {
		t.Fatalf("the fixture's monster radius reads %g, want 50 (Size S)", r)
	}

	// OVERLAP IS NEGATIVE, not zero.
	movePlayer(h, ann, 50, 0)
	d, ok = h.Distance(mon.Handle, ann)
	if !ok {
		t.Fatalf("two overlapping bodies answered no distance")
	}
	if d != -50 {
		t.Fatalf("bodies 50 apart with radii 50+50 answered %g, want -50 - the "+
			"reference clamps nothing", d)
	}

	// A handle that is not on the field answers NULL, which is what the script's
	// `dist != nil` guard exists for (mon_ai_lib.scp:97).
	if _, ok := h.Distance(mon.Handle, MonsterHandle(99)); ok {
		t.Fatalf("a distance to a handle this zone never spawned was answered")
	}
}

// movePlayer puts one character's session at a new point, the way a move report
// leaves it: Session.Pos and the swing leg's start.
func movePlayer(h *Handler, handle uint32, x, y int32) {
	for _, mem := range h.Players.Snapshot() {
		if mem.Sess == nil {
			continue
		}
		for j := range mem.Sess.Team {
			if mem.Sess.Team[j].Handle == handle {
				mem.Sess.Pos = [3]int32{x, y, 1}
				mem.Sess.Team[j].Position = [3]int32{x, y, 1}
			}
		}
	}
}

// TestGetMonAiOptionReadsTheBodysTacticsRow is the FIRST census correction, and the
// one a reader is most likely to get wrong: the row is chosen by the body's `Tactics`
// NAME (`MON_AGGRESSIVE`), never by its class id and never by its tactics STATE. The
// eight keys the battle script reads are answered from the real
// `datatable_monsterai.xml`, and a string column stays a string ('TRUE') while a
// numeric one is a number (0), which is what the script's comparisons need.
func TestGetMonAiOptionReadsTheBodysTacticsRow(t *testing.T) {
	if _, err := LoadMonsterAiOptions(XMLDir()); err != nil {
		t.Skipf("the client's MonsterAi table is not readable here (%v)", err)
	}
	h, _ := engineZone(t)
	mon := engineBody(h, MonsterHandle(23), "MON_AGGRESSIVE", 0, 0)

	for _, c := range []struct {
		key   string
		text  string
		isStr bool
	}{
		{"AIType", "AGGRESSIVE", true},
		{"Attack", "TRUE", true},
		{"LtdMinion", "FALSE", true},
		{"RunAway", "FALSE", true},
		{"SelfBomb", "FALSE", true},
		{"Hatred", "FALSE", true},
	} {
		opt, ok := h.AiOption(mon.Handle, c.key)
		if !ok {
			t.Fatalf("GetMonAiOption(self, %q) answered NULL for a MON_AGGRESSIVE body",
				c.key)
		}
		if !opt.IsText || opt.Text != c.text {
			t.Fatalf("GetMonAiOption(self, %q) answered %q (text=%v), want the row's %q",
				c.key, opt.Text, opt.IsText, c.text)
		}
	}
	for _, key := range []string{"HateRange", "HateCount"} {
		opt, ok := h.AiOption(mon.Handle, key)
		if !ok {
			t.Fatalf("GetMonAiOption(self, %q) answered NULL", key)
		}
		if opt.IsText {
			t.Fatalf("GetMonAiOption(self, %q) answered the STRING %q - the reference "+
				"pushes a float for a non-string property (MonsterNPCFunc.cpp:856-860)",
				key, opt.Text)
		}
		if opt.Num != 0 {
			t.Fatalf("MON_AGGRESSIVE's %s reads %g, want 0", key, opt.Num)
		}
	}

	// A column the file does not carry answers NULL, like an unknown property
	// (:849-852).
	if _, ok := h.AiOption(mon.Handle, "NoSuchColumn"); ok {
		t.Fatalf("an unknown AI column was answered")
	}

	// A tactics NAME the file has no row for answers NULL (:843-847).
	stranger := engineBody(h, MonsterHandle(24), "NO_SUCH_TACTICS", 0, 0)
	if _, ok := h.AiOption(stranger.Handle, "AIType"); ok {
		t.Fatalf("a body whose Tactics names no AI row answered an option")
	}

	// A PLAYER is not a monster or a pet, which the reference refuses before it even
	// looks at the row (:838-841).
	ann := enginePlayer(h, 0x40000201, "Ann", 0, 0)
	if _, ok := h.AiOption(ann, "AIType"); ok {
		t.Fatalf("a player handle answered a monster AI option")
	}
}

// TestHomeIsTheSpawnPointAndItsDistanceIsCentreToCentre is the fourth census
// correction: `GetMyHomeDistance` is `GetDistance(m_createdPos, GetPos())` with the
// FREE centre-to-centre function (MonsterNPC.cpp:1906-1910, Common.cpp:39-45) - no
// radii - and `GetMyHomePosition` answers the map's own height there
// (MonsterNPCFunc.cpp:762-766).
func TestHomeIsTheSpawnPointAndItsDistanceIsCentreToCentre(t *testing.T) {
	h, _ := engineZone(t)
	h.Monsters = newMonsters(map[uint32]MonsterRow{chaseClassID: chaseRow()})
	mon := engineBody(h, MonsterHandle(25), "MON_AGGRESSIVE", 100, 200)

	home, ok := h.Home(mon.Handle)
	if !ok {
		t.Fatalf("a body on the field has no spawn point")
	}
	if home.X != 100 || home.Y != 200 || home.Z != 29 {
		t.Fatalf("the spawn point reads (%d, %d, %d), want (100, 200, 29) - x/y from "+
			"the roster and z from the map's own height", home.X, home.Y, home.Z)
	}
	if d, ok := h.HomeDistance(mon.Handle); !ok || d != 0 {
		t.Fatalf("a body standing on its spawn point answers home distance %g (ok=%v), "+
			"want 0", d, ok)
	}

	// The body walks 3000 east and 4000 north - the chase's own writer moves the
	// roster in place (monster_chase.go:512-515).
	h.spawnMu.Lock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == mon.Handle {
			h.spawnedMonster[i].X, h.spawnedMonster[i].Y = 3100, 4200
		}
	}
	h.spawnMu.Unlock()

	d, ok := h.HomeDistance(mon.Handle)
	if !ok {
		t.Fatalf("a moved body answered no home distance")
	}
	if d != 5000 {
		t.Fatalf("a body 3000/4000 from its spawn answered %g, want 5000 (centre to "+
			"centre, NO radii - MonsterNPC.cpp:1906-1910)", d)
	}

	// A PLAYER standing 100 units away is NOT part of a home distance: the free
	// function subtracts no collision bodies. (1000 + 3000, 4200) is 1000 away.
	enginePlayer(h, 0x40000301, "Near", 4100, 4200)
	if d, _ := h.HomeDistance(mon.Handle); d != 5000 {
		t.Fatalf("a nearby player changed the home distance to %g - the free "+
			"GetDistance subtracts no radii", d)
	}

	// THE EXACT HOOK: the spawn path records the point (and its height) itself.
	if !h.SetMonsterHome(mon.Handle, script.MonsterHome{X: 3100, Y: 4200, Z: 77}) {
		t.Fatalf("SetMonsterHome refused a body that is on the field")
	}
	if d, _ := h.HomeDistance(mon.Handle); d != 0 {
		t.Fatalf("after SetMonsterHome to where the body stands the home distance is "+
			"%g, want 0", d)
	}
	if home, _ := h.Home(mon.Handle); home.Z != 77 {
		t.Fatalf("the recorded home height reads %d, want 77", home.Z)
	}
	if h.SetMonsterHome(MonsterHandle(99), script.MonsterHome{}) {
		t.Fatalf("SetMonsterHome accepted a handle that is not a body")
	}
}

// TestMonsterEngineStateDiesWithTheBody pins the lifetime: the state is per handle
// and pruned against the roster, so a corpse's leave takes the tactics state and the
// spawn point with it - the same rule the hate table keeps.
func TestMonsterEngineStateDiesWithTheBody(t *testing.T) {
	h, _ := engineZone(t)
	first := engineBody(h, MonsterHandle(26), "MON_AGGRESSIVE", 10, 10)
	second := engineBody(h, MonsterHandle(27), "MON_AGGRESSIVE", 20, 20)
	h.ChangeTactics(first.Handle, "TS_BATTLE")
	h.ChangeTactics(second.Handle, "TS_FLEE")
	if _, ok := h.Home(first.Handle); !ok {
		t.Fatalf("the first body has no spawn point before its leave")
	}

	// THE LEAVE, exactly as takeDueLeaves does it: the roster row and the HP entry go
	// in one locked step.
	h.spawnMu.Lock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == first.Handle {
			h.spawnedMonster = append(h.spawnedMonster[:i], h.spawnedMonster[i+1:]...)
			break
		}
	}
	delete(h.monsterHP, first.Handle)
	h.spawnMu.Unlock()

	if got := h.MonsterTactic(first.Handle); got != "" {
		t.Fatalf("a body that left the field still answers the tactics state %q", got)
	}
	if _, ok := h.Home(first.Handle); ok {
		t.Fatalf("a body that left the field still answers a spawn point")
	}
	if got := h.MonsterTactic(second.Handle); got != "TS_FLEE" {
		t.Fatalf("the OTHER body's state died with its neighbour (reads %q)", got)
	}
	h.spawnMu.Lock()
	n := len(h.monsterEngines)
	h.spawnMu.Unlock()
	if n != 1 {
		t.Fatalf("%d engine states survive a one-body leave, want 1", n)
	}
}

// TestMonsterEngineSendsNothing is the card's "no behaviour change" line, measured on
// the wire: on a live chase fixture, every one of the five calls reads and stores and
// puts ZERO bytes on the socket. The chase's own test (`monster_chase_test.go`) then
// proves the chase still walks and swings exactly as before - this test only proves
// these five names cannot be the thing that moved it.
func TestMonsterEngineSendsNothing(t *testing.T) {
	h, _, victim, watcher, mon := chaseZone(t, [3]int32{400, 0, 29}, [3]int32{9000, 0, 29})
	home := script.MonsterHome{X: mon.X, Y: mon.Y, Z: 29}
	if !h.SetMonsterHome(mon.Handle, home) {
		t.Fatalf("SetMonsterHome refused the fixture's own body")
	}
	h.ChangeTactics(mon.Handle, "TS_BATTLE")
	for _, key := range []string{"AIType", "LtdMinion", "RunAway", "SelfBomb", "Hatred",
		"Attack", "HateRange", "HateCount"} {
		h.AiOption(mon.Handle, key)
	}
	h.Distance(mon.Handle, victim.sess.Team[0].Handle)
	h.HomeDistance(mon.Handle)
	h.Home(mon.Handle)

	if n := len(victim.conn.sent()) + len(watcher.conn.sent()); n != 0 {
		t.Fatalf("the five read-only calls sent %d packet(s); they must send none", n)
	}
	if got := h.MonsterTactic(mon.Handle); got != "TS_BATTLE" {
		t.Fatalf("the tactics state was not stored on a live fixture (reads %q)", got)
	}
}

// TestMonsterEngineBuiltinsRunFromLuaWithTheScriptsArity is the pass condition: a real
// gopher-lua runtime, a Handler behind it, and a snippet that calls EVERY bound name
// with the arity `mon_ts_battle.scp` gives it - including the eight AI keys in the
// script's own order (:11-18) and the THREE returns of GetMyHomePosition (:251).
func TestMonsterEngineBuiltinsRunFromLuaWithTheScriptsArity(t *testing.T) {
	if _, err := LoadMonsterAiOptions(XMLDir()); err != nil {
		t.Skipf("the client's MonsterAi table is not readable here (%v)", err)
	}
	h, logs := engineZone(t)
	h.Monsters = newMonsters(map[uint32]MonsterRow{chaseClassID: chaseRow()})
	mon := engineBody(h, MonsterHandle(28), "MON_AGGRESSIVE", 100, 200)
	ann := enginePlayer(h, 0x40000401, "Ann", 1100, 200)

	host := script.New()
	defer host.Close()
	host.Monster = h

	src := `
		local mon = { Handle = ` + luaNum(mon.Handle) + ` }
		local ann = { Handle = ` + luaNum(ann) + ` }

		-- :11-18, the eight reads the battle profile is built from, in its own order.
		local aiType    = GetMonAiOption(mon, 'AIType')
		local ltdMinion = GetMonAiOption(mon, 'LtdMinion')
		local runaway   = GetMonAiOption(mon, 'RunAway')
		local selfbomb  = GetMonAiOption(mon, 'SelfBomb')
		local hatred    = GetMonAiOption(mon, 'Hatred')
		local attackable= GetMonAiOption(mon, 'Attack')
		local haterange = GetMonAiOption(mon, 'HateRange')
		local hatecount = GetMonAiOption(mon, 'HateCount')

		assert(aiType == 'AGGRESSIVE', 'AIType answered ' .. tostring(aiType))
		assert(ltdMinion == 'FALSE', 'LtdMinion answered ' .. tostring(ltdMinion))
		assert(runaway == 'FALSE', 'RunAway answered ' .. tostring(runaway))
		assert(selfbomb == 'FALSE', 'SelfBomb answered ' .. tostring(selfbomb))
		assert(hatred == 'FALSE', 'Hatred answered ' .. tostring(hatred))
		assert(attackable == 'TRUE', 'Attack answered ' .. tostring(attackable))
		assert(haterange == 0, 'HateRange answered ' .. tostring(haterange))
		assert(hatecount == 0, 'HateCount answered ' .. tostring(hatecount))

		-- :185 / :197 GetDistance(self, other): edge to edge, 1000 - 50 - 50 = 900.
		local dist = GetDistance(mon, ann)
		assert(dist ~= nil, 'GetDistance answered nil for two live bodies')
		assert(dist == 900, 'GetDistance answered ' .. tostring(dist) .. ', want 900')

		-- :250 GetMyHomeDistance(self) - the body is standing on its spawn point.
		local homedist = GetMyHomeDistance(mon)
		assert(homedist == 0, 'GetMyHomeDistance answered ' .. tostring(homedist))

		-- :251 GetMyHomePosition(self) into THREE variables, then handed to Move.
		local x, y, z = GetMyHomePosition(mon)
		assert(x == 100 and y == 200, 'home position ' .. tostring(x) .. ',' .. tostring(y))
		assert(z == 29, 'home height ' .. tostring(z) .. ', want the map height 29')

		-- :158 ChangeTactics(self, 'TS_NONE') - two arguments, no return value.
		ChangeTactics(mon, 'TS_BATTLE')

		-- A handle that is not a body is NULL / 0, never a made-up answer.
		local ghost = { Handle = 12345 }
		assert(nil == GetDistance(mon, ghost), 'a ghost answered a distance')
		assert(nil == GetMonAiOption(ghost, 'AIType'), 'a ghost answered an AI option')
		assert(0 == GetMyHomeDistance(ghost), 'a ghost answered a home distance')
		local gx, gy, gz = GetMyHomePosition(ghost)
		assert(gx == 0 and gy == 0 and gz == 0, 'a ghost answered a home position')
	`
	if err := host.L.DoString(src); err != nil {
		t.Fatalf("the Lua snippet failed: %v\nengine log:\n%s", err, logs.String())
	}

	bound := map[string]bool{"ChangeTactics": true, "GetDistance": true,
		"GetMonAiOption": true, "GetMyHomeDistance": true, "GetMyHomePosition": true}
	for _, miss := range host.Missing() {
		if bound[miss.Name] {
			t.Fatalf("%s fell through to the missing-builtin stub %d time(s) - it is "+
				"not bound on the host", miss.Name, miss.Calls)
		}
	}

	if got := h.MonsterTactic(mon.Handle); got != "TS_BATTLE" {
		t.Fatalf("the snippet's ChangeTactics did not reach the zone (state %q)", got)
	}
	if !strings.Contains(logs.String(), "-> TS_BATTLE (was TS_NONE)") {
		t.Fatalf("the snippet's tactics change wrote no line:\n%s", logs.String())
	}
}
