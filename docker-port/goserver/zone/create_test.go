package zone

import (
	"encoding/binary"
	"log"
	"os"
	"strings"
	"testing"
)

// THE SCARECROW LANE'S OWN TESTS.
//
// Three things are pinned here, and nothing else:
//
//  1. A runtime NPC is sent with the TOWN NPC's packet, unchanged - built by
//     BuildSpawnTrigger and by no other code (the DoD's "the 3543 NPC spawn reused
//     unchanged").
//  2. A runtime monster is sent with the 360-byte 3527 from monster.go, whose bytes
//     are already held against a live capture (zone\monster_capture_test.go). What
//     THIS file adds is that the numbers reaching it are the MESH's answer and the
//     SCRIPT's arguments - not the script's z in the cell slot.
//  3. Every refusal is one log line, no packet, no handle, and no state change.
//
// The two tables and the mesh are stubs, so the tests say exactly which numbers
// came from where. The real loaders are covered by create_tables_test.go, and the
// real mesh by the live probe in the session log.

// fakeWorld is a Handler with the two tables loaded by hand and the mesh stubbed.
//
// navNear answers with `resolved` for ANY request, so a test can name the cell the
// mesh "found" and check that it is the number on the wire. It also RECORDS the
// request, which is how the "z is a height hint, not a cell" rule is proved: the
// ranges must be the original's 1000/300 and the z must arrive unchanged.
type fakeWorld struct {
	*Handler
	lines    *[]string
	navCalls [][3]int32
	ranges   [][2]int
	resolved [3]int32
	ground   int32
	navOK    bool
}

func newFakeWorld(t *testing.T) *fakeWorld {
	t.Helper()
	var lines []string
	w := &fakeWorld{
		Handler: &Handler{
			Log:      log.New(&logWriter{lines: &lines}, "", 0),
			MapID:    1331,
			NPCTypes: newNPCTypes(map[string]uint32{"npc_tbk_contbox_off": 91024}),
			Monsters: newMonsters(map[uint32]MonsterRow{
				11600: {ID: 11600, Name: "Monster_Hersu01", Level: 130, Exp: 0,
					Tactics: "MON_MISSION_84"},
			}),
		},
		lines:    &lines,
		resolved: [3]int32{2240, -590, 108},
		ground:   12,
		navOK:    true,
	}
	w.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		w.navCalls = append(w.navCalls, pt)
		w.ranges = append(w.ranges, [2]int{hRange, vRange})
		if !w.navOK {
			return [3]int32{}, 0, false
		}
		return w.resolved, w.ground, true
	}
	return w
}

// logWriter collects the zone's log lines without a file.
type logWriter struct{ lines *[]string }

func (w *logWriter) Write(p []byte) (int, error) {
	*w.lines = append(*w.lines, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// newNPCTypes builds the NPC table by hand: the loader is tested separately, and a
// spawn test must not depend on a 3 MB file being on this machine.
func newNPCTypes(byName map[string]uint32) *NPCTypeTable {
	return &NPCTypeTable{byName: byName}
}

func newMonsters(byID map[uint32]MonsterRow) *MonsterTable {
	return &MonsterTable{byID: byID}
}

// joinPlayer puts one player in the zone's roster with a recording socket, which
// is what a broadcast writes to.
func (w *fakeWorld) joinPlayer(handle uint32) *player {
	p := newPlayer(handle, "Tester", [3]int32{0, 0, 0})
	w.Players.Add(p.sess)
	return p
}

// TestCreateTNPCSendsTheTownNPCPacketUnchanged is the DoD's "the 3543 NPC spawn
// reused unchanged".
//
// The packet the world sent must be byte for byte BuildSpawnTrigger of the NPC that
// was stored - so there is ONE builder for a town NPC and a scripted one, and a fix
// to either reaches both. If somebody ever copies the builder into create.go, this
// test still passes while the two drift; that is why the second half checks the
// fields a copy would most likely get wrong (the handle, the position, the type).
func TestCreateTNPCSendsTheTownNPCPacketUnchanged(t *testing.T) {
	w := newFakeWorld(t)
	p := w.joinPlayer(1792)

	handle, err := w.SpawnTNPCObject(TNPCSpec{
		Name: "NPC_HITDUMMY01", TypeName: "npc_tbk_contbox_off",
		X: 2240, Y: -590, Z: 7, Direction: 270,
		Occupation: "<$>10089936</>", Possession: 1,
	})
	if err != nil {
		t.Fatalf("SpawnTNPCObject: %v", err)
	}

	sent := p.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("%d packet(s) sent, want exactly 1", len(sent))
	}
	pkt := sent[0]
	if len(pkt) != trSize {
		t.Fatalf("the spawn is %d bytes, the town NPC spawn is %d", len(pkt), trSize)
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCSpawnTrigger {
		t.Fatalf("cmd = %d, want %d (ZC_SPAWN_STANDINGTRIGGER)", cmd, CmdZCSpawnTrigger)
	}

	made := w.runtimeNPCs()
	if len(made) != 1 {
		t.Fatalf("%d runtime NPC(s) stored, want 1", len(made))
	}
	npc := made[0]
	if want := BuildSpawnTrigger(npc); string(want) != string(pkt) {
		t.Fatalf("the packet is not BuildSpawnTrigger's:\n ours %x\n town %x", pkt, want)
	}

	// What a copy would get wrong, named.
	if npc.Handle != handle {
		t.Errorf("stored handle 0x%08x, the call answered 0x%08x", npc.Handle, handle)
	}
	if npc.X != 2240 || npc.Y != -590 {
		t.Errorf("stored position (%d, %d), want (2240, -590) - the MESH's answer",
			npc.X, npc.Y)
	}
	if npc.Cell != 108 {
		t.Errorf("stored cell %d, want 108, the cell the mesh answered - not the "+
			"script's z (7)", npc.Cell)
	}
	if npc.NPCType != 91024 {
		t.Errorf("npc_type %d, want 91024 (npc_tbk_contbox_off's ClassID)", npc.NPCType)
	}
	if npc.Script != "NPC_HITDUMMY01" {
		t.Errorf("script %q, want the npcName - that is where SCR_CLICK_ comes from",
			npc.Script)
	}
	if npc.Direction != 191 {
		t.Errorf("direction %d, want 191 - 270 degrees through the game's own "+
			"GetByteAngle, not a byte cast", npc.Direction)
	}

	// The Z went to the MESH as a height, with the original's own ranges.
	if len(w.navCalls) != 1 {
		t.Fatalf("%d mesh call(s), want 1", len(w.navCalls))
	}
	if got := w.navCalls[0]; got != [3]int32{2240, -590, 7} {
		t.Fatalf("asked the mesh about %v, want the script's (x, y, z) = (2240, -590, 7)",
			got)
	}
	if r := w.ranges[0]; r != [2]int{1000, 300} {
		t.Fatalf("mesh ranges %v, want the original's 1000/300 "+
			"(Get3DPointToPosLocaly)", r)
	}
}

// TestCreateTNPCHandleContinuesTheTownNPCBlock is the handle rule: a runtime NPC is
// an NPC, so it lives in the NPCs' own block and continues their numbering. Two
// creations must never get one number, and no number may land in the monster or
// warp block.
func TestCreateTNPCHandleContinuesTheTownNPCBlock(t *testing.T) {
	w := newFakeWorld(t)
	// The map's own town NPCs: 11 of them on 1331, numbered from 0x40000000.
	w.NPCs = make([]NPC, 11)
	for i := range w.NPCs {
		w.NPCs[i] = NPC{Handle: NPC_HANDLE_BASE + uint32(i), ClassType: "town"}
	}

	first, err := w.SpawnTNPCObject(TNPCSpec{Name: "NPC_HITDUMMY01",
		TypeName: "npc_tbk_contbox_off", X: 2240, Y: -590, Z: 7, Direction: 270})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := w.SpawnTNPCObject(TNPCSpec{Name: "NPC_HITDUMMY01",
		TypeName: "npc_tbk_contbox_off", X: 3896, Y: -749, Z: 17, Direction: 270})
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first != NPC_HANDLE_BASE+11 {
		t.Errorf("first handle 0x%08x, want 0x%08x (the block, after 11 town NPCs)",
			first, uint32(NPC_HANDLE_BASE+11))
	}
	if second != first+1 {
		t.Errorf("second handle 0x%08x, want 0x%08x", second, first+1)
	}

	// Against every OTHER block this server hands out.
	for _, h := range []uint32{first, second} {
		if h == 0 || h>>24 == 0x7F {
			t.Errorf("handle 0x%08x is the warp block (0x7F......)", h)
		}
		if h >= MonsterHandleBase && h < NPC_HANDLE_BASE {
			t.Errorf("handle 0x%08x is inside the monster block", h)
		}
		if h < 0x10000000 {
			t.Errorf("handle 0x%08x is where a player's own row id lives", h)
		}
	}
	// The click path resolves them, which is what "clickable like a town NPC" means.
	if _, ok := w.npcByHandle(first); !ok {
		t.Error("a click on the created NPC does not resolve")
	}
}

// TestCreateMonsterCarriesTheMeshCellNotTheZ is the trap the brief names: the sixth
// argument is a HEIGHT (measured: `valid 2240 -590 7` is refused, `near 2240 -590 7
// 1000 300` answers cell 108). If it reached the packet as a cell, the Scarecrow
// would be drawn nowhere and the log would look healthy.
func TestCreateMonsterCarriesTheMeshCellNotTheZ(t *testing.T) {
	w := newFakeWorld(t)
	w.resolved = [3]int32{2623, -665, 108}
	w.ground = 11
	p := w.joinPlayer(1792)

	handle, err := w.SpawnMonsterObject(MonsterSpec{
		MonsterID: 11600, X: 2623, Y: -665, Z: 8, Count: 1,
		Direction: 270, Level: 130, MHP: 499900, Tactics: "MON_MISSION_84",
	})
	if err != nil {
		t.Fatalf("SpawnMonsterObject: %v", err)
	}

	sent := p.conn.sent()
	if len(sent) != 1 {
		t.Fatalf("%d packet(s) sent, want exactly 1", len(sent))
	}
	pkt := sent[0]
	if len(pkt) != EnterStandingMonsterSize {
		t.Fatalf("%d bytes, want the client's %d", len(pkt), EnterStandingMonsterSize)
	}
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCEnterStandingMonster {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCEnterStandingMonster)
	}

	made := w.runtimeMonsters()
	if len(made) != 1 {
		t.Fatalf("%d monster(s) stored, want 1", len(made))
	}
	m := made[0]
	if x := int32(binary.LittleEndian.Uint32(pkt[emPos:])); x != 2623 {
		t.Errorf("wire x %d, want 2623", x)
	}
	if y := int32(binary.LittleEndian.Uint32(pkt[emPos+4:])); y != -665 {
		t.Errorf("wire y %d, want -665", y)
	}
	if cell := int32(binary.LittleEndian.Uint32(pkt[emPos+8:])); cell != 108 {
		t.Fatalf("wire cell %d, want 108 - the MESH's answer. 8 is the script's z "+
			"(a height) and putting it here draws nothing", cell)
	}
	if h := binary.LittleEndian.Uint32(pkt[emHandle:]); h != handle {
		t.Errorf("wire handle 0x%08x, the call answered 0x%08x", h, handle)
	}
	if m.Handle != handle {
		t.Errorf("stored handle 0x%08x, the call answered 0x%08x", m.Handle, handle)
	}

	// The whole packet is the adopted builder's, byte for byte.
	if want := BuildEnterStandingMonster(m); string(want) != string(pkt) {
		t.Fatalf("the packet is not BuildEnterStandingMonster's")
	}

	// What the monster IS, kept for the damage slice.
	if m.ClassID != 11600 {
		t.Errorf("class %d, want 11600", m.ClassID)
	}
	if m.Level != 130 {
		t.Errorf("level %d, want 130", m.Level)
	}
	if m.HP != 499900 {
		t.Errorf("hp %d, want the script's 499900", m.HP)
	}
	if m.Tactics != "MON_MISSION_84" {
		t.Errorf("tactics %q, want MON_MISSION_84", m.Tactics)
	}

	// The handle is in the monster block, ABOVE the run's own clearing window. It is
	// deliberately NOT pinned to 0x20000002 any more: that was every run's first
	// number, which is exactly what let a client that outlived a zone restart be
	// holding it (zone\monster.go, nextMonsterHandleLocked).
	if m.Handle < MonsterHandleBase+monsterHandleWindow ||
		m.Handle >= MonsterHandleBase+monsterBlockSpan {
		t.Errorf("first runtime monster handle 0x%08x is outside the block's seeded "+
			"window (0x%08x..0x%08x) - a run must not start where every run used to",
			m.Handle, uint32(MonsterHandleBase+monsterHandleWindow),
			uint32(MonsterHandleBase+monsterBlockSpan))
	}
	if m.Handle == MonsterHandle(1) {
		t.Errorf("the runtime monster took the fixed S1 monster's own handle 0x%08x",
			m.Handle)
	}
}

// TestCreateMonsterCarriesItsOwnClass is the correction the packet evidence forced:
// +292 is not a constant, it is the monster's CLASS id, and the client builds the
// body from it (`GeBaseObject.cpp:227-232` -> `Monster.cpp:31` "Shape" -> BindModel).
// The parked lane sent the capture's 19763 for every monster; the Scarecrow must
// carry 11600 or the client would draw a flea-louse, or - with a 0 - crash.
func TestCreateMonsterCarriesItsOwnClass(t *testing.T) {
	// MaxHP is the capture's own HP double; the class is the ONLY field this test may
	// move, so the packet must otherwise equal the live bytes - including both HP
	// fields (see zone\monster.go emHP/emMaxHP).
	m := Monster{Handle: MonsterHandle(runtimeMonsterFirst), X: 2623, Y: -665,
		Cell: 108, ClassID: 11600, MaxHP: capturedMaxHP}
	pkt := BuildEnterStandingMonster(m)

	if got := binary.LittleEndian.Uint32(pkt[emClass:]); got != 11600 {
		t.Fatalf("the class at +%d is %d, want 11600 (Monster_Hersu01)", emClass, got)
	}
	// Nothing else in the packet may move because of it: the class is the ONLY
	// per-monster byte here besides the handle and the position, so the rest must
	// still equal the live capture exactly.
	live := liveEnterMonsterBytes(t)
	for i := range live {
		if i >= emHandle && i < emHandle+4 {
			continue
		}
		if i >= emPos && i < emPos+12 {
			continue
		}
		if i >= emClass && i < emClass+4 {
			continue
		}
		if pkt[i] != live[i] {
			t.Fatalf("offset +%d moved (ours 0x%02x, live 0x%02x) - only the handle, "+
				"the position and the class may differ", i, pkt[i], live[i])
		}
	}

	// The one fixed S1 monster still reproduces the capture's own class, which is
	// what keeps TestEnterMonsterMatchesLiveCapture strict.
	if got := DevStandingMonster().ClassID; got != emCaptureClass {
		t.Fatalf("the fixed monster's class is %d, want the capture's %d",
			got, emCaptureClass)
	}
}

func TestCreateMonsterRefusesAnUnknownId(t *testing.T) {
	w := newFakeWorld(t)
	p := w.joinPlayer(1792)

	_, err := w.SpawnMonsterObject(MonsterSpec{
		MonsterID: 99999, X: 2623, Y: -665, Z: 8, Count: 1, Direction: 270,
		MHP: 100, Tactics: "MON_MISSION_84",
	})
	if err == nil {
		t.Fatal("an unknown monster id was accepted")
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Errorf("the refusal does not name the id: %v", err)
	}
	if !strings.Contains(err.Error(), "Monster table") {
		t.Errorf("the refusal does not name the table: %v", err)
	}
	if got := p.conn.sent(); len(got) != 0 {
		t.Fatalf("%d packet(s) sent for a refused monster", len(got))
	}
	if got := w.runtimeMonsters(); len(got) != 0 {
		t.Fatalf("%d monster(s) stored for a refused one", len(got))
	}
	if got := w.monsterSeq; got != 0 {
		t.Fatalf("a refused monster spent handle index %d", got)
	}
}

// TestCreateTNPCRefusesAnUnknownType: the type name is an NPC table row; the
// Scarecrow's own 'npc_tbk_contbox_off' resolves, a typo does not.
func TestCreateTNPCRefusesAnUnknownType(t *testing.T) {
	w := newFakeWorld(t)
	p := w.joinPlayer(1792)

	_, err := w.SpawnTNPCObject(TNPCSpec{Name: "NPC_HITDUMMY01",
		TypeName: "npc_tbk_contbox_of", X: 2240, Y: -590, Z: 7, Direction: 270})
	if err == nil {
		t.Fatal("an unknown NPC type was accepted")
	}
	if !strings.Contains(err.Error(), "npc_tbk_contbox_of") {
		t.Errorf("the refusal does not name the type: %v", err)
	}
	if len(p.conn.sent()) != 0 || len(w.runtimeNPCs()) != 0 {
		t.Fatal("a refused NPC was sent or stored")
	}
}

// TestCreateRefusesAnOffMeshSpot: the mesh is the ONLY thing that can say a spot is
// ground. When it says no - or when there is no mesh at all - nothing is created
// and nothing is sent. Never (0, 0, 0).
func TestCreateRefusesAnOffMeshSpot(t *testing.T) {
	t.Run("the mesh has no ground there", func(t *testing.T) {
		w := newFakeWorld(t)
		w.navOK = false
		p := w.joinPlayer(1792)

		_, err := w.SpawnTNPCObject(TNPCSpec{Name: "NPC_HITDUMMY01",
			TypeName: "npc_tbk_contbox_off", X: 2240, Y: -590, Z: 7, Direction: 270})
		if err == nil {
			t.Fatal("a spot off the mesh was accepted")
		}
		if !strings.Contains(err.Error(), "no ground") {
			t.Errorf("the refusal does not say the mesh answered no: %v", err)
		}
		if len(p.conn.sent()) != 0 || len(w.runtimeNPCs()) != 0 {
			t.Fatal("an actor was sent or stored for a spot off the mesh")
		}
	})
	t.Run("no path engine at all", func(t *testing.T) {
		w := newFakeWorld(t)
		w.NavNear = nil
		p := w.joinPlayer(1792)

		_, err := w.SpawnMonsterObject(MonsterSpec{MonsterID: 11600,
			X: 2623, Y: -665, Z: 8, Count: 1, Direction: 270, MHP: 100})
		if err == nil {
			t.Fatal("a spawn with no mesh was accepted")
		}
		if !strings.Contains(err.Error(), "no path engine") {
			t.Errorf("the refusal does not say why: %v", err)
		}
		if len(p.conn.sent()) != 0 || len(w.runtimeMonsters()) != 0 {
			t.Fatal("an actor was sent or stored with no mesh to prove its cell")
		}
	})
}

// TestCreateLoudFailureIsOneLine: the failure rule is ONE line that names the
// argument - not a burst, and not silence. The stored error and the log line are
// read together, because the script host logs the line and the world logs the
// reason.
func TestCreateLoudFailureIsOneLine(t *testing.T) {
	w := newFakeWorld(t)
	w.Monsters = nil // the table did not load
	_, err := w.SpawnMonsterObject(MonsterSpec{MonsterID: 11600, X: 0, Y: 0, Z: 0,
		Count: 1, Direction: 270, MHP: 100})
	if err == nil {
		t.Fatal("a spawn with no monster table was accepted")
	}
	if !strings.Contains(err.Error(), "no monster table") {
		t.Errorf("the refusal does not name the missing table: %v", err)
	}
	if !strings.Contains(err.Error(), "11600") {
		t.Errorf("the refusal does not name the argument: %v", err)
	}
}

// TestCreateRefusesBeforeItLooksAtThemesh: an unknown id must be refused WITHOUT
// asking the mesh. Otherwise a zone with no path engine would report the mesh
// problem for a call whose real bug is the id, which is how a log sends the next
// session to the wrong file.
func TestCreateRefusesBeforeItLooksAtTheMesh(t *testing.T) {
	w := newFakeWorld(t)
	_, _ = w.SpawnMonsterObject(MonsterSpec{MonsterID: 99999, X: 1, Y: 2, Z: 3,
		Count: 1, MHP: 1})
	if len(w.navCalls) != 0 {
		t.Fatalf("the mesh was asked %d time(s) for an id that does not exist",
			len(w.navCalls))
	}
}

// TestRuntimeActorsReachALateJoiner is the other half of "exist": an object made at
// zone start must also be sent to a player who enters afterwards, or he walks past
// a Scarecrow that is there for everybody else.
//
// It reproduces the GAME_READY branch's own two calls (zone.go) rather than a copy
// of its logic: the packets a joining player gets are BuildNPCs(runtimeNPCs) and one
// BuildEnterStandingMonster per runtimeMonster.
func TestRuntimeActorsReachALateJoiner(t *testing.T) {
	w := newFakeWorld(t)
	if _, err := w.SpawnTNPCObject(TNPCSpec{Name: "NPC_HITDUMMY01",
		TypeName: "npc_tbk_contbox_off", X: 2240, Y: -590, Z: 7, Direction: 270}); err != nil {
		t.Fatalf("NPC: %v", err)
	}
	if _, err := w.SpawnMonsterObject(MonsterSpec{MonsterID: 11600, X: 2623,
		Y: -665, Z: 8, Count: 1, Direction: 270, MHP: 499900,
		Tactics: "MON_MISSION_84"}); err != nil {
		t.Fatalf("monster: %v", err)
	}

	npcPackets := BuildNPCs(w.runtimeNPCs())
	if len(npcPackets) != 1 {
		t.Fatalf("%d NPC packet(s) for a late joiner, want 1", len(npcPackets))
	}
	if cmd := binary.LittleEndian.Uint16(npcPackets[0]); cmd != CmdZCSpawnTrigger {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCSpawnTrigger)
	}
	monsters := w.runtimeMonsters()
	if len(monsters) != 1 {
		t.Fatalf("%d monster(s) for a late joiner, want 1", len(monsters))
	}
	pkt := BuildEnterStandingMonster(monsters[0])
	if cmd := binary.LittleEndian.Uint16(pkt); cmd != CmdZCEnterStandingMonster {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCEnterStandingMonster)
	}
	if len(pkt) != EnterStandingMonsterSize {
		t.Fatalf("%d bytes, want %d", len(pkt), EnterStandingMonsterSize)
	}
}

// TestStartupTacticsRunsTheScarecrowManagersEntry is the half that makes the
// builtins reachable: `SCR_<script>_TS_NONE` is the ONLY caller of CreateTNPC in
// the corpus, and the trigger's Script column is NPC_HITDUMMYMANAGER.
//
// It also pins the TWO rules that matter in a live zone: an NPC with no script is
// skipped (not logged as a failure), and the entries run ONCE - a second player
// entering must not make a second set of dummies.
func TestStartupTacticsRunsTheScarecrowManagersEntry(t *testing.T) {
	w := newFakeWorld(t)
	w.NPCs = []NPC{
		{Handle: NPC_HANDLE_BASE + 0, ClassType: "Trigger_Black_None",
			Script: "NPC_HITDUMMYMANAGER", X: 2240, Y: -590, Cell: 108},
		{Handle: NPC_HANDLE_BASE + 1, ClassType: "Shop", Script: ""},
		{Handle: NPC_HANDLE_BASE + 2, ClassType: "Warp", Script: "COIM"},
	}
	var ran []string
	w.Tactics = func(entry string, self ClickObject) (bool, error) {
		ran = append(ran, entry)
		if entry != "SCR_NPC_HITDUMMYMANAGER_TS_NONE" {
			return false, nil // the other NPCs have no such entry
		}
		// The entry as the corpus has it: four CreateTNPC calls.
		for _, spot := range [][3]float64{{2240, -590, 7}, {3896, -749, 17},
			{5936, -823, 19}, {7204, -1248, 14}} {
			if _, err := w.SpawnTNPCObject(TNPCSpec{Name: "NPC_HITDUMMY01",
				TypeName: "npc_tbk_contbox_off", X: spot[0], Y: spot[1],
				Z: spot[2], Direction: 270}); err != nil {
				return true, err
			}
		}
		return true, nil
	}

	w.StartupTactics()

	if len(ran) != 2 {
		t.Fatalf("%d tactic entr(y/ies) run, want 2 (one per SCRIPTED npc): %v",
			len(ran), ran)
	}
	if ran[0] != "SCR_NPC_HITDUMMYMANAGER_TS_NONE" {
		t.Fatalf("first entry %q, want the manager's", ran[0])
	}
	if got := len(w.runtimeNPCs()); got != 4 {
		t.Fatalf("%d dummy/dummies created, want the script's FOUR", got)
	}

	// Each one is on its own spot, all in the NPC block, all distinct.
	seen := map[uint32]bool{}
	for i, npc := range w.runtimeNPCs() {
		if seen[npc.Handle] {
			t.Fatalf("handle 0x%08x handed out twice", npc.Handle)
		}
		seen[npc.Handle] = true
		if npc.Handle < NPC_HANDLE_BASE {
			t.Errorf("dummy %d handle 0x%08x is outside the NPC block", i, npc.Handle)
		}
	}

	// A second call (as a second GAME_READY would be if it lived there) must not
	// double anything: StartupTactics is called once, at start, and the runtime list
	// is what a late joiner is drawn from.
	before := len(w.runtimeNPCs())
	if before != 4 {
		t.Fatalf("expected the four dummies to be stored once, got %d", before)
	}
}

// TestTacticsEntrySpelling pins the original's name construction, because the whole
// feature is one string: `sprintf_s(funcName,"SCR_%s_%s", m_scriptName, name);
// _strupr_s(funcName)` (TriggerObject.cpp:536-537).
func TestTacticsEntrySpelling(t *testing.T) {
	if got := TacticsEntry("NPC_HITDUMMYMANAGER", "TS_NONE"); got != "SCR_NPC_HITDUMMYMANAGER_TS_NONE" {
		t.Fatalf("%q, want SCR_NPC_HITDUMMYMANAGER_TS_NONE", got)
	}
	if got := TacticsEntry("leo_service", "TS_NONE"); got != "SCR_LEO_SERVICE_TS_NONE" {
		t.Fatalf("%q, want it upper-cased (the corpus names are upper case)", got)
	}
}

// TestByteAngle pins the game's own conversion (`GetByteAngle`, gePacketBase.h:11)
// and the error it exists to avoid: a plain cast turns 278 into 22, a silent
// 256-degree error that faces the actor backwards (`mapdata.go`, TargetDirByte).
func TestByteAngle(t *testing.T) {
	cases := []struct {
		in   float64
		want uint8
	}{
		{0, 0},
		{270, 191},   // 270/360*255 + 0.5
		{230, 163},   //
		{278, 197},   // the number a plain cast gets wrong (22)
		{359.7, 255}, // rounds up to the top of the byte
		{360, 0},     // one whole turn is 0
		{-90, 191},   // wrapped into the compass, then converted
	}
	for _, c := range cases {
		if got := byteAngle(c.in); got != c.want {
			t.Errorf("byteAngle(%g) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestCreateTablesReadTheClientsOwnFiles reads the two REAL tables, because a
// loader that passes a stub can still be wrong about the client's own attribute
// names. It skips when the xml folder is not on this machine (a CI box).
func TestCreateTablesReadTheClientsOwnFiles(t *testing.T) {
	dir := XMLDir()
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no client xml folder at %s: %v", dir, err)
	}
	types, err := LoadNPCTypes(dir)
	if err != nil {
		t.Fatalf("LoadNPCTypes(%s): %v", dir, err)
	}
	if id, ok := types.ClassID("npc_tbk_contbox_off"); !ok || id != 91024 {
		t.Fatalf("npc_tbk_contbox_off = %d (found %v), want 91024", id, ok)
	}

	monsters, err := LoadMonsters(dir)
	if err != nil {
		t.Fatalf("LoadMonsters(%s): %v", dir, err)
	}
	// The Scarecrow itself: Lv 130, EXP 0 - a dummy that pays nothing.
	row, ok := monsters.Row(11600)
	if !ok {
		t.Fatal("monster 11600 (the Scarecrow) is not in the table")
	}
	if row.Name != "Monster_Hersu01" {
		t.Errorf("11600 is %q, want Monster_Hersu01", row.Name)
	}
	if row.Level != 130 {
		t.Errorf("level %d, want 130", row.Level)
	}
	if row.Exp != 0 {
		t.Errorf("EXP %g, want 0 - a damage dummy pays no exp", row.Exp)
	}
	// All fifteen the script picks from must resolve, or a player's window choice
	// would fall through to a refusal.
	for id := uint32(11600); id <= 11614; id++ {
		if _, ok := monsters.Row(id); !ok {
			t.Errorf("monster %d is missing from the table", id)
		}
	}
}
