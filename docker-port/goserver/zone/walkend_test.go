package zone

import (
	"encoding/binary"
	"io"
	"log"
	"testing"
	"time"

	"geserver/protocol"
)

// Ending the walk the PLAYER drove, and the spawn value the live server uses for a
// standing character. Both were measured on 2026-10-28 with two real clients in map
// 10: the other player T-posed while standing and ran in place after stopping.
//
// THE CLOCK IN THESE TESTS IS REAL TIME. A walk is simulated at the character's own
// speed (MoveBaseSpeed, 469 units/second - see speed.go), so a 300-unit walk is over
// in ~640 ms and the tests wait it out instead of reaching into the session's
// segment. That keeps them honest: they measure the rule, not a field.

// walkOverWait is how long a test waits for a 300-unit walk to be over under the
// arrival rule: 300 / 469 x 1.4 + 250 ms is 1146 ms, so 1250 ms is past it. It is a
// SEPARATE number from the rule on purpose - a test that reuses the code's own margin
// proves only that the code agrees with itself.
const walkOverWait = 1250 * time.Millisecond

// moveTo builds the CZ_MOVE_PC the client sends for one character.
func moveTo(charIndex byte, pos [3]int32) protocol.Frame {
	data := make([]byte, 23)
	binary.LittleEndian.PutUint16(data, CmdCZMovePC)
	data[10] = charIndex
	binary.LittleEndian.PutUint32(data[11:], uint32(pos[0]))
	binary.LittleEndian.PutUint32(data[15:], uint32(pos[1]))
	binary.LittleEndian.PutUint32(data[19:], uint32(pos[2]))
	return protocol.Frame{Cmd: CmdCZMovePC, Size: len(data), Data: data}
}

// stopsOf is every ZC_STOP_PC (3549) in what a connection was written, in order.
func stopsOf(packets [][]byte) [][]byte {
	var out [][]byte
	for _, p := range packets {
		if len(p) >= 2 && cmdOf(p) == CmdZCStopPC {
			out = append(out, p)
		}
	}
	return out
}

// walkPair is one walking player (A, team of one) and one observer (B), already in
// the same zone, with both connections emptied.
func walkPair(t *testing.T) (*Handler, *player, *player) {
	t.Helper()
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newPlayer(0x40000031, "A", [3]int32{1000, 1000, 1})
	b := newPlayer(0x40000041, "B", [3]int32{2000, 2000, 1})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil
	return h, a, b
}

// TestReportWalkEndSendsOneStopToOthers: a client move stream that ENDS produces
// exactly one ZC_STOP_PC to the other session, none to the mover, and exactly one
// even when the zone ticks again afterwards.
//
// This is the defect the owner reported: nothing ever told the other client that the
// walk was over, so its actor ran on the spot for ever.
func TestReportWalkEndSendsOneStopToOthers(t *testing.T) {
	h, a, b := walkPair(t)
	dest := [3]int32{1000, 1300, 1} // 300 units away: 640 ms at 469 u/s, 896 ms
	// under the arrival rule plus its 250 ms slack (move.go, StopSafety)

	if _, err := h.movePC(a.sess, moveTo(0, dest)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	b.conn.writes = nil // the move echo only
	a.conn.writes = nil

	// Still walking: the tick must not stop the actor.
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 0 {
		t.Fatalf("a walk that is still running sent %d stop(s), want 0", got)
	}

	// Past the arrival rule (640 ms x 1.4 + 250 ms = 1146 ms), so the stop is due.
	time.Sleep(walkOverWait)

	h.chaseTick(a.sess)
	sent := stopsOf(b.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("the finished walk sent %d stop(s) to the other player, want exactly 1",
			len(sent))
	}
	// A second tick owes nothing: one stop per stop.
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 1 {
		t.Fatalf("a second tick sent %d stop(s), want the same 1", got)
	}
	// The mover is never told to stop: its own client drove the walk.
	if got := len(stopsOf(a.conn.sent())); got != 0 {
		t.Fatalf("the mover got %d stop(s), want 0", got)
	}
}

// TestReportWalkStopIsTwentyTwoBytesWithStoppedPos pins the packet the walk produces:
// 22 bytes carrying the position the character actually stopped at.
//
// There is NO length word in 3549 - bytes 2..3 are the low half of the handle, which
// is what makes a "length word" check here a trap: the packet is sized by its command
// (data\cmd_table.txt: 3549 22) and the zone sends it whole.
func TestReportWalkStopIsTwentyTwoBytesWithStoppedPos(t *testing.T) {
	h, a, b := walkPair(t)
	dest := [3]int32{1000, 1300, 1}
	handle := a.sess.Team[0].Handle

	if _, err := h.movePC(a.sess, moveTo(0, dest)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	b.conn.writes = nil
	// Past the arrival rule (640 ms x 1.4 + 250 ms = 1146 ms), so the stop is due.
	time.Sleep(walkOverWait)
	h.chaseTick(a.sess)

	sent := stopsOf(b.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("got %d stop(s), want 1", len(sent))
	}
	p := sent[0]
	if len(p) != 22 {
		t.Fatalf("stop is %d bytes, want 22", len(p))
	}
	if got := binary.LittleEndian.Uint16(p[2:]); got != uint16(handle) {
		t.Fatalf("bytes 2..3 = %#x, want the low half of handle %#x - there is no "+
			"length word in 3549", got, uint16(handle))
	}
	if got := binary.LittleEndian.Uint32(p[2:]); got != handle {
		t.Fatalf("handle = %#x, want %#x", got, handle)
	}
	if tick := binary.LittleEndian.Uint32(p[6:]); tick == 0 {
		t.Fatal("tick is 0: the client uses it to order stops of one team")
	}
	for i, want := range dest {
		off := 10 + i*4
		if got := int32(binary.LittleEndian.Uint32(p[off:])); got != want {
			t.Fatalf("pos[%d] = %d, want %d (the point the character stopped at)",
				i, got, want)
		}
	}
}

// TestReportWalkStillRunningSendsNoStop: a stream that CONTINUES produces none. The
// second report re-aims the character farther away, and neither the old destination
// nor the new one is reached inside the test's wait.
func TestReportWalkStillRunningSendsNoStop(t *testing.T) {
	h, a, b := walkPair(t)

	// 4000 units: over 8 seconds of walking, so nothing here can arrive.
	if _, err := h.movePC(a.sess, moveTo(0, [3]int32{1000, 5000, 1})); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 0 {
		t.Fatalf("the first report sent %d stop(s), want 0", got)
	}

	// The player keeps walking: another report, farther out.
	//
	// NO GUARD STAMP IS NEEDED: the move guard (zone\move_guard.go) is an alarm, not a
	// clamp, so the click is honoured as sent whether it is 4178 units away or 200. The
	// point of this test is the STOP rule, and it is unchanged: the segment is 600
	// units, 2.04 s of walking under the arrival rule, so nothing is stopped inside the
	// 1250 ms wait.
	if _, err := h.movePC(a.sess, moveTo(0, [3]int32{1000, 5600, 1})); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	// Past the arrival rule (640 ms x 1.4 + 250 ms = 1146 ms), so the stop is due.
	time.Sleep(walkOverWait)
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 0 {
		t.Fatalf("a walk that is still running sent %d stop(s), want 0", got)
	}
}

// TestTeamWalkStopsEveryCharacter: a team of 3 walks, and EVERY character is stopped
// on the other screen - one stop per handle, at its own position, and none repeated.
//
// This is the case the owner's own test walks into: all three members walk, so all
// three actors on the other client animate, and stopping only the leader leaves two
// characters running in place.
func TestTeamWalkStopsEveryCharacter(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1000, 1000, 1}, {1200, 1000, 1}, {1400, 1000, 1}})
	b := newPlayer(0x40000041, "B", [3]int32{2000, 2000, 1})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	b.conn.writes = nil

	dests := [][3]int32{{1000, 1300, 1}, {1200, 1300, 1}, {1400, 1300, 1}}
	for i, d := range dests {
		if _, err := h.movePC(a.sess, moveTo(byte(i), d)); err != nil {
			t.Fatalf("movePC char %d: %v", i, err)
		}
	}
	b.conn.writes = nil
	// Past the arrival rule (640 ms x 1.4 + 250 ms = 1146 ms), so the stop is due.
	time.Sleep(walkOverWait)
	h.chaseTick(a.sess)

	sent := stopsOf(b.conn.sent())
	if len(sent) != 3 {
		t.Fatalf("the team's walk sent %d stop(s), want one per character (3)", len(sent))
	}
	seen := map[uint32][3]int32{}
	for _, p := range sent {
		handle := binary.LittleEndian.Uint32(p[2:])
		pos := [3]int32{
			int32(binary.LittleEndian.Uint32(p[10:])),
			int32(binary.LittleEndian.Uint32(p[14:])),
			int32(binary.LittleEndian.Uint32(p[18:])),
		}
		if _, dup := seen[handle]; dup {
			t.Fatalf("handle %#x was stopped twice", handle)
		}
		seen[handle] = pos
	}
	for i, c := range a.sess.Team {
		pos, ok := seen[c.Handle]
		if !ok {
			t.Fatalf("character %d (handle %#x) was never stopped", i, c.Handle)
		}
		if pos != dests[i] {
			t.Fatalf("character %d stopped at %v, want %v", i, pos, dests[i])
		}
	}
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 3 {
		t.Fatalf("a second tick sent %d stop(s), want the same 3", got)
	}
}

// TestDuplicateReportDoesNotRepeatTheStop: the client's last report names the place
// the character already stands, and that must not become a SECOND stop for the same
// stop - the other client would be told to stop twice for one arrival.
func TestDuplicateReportDoesNotRepeatTheStop(t *testing.T) {
	h, a, b := walkPair(t)
	dest := [3]int32{1000, 1300, 1}

	if _, err := h.movePC(a.sess, moveTo(0, dest)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	b.conn.writes = nil
	// Past the arrival rule (640 ms x 1.4 + 250 ms = 1146 ms), so the stop is due.
	time.Sleep(walkOverWait)
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 1 {
		t.Fatalf("the walk sent %d stop(s), want 1", got)
	}

	// The same destination again, which is what the client's last packet looks like.
	if _, err := h.movePC(a.sess, moveTo(0, dest)); err != nil {
		t.Fatalf("duplicate movePC: %v", err)
	}
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 1 {
		t.Fatalf("the duplicate report sent %d stop(s), want the same 1", got)
	}
}

// TestSpawnCarriesTheLivePostureAndStance pins the two spawn fields the 2026-10-28
// T-pose investigation turned on:
//
//   - +13 posture: the client's own 3520 macro hardcodes the pose
//     (geClientShared\include\GePacketUtil.h:153) and the live server sends 0 in
//     58 of 58 captured spawns. It is NOT the idle selector; we used to send 1, the
//     value the client's enum calls POSTURE_SITTING.
//   - +30 stance: the field that DOES pick the standing animation (PC.cpp:503 reads
//     ASTD out of the stance's row). The live server never sends 0 there, and a 0
//     leaves the client with no animation at all.
func TestSpawnCarriesTheLivePostureAndStance(t *testing.T) {
	c := Character{Handle: 0x40000031, Name: "A", Job: 72, Stance: 30260}
	pkt := BuildSpawnStandingPC(c.Handle, c, [3]int32{1000, 1000, 1}, DefaultPosture)

	if len(pkt) != SpawnStandingPCSize {
		t.Fatalf("spawn is %d bytes, want %d", len(pkt), SpawnStandingPCSize)
	}
	// This packet DOES have a length word, and it must stay the whole packet.
	if got := binary.LittleEndian.Uint16(pkt[2:]); got != uint16(len(pkt)) {
		t.Fatalf("length word = %d, want %d", got, len(pkt))
	}
	if got := binary.LittleEndian.Uint32(pkt[spPosture:]); got != 0 {
		t.Fatalf("posture at +13 = %d, want 0 (the live value; the client ignores "+
			"this field on 3520)", got)
	}
	if got := binary.LittleEndian.Uint32(pkt[spStance:]); got != c.Stance {
		t.Fatalf("stance at +30 = %d, want %d - the idle (ASTD) is read out of this "+
			"stance, so it must be the character's real stance and never 0", got, c.Stance)
	}
}

// ---- R63: the stop is never sent before the animation could have arrived ------

// TestStopWaitsForTheAnimationNotTheEstimator: a 300-unit walk at 469 u/s takes
// 640 ms of animation. At 1100 ms the walk is therefore NOT provably over - the
// arrival rule needs 640 ms x 1.4 + 250 ms = 1146 ms - and no stop may go out.
//
// The old code stopped this walk at 640 ms, and that is the owner's second symptom:
// the other client is told "he stopped at X" while its copy is still walking, so it
// snaps to X.
func TestStopWaitsForTheAnimationNotTheEstimator(t *testing.T) {
	h, a, b := walkPair(t)
	if _, err := h.movePC(a.sess, moveTo(0, [3]int32{1000, 1300, 1})); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	b.conn.writes = nil

	// 1100 ms: past the animation itself, NOT past the rule.
	time.Sleep(1100 * time.Millisecond)
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 0 {
		t.Fatalf("a stop went out %d ms into a walk the rule gives 1146 ms - the other "+
			"client is told to jump to a point the character has not reached", 1100)
	}

	time.Sleep(walkOverWait - 1100*time.Millisecond)
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 1 {
		t.Fatalf("after the rule the walk sent %d stop(s), want exactly 1", got)
	}
}

// TestChainedClickSendsNoStopWhereTheCharacterNeverWas: the player clicks a second
// destination mid-walk. The first segment is REPLACED, and a replaced segment must
// never produce a stop - the position it would name is a destination the character
// provably never stood at.
//
// On the old code the first segment's clock fired 640 ms after the first click, while
// the character was still a third of the way along it, and the other client was yanked
// to the first destination. That is the "sometimes it jumps" report.
func TestChainedClickSendsNoStopWhereTheCharacterNeverWas(t *testing.T) {
	h, a, b := walkPair(t)
	first := [3]int32{1000, 1300, 1}
	second := [3]int32{1000, 1700, 1}

	if _, err := h.movePC(a.sess, moveTo(0, first)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	// The estimator trap in one line: the measured "speed" is how fast the player
	// re-clicks destinations, not how fast the body walks. At 800 u/s the OLD clock
	// called a 300-unit walk over after 375 ms, while the actor needs 640 ms.
	a.sess.MoveSpeed, a.sess.moveSamples = 800, 4

	time.Sleep(400 * time.Millisecond)
	h.chaseTick(a.sess)
	for _, p := range stopsOf(b.conn.sent()) {
		if x, y := int32(le32(p[10:])), int32(le32(p[14:])); x == first[0] && y == first[1] {
			t.Fatalf("the high estimate stopped the other player at (%d,%d) 400 ms into a "+
				"walk the body needs 640 ms to finish - that is the jump", x, y)
		}
	}

	// The player clicks a second destination mid-walk: the first segment is REPLACED.
	//
	// NO GUARD STAMP IS NEEDED (zone\move_guard.go): the guard is an alarm, not a clamp,
	// so a 512-unit re-path 400 ms later is honoured as sent. Every assertion below is
	// unchanged.
	if _, err := h.movePC(a.sess, moveTo(0, second)); err != nil {
		t.Fatalf("chained movePC: %v", err)
	}
	b.conn.writes = nil

	time.Sleep(1900 * time.Millisecond) // past the surviving segment's own rule
	h.chaseTick(a.sess)
	sent := stopsOf(b.conn.sent())
	if len(sent) != 1 {
		t.Fatalf("%d stop(s) after the chained walk, want exactly 1", len(sent))
	}
	if x, y := int32(le32(sent[0][10:])), int32(le32(sent[0][14:])); x != second[0] || y != second[1] {
		t.Fatalf("stop = (%d,%d), want the second destination (%d,%d)", x, y, second[0], second[1])
	}
}

// TestTeamWalkAnnouncesAndStopsEachCharacter: a team of three walks, and on the other
// screen each character is (a) moved by its own ZC_MOVE_PC and (b) stopped by its own
// ZC_STOP_PC - one packet each, per handle, in that order.
func TestTeamWalkAnnouncesAndStopsEachCharacter(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newTeam([]uint32{0x40000051, 0x40000052, 0x40000053},
		[][3]int32{{1000, 1000, 1}, {1200, 1000, 1}, {1400, 1000, 1}})
	b := newPlayer(0x40000061, "B", [3]int32{2000, 2000, 1})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	b.conn.writes = nil

	dests := [][3]int32{{1000, 1300, 1}, {1200, 1300, 1}, {1400, 1300, 1}}
	for i, d := range dests {
		if _, err := h.movePC(a.sess, moveTo(byte(i), d)); err != nil {
			t.Fatalf("movePC char %d: %v", i, err)
		}
	}
	moves := 0
	for _, p := range b.conn.sent() {
		if cmdOf(p) == CmdZCMovePC {
			moves++
		}
	}
	if moves != 3 {
		t.Fatalf("%d move(s) announced for a team of 3, want one per character", moves)
	}

	// A high measured speed must not shorten any of the three walks: at 800 u/s the
	// OLD clock called each 300-unit walk over after 375 ms and stopped all three
	// actors a third of the way along them.
	a.sess.MoveSpeed, a.sess.moveSamples = 800, 4
	time.Sleep(500 * time.Millisecond)
	h.chaseTick(a.sess)
	if got := len(stopsOf(b.conn.sent())); got != 0 {
		t.Fatalf("%d stop(s) 500 ms into a walk the body needs 640 ms to walk", got)
	}

	time.Sleep(walkOverWait)
	h.chaseTick(a.sess)

	seen := map[uint32]int{}
	for _, p := range stopsOf(b.conn.sent()) {
		seen[binary.LittleEndian.Uint32(p[2:])]++
	}
	for i, c := range a.sess.Team {
		if seen[c.Handle] != 1 {
			t.Fatalf("character %d (handle %#x) got %d stop(s), want exactly 1",
				i, c.Handle, seen[c.Handle])
		}
	}
}
