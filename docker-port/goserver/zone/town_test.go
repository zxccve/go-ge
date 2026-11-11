package zone

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"geserver/protocol"
)

// townMoveFrame is CZ_REQ_TOWN_MOVE: the 10-byte security header, then the button as
// an int32 - 14 bytes and nothing else (src\zone_server.py:4665-4667).
func townMoveFrame(button int32) protocol.Frame {
	pt := pack(CmdCZReqTownMove, appendU32(nil, uint32(button))...)
	return protocol.Frame{Cmd: CmdCZReqTownMove, Size: len(pt), Data: pt}
}

// map15Points reads the REAL table, because the point numbers are the data this
// feature exists to serve, and a hand-written copy could drift from it.
func map15Points(t *testing.T) []TownPoint {
	t.Helper()
	path := filepath.Join("..", "..", "data", "zone_points.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no spawn table at %s (run: cd src && python emit_zone_points.py)", path)
	}
	points, err := LoadZonePoints(path)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	list := points.TownPoints(15)
	if len(list) != townMoveButtons {
		t.Fatalf("map 15 carries %d town point(s), want %d", len(list), townMoveButtons)
	}
	return list
}

// map15Formation reads map 15 point 1's formation out of the REAL file, so the test
// checks the packets against the data instead of against a copy of it. The offsets are
// the ORIGINAL server's (CCommander::GetTeamPos, Commander.cpp:1605) and have been
// corrected once already; a number written into this test only ever proves the test was
// edited with it.
func map15Formation(t *testing.T) (point [3]int32, slots [][3]int32) {
	t.Helper()
	p := map15Points(t)[0]
	if p.N != 1 {
		t.Fatalf("the first town point of map 15 is n=%d, want 1", p.N)
	}
	if p.Cell == nil {
		t.Fatalf("map 15 point 1 has no cell; this test needs a point with ground")
	}
	point = [3]int32{p.X, p.Y, *p.Cell}
	slots = make([][3]int32, len(p.Formation))
	for i, s := range p.Formation {
		if s.Cell == nil {
			t.Fatalf("formation slot %d of map 15 point 1 has a null cell; the fallback "+
				"test covers that case, this one needs resolved ground", i)
		}
		slots[i] = [3]int32{s.X, s.Y, *s.Cell}
	}
	return point, slots
}

// assertFormationShape is the invariant that must hold whatever the numbers are: slot 0
// IS the point, the slots are DIFFERENT spots, and every slot carries the LEADER's cell.
// The original never recomputes the third int per follower, and a triangle of its own put
// one character away from the group in game.
func assertFormationShape(t *testing.T, point [3]int32, slots [][3]int32) {
	t.Helper()
	if len(slots) == 0 {
		t.Fatal("the formation is empty")
	}
	if slots[0] != point {
		t.Fatalf("formation slot 0 = %v, want the point itself %v", slots[0], point)
	}
	for i := range slots {
		if slots[i][2] != point[2] {
			t.Fatalf("formation slot %d carries cell %d, want the leader's %d",
				i, slots[i][2], point[2])
		}
		for j := 0; j < i; j++ {
			if slots[i][0] == slots[j][0] && slots[i][1] == slots[j][1] {
				t.Fatalf("formation slots %d and %d stand on the same spot %v",
					j, i, slots[i])
			}
		}
	}
}

// jumpPkt is one decoded packet of a warp: which command, which actor, where, and the
// tick it carries.
type jumpPkt struct {
	cmd    uint16
	handle uint32
	tick   uint32
	pos    [3]int32
}

// decodeJump reads one packet of a warp and insists it is a ZC_STOP_PC.
//
// A ZC_MOVE_PC (3548) or a ZC_ROTATE (3584) here is a FAILURE, not an alternative: the
// live server's warp is three 3549s and nothing else (capture 2026-10-27, see the file
// comment in zone\town.go). A move would start a path and turn the actor, and a rotate is
// ours to invent - neither belongs in this answer any more.
//
// The size check is the same point: a stop is 22 bytes and a move 30, and neither has a
// length word - +2 is the HANDLE, so a length written there would move an actor with
// handle 30.
func decodeJump(t *testing.T, pkt []byte) jumpPkt {
	t.Helper()
	p := jumpPkt{cmd: cmdOf(pkt)}
	if p.cmd != CmdZCStopPC {
		t.Fatalf("warp packet is cmd %d, want only %d (ZC_STOP_PC)", p.cmd, CmdZCStopPC)
	}
	if len(pkt) != 22 {
		t.Fatalf("ZC_STOP_PC is %d bytes, want 22", len(pkt))
	}
	p.handle = binary.LittleEndian.Uint32(pkt[2:])
	p.tick = binary.LittleEndian.Uint32(pkt[6:])
	p.pos = [3]int32{
		int32(binary.LittleEndian.Uint32(pkt[10:])),
		int32(binary.LittleEndian.Uint32(pkt[14:])),
		int32(binary.LittleEndian.Uint32(pkt[18:])),
	}
	return p
}

// TestTownMoveJumpsTheWholeTeam is the panel button working, in the shape the LIVE server
// was captured sending: one ZC_STOP_PC per character, in REVERSE team order (last member
// first, LEADER LAST), every packet on the SAME tick.
//
// The order and the shared tick are the whole point. The client anchors the team on the
// leader, so a stop naming the leader first makes the follower stops that follow stale -
// that was the in-game failure of the first stop-only attempt (one character missing, the
// rest stacked). The original warps the whole team (CCommander::OnReqTownMove -> SetPos ->
// OM_WARP -> CFieldChar::OnWarp, Commander.cpp:1270-1286), so a leader-only answer leaves
// the other two standing.
func TestTownMoveJumpsTheWholeTeam(t *testing.T) {
	points := map15Points(t)
	handles := []uint32{0x40000031, 0x40000032, 0x40000033}

	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 15, Points: points}
	a := newTeam(handles, [][3]int32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})
	b := newPlayer(0x40000041, "B", [3]int32{7, 8, 9})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil

	out, err := h.townMove(a.sess, townMoveFrame(1))
	if err != nil {
		t.Fatalf("townMove: %v", err)
	}
	if len(out) != len(handles) {
		t.Fatalf("sent %d packet(s), want %d (one stop per character, nothing else)",
			len(out), len(handles))
	}

	// Coimbra point 1's FORMATION comes from the FILE, not from this test: the leader on
	// the point and the two followers beside it, each on its own offset. Nobody stands on
	// another's spot, and the three share one cell, because the original server carries
	// the leader's cell into every slot.
	leader, want := map15Formation(t)
	if len(want) != len(handles) {
		t.Fatalf("map 15 point 1 carries %d formation slot(s), want %d for this team",
			len(want), len(handles))
	}
	assertFormationShape(t, leader, want)

	// REVERSE order: slot len-1 first, the leader's slot 0 LAST. Every packet carries its
	// own character's handle, that character's slot, and the warp's ONE tick.
	ticks := make([]uint32, len(out))
	for k, i := range []int{2, 1, 0} {
		stop := decodeJump(t, out[k])
		ticks[k] = stop.tick
		if stop.handle != handles[i] {
			t.Fatalf("packet %d (slot %d) handle = %#x, want its own %#x",
				k, i, stop.handle, handles[i])
		}
		if stop.pos != want[i] {
			t.Fatalf("packet %d (slot %d) WORLDPOS = %v, want %v",
				k, i, stop.pos, want[i])
		}
	}
	if last := decodeJump(t, out[len(out)-1]); last.handle != handles[0] {
		t.Fatalf("the LAST packet names handle %#x, want the leader's %#x",
			last.handle, handles[0])
	}
	for k := range ticks {
		if ticks[k] != ticks[0] {
			t.Fatalf("packet %d tick = %d, want the warp's one tick %d",
				k, ticks[k], ticks[0])
		}
	}

	if a.sess.Pos != leader {
		t.Fatalf("session position = %v, want the leader's slot %v", a.sess.Pos, leader)
	}
	for i := range a.sess.Team {
		if got := a.sess.Team[i].Position; got != want[i] {
			t.Fatalf("stored position of character %d = %v, want slot %d %v",
				i, got, i, want[i])
		}
	}

	// The other player sees the same three, in the same order, byte for byte - the
	// reference broadcasts, and only answering the mover is a multiplayer bug.
	sent := b.conn.sent()
	if len(sent) != len(out) {
		t.Fatalf("the other player got %d packet(s), want %d", len(sent), len(out))
	}
	for i := range out {
		if !bytes.Equal(sent[i], out[i]) {
			t.Fatalf("broadcast packet %d differs from the mover's", i)
		}
	}

	// The roster agrees for EVERY character, or the next spawn puts one back.
	for _, m := range h.Players.Snapshot() {
		if m.Sess != a.sess {
			continue
		}
		for i := range m.Pos {
			if m.Pos[i] != want[i] {
				t.Fatalf("roster position of character %d = %v, want slot %d %v",
					i, m.Pos[i], i, want[i])
			}
		}
	}
	// Compare with the capture: one line per packet, in the order written.
	for k := range out {
		p := decodeJump(t, out[k])
		t.Logf("packet %d: cmd %d handle %d pos (%d, %d, cell %d) tick %d",
			k, p.cmd, p.handle, p.pos[0], p.pos[1], p.pos[2], p.tick)
	}
}

// TestTownMoveMatchesTheLiveCapture pins the answer against the capture itself: map 10
// (reb_basic) point 2 - "Market Street" - which is the first cluster of the capture of
// 2026-10-27. The handles are the capture's own, so the two can be compared line by line.
//
// VERBATIM FROM THE CAPTURE, and what this test must reproduce:
//
//	handle 972620 (-5909, 1269, cell 218) slot 2, FIRST
//	handle 972619 (-5909, 1356, cell 218) slot 1
//	handle 972618 (-5834, 1313, cell 218) the point, LEADER LAST
func TestTownMoveMatchesTheLiveCapture(t *testing.T) {
	path := filepath.Join("..", "..", "data", "zone_points.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no spawn table at %s (run: cd src && python emit_zone_points.py)", path)
	}
	points, err := LoadZonePoints(path)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	list := points.TownPoints(10)
	var point TownPoint
	for _, p := range list {
		if p.N == 2 {
			point = p
		}
	}
	if point.N != 2 {
		t.Fatalf("map 10 (reb_basic) carries no town point 2; it has %d point(s)", len(list))
	}

	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 10, Points: list}
	handles := []uint32{972618, 972619, 972620} // leader first in the team
	a := newTeam(handles, [][3]int32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})
	h.joinZone(a.sess)
	a.conn.writes = nil

	out, err := h.townMove(a.sess, townMoveFrame(2))
	if err != nil {
		t.Fatalf("townMove: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("sent %d packet(s), want the capture's 3", len(out))
	}

	want := []struct {
		handle uint32
		pos    [3]int32
	}{
		{972620, [3]int32{-5909, 1269, 218}}, // slot 2, written FIRST
		{972619, [3]int32{-5909, 1356, 218}}, // slot 1
		{972618, [3]int32{-5834, 1313, 218}}, // the leader, written LAST
	}
	for k, w := range want {
		p := decodeJump(t, out[k])
		if p.handle != w.handle || p.pos != w.pos {
			t.Fatalf("packet %d = handle %d pos %v, want the capture's handle %d pos %v",
				k, p.handle, p.pos, w.handle, w.pos)
		}
		if p.tick != decodeJump(t, out[0]).tick {
			t.Fatalf("packet %d tick = %d, want the warp's one tick %d",
				k, p.tick, decodeJump(t, out[0]).tick)
		}
		t.Logf("packet %d: cmd %d (%d B) handle %d pos (%d, %d, cell %d) tick %d",
			k, p.cmd, len(out[k]), p.handle, p.pos[0], p.pos[1], p.pos[2], p.tick)
	}
}

// TestTownMoveFallsBackWhenASlotHasNoCell: a follower slot the emitter could not
// resolve (a null cell) must land on the LEADER's point - never on an unresolved
// coordinate, and never on the follower offset with a made-up cell.
func TestTownMoveFallsBackWhenASlotHasNoCell(t *testing.T) {
	// The REAL point and offsets, with only the followers' cells blanked: the numbers
	// stay the table's, and the case under test is the missing ground.
	point := map15Points(t)[0]
	if point.Cell == nil {
		t.Fatalf("map 15 point 1 has no cell")
	}
	leader := [3]int32{point.X, point.Y, *point.Cell}
	formation := make([]FormationSlot, len(point.Formation))
	copy(formation, point.Formation)
	for i := 1; i < len(formation); i++ {
		formation[i].Cell = nil
	}
	point.Formation = formation

	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 15,
		Points: []TownPoint{point}}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})
	h.joinZone(a.sess)
	a.conn.writes = nil

	out, err := h.townMove(a.sess, townMoveFrame(1))
	if err != nil {
		t.Fatalf("townMove: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("sent %d packet(s), want 3 (one stop per character)", len(out))
	}
	for i := 0; i < 3; i++ {
		// Packet k holds slot len-1-k: the leader's stop is last.
		k := len(out) - 1 - i
		if got := decodeJump(t, out[k]).pos; got != leader {
			t.Fatalf("character %d with a null-cell slot went to %v, want the leader's %v",
				i, got, leader)
		}
		if got := a.sess.Team[i].Position; got != leader {
			t.Fatalf("stored position of character %d = %v, want the leader's %v",
				i, got, leader)
		}
	}
}

// TestTownMoveWithoutFormationUsesThePoint: a version-1 file carries no formation, and
// that must behave exactly as it did - everybody on the point, no failure. The real
// point stands in for such a file, with its formation dropped.
func TestTownMoveWithoutFormationUsesThePoint(t *testing.T) {
	point := map15Points(t)[0]
	if point.Cell == nil {
		t.Fatalf("map 15 point 1 has no cell")
	}
	point.Formation = nil
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 15,
		Points: []TownPoint{point}}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})
	h.joinZone(a.sess)
	a.conn.writes = nil

	out, err := h.townMove(a.sess, townMoveFrame(1))
	if err != nil {
		t.Fatalf("townMove: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("sent %d packet(s), want 3 (one stop per character)", len(out))
	}
	want := [3]int32{point.X, point.Y, *point.Cell}
	for k := range out {
		if got := decodeJump(t, out[k]).pos; got != want {
			t.Fatalf("packet %d with no formation went to %v, want the point %v",
				k, got, want)
		}
	}
}

// TestTownMoveExtraMembersUseThePoint: more characters than the file has slots - the
// extras stand on the leader's point rather than on a slot that does not exist. The real
// formation is cut down to two slots, so the offsets are still the table's.
func TestTownMoveExtraMembersUseThePoint(t *testing.T) {
	point := map15Points(t)[0]
	if point.Cell == nil {
		t.Fatalf("map 15 point 1 has no cell")
	}
	if len(point.Formation) < 2 {
		t.Fatalf("map 15 point 1 carries %d formation slot(s), want at least 2",
			len(point.Formation))
	}
	point.Formation = point.Formation[:2]
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 15,
		Points: []TownPoint{point}}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}})
	h.joinZone(a.sess)
	a.conn.writes = nil

	out, err := h.townMove(a.sess, townMoveFrame(1))
	if err != nil {
		t.Fatalf("townMove: %v", err)
	}
	leader := [3]int32{point.X, point.Y, *point.Cell}
	// The third character is slot 2, so it is written FIRST; the leader is written last.
	if got := decodeJump(t, out[0]).pos; got != leader {
		t.Fatalf("the third character went to %v, want the leader's point", got)
	}
	if got := decodeJump(t, out[2]).handle; got != a.sess.Team[0].Handle {
		t.Fatalf("the last packet names handle %#x, want the leader's %#x",
			got, a.sess.Team[0].Handle)
	}
}

// TestTownMoveRefusesAnUnknownButton: the panel has four buttons. A fifth (or a
// zero, or a negative) moves nobody and sends nothing - not the leader and not a
// follower - and every stored position stays where it was.
func TestTownMoveRefusesAnUnknownButton(t *testing.T) {
	before := [][3]int32{{11, 12, 13}, {21, 22, 23}, {31, 32, 33}}
	for _, button := range []int32{0, 5, -1} {
		h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 15,
			Points: map15Points(t)}
		a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033}, before)
		b := newPlayer(0x40000041, "B", [3]int32{1, 1, 1})
		h.joinZone(a.sess)
		h.joinZone(b.sess)
		a.conn.writes, b.conn.writes = nil, nil

		out, err := h.townMove(a.sess, townMoveFrame(button))
		if err != nil {
			t.Fatalf("button %d: townMove: %v", button, err)
		}
		if len(out) != 0 {
			t.Fatalf("button %d emitted %d packet(s), want none", button, len(out))
		}
		if got := a.sess.Pos; got != before[0] {
			t.Fatalf("button %d moved the session to %v", button, got)
		}
		for i := range a.sess.Team {
			if got := a.sess.Team[i].Position; got != before[i] {
				t.Fatalf("button %d moved character %d to %v", button, i, got)
			}
		}
		if got := len(b.conn.sent()); got != 0 {
			t.Fatalf("button %d told the other player about %d packet(s)", button, got)
		}
	}
}

// TestTownMoveRefusesAFieldMap: map 208 (fld_coim) has no town points in the real
// file, and the panel has nowhere to go on it. The refusal must not fall back to
// another map's points.
func TestTownMoveRefusesAFieldMap(t *testing.T) {
	path := filepath.Join("..", "..", "data", "zone_points.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no spawn table at %s (run: cd src && python emit_zone_points.py)", path)
	}
	points, err := LoadZonePoints(path)
	if err != nil {
		t.Fatalf("LoadZonePoints: %v", err)
	}
	if got := points.TownPoints(208); len(got) != 0 {
		t.Fatalf("map 208 carries %d town point(s), expected a field map with none", len(got))
	}

	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 208, Points: points.TownPoints(208)}
	a := newTeam([]uint32{0x40000031, 0x40000032},
		[][3]int32{{-17212, -421, 31}, {-17100, -400, 31}})
	h.joinZone(a.sess)
	a.conn.writes = nil

	out, err := h.townMove(a.sess, townMoveFrame(1))
	if err != nil {
		t.Fatalf("townMove: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a field map emitted %d packet(s), want none", len(out))
	}
	if got := a.sess.Pos; got != [3]int32{-17212, -421, 31} {
		t.Fatalf("a field map moved the session to %v", got)
	}
	if got := a.sess.Team[1].Position; got != [3]int32{-17100, -400, 31} {
		t.Fatalf("a field map moved the follower to %v", got)
	}
}

// TestTownMoveRefusesANullCell: a null cell is the emitter saying "the path engine
// found no ground here". Sending the height instead is the crash, so the button is
// refused and nobody moves.
func TestTownMoveRefusesANullCell(t *testing.T) {
	point := map15Points(t)[0]
	point.Formation = nil
	point.Cell = nil

	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 15,
		Points: []TownPoint{point}}
	a := newTeam([]uint32{0x40000031, 0x40000032}, [][3]int32{{1, 2, 3}, {4, 5, 6}})
	h.joinZone(a.sess)
	a.conn.writes = nil

	out, err := h.townMove(a.sess, townMoveFrame(1))
	if err != nil {
		t.Fatalf("townMove: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a point with no cell emitted %d packet(s), want none", len(out))
	}
	if got := a.sess.Pos; got != [3]int32{1, 2, 3} {
		t.Fatalf("a point with no cell moved the session to %v", got)
	}
	if got := a.sess.Team[1].Position; got != [3]int32{4, 5, 6} {
		t.Fatalf("a point with no cell moved the follower to %v", got)
	}

	// A HEIGHT in the cell slot is refused by the same guard ParseSpawn uses.
	point.Cell = int32p(-1088)
	h.Points = []TownPoint{point}
	if out, _ := h.townMove(a.sess, townMoveFrame(1)); len(out) != 0 {
		t.Fatal("a height in the cell slot was sent")
	}
}

// TestParseTownMove pins the offset: the button is an int32 at +10, and a short
// packet is a malformed client rather than a panic.
func TestParseTownMove(t *testing.T) {
	button, ok := ParseTownMove(pack(CmdCZReqTownMove, appendU32(nil, 3)...))
	if !ok || button != 3 {
		t.Fatalf("ParseTownMove = %d %v, want 3 true", button, ok)
	}
	if _, ok := ParseTownMove(pack(CmdCZReqTownMove, 1, 2)); ok {
		t.Fatal("ParseTownMove accepted a packet with no room for the button")
	}
}

// TestTownMoveRepliesThroughTheDispatch: cmd 3277 is routed, and a short frame is
// refused through Reply (the path the socket actually takes) with no packets.
func TestTownMoveRepliesThroughTheDispatch(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 15, Points: map15Points(t)}
	a := newTeam([]uint32{0x40000031}, [][3]int32{{1, 2, 3}})
	h.joinZone(a.sess)
	a.conn.writes = nil

	pt := pack(CmdCZReqTownMove, 1) // 11 bytes: too short for the int32
	out, err := h.Reply(nil, a.sess, protocol.Frame{Cmd: CmdCZReqTownMove, Size: len(pt), Data: pt})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a short REQ_TOWN_MOVE emitted %d packet(s)", len(out))
	}
}
