package zone

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// R73, BUG 2: `character_position.ZPOS` IS A WORLD HEIGHT, NEVER A NAVMESH CELL.
//
// The oracle's table is `CHARACTER_POSITION(CHARACTER_ID, XPOS, YPOS, ZPOS, MAP_ID,
// UPDATED)`, and the original fills ZPOS with `m_world->GetHeight(pos)` on the way out
// (Commander_FSM.cpp:644-650) and turns it back into a cell with
// `Get3DPointToPosLocaly(point, leaderPos, 1000, 300)` on the way in
// (Commander_Load.cpp:575-580).
//
// THE BUG THESE TESTS PIN (owner, 2026-10-29 22:31: "Last Point works but im being send
// under the terrain"): the writer stored the CELL, so `zpos` read 146 for a spot whose
// ground is at 257. The entry then handed 146 to the client as a triangle index, and the
// character stood under the floor. A height of 255 - the value other rows held - is a
// perfectly believable cell, so NOTHING downstream could catch it: only the mesh can.
//
// No client and no database here: the mesh is `testMesh` (last_point_test.go), which is
// the same two answers the real `pathtool.exe` gives.

// TestLastPointSavesAHeightNotTheCell is the WRITE half: the leader stands on cell 146,
// whose ground is at height 257, and the row must carry 257.
//
// THE MISTAKE IT CATCHES: writing the cell. It is invisible in the database (146 is a
// number, and so is 257) and shows up only as a character under the floor on the next
// entry - which is why this asserts the cell is NOT what landed in zpos.
func TestLastPointSavesAHeightNotTheCell(t *testing.T) {
	f := &lastPointFake{}
	h, logs := lastPointHandler(t, f) // mesh: (4301, 233, cell 146) is at height 257
	sess := lastPointSession()

	h.noteLastPoint(sess, lastPointSysMenu)

	if f.saves != 1 {
		t.Fatalf("%d save(s), want exactly 1; the log says:\n%s", f.saves, logs)
	}
	lp := f.saved[0]
	if lp.Pos[0] != sess.Pos[0] || lp.Pos[1] != sess.Pos[1] {
		t.Fatalf("saved x/y = %d, %d, want the leader's own %d, %d",
			lp.Pos[0], lp.Pos[1], sess.Pos[0], sess.Pos[1])
	}
	if lp.Pos[2] != 257 {
		t.Fatalf("zpos = %d, want the HEIGHT 257", lp.Pos[2])
	}
	if lp.Pos[2] == sess.Pos[2] {
		t.Fatalf("zpos = %d, which is the navmesh CELL the character stands on - the "+
			"oracle's ZPOS holds a height, and that is what puts a team under the floor",
			lp.Pos[2])
	}
	for _, want := range []string{"cell 146 = height 257", "with ZPOS = the HEIGHT 257"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the save line does not say %q; it says:\n%s", want, logs)
		}
	}
}

// TestLastPointIsNotSavedWithoutAMesh: without a path engine there is no height to store,
// and writing the cell is the bug - so nothing is written and the log says why.
//
// THE MISTAKE IT CATCHES: falling back to the cell when the mesh is missing, which would
// put the bug straight back on every machine that cannot run pathtool.exe.
func TestLastPointIsNotSavedWithoutAMesh(t *testing.T) {
	f := &lastPointFake{}
	h, logs := lastPointHandler(t, f)
	h.NavHeight = nil // no path engine on this machine
	sess := lastPointSession()

	h.noteLastPoint(sess, lastPointSysMenu)

	if f.saves != 0 {
		t.Fatalf("%d save(s), want none: the cell must never be written as a height",
			f.saves)
	}
	for _, want := range []string{"LAST POINT not saved", "no path engine is wired in",
		"writing the cell into zpos is the bug"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the refusal does not say %q; it says:\n%s", want, logs)
		}
	}
}

// TestLastPointIsNotSavedForAGapInTheMesh: the mesh has no ground at the walked spot, so
// there is no honest number to write either.
//
// THE MISTAKE IT CATCHES: writing a zero, or the cell, when the height query fails.
func TestLastPointIsNotSavedForAGapInTheMesh(t *testing.T) {
	f := &lastPointFake{}
	h, logs := lastPointHandler(t, f)
	sess := lastPointSession()
	sess.Pos = [3]int32{999999, 999999, 5} // valid-looking cell, no ground there

	h.noteLastPoint(sess, lastPointSysMenu)

	if f.saves != 0 {
		t.Fatalf("%d save(s), want none: the mesh has no height at that spot", f.saves)
	}
	if !strings.Contains(logs.String(), "no valid ground at") {
		t.Fatalf("the refusal does not name the gap; it says:\n%s", logs)
	}
}

// TestContinueEntryRefusesARowWhoseZIsTheCell is the READ half of the repair: an OLD row
// written by the buggy writer holds the cell, and it must be REFUSED rather than placed
// from - the team goes to the map's own spawn and the log says which happened.
//
// THE DETECTOR: the mesh's own height at that spot is a different number from the cell,
// so a stored z that EQUALS the resolved cell is not a height. Nothing else can tell them
// apart - a cell of 255 is a believable height.
func TestContinueEntryRefusesARowWhoseZIsTheCell(t *testing.T) {
	// Exactly the owner's row: map 10, (4110, -130), zpos 146 - and the mesh says that
	// spot is cell 146 at height 257.
	f := &lastPointFake{
		point: LastPoint{MapID: 10, Pos: [3]int32{4110, -130, 146}, Name: "Jin"},
		found: true,
	}
	h, _, logs := continueHandler(t, f)
	// The spot the row names, as the map's mesh knows it.
	newTestMesh().onGround([3]int32{4110, -130, 146}, 257).wire(h)
	sess := &Session{skipped: map[uint16]int{}}

	if _, err := h.Reply(context.Background(), sess, startGameFrame(4242)); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := sess.Team[0].Position; got != h.Spawn {
		t.Fatalf("the leader enters at %v, want the map's own spawn %v - a row whose z is "+
			"the cell must be refused, not used", got, h.Spawn)
	}
	for _, want := range []string{"z EQUALS the navmesh cell", "holds a CELL, not a height",
		"the map's own spawn is used instead"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("the refusal does not say %q; it says:\n%s", want, logs)
		}
	}
}

// TestContinueEntryRefusesARowWhoseZIsOffTheMesh: a stored z the mesh cannot reach within// the original's own tolerance (1000 horizontal, 300 vertical) is not a height either.
//
// THE MISTAKE IT CATCHES: searching with an unbounded vertical range, which would snap a
// cell-valued z onto the wrong level of the map - a bridge, a roof - and call it good.
func TestContinueEntryRefusesARowWhoseZIsOffTheMesh(t *testing.T) {
	f := &lastPointFake{
		point: LastPoint{MapID: 10, Pos: [3]int32{4301, 233, 20000}, Name: "TesterF"},
		found: true,
	}
	h, _, logs := continueHandler(t, f)
	sess := &Session{skipped: map[uint16]int{}}

	if _, err := h.Reply(context.Background(), sess, startGameFrame(4242)); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := sess.Team[0].Position; got != h.Spawn {
		t.Fatalf("the leader enters at %v, want the map's own spawn %v", got, h.Spawn)
	}
	if !strings.Contains(logs.String(), "no ground within") {
		t.Fatalf("the refusal does not name the mesh search; it says:\n%s", logs)
	}
}

// TestPathEngineHeightAndNearReadTheRealMesh runs the two new queries against the map's
// REAL navmesh (navmesh\pathtool.exe on navmesh\mesheb_basic.tok), because the parsing
// of their answers is where a silent bug would live: `valid` answers
// `valid <x> <y> <cell> <ok> <height>` and `near` answers
// `near <x> <y> <z> <ok> <px> <py> <cell> <height>`, and reading the wrong field would
// produce a number that looks like a position.
//
// The numbers are the owner's own row: (4110, -130) is cell 146, and the ground there is
// at height 257 - so the cell and the height really are different numbers, which is the
// whole reason this bug was invisible in the database.
func TestPathEngineHeightAndNearReadTheRealMesh(t *testing.T) {
	p := &PathEngine{
		Tool:   filepath.Join("..", "..", "navmesh", "pathtool.exe"),
		Mesh:   filepath.Join("..", "..", "navmesh", "mesh", "reb_basic.tok"),
		Radius: 50,
	}
	if !p.Available() {
		t.Skipf("no path engine at %s on %s", p.Tool, p.Mesh)
	}
	t.Cleanup(func() { p.Close() }) // the engine keeps one pathtool.exe running

	height, ok, err := p.Height([3]int32{4110, -130, 146})
	if err != nil {
		t.Fatalf("Height: %v", err)
	}
	if !ok {
		t.Fatal("the mesh says (4110, -130, cell 146) is not valid ground")
	}
	if height != 257 {
		t.Fatalf("height at (4110, -130, cell 146) = %d, want 257", height)
	}

	at, ground, ok, err := p.Near([3]int32{4110, -130, height}, NearRangeXY, NearRangeZ)
	if err != nil {
		t.Fatalf("Near: %v", err)
	}
	if !ok {
		t.Fatalf("the mesh found no ground within %d/%d of (4110, -130, %d)",
			NearRangeXY, NearRangeZ, height)
	}
	if at != [3]int32{4110, -130, 146} {
		t.Fatalf("Near answered %v, want (4110, -130, cell 146)", at)
	}
	if ground != height {
		t.Fatalf("Near says the ground is %d, Height says %d - they must agree",
			ground, height)
	}
	if at[2] == height {
		t.Fatalf("cell %d equals the height %d - the fixture no longer proves the two "+
			"numbers differ", at[2], height)
	}
}
