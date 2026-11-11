package zone

// THE DROP'S POINT: WHERE THE ITEM IS LAID AND THE CELL THAT DECIDES ITS HEIGHT.
//
// The complaint (the owner, 2026-11-08 in game) is "dropping something sometimes put the
// item in wrong position", and the two things a ground item is placed by are its X/Y and
// its CELL. The X/Y is the character's drawn body (bag_verbs.go dropItem ->
// sessionBodyAt); the CELL is a navmesh TRIANGLE INDEX the client takes the item's HEIGHT
// from (history\zone.md:42, history\zone-npc-click.md:324-326), so a cell copied from the
// walk's FAR END draws the item at a triangle it is not standing on - a live drop kept
// `cell 181` for a point 2,500 units away from that triangle (logs\zone_spawner.log,
// 2026-11-08 23:18:05.708).
//
// WHAT THESE TESTS PIN, and why each one can fail:
//
//	1  a drop taken WHILE WALKING, the move report and the drop in the same tick: the
//	   item lies on the character's own point (the report's from, where the body is) and
//	   NOT on the destination the report named. The cell is the triangle AT that point -
//	   the report's destination is in the next triangle, so a cell read from the leg
//	   fails this test.
//	2  a drop at a CELL BOUNDARY: the cell of the triangle the point is really in, both
//	   one step inside the triangle being left and one step into the one being entered -
//	   not the leg's end cell, which is the same 21 in both rows.
//
// The mesh is a fake two-triangle map, because the point of the test is the QUESTION the
// drop asks (which x/y, which search box) and the answer it copies, not the geometry of
// fld_auch. No test here sleeps, seeds a global or starts a path engine.

import (
	"encoding/binary"
	"strings"
	"sync"
	"testing"
	"time"

	"geserver/item"
)

// dropMesh is a two-triangle map: everything with x < 1500 is triangle 20, everything at
// or past it is 21. It answers the way the engine does - the ground nearest the point asked
// about - and it REMEMBERS every question, so a test can prove the drop asked about its own
// point and not about the walk's far end.
type dropMesh struct {
	mu     sync.Mutex
	asked  [][3]int32
	hRange int
	vRange int
}

func (m *dropMesh) near(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
	m.mu.Lock()
	m.asked = append(m.asked, pt)
	m.hRange, m.vRange = hRange, vRange
	m.mu.Unlock()
	if pt[0] < 1500 {
		return [3]int32{pt[0], pt[1], 20}, 0, true
	}
	return [3]int32{pt[0], pt[1], 21}, 40, true
}

// last is the last point the mesh was asked about.
func (m *dropMesh) last() ([3]int32, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.asked) == 0 {
		return [3]int32{}, false
	}
	return m.asked[len(m.asked)-1], true
}

// dropPosCellOf is the mesh's own rule, for a test that has to name the cell a point is
// really in without copying the code under test.
func dropPosCellOf(x int32) int32 {
	if x < 1500 {
		return 20
	}
	return 21
}

// newDropPosFixture is the bag fixture with the two-triangle mesh wired, and the character
// placed where the test wants her. The bag holds one Etc stack of 5.
func newDropPosFixture(t *testing.T, at [3]int32, mesh *dropMesh) *bagVerbFixture {
	t.Helper()
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 21301, Count: 5})
	f.h.NavNear = mesh.near
	f.p.sess.Pos = at
	f.p.sess.Team[0].Position = at
	return f
}

// walkFrom sets the leg a CZ_MOVE_PC opens, without waiting for one: `from` -> `dest` at
// the fixture character's own speed (469 u/s, the bare base: moveSpeedTerms with no stance
// table), with the leg's clock already `age` old. That is exactly what movePC writes.
func walkFrom(s *Session, from, dest [3]int32, age time.Duration) {
	s.moveFrom = from
	s.Pos = dest
	s.moveAt = time.Now().Add(-age)
	s.moveRoute = nil
}

// TestADropWhileWalkingLandsAtTheBodyNotTheLastReport is pass condition 1: the move report
// and the drop arrive in the SAME tick, and the item must lie where the character stands -
// the point the report makes her walk FROM - not the destination it names and not the
// cell of the triangle that destination sits in.
func TestADropWhileWalkingLandsAtTheBodyNotTheLastReport(t *testing.T) {
	mesh := &dropMesh{}
	// The character stands at (1000, 2000) in triangle 20 ...
	f := newDropPosFixture(t, [3]int32{1000, 2000, 20}, mesh)

	// ... and reports ONE walk to (2000, 2000), which is in triangle 21. A cell read
	// from this leg names 21 - a triangle the body is nowhere near.
	dest := [3]int32{2000, 2000, 21}
	if _, err := f.h.movePC(f.p.sess, moveFrame(0, dest)); err != nil {
		t.Fatalf("MOVE_PC: %v", err)
	}
	// THE SAME TICK: nothing between the report and the drop.
	dropOnce(t, f, 1, 1)

	ground := groundItemsOf(f.h)
	if len(ground) != 1 {
		t.Fatalf("the ground holds %d item(s), want exactly 1: %+v", len(ground), ground)
	}
	it := ground[0]

	// THE POINT: the body on the leg the report just opened, which for a same-tick
	// drop is the point the report starts FROM - the character's own live point. The
	// destination is 1,000 units away and must never be what the item is laid on: a
	// CZ_MOVE_PC names a destination, and `sess.Pos` holds exactly that.
	if it.Pos[1] != 2000 {
		t.Fatalf("the item lies at y %d, want 2000 (the walk's line)", it.Pos[1])
	}
	if it.Pos[0] < 1000 || it.Pos[0] >= 2000 {
		t.Fatalf("the item lies at x %d, want the body on the leg the report opened "+
			"(1000 .. well short of 2000): x 2000 is the DESTINATION the report named, "+
			"and a destination is not a position", it.Pos[0])
	}

	// THE CELL: the triangle that point is IN (20), never the one the leg ends in (21).
	if want := dropPosCellOf(it.Pos[0]); it.Pos[2] != want {
		t.Fatalf("the item lies at (%d, %d, cell %d) and the triangle at x %d is %d - "+
			"cell 21 is the walk's END, and the client takes the item's height from the "+
			"cell it is given", it.Pos[0], it.Pos[1], it.Pos[2], it.Pos[0], want)
	}
	if it.Pos[2] == 21 {
		t.Fatalf("the item carries the DESTINATION's cell 21 at x %d - the walk's far "+
			"end, which is the bug", it.Pos[0])
	}

	// THE QUESTION: the mesh was asked about the item's own point, with no height
	// (z 0) and the wide vertical band a point without one needs.
	asked, ok := mesh.last()
	if !ok {
		t.Fatal("the drop never asked the mesh for the ground under it")
	}
	if asked[0] != it.Pos[0] || asked[1] != it.Pos[1] || asked[2] != 0 {
		t.Fatalf("the drop asked the mesh about %v, want the item's own point (%d, %d, z 0)",
			asked, it.Pos[0], it.Pos[1])
	}
	if mesh.hRange != dropNearXY || mesh.vRange != dropNearZ {
		t.Fatalf("the drop searched %d x %d, want the mesh's own %d x %d",
			mesh.hRange, mesh.vRange, dropNearXY, dropNearZ)
	}

	// THE PACKET: the client draws what the frame carries, so the 3536 must name the
	// same point - a registry entry the client does not draw is not the item it sees.
	frames := itemFrames(f.p.conn.sent())
	if len(frames) != 1 {
		t.Fatalf("the dropper's socket got %d 3536 frame(s), want exactly 1:\n%s",
			len(frames), f.logs.String())
	}
	got := [3]int32{
		int32(binary.LittleEndian.Uint32(frames[0][ssiPosX:])),
		int32(binary.LittleEndian.Uint32(frames[0][ssiPosY:])),
		int32(binary.LittleEndian.Uint32(frames[0][ssiPosCell:])),
	}
	if got != it.Pos {
		t.Fatalf("the 3536 carries %v and the registry holds %v - the item the client "+
			"draws is not the item a pickup resolves", got, it.Pos)
	}
}

// TestADropOnACellBoundaryCarriesTheCellOfItsOwnPoint is pass condition 2: the same walk,
// dropped at three points around the x = 1500 triangle edge. The cell must be the triangle
// the item's own X/Y is in - 20 before the edge, 21 at and past it - and the leg's end cell
// is 21 in every row, so a cell copied from the walk's end cannot pass the first row.
func TestADropOnACellBoundaryCarriesTheCellOfItsOwnPoint(t *testing.T) {
	const from, step = 1200, 469.0 // the fixture character's own bare speed
	dest := [3]int32{1800, 2000, 21}

	cases := []struct {
		name string
		x    int32 // where the body is when the drop lands
		want int32 // -1: whichever triangle that x is really in
		why  string
	}{
		{"one step inside the triangle it is leaving", 1490, 20,
			"the point is 10 units short of the edge and the cell names 21"},
		{"one step into the triangle it is walking to", 1510, 21,
			"the point is past the edge, so 21 is its own triangle too"},
		{"exactly on the boundary", 1500, -1,
			"which side a point on the edge truncates to is arithmetic; its cell must " +
				"still be the triangle it landed in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mesh := &dropMesh{}
			f := newDropPosFixture(t, [3]int32{from, 2000, 20}, mesh)
			age := time.Duration(float64(c.x-from) / step * float64(time.Second))
			walkFrom(f.p.sess, [3]int32{from, 2000, 20}, dest, age)

			dropOnce(t, f, 1, 1)

			ground := groundItemsOf(f.h)
			if len(ground) != 1 {
				t.Fatalf("the ground holds %d item(s), want exactly 1: %+v",
					len(ground), ground)
			}
			it := ground[0]
			if d := it.Pos[0] - c.x; d < -3 || d > 3 {
				t.Fatalf("the item lies at x %d, want the body at %d +/- 3 - the clock is "+
					"the leg's own and the drop runs microseconds after it was set", it.Pos[0], c.x)
			}
			want := c.want
			if want < 0 {
				want = dropPosCellOf(it.Pos[0])
			}
			if it.Pos[2] != want {
				t.Fatalf("%s: the item lies at (%d, %d, cell %d), want cell %d - %s",
					c.name, it.Pos[0], it.Pos[1], it.Pos[2], want, c.why)
			}
			// AND THE LEG'S OWN CELL IS 21 IN EVERY ROW: a drop that copied it would
			// fail the first row and pass the other two by luck, which is the "sometimes".
			if it.Pos[0] < 1500 && it.Pos[2] == dest[2] {
				t.Fatalf("the item carries the walk's end cell %d at x %d (%s)",
					dest[2], it.Pos[0], c.why)
			}
		})
	}
}

// TestADropWithNoMeshKeepsThePointAndSaysSo is the fallback: a zone with no path engine (or
// a point with no ground under it) must still drop the item where the body is, keep the
// cell it had, and SAY WHICH in the one line - a silent fallback is a guess nobody can see.
func TestADropWithNoMeshKeepsThePointAndSaysSo(t *testing.T) {
	f := newBagVerbFixture(t, item.Stack{ItemID: 7001, ClassID: 21301, Count: 5})
	f.p.sess.Pos = [3]int32{1000, 2000, 20}
	f.p.sess.Team[0].Position = f.p.sess.Pos
	// NO NavNear: this zone cannot answer a cell, exactly as a machine with no
	// pathtool.exe cannot.
	dropOnce(t, f, 1, 1)

	ground := groundItemsOf(f.h)
	if len(ground) != 1 {
		t.Fatalf("the ground holds %d item(s), want exactly 1: %+v", len(ground), ground)
	}
	if ground[0].Pos != [3]int32{1000, 2000, 20} {
		t.Fatalf("the item lies at %v, want the body's own point [1000 2000 20]",
			ground[0].Pos)
	}
	if !contains(f.logs.String(), "cell 20 from the walk (no path engine to ask)") {
		t.Fatalf("the DROP line does not say the cell came from the walk:\n%s",
			f.logs.String())
	}
}

// TestARefusedMeshKeepsTheWalkCellAndSaysSo is the other refusal: the engine is there but
// it has no ground under the point (a body drawn off the mesh). The cell stays, and the
// line names it, so the next live drop is diagnosable from one line.
func TestARefusedMeshKeepsTheWalkCellAndSaysSo(t *testing.T) {
	mesh := &dropMesh{}
	f := newDropPosFixture(t, [3]int32{1000, 2000, 20}, mesh)
	f.h.NavNear = func(pt [3]int32, hRange, vRange int) ([3]int32, int32, bool) {
		return [3]int32{}, 0, false // no ground there
	}
	dropOnce(t, f, 1, 1)

	ground := groundItemsOf(f.h)
	if len(ground) != 1 || ground[0].Pos != [3]int32{1000, 2000, 20} {
		t.Fatalf("the ground holds %+v, want one item at the body's own point "+
			"[1000 2000 20]", ground)
	}
	if !contains(f.logs.String(), "cell 20 from the walk (the mesh has no ground under the point)") {
		t.Fatalf("the DROP line does not say the mesh refused the point:\n%s",
			f.logs.String())
	}
}

// contains is strings.Contains, asked through the one import this file does not need to
// repeat the standard library call for.
func contains(hay, needle string) bool {
	return strings.Contains(hay, needle)
}
