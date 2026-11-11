package zone

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// R76 FIX: A SAME-MAP JUMP SPREADS THE TEAM - a saved point has no formation of its own.
//
// THE REPORT (the owner, in play, 2026-10-30): "when u are in the same zone, warping will
// bundled your character together. should not happen. formation should be the same as
// spawn/quick move or moving to other town. this happen only when u in the same zone
// warping around. for example zone 10 warping to different coordinates in zone 10".
//
// THE CAUSE IT PINS: the saved row is (x, y, cell) and carries NO formation, so the jump
// used to place every member on the row's own spot - `formationSlot` answers `pos` for
// every index when the shape is empty. "Everybody on the spot" was written down as a rule
// in the same-map branch; it was never a formation, and his report is the evidence.
//
// WHAT IT DOES INSTEAD: the SAME shape every other arrival uses - this map's own arrival
// formation, laid around the row's point by `formationAround` (mapdata.go), the one
// function the gate's exit, the saved start point and the Last Point go through. Each
// follower's navmesh CELL is its OWN, resolved on this map's mesh at the leader's height
// (`formationCellAt`), because the archived trap says a formation slot's cell is NOT the
// leader's: copying it makes the client walk that character back to her own triangle on
// every warp, turning while she walks.
//
// THE CROSS-MAP BRANCH IS NOT TOUCHED by this, and its tests are the proof that stays
// green: TestWarpSelectCrossMapSpendsAScrollAndStartsTheMove and
// TestWarpSelectCrossMapWithoutAZoneRefuses (a map change is one 3593 plus a handoff, so
// it inherits the destination map's own arrival formation on that side).
const (
	// warpSpreadSpawn is map 15's own spawn origin, the shape's reference point. The two
	// follower offsets are the reference's `CCommander::GetTeamPos` V:
	// slot 1 = leader + (-75, +43), slot 2 = leader + (-75, -44).
	warpSpreadSpawnX = 2209
	warpSpreadSpawnY = 16830
	// warpSpreadPoint is the saved row the team jumps to: a spot that is NOT the spawn, so
	// "did the team move" and "where did each member land" are questions with one answer.
	warpSpreadPointX = 2222
	warpSpreadPointY = 16900
	// warpSpreadLeaderCell is the CELL the row itself stores for its own point.
	warpSpreadLeaderCell = 233
	// warpSpreadLeaderHeight is the map's ground height there. A saved row stores a cell
	// and no height, so this is the z the mesh is asked about - the emitter hands each
	// follower slot the point's own z for exactly the same reason.
	warpSpreadLeaderHeight = -1088
)

// warpSpreadHandler is warpTravelHandler with the two things a SPREAD needs: this map's
// own arrival formation (three slots, the reference's offsets) and a mesh that can answer
// a per-slot cell.
func warpSpreadHandler(t *testing.T, rows ...SavedPoint) (*Handler, *fakeShop,
	*bytes.Buffer, *testMesh) {
	t.Helper()
	h, _, bag, logs := warpTravelHandler(t, rows...)
	spawnCell, slot1Cell, slot2Cell := int32(warpSpreadLeaderCell), int32(231), int32(236)
	h.Spawn = [3]int32{warpSpreadSpawnX, warpSpreadSpawnY, spawnCell}
	h.SpawnFormation = []FormationSlot{
		{X: warpSpreadSpawnX, Y: warpSpreadSpawnY, Cell: &spawnCell},
		{X: warpSpreadSpawnX - 75, Y: warpSpreadSpawnY + 43, Cell: &slot1Cell},
		{X: warpSpreadSpawnX - 75, Y: warpSpreadSpawnY - 44, Cell: &slot2Cell},
	}
	mesh := newTestMesh().
		onGround([3]int32{warpSpreadPointX, warpSpreadPointY, warpSpreadLeaderCell},
			warpSpreadLeaderHeight).
		onGround([3]int32{warpSpreadPointX - 75, warpSpreadPointY + 43, 231},
			warpSpreadLeaderHeight+3).
		onGround([3]int32{warpSpreadPointX - 75, warpSpreadPointY - 44, 236},
			warpSpreadLeaderHeight-2)
	mesh.wire(h)
	return h, bag, logs, mesh
}

// warpTravelTeamSession is warpTravelSession for a team of n, so a formation can be
// checked with the THREE members the reference's V has slots for - and with one, for the
// case that must not change at all.
func warpTravelTeamSession(t *testing.T, bag *fakeShop, n int) *Session {
	t.Helper()
	handles := make([]uint32, n)
	at := make([][3]int32, n)
	for i := range handles {
		handles[i] = uint32(0x40000031 + i)
		at[i] = [3]int32{100, 200, 5} // a spot that is NOT the saved point
	}
	sess := newTeam(handles, at).sess
	sess.CommanderID = 25 // the throwaway test commander
	sess.Inv = NewInvMirror(bag.stacks)
	return sess
}

// TestWarpSelectSameMapSpreadsAThreeMemberTeam is DoD 1: three 3549s, at THREE DISTINCT
// positions, on the offsets the reference lays out, the leader exactly on the row's spot,
// and each slot's CELL its own - asserted as cells, not only as positions, because a copy
// of the leader's cell is the trap that made the third character walk on every warp.
func TestWarpSelectSameMapSpreadsAThreeMemberTeam(t *testing.T) {
	spot := [3]int32{warpSpreadPointX, warpSpreadPointY, warpSpreadLeaderCell}
	h, bag, logs, mesh := warpSpreadHandler(t,
		warpRow(0, 15, spot[0], spot[1], spot[2], "Bank"))
	sess := warpTravelTeamSession(t, bag, 3)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect,
		warpSelectPacket(0, commWarpOwnList, false))

	// ONE item line, then ONE stop per member, and NOT a 3634 refresh.
	if len(out) != 1+3 {
		t.Fatalf("%d packet(s), want 4 (one 3608 + three 3549):\n%s", len(out), logs)
	}
	if cmd := cmdOf(out[0]); cmd != CmdZCRemoveItem {
		t.Fatalf("the first packet is cmd %d, want the item line %d", cmd, CmdZCRemoveItem)
	}
	// LEADER LAST: the loop runs backwards, so slot 2 goes out first and slot 0 last.
	wantStop(t, out[1], sess.Team[2].Handle, [3]int32{2147, 16856, 236},
		"slot 2 = leader + (-75,-44), sent FIRST")
	wantStop(t, out[2], sess.Team[1].Handle, [3]int32{2147, 16943, 231},
		"slot 1 = leader + (-75,+43)")
	wantStop(t, out[3], sess.Team[0].Handle, spot, "the leader, on the row's own spot, LAST")

	// ONE tick for all three: three ticks would play the move as three separate jumps.
	tick := binary.LittleEndian.Uint32(out[1][6:])
	for i, p := range out[1:] {
		if got := binary.LittleEndian.Uint32(p[6:]); got != tick {
			t.Fatalf("stop %d carries tick %d, want %d - a warp is ONE tick", i, got, tick)
		}
	}

	// THREE DISTINCT positions and three distinct CELLS. The cells are the point of this
	// test: 233 (the row's own), 231 and 236 are three different triangles, so each slot
	// was resolved on the mesh for itself.
	positions := map[[3]int32]bool{}
	cells := map[int32]bool{}
	for i := range sess.Team {
		pos := sess.Team[i].Position
		positions[pos] = true
		cells[pos[2]] = true
	}
	if len(positions) != 3 {
		t.Fatalf("the team stands on %d distinct position(s), want 3 - that is the "+
			"bundling the owner reported: %v", len(positions), positions)
	}
	if len(cells) != 3 {
		t.Fatalf("the three members stand on %d distinct CELL(s), want 3 - a formation "+
			"slot's cell is its OWN, never the leader's: %v", len(cells), cells)
	}
	if sess.Team[0].Position != spot {
		t.Fatalf("the leader is at %v, want the row's own spot %v", sess.Team[0].Position,
			spot)
	}
	if sess.Pos != spot {
		t.Fatalf("the session stands at %v, want the row's spot %v", sess.Pos, spot)
	}

	// The mesh was asked about a HEIGHT, not about a cell: every question carried the
	// leader's ground height, which is what the emitter hands each follower slot.
	if len(mesh.asked) != 2 {
		t.Fatalf("the mesh was asked %d time(s), want 2 (one per follower slot): %v",
			len(mesh.asked), mesh.asked)
	}
	for _, z := range mesh.asked {
		if z != warpSpreadLeaderHeight {
			t.Fatalf("the mesh was asked with z = %d, want the leader's HEIGHT %d - a cell "+
				"passed as a height is a different question", z, warpSpreadLeaderHeight)
		}
	}

	// ONE scroll, one op, and ONE log line naming all three slots.
	if n := bagCount(bag, warpScrollClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll, want 1", n)
	}
	if len(bag.ops) != 1 {
		t.Fatalf("%d item op(s), want exactly 1", len(bag.ops))
	}
	lines := logLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d log line(s), want 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{
		"CZ_COMMWARP_SELECT (type0 row 0)", "CHARGED 1 WarpScroll", "same map",
		"no 3634 refresh", "spread by this map's own arrival formation",
		"3 slot(s), 0 the mesh refused",
		"slot 2 handle 1073741875 -> (2147, 16856, cell 236)",
		"slot 1 handle 1073741874 -> (2147, 16943, cell 231)",
		"slot 0 handle 1073741873 -> (2222, 16900, cell 233)",
	} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the jump line does not name %q:\n%s", want, lines[0])
		}
	}
	t.Logf("three-member jump line: %s", lines[0])
}

// TestWarpSelectSameMapGivesAOneMemberTeamExactlyOneStop is DoD 2: a team of one has one
// slot in the shape (slot 0 IS the point), so it gets exactly one stop - on the row's own
// spot - and the spread changes nothing about a solo jump.
func TestWarpSelectSameMapGivesAOneMemberTeamExactlyOneStop(t *testing.T) {
	spot := [3]int32{warpSpreadPointX, warpSpreadPointY, warpSpreadLeaderCell}
	h, bag, logs, _ := warpSpreadHandler(t,
		warpRow(0, 15, spot[0], spot[1], spot[2], "Bank"))
	sess := warpTravelTeamSession(t, bag, 1)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect,
		warpSelectPacket(0, commWarpOwnList, false))

	if len(out) != 2 {
		t.Fatalf("%d packet(s), want 2 (one 3608 + ONE 3549):\n%s", len(out), logs)
	}
	wantStop(t, out[1], sess.Team[0].Handle, spot, "the only member, on the row's own spot")
	if sess.Pos != spot {
		t.Fatalf("the session stands at %v, want %v", sess.Pos, spot)
	}
	if n := countStops(out); n != 1 {
		t.Fatalf("%d stop(s), want exactly 1", n)
	}
	t.Logf("one-member jump line: %s", strings.TrimSpace(logs.String()))
}

// TestWarpSelectSameMapFallsBackToTheLeaderForASlotTheMeshRefuses is DoD 3: when this
// map's mesh has no ground under one follower slot, THAT member stands with the leader and
// the jump still happens - one character beside the leader is better than a refused jump,
// and it is `formationSlot`'s own rule for a slot with no cell.
func TestWarpSelectSameMapFallsBackToTheLeaderForASlotTheMeshRefuses(t *testing.T) {
	spot := [3]int32{warpSpreadPointX, warpSpreadPointY, warpSpreadLeaderCell}
	slot2 := [3]int32{spot[0] - 75, spot[1] - 44, 236}
	h, bag, logs, mesh := warpSpreadHandler(t,
		warpRow(0, 15, spot[0], spot[1], spot[2], "Bank"))
	// Take away the ground under slot 1 ONLY (leader + (-75,+43)). Slot 2's stays.
	mesh.ground = map[[2]int32][3]int32{
		{spot[0], spot[1]}:           {spot[0], spot[1], warpSpreadLeaderCell},
		{slot2[0], slot2[1]}:         slot2,
	}
	mesh.heights = map[[3]int32]int32{
		{spot[0], spot[1], warpSpreadLeaderCell}: warpSpreadLeaderHeight,
		slot2:                                    warpSpreadLeaderHeight - 2,
	}
	sess := warpTravelTeamSession(t, bag, 3)

	out := warpReply(t, h, sess, CmdCZCommWarpSelect,
		warpSelectPacket(0, commWarpOwnList, false))

	// THE JUMP IS NOT REFUSED: still one item line and three stops.
	if len(out) != 4 {
		t.Fatalf("%d packet(s), want 4 (one 3608 + three 3549) - a slot the mesh refuses "+
			"must not refuse the jump:\n%s", len(out), logs)
	}
	wantStop(t, out[1], sess.Team[2].Handle, slot2,
		"slot 2, whose ground IS on the mesh")
	wantStop(t, out[2], sess.Team[1].Handle, spot,
		"slot 1, whose ground the mesh REFUSES - it stands with the leader")
	wantStop(t, out[3], sess.Team[0].Handle, spot, "the leader, on the row's own spot, LAST")
	if n := bagCount(bag, warpScrollClass); n != 1 {
		t.Fatalf("the bag holds %d WarpScroll, want 1 - the jump happened, so it was paid "+
			"for", n)
	}
	// The log says WHICH case this was, so "why is she standing on me" is answerable.
	if !strings.Contains(logs.String(), "3 slot(s), 1 the mesh refused") {
		t.Fatalf("the jump line does not name the refused slot:\n%s", logs.String())
	}
	t.Logf("refused-slot jump line: %s", strings.TrimSpace(logs.String()))
}

// countStops is how many ZC_STOP_PC packets (3549) one reply carries.
func countStops(out [][]byte) int {
	n := 0
	for _, p := range out {
		if cmdOf(p) == CmdZCStopPC {
			n++
		}
	}
	return n
}
