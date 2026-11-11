package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"geserver/protocol"
)

// S1's oracle: ONE real ZC_ENTER_STANDINGMONSTER (3527) from the live server, held
// against our builder byte for byte.
//
// The fixture is the RAW 360 bytes of the first 3527 in
// captures\queststage5\20261023-160543\s2c_7102_63470.bin, cut at file offset +296
// with the project's own splitter (`src\analyze_capture.py`, which reads the client's
// command table: 3527 is 360 bytes and the next packet starts exactly 360 bytes
// later). It is the packet for monster handle 190143, standing at (17666, -18525,
// cell 33).
//
// WHY THAT ONE OF THE 56. Its unknown bytes at +296/+343 read ZERO, and those two
// offsets are STALE MEMORY in the live server's own buffer, not data: in other
// packets +296/+297 hold the low half of the float that also sits at +302, i.e. a
// reused buffer that is not zeroed. Comparing against a packet whose leftovers are
// zero keeps our "we write zero there" a copy instead of a tidy-up.
//
// It is a fixture and not a live read for the same reason the spawn fixture is:
// the capture lives outside this repository, and a test that cannot run without
// it stops being run.
const liveEnterMonster = "testdata/live_enter_monster.bin"

// liveEnterMonsterBytes reads the fixture and refuses it unless it is the packet it
// claims to be: the fixed size, the command, and the monster handle the doc comment
// names. A fixture that drifted would make every comparison below meaningless.
func liveEnterMonsterBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(liveEnterMonster)
	if err != nil {
		t.Fatalf("read %s: %v", liveEnterMonster, err)
	}
	if len(b) != EnterStandingMonsterSize {
		t.Fatalf("%s is %d bytes, want the fixed size %d",
			liveEnterMonster, len(b), EnterStandingMonsterSize)
	}
	if cmd := binary.LittleEndian.Uint16(b); cmd != CmdZCEnterStandingMonster {
		t.Fatalf("%s is cmd %d, want %d", liveEnterMonster, cmd, CmdZCEnterStandingMonster)
	}
	if h := binary.LittleEndian.Uint32(b[emHandle:]); h != 190143 {
		t.Fatalf("%s names handle %d, the fixture this test was written against is "+
			"190143 - do not swap the file without re-reading the comment", liveEnterMonster, h)
	}
	return b
}

// enterMonsterDiffAllowed answers why a differing byte offset is EXPECTED to differ,
// or "" when it is not allowed and the comparison must fail.
//
// The list is four fields long, and every entry names the reason: the HANDLE (a
// monster's own id) and the POSITION - x, y and the navmesh CELL. Nothing else may
// differ. In particular every constant this builder writes - the flag at +6, both
// copies of the float 0x412F3AF8 at +32 and +302, the WORDs at +292/+306/+347/+358,
// and the two bytes at +308/+350 - must equal the live server's byte, so ONE wrong
// constant fails the test.
//
// There is no timestamp allowance: 3527 is a FIXED packet with no length word and no
// tick field, so nothing in it is clock-dependent.
func enterMonsterDiffAllowed(off int) string {
	switch {
	case off >= emHandle && off < emHandle+4:
		return "+2 handle: the monster's own handle"
	case off >= emPos && off < emPos+4:
		return "+15 x: where our monster stands"
	case off >= emPos+4 && off < emPos+8:
		return "+19 y: where our monster stands"
	case off >= emPos+8 && off < emPos+12:
		return "+23 cell: the navmesh triangle our monster stands on"
	}
	return ""
}

// TestEnterMonsterMatchesLiveCapture is S1's definition of done: our 360-byte 3527
// against the live server's own 360 bytes, one at a time, with only the handle and
// the position allowed to differ.
//
// It fails on a wrong constant AND on a wrong offset: the two copies of the float
// (at +32 and +302) are compared separately, so moving a value from one to the other
// is caught as well.
func TestEnterMonsterMatchesLiveCapture(t *testing.T) {
	live := liveEnterMonsterBytes(t)

	m := DevStandingMonster()
	got := BuildEnterStandingMonster(m)

	if len(got) != len(live) {
		t.Fatalf("built %d bytes, the capture is %d", len(got), len(live))
	}
	if cmd := binary.LittleEndian.Uint16(got); cmd != CmdZCEnterStandingMonster {
		t.Fatalf("cmd = %d, want %d", cmd, CmdZCEnterStandingMonster)
	}
	// 3527 has no length word, so this is the only place a "wrong size" can be said.
	if len(got) != EnterStandingMonsterSize {
		t.Fatalf("the packet is %d bytes, the client allocates %d for this id",
			len(got), EnterStandingMonsterSize)
	}

	// The four fields that ARE allowed to differ, named before the loop so a failure
	// reads as a fact rather than as an offset.
	if m.Handle != MonsterHandleBase+1 {
		t.Fatalf("the dev monster's handle is 0x%x, want 0x%x - the block is the point",
			m.Handle, uint32(MonsterHandleBase+1))
	}
	if m.Cell != devMonsterCell || m.X != devMonsterX || m.Y != devMonsterY {
		t.Fatalf("the dev monster stands at (%d, %d, cell %d), want the fixed spot "+
			"(%d, %d, cell %d)", m.X, m.Y, m.Cell, devMonsterX, devMonsterY, devMonsterCell)
	}

	differed, allowed := 0, 0
	var firstUnexplained string
	for i := range live {
		if got[i] == live[i] {
			continue
		}
		differed++
		why := enterMonsterDiffAllowed(i)
		if why == "" {
			if firstUnexplained == "" {
				firstUnexplained = fmt.Sprintf("offset +%d: ours 0x%02x, live 0x%02x",
					i, got[i], live[i])
			}
			continue
		}
		allowed++
		t.Logf("differs at +%d (%s): ours 0x%02x, live 0x%02x", i, why, got[i], live[i])
	}
	if firstUnexplained != "" {
		t.Fatalf("the built packet differs from the live capture at an offset that is "+
			"NOT allowed to differ - %s", firstUnexplained)
	}
	if allowed != differed {
		t.Fatalf("%d bytes differ, %d of them explained - the two counts must match",
			differed, allowed)
	}
	t.Logf("%d of %d bytes differ from the live capture (%s), all of them in the named "+
		"list: %d allowed, 0 unexplained", differed, len(live), liveEnterMonster, allowed)
}

// TestSpawnMonsterOffSendsNothing pins the FLAG, not the builder: with
// `-spawnmonster` off - the default, and what every live role runs - a GAME_READY
// reply carries no 3527 at all, and the whole reply is byte for byte the reply this
// server sent before S1 existed.
//
// The "identical to before" half is asserted by comparing the two replies packet by
// packet: the flag-on reply must be the flag-off reply plus exactly one 3527, in one
// place, and nothing else may move. That is what makes "no other map's behaviour
// changes" a measurement instead of a promise.
func TestSpawnMonsterOffSendsNothing(t *testing.T) {
	ready := pack(CmdCZGameReady)
	handles := []uint32{7, 40, 38}
	spots := [][3]int32{{-419, 870, 29}, {-494, 913, 29}, {-494, 826, 29}}

	ask := func(h *Handler) [][]byte {
		p := newTeam(handles, spots)
		out, err := h.Reply(context.Background(), p.sess,
			protocol.Frame{Cmd: CmdCZGameReady, Size: len(ready), Data: ready})
		if err != nil {
			t.Fatalf("GAME_READY: %v", err)
		}
		return out
	}

	off := ask(&Handler{Log: log.New(io.Discard, "", 0)})
	for _, pkt := range off {
		if cmd := cmdOf(pkt); cmd == CmdZCEnterStandingMonster {
			t.Fatalf("with -spawnmonster OFF the reply carries a %d - the flag is off by "+
				"default and no other map may see this packet", cmd)
		}
	}
	if len(off) == 0 {
		t.Fatal("the flag-off reply sent nothing at all - the comparison below would be empty")
	}

	on := ask(&Handler{
		Log:          log.New(io.Discard, "", 0),
		SpawnMonster: true,
		// The mesh's answer for the fixed spot, so this test measures the WIRE and
		// not whether pathtool.exe happens to be installed.
		NavHeight: func(pos [3]int32) (int32, bool) { return 26, true },
	})

	var monsters int
	var at int
	for i, pkt := range on {
		if cmdOf(pkt) == CmdZCEnterStandingMonster {
			monsters++
			at = i
		}
	}
	if monsters != 1 {
		t.Fatalf("with -spawnmonster ON the reply carries %d monster packet(s), want "+
			"exactly 1: S1 is ONE standing monster, sent once", monsters)
	}
	if want := len(off) + 1; len(on) != want {
		t.Fatalf("the flag-on reply is %d packets, the flag-off reply %d - the monster "+
			"must be the ONLY difference", len(on), len(off))
	}

	// Everything before the monster is unchanged, and everything after it is too.
	rest := append(append([][]byte{}, on[:at]...), on[at+1:]...)
	if len(rest) != len(off) {
		t.Fatalf("removing the monster leaves %d packet(s), the flag-off reply has %d",
			len(rest), len(off))
	}
	for i := range off {
		if !bytes.Equal(rest[i], off[i]) {
			t.Fatalf("packet %d differs between the flag-on and flag-off replies (cmd %d "+
				"vs %d, %d vs %d bytes) - the flag must change nothing else",
				i, cmdOf(rest[i]), cmdOf(off[i]), len(rest[i]), len(off[i]))
		}
	}

	// And the packet itself is the builder's own, with the handle and spot the log
	// line names - not a packet that merely carries the right command.
	want := BuildEnterStandingMonster(DevStandingMonster())
	if !bytes.Equal(on[at], want) {
		t.Fatalf("the sent monster is not BuildEnterStandingMonster(DevStandingMonster())")
	}
	t.Logf("flag OFF: %d packets, no 3527. flag ON: %d packets, one 3527 as packet %d "+
		"(%d bytes, handle %d, at (%d, %d) cell %d), everything else identical",
		len(off), len(on), at, len(on[at]), DevStandingMonster().Handle,
		devMonsterX, devMonsterY, devMonsterCell)
}

// TestSpawnMonsterRefusesAnUnprovedCell pins the guard that makes a wrong cell
// impossible to ship: with the flag ON but the map's mesh not answering for the
// fixed spot, the zone sends NOTHING.
//
// This is the failure that costs a card: a wrong cell draws nothing at all, on the
// wire everything looks perfect, and the report says "the monster is missing".
func TestSpawnMonsterRefusesAnUnprovedCell(t *testing.T) {
	ready := pack(CmdCZGameReady)

	// No path engine at all: the cell cannot be asked, so it cannot be sent.
	noEngine := &Handler{Log: log.New(io.Discard, "", 0), SpawnMonster: true}
	// A mesh that answers, but not for this spot (the state of every other map).
	wrongMap := &Handler{
		Log: log.New(io.Discard, "", 0), SpawnMonster: true,
		NavHeight: func(pos [3]int32) (int32, bool) { return 0, false },
	}

	for name, h := range map[string]*Handler{"no path engine": noEngine, "cell not on the mesh": wrongMap} {
		p := newTeam([]uint32{7}, [][3]int32{{-419, 870, 29}})
		out, err := h.Reply(context.Background(), p.sess,
			protocol.Frame{Cmd: CmdCZGameReady, Size: len(ready), Data: ready})
		if err != nil {
			t.Fatalf("%s: GAME_READY: %v", name, err)
		}
		for _, pkt := range out {
			if cmd := cmdOf(pkt); cmd == CmdZCEnterStandingMonster {
				t.Fatalf("%s: a %d was sent anyway - an unproved cell draws nothing and "+
					"reads as a server fault", name, cmd)
			}
		}
	}
}

// TestDevMonsterCellIsRealOnFldAuch is the position's own proof, asked the way the
// Last Point asks it (`Handler.NavHeight`, the game's own `positionIsValid`): the
// fixed cell is ground on Thueringen Lakeside's mesh, the CELL is what makes it
// ground (the triangles either side are refused), it is reachable from the field
// spawn, and the mesh puts the monster's feet at a real height.
//
// It SKIPS only when `pathtool.exe` is absent (a Linux zone has none) and FAILS when
// the tool is there and the mesh is missing - a missing mesh is exactly the bug this
// test exists to catch, because then the zone refuses to spawn and the screen stays
// empty.
func TestDevMonsterCellIsRealOnFldAuch(t *testing.T) {
	tool := filepath.Join("..", "..", "navmesh", "pathtool.exe")
	mesh := filepath.Join("..", "..", "navmesh", "mesh", "fld_auch.tok")

	if _, err := os.Stat(tool); err != nil {
		t.Skipf("no pathtool.exe at %s: the helper is a 32-bit Windows binary, so this "+
			"question cannot be asked here", tool)
	}
	if _, err := os.Stat(mesh); err != nil {
		t.Fatalf("pathtool.exe IS here, but fld_auch.tok is missing at %s - deploy the "+
			"client's own ge\\bg\\fld_auch.tok, or the zone refuses to draw the monster", mesh)
	}

	engine := &PathEngine{Tool: tool, Mesh: mesh, Radius: 50}
	t.Cleanup(func() { engine.Close() }) // the engine keeps one pathtool.exe running
	pos := [3]int32{devMonsterX, devMonsterY, devMonsterCell}

	height, ok, err := engine.Height(pos)
	if err != nil {
		t.Fatalf("the mesh could not answer for %v: %v", pos, err)
	}
	if !ok {
		t.Fatalf("the mesh says %v is not valid ground - the zone would refuse to send "+
			"the monster, and the screen would stay empty", pos)
	}
	if height == pos[2] {
		t.Fatalf("the height at %v is %d, which IS the cell - that is a cell in a "+
			"height field, the R73 bug", pos, height)
	}
	if height == 0 {
		t.Fatalf("the height at %v is 0 - a made-up number, not this map's ground", pos)
	}

	// The cell is not decoration: the neighbouring triangle indices are refused at
	// the same x/y, so a right point with a wrong cell is a packet that draws nothing.
	for _, cell := range []int32{pos[2] - 1, pos[2] + 1, 100} {
		if _, ok, _ := engine.Height([3]int32{pos[0], pos[1], cell}); ok {
			t.Fatalf("(%d, %d, cell %d) is valid ground too - the CELL would then not be "+
				"the thing that makes this spot work, and this test proves nothing",
				pos[0], pos[1], cell)
		}
	}

	// Reachable from the field spawn: the team lands at (-419, 870, cell 29), so the
	// monster must be walkable ground from there or the player cannot reach it.
	spawn := [3]int32{-419, 870, 29}
	route, err := engine.path(spawn, pos)
	if err != nil {
		t.Fatalf("path %v -> %v: %v", spawn, pos, err)
	}
	if len(route) < 2 {
		t.Fatalf("the mesh found no walkable route from the field spawn %v to the "+
			"monster's spot %v", spawn, pos)
	}

	// The restore side: the height turns back into the same cell.
	at, ground, ok, err := engine.Near([3]int32{pos[0], pos[1], height}, NearRangeXY, NearRangeZ)
	if err != nil || !ok {
		t.Fatalf("Near(%d, %d, %d): ok=%v err=%v", pos[0], pos[1], height, ok, err)
	}
	if at[2] != pos[2] || at[0] != pos[0] || at[1] != pos[1] {
		t.Fatalf("Near turned %v into %v, want the same spot and cell", pos, at)
	}
	if ground != height {
		t.Fatalf("Near says the ground there is %d, Height says %d - they must agree",
			ground, height)
	}
	t.Logf("fld_auch: (%d, %d, cell %d) is ground at height %d; cells %d and %d are "+
		"refused there; the field spawn %v walks to it in %d point(s)",
		pos[0], pos[1], pos[2], height, pos[2]-1, pos[2]+1, spawn, len(route))
}
