package zone

import (
	"encoding/binary"
	"io"
	"log"
	"testing"
	"time"
)

// THE STANCE PUSH (ZC_CHANGE_STANCE 3554) AND THE HANDLE IT NAMES.
//
// Why the push has to exist at all: the client's spawn handler (FUN_004f3fbb) applies
// the stance at +30 with the apply flag 0, which STORES it and starts NOTHING; 3554
// applies the same stance with flag 1, which is what starts the stance's idle
// (CPC::SetStance, PC.cpp:81-92: `if (updateMotion && !IsMoveAnimPlaying())
// PlayASTDAnim();`). So an actor that never gets a 3554 stands in whatever it played
// when it was built.
//
// And why the push can also come AFTER A STOP: the same guard refuses a stance change
// while a move animation plays, so a character whose 3554 never went out can only be
// given one at the moment the move ends - the stop.
//
// The handle test is the other half of "the stop is ignored": the 3549 handler's only
// silent exit is a handle the world map does not hold (FUN_00612978 ->
// FUN_005f63ac). The move path looks a handle up the SAME way (FUN_004b88d8), so a
// handle that can walk can stop - and this file pins that the two packets carry the
// same four bytes.

// stanceTeamPair is a walking player A with ONE character that already stands in
// `stance` at join time, and an observer B. A's own connection is emptied; B's keeps
// the spawns it was told about A.
//
// B's spawns arrive as the RETURN value of joinZone (the caller writes them), so they
// are appended to B's connection here: the point is one ordered history per socket,
// which is what a test about packet order has to read.
func stanceTeamPair(t *testing.T, stance uint32) (*Handler, *player, *player, uint32) {
	t.Helper()
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	const handle = uint32(0x40000031)
	a := newPlayer(handle, "Fighter", [3]int32{1000, 1000, 1})
	a.sess.Team[0].Stance = stance
	b := newPlayer(0x40000041, "Observer", [3]int32{2000, 2000, 1})
	h.joinZone(a.sess)
	out := h.joinZone(b.sess)
	a.conn.writes = nil
	b.conn.writes = nil
	b.conn.writes = append(b.conn.writes, out...)
	return h, a, b, handle
}

// stancesOf is every ZC_CHANGE_STANCE in what a connection was written, in order.
func stancesOf(packets [][]byte) [][]byte {
	var out [][]byte
	for _, p := range packets {
		if len(p) >= 2 && cmdOf(p) == CmdZCChangeStance {
			out = append(out, p)
		}
	}
	return out
}

// TestJoinSendsExactlyOneStancePerCharacter: one 3554 per remote character - no
// duplicates, each naming that character's own handle and its chosen stance, and each
// AFTER the 3520 that introduced it.
//
// "After" is not cosmetic: the client keys its actors by handle, so a stance for an
// actor that does not exist yet is applied to nothing.
func TestJoinSendsExactlyOneStancePerCharacter(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	a := newTeam([]uint32{0x40000031, 0x40000032, 0x40000033},
		[][3]int32{{1000, 1000, 1}, {1200, 1000, 1}, {1400, 1000, 1}})
	b := newTeam([]uint32{0x40000041}, [][3]int32{{2000, 2000, 1}})

	h.joinZone(a.sess)
	out := h.joinZone(b.sess) // everything b is told about a

	spawnAt := map[uint32]int{}
	stanceCount := map[uint32]int{}
	for i, p := range out {
		if len(p) < 10 {
			continue
		}
		switch cmdOf(p) {
		case CmdZCSpawnStandingPC:
			// The spawn's handle is at +4 (after cmd and the length word).
			spawnAt[binary.LittleEndian.Uint32(p[4:])] = i
		case CmdZCChangeStance:
			// A 3554 has NO length word: bytes 2..5 are the whole handle.
			handle := binary.LittleEndian.Uint32(p[2:])
			stanceCount[handle]++
			if first, ok := spawnAt[handle]; !ok {
				t.Fatalf("a stance for handle %#x arrived BEFORE any spawn of it", handle)
			} else if first > i {
				t.Fatalf("handle %#x got its stance at %d, before its spawn at %d",
					handle, i, first)
			}
			var want uint32
			for _, c := range a.sess.Team {
				if c.Handle == handle {
					want = c.Stance
				}
			}
			if got := binary.LittleEndian.Uint32(p[6:]); got != want {
				t.Fatalf("stance for handle %#x carries %d, want the character's own %d",
					handle, got, want)
			}
		}
	}
	for _, c := range a.sess.Team {
		if n := stanceCount[c.Handle]; n != 1 {
			t.Fatalf("handle %#x got %d stance packet(s), want exactly 1", c.Handle, n)
		}
	}
}

// TestStopPushesTheStanceThatWasNeverSent: a character whose 3554 never went out gets
// ONE, and it goes out AFTER its stop - the moment the client will accept it.
//
// THE SETUP IS THE REAL BUG'S SHAPE: the character joined before its stance was known
// (a stance of 0 is not sent at all), so no 3554 ever went out for its handle.
func TestStopPushesTheStanceThatWasNeverSent(t *testing.T) {
	h := &Handler{Log: log.New(io.Discard, "", 0)}
	const handle = uint32(0x40000031)
	a := newPlayer(handle, "Fighter", [3]int32{1000, 1000, 1}) // Stance 0: nothing is sent
	b := newPlayer(0x40000041, "Observer", [3]int32{2000, 2000, 1})
	h.joinZone(a.sess)
	h.joinZone(b.sess)
	a.conn.writes, b.conn.writes = nil, nil

	// The stance becomes known after the join: 10210 EpeeGarde, the rapier guard.
	const stance = uint32(10210)
	a.sess.Team[0].Stance = stance
	if got := len(stancesOf(b.conn.sent())); got != 0 {
		t.Fatalf("the observer already got %d stance packet(s) before the walk", got)
	}

	dest := [3]int32{1000, 1300, 1} // 300 units: ~640 ms at 469 u/s
	if _, err := h.movePC(a.sess, moveTo(0, dest)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	time.Sleep(walkOverWait)
	h.chaseTick(a.sess)

	packets := b.conn.sent()
	stop, push := -1, -1
	for i, p := range packets {
		switch cmdOf(p) {
		case CmdZCStopPC:
			stop = i
		case CmdZCChangeStance:
			push = i
		}
	}
	if stop < 0 {
		t.Fatal("the finished walk sent no stop")
	}
	if push < 0 {
		t.Fatal("no ZC_CHANGE_STANCE was sent for a character that never had one - the " +
			"actor keeps whatever animation it was built with")
	}
	if push < stop {
		t.Fatalf("the stance went out at %d, BEFORE the stop at %d - the client refuses "+
			"a stance change while a move animation plays, so it would be dropped", push, stop)
	}
	p := packets[push]
	if len(p) != 10 {
		t.Fatalf("the pushed stance is %d bytes, want 10", len(p))
	}
	if got := binary.LittleEndian.Uint32(p[2:]); got != handle {
		t.Fatalf("the pushed stance names handle %#x, want %#x", got, handle)
	}
	if got := binary.LittleEndian.Uint32(p[6:]); got != stance {
		t.Fatalf("the pushed stance carries %d, want %d", got, stance)
	}

	// ONE push: a second stop for the same character owes nothing.
	h.broadcastStop(a.sess, 0, handle, dest, BuildStopPC(handle, dest))
	if got := len(stancesOf(b.conn.sent())); got != 1 {
		t.Fatalf("a second stop pushed the stance again (%d in total), want 1", got)
	}
}

// TestStopDoesNotRepeatAStanceItAlreadySent: a character that got its 3554 at join
// must NOT get a second one when it stops - the client would restart the idle for no
// reason, and the live server sends it once per join.
func TestStopDoesNotRepeatAStanceItAlreadySent(t *testing.T) {
	h, a, b, handle := stanceTeamPair(t, 10010)
	before := len(stancesOf(b.conn.sent()))
	if before != 1 {
		t.Fatalf("the join sent %d stance packet(s), want the spawn's own 1", before)
	}
	dest := [3]int32{1000, 1300, 1}
	if _, err := h.movePC(a.sess, moveTo(0, dest)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	time.Sleep(walkOverWait)
	h.chaseTick(a.sess)

	if got := len(stopsOf(b.conn.sent())); got != 1 {
		t.Fatalf("the walk ended with %d stop(s), want 1", got)
	}
	if got := len(stancesOf(b.conn.sent())); got != before {
		t.Fatalf("%d stance packet(s) after the stop, want the same %d - its stance "+
			"already went out with the join (handle %#x)", got, before, handle)
	}
}

// TestSpawnAndStopCarryTheSameHandle is the answer to "is the 3549 ignored because the
// handle is wrong?": the four bytes in the spawn and the four bytes in the stop are
// the same value, byte for byte, for the same character.
//
// It is an ASSERTION, not a fix - the handles already agreed. Its value is that it
// fails the moment the two paths start taking the handle from different places (the
// roster copy, a "+4 vs +2" mix-up, a truncated 16-bit read), which is exactly the
// class of bug that makes a stop look ignored.
func TestSpawnAndStopCarryTheSameHandle(t *testing.T) {
	h, a, b, handle := stanceTeamPair(t, 10010)
	dest := [3]int32{1000, 1300, 1}
	if _, err := h.movePC(a.sess, moveTo(0, dest)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	time.Sleep(walkOverWait)
	h.chaseTick(a.sess)

	var spawn, stop []byte
	for _, p := range b.conn.sent() {
		switch cmdOf(p) {
		case CmdZCSpawnStandingPC:
			if binary.LittleEndian.Uint32(p[4:]) == handle {
				spawn = p
			}
		case CmdZCStopPC:
			if binary.LittleEndian.Uint32(p[2:]) == handle {
				stop = p
			}
		}
	}
	if spawn == nil {
		t.Fatal("the observer never got the spawn")
	}
	if stop == nil {
		t.Fatal("the observer never got the stop")
	}
	if spawn[4] != stop[2] || spawn[5] != stop[3] ||
		spawn[6] != stop[4] || spawn[7] != stop[5] {
		t.Fatalf("the spawn names handle % x and the stop names % x - different bytes, "+
			"so one of them points at an actor the client does not have",
			spawn[4:8], stop[2:6])
	}
	if got := binary.LittleEndian.Uint32(stop[2:]); got != handle {
		t.Fatalf("the stop names %#x, want %#x", got, handle)
	}
	// The 16-bit trap: bytes 2..3 of a 3549 are the LOW HALF of the handle, not a
	// length. A reader who takes them as a length sees 0x0031 = 49 here.
	if lw := binary.LittleEndian.Uint16(stop[2:]); int(lw) == len(stop) {
		t.Logf("bytes 2..3 of this handle happen to equal the packet length (%d) - the "+
			"packet still has no length word", lw)
	}
}
