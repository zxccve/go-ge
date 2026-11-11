package zone

// THE SIGHT RING's own tests (zone\sight.go): the map's ViewRange, the strict "<",
// one leave per walk-out, THE CLIENT'S OWN RE-ENTER FRAME (3522 standing / 3524 moving)
// when an actor comes back, the client's frame sizes and byte offsets, and the promise
// that a session's OWN team is never left.
//
// What a unit test CANNOT prove is what the client draws. These pin the wire: which
// command, how many bytes, and which byte holds which field. The offsets come from
// docker-port\research\reenter-layout.md, measured from the 2020 client's decompiled
// handlers - not from a guess and not from the spawn frame shifted.
//
// THE ONE BEHAVIOUR THESE EXIST FOR (owner's report, two clients, 2026-11-06 21:50):
// "the character wont show up on the observer client until the other character moves
// once more". A 3520 for a handle the client already knows is ignored, so a RETURN must
// be the re-enter frame and must NEVER be the spawn burst.

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"math"
	"strings"
	"testing"
	"time"
)

// sightUseTable installs a map table for ONE test and puts the resolved-radius cache
// back afterwards, so two tests cannot see each other's radius.
//
// THE "once per map id" LOG FLAG IS PART OF THAT CACHE. sightRadius writes the
// missing-row line once per process (sightRadiusLogged, keyed by map id), which is
// right for a run and wrong for tests: a test that reads the line for map 99 first
// would silence the next test's own reading of it. So the record is saved and emptied
// per test, like the radius itself.
func sightUseTable(t *testing.T, rows map[int]SightRow) {
	t.Helper()
	sightTableMu.Lock()
	oldDone, oldData, oldErr := sightTableDone, sightTableData, sightTableErr
	sightTableData, sightTableDone, sightTableErr = &SightTable{rows: rows}, true, nil
	sightTableMu.Unlock()

	sightRadiusMu.Lock()
	sightRadiusDone = false
	oldLogged := sightRadiusLogged
	sightRadiusLogged = map[int]bool{}
	sightRadiusMu.Unlock()

	t.Cleanup(func() {
		sightTableMu.Lock()
		sightTableDone, sightTableData, sightTableErr = oldDone, oldData, oldErr
		sightTableMu.Unlock()
		sightRadiusMu.Lock()
		sightRadiusDone = false
		sightRadiusLogged = oldLogged
		sightRadiusMu.Unlock()
	})
}

// readFloat64At is the little-endian FLOAT64 at `off` - the mirror of the package's
// own putFloat64 (zone\entry.go).
func readFloat64At(b []byte, off int) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
}

// sightSession is one player in the zone: one character at `pos`, driving slot 0.
func sightSession(handle uint32, pos [3]int32) *Session {
	return &Session{
		Pos:  pos,
		Team: []Character{{Handle: handle, Name: "T", Level: 10, HP: 500, MaxHP: 800, Stance: 3}},
	}
}

// sightCmds is the command word of every packet, in order - which is what the ring's
// promises are about (one 3525, then nothing; one 3522; ...).
func sightCmds(pkts [][]byte) []uint16 {
	out := make([]uint16, 0, len(pkts))
	for _, p := range pkts {
		if len(p) < 2 {
			continue
		}
		out = append(out, binary.LittleEndian.Uint16(p))
	}
	return out
}

func countCmd(pkts [][]byte, cmd uint16) int {
	n := 0
	for _, p := range pkts {
		if len(p) >= 2 && binary.LittleEndian.Uint16(p) == cmd {
			n++
		}
	}
	return n
}

// TestSightRadiusIsTheMapsViewRangeStrictly is pass condition 1: the radius is the
// map's own ViewRange, and a body EXACTLY on it is outside.
func TestSightRadiusIsTheMapsViewRangeStrictly(t *testing.T) {
	cases := []struct {
		mapID   int
		radius  int32
		in      int32
		out     int32
		onEdge  int32
		mapName string
	}{
		{mapID: 10, radius: 4000, in: 3999, out: 4001, onEdge: 4000, mapName: "reb_basic"},
		{mapID: 1331, radius: 6000, in: 5999, out: 6001, onEdge: 6000, mapName: "fld_auch"},
	}
	for _, c := range cases {
		sightUseTable(t, map[int]SightRow{
			c.mapID: {ViewRange: c.radius, ViewMiddleRange: 2500},
		})
		h := &Handler{Log: log.New(io.Discard, "", 0), MapID: c.mapID, MapName: c.mapName}
		a := sightSession(101, [3]int32{0, 0, 0})
		b := sightSession(202, [3]int32{c.in, 0, 0})
		h.Players.Add(a)
		h.Players.Add(b)

		if got := sightCmds(h.sightTick(a)); len(got) == 0 {
			t.Fatalf("map %d: a character %d units away (radius %d) was NOT announced",
				c.mapID, c.in, c.radius)
		}

		// ...and the other side of the same radius.
		for _, d := range []int32{c.out, c.onEdge} {
			h2 := &Handler{Log: log.New(io.Discard, "", 0), MapID: c.mapID, MapName: c.mapName}
			a2 := sightSession(101, [3]int32{0, 0, 0})
			b2 := sightSession(202, [3]int32{d, 0, 0})
			h2.Players.Add(a2)
			h2.Players.Add(b2)
			if got := sightCmds(h2.sightTick(a2)); len(got) != 0 {
				t.Fatalf("map %d: a character %d units away (radius %d) was announced "+
					"with %d packet(s) %v - the comparison must be STRICTLY less",
					c.mapID, d, c.radius, len(got), got)
			}
		}
	}
}

// TestSightWalksOutOnceAndComesBackAsReEnter is pass condition 2c/2d: one 3525 reason 3,
// then silence, then THE CLIENT'S OWN RE-ENTER FRAME - one 3522 for a body that is
// standing - and the 3520 spawn burst appears NOWHERE in the return.
//
// WHY THE SPAWN IS FORBIDDEN HERE (owner, two clients, 2026-11-06 21:50): "the character
// wont show up on the observer client until the other character moves once more ... like
// the character is spawning from barrack/teleport/warping". The client ignores a 3520 for
// a handle it already knows and only re-attaches the actor on its next 3548. The 20:05
// attempt used a 3522 too, but with stance/hp/mhp/mask one byte early and no posture byte,
// so the client read our stance's low byte as a posture code and T-posed the actor
// (research doc section 4) - which is why the offsets are pinned in the test below.
func TestSightWalksOutOnceAndComesBackAsReEnter(t *testing.T) {
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 10}
	a := sightSession(101, [3]int32{0, 0, 0})
	b := sightSession(202, [3]int32{100, 0, 0})
	h.Players.Add(a)
	h.Players.Add(b)

	// A FIRST SIGHT IS STILL THE SPAWN BURST: 3520, 3572, 3554, in that order.
	first := h.sightTick(a)
	if got := sightCmds(first); len(got) != 3 ||
		got[0] != CmdZCSpawnStandingPC || got[1] != CmdZCSkillDisable || got[2] != CmdZCChangeStance {
		t.Fatalf("first sight sent %v, want [%d %d %d] (spawn, skill-disable, stance)",
			got, CmdZCSpawnStandingPC, CmdZCSkillDisable, CmdZCChangeStance)
	}

	// B walks 5,000 units away.
	h.Players.SetPos(b, 0, [3]int32{5000, 0, 0})
	out := h.sightTick(a)
	leaves := 0
	for _, p := range out {
		if len(p) == 8 && binary.LittleEndian.Uint16(p) == CmdZCLeavePC {
			leaves++
			if reason := binary.LittleEndian.Uint16(p[6:]); reason != LeaveCodeOutOfSight {
				t.Fatalf("the range leave carried reason %d, want %d (OBJ_OUTOFSIGHT)",
					reason, LeaveCodeOutOfSight)
			}
			if got := binary.LittleEndian.Uint32(p[2:]); got != 202 {
				t.Fatalf("the range leave names handle %d, want 202", got)
			}
		}
	}
	if leaves != 1 {
		t.Fatalf("walking out sent %d leave(s), want exactly 1 (%v)", leaves, sightCmds(out))
	}
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("the next tick after a leave sent %d more packet(s) %v, want none",
			len(got), sightCmds(got))
	}

	// B comes back, standing: the RE-ENTER form at the body (a spawn for a handle the client
	// still holds is ignored - the owner's 21:50 rule; a return re-adds it).
	h.Players.SetPos(b, 0, [3]int32{100, 0, 0})
	back := h.sightTick(a)
	if n := countCmd(back, CmdZCReEnterPC); n != 1 {
		t.Fatalf("coming back sent %d 3522 re-enter form(s) %v, want exactly one",
			n, sightCmds(back))
	}
	if n := countCmd(back, CmdZCSpawnStandingPC); n != 0 {
		t.Fatalf("coming back sent %d 3520 spawn(s) %v - a return must NOT re-send the spawn",
			n, sightCmds(back))
	}
	if n := countCmd(back, CmdZCReEnterMovingPC); n != 0 {
		t.Fatalf("a STANDING body came back as %d 3524 moving form(s) %v, want none",
			n, sightCmds(back))
	}
	if len(back) != 3 {
		t.Fatalf("the return sent %d packet(s) %v, want the re-enter form, its stance and "+
			"its 3572 (measured 2026-11-10)", len(back), sightCmds(back))
	}
	if n := countCmd(back, CmdZCChangeStance); n != 1 {
		t.Fatalf("the return sent %d change-stance packet(s) %v; the client builds the model "+
			"from it", n, sightCmds(back))
	}
	if n := countCmd(back, CmdZCSkillDisable); n != 1 {
		t.Fatalf("the return sent %d skill-disable packet(s) %v; the live server sends one "+
			"per re-enter", n, sightCmds(back))
	}
	if got := binary.LittleEndian.Uint32(back[2][2:]); got != 202 {
		t.Fatalf("the skill-disable names handle %d, want 202 (the body's own)", got)
	}
	if n := countCmd(back, CmdZCSkillDisable); n != 1 {
		t.Fatalf("a mid-walk return sent %d 3572 packet(s) %v, want exactly one",
			n, sightCmds(back))
	}
	if n := countCmd(back, CmdZCChangeStance); n != 1 {
		t.Fatalf("a mid-walk return sent %d 3554 packet(s) %v, want exactly one",
			n, sightCmds(back))
	}
	p := back[0]
	if len(p) != ReEnterPCSize {
		t.Fatalf("the return frame is %d bytes, want %d (the client's own table)",
			len(p), ReEnterPCSize)
	}
	if got := binary.LittleEndian.Uint32(p[rpHandle:]); got != 202 {
		t.Fatalf("the return names handle %d, want 202", got)
	}
	if got := binary.LittleEndian.Uint32(p[rpPos:]); got != 100 {
		t.Fatalf("the return places the body at x=%d, want 100 (where it stands)", int32(got))
	}

	// The handle is in the seen set again: the pass after it owes nothing.
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("the tick after the return sent %v more, want nothing", sightCmds(got))
	}
}

// TestSightReturnMidWalkIsTheStandingFormAtTheBody pins the 2026-11-10 behaviour: a character
// that comes back into sight - walking or not - is re-entered with the STANDING form (3522)
// AT ITS BODY. The moving form (3524) is not sent: its leg is the CLICK's destination (a
// 9,900-unit leg measured on map 1331) and its tick counts from the server's process start,
// so the client placed the actor at the destination. And a return must NOT re-send the spawn:
// the client ignores a 3520 for a handle it still holds (the owner's 21:50 rule), which is
// exactly why the returning actor stayed invisible when the spawn was what we sent.
func TestSightReturnMidWalkIsTheMovingFormAtTheBody(t *testing.T) {
	h, a, b := bodyRing(t, [3]int32{0, 0, 0}, [3]int32{100, 0, 0})

	if got := h.sightTick(a); countCmd(got, CmdZCSpawnStandingPC) != 1 {
		t.Fatalf("setup: B was not announced to A: %v", sightCmds(got))
	}

	at := time.Now()
	reportWalk(h, b, 0, [3]int32{100, 0, 0}, [3]int32{9100, 0, 0}, at, MoveBaseSpeed)
	b.moveAt = time.Now().Add(-9 * time.Second)
	if got := h.sightTick(a); countCmd(got, CmdZCLeavePC) != 1 {
		t.Fatalf("B's body never left the ring: %v", sightCmds(got))
	}

	// Coming back while WALKING: the live server uses 3524 for a moving body and 3522 only
	// for a standing one (capture ring1, 2026-11-10, 3x 3524 and 4x 3522 on the two sockets).
	start := [3]int32{4221, 0, 0}
	end := [3]int32{100, 0, 0}
	reportWalk(h, b, 0, start, end, time.Now(), MoveBaseSpeed)
	b.moveAt = time.Now().Add(-time.Second)

	back := h.sightTick(a)
	if n := countCmd(back, CmdZCReEnterMovingPC); n != 1 {
		t.Fatalf("a body that came back MID-WALK sent %d 3524 form(s) %v, want exactly one",
			n, sightCmds(back))
	}
	if n := countCmd(back, CmdZCReEnterPC); n != 0 {
		t.Fatalf("the mid-walk return sent %d 3522 standing form(s) %v - a walking body "+
			"must be re-entered MOVING or the client holds it static until its stop",
			n, sightCmds(back))
	}
	if n := countCmd(back, CmdZCSpawnStandingPC); n != 0 {
		t.Fatalf("the mid-walk return sent %d 3520 spawn(s) %v - a return never re-sends the spawn",
			n, sightCmds(back))
	}
	p := back[0]
	if len(p) != ReEnterMovingPCSize {
		t.Fatalf("the return frame is %d bytes, want %d", len(p), ReEnterMovingPCSize)
	}
	if got := binary.LittleEndian.Uint32(p[rmHandle:]); got != 202 {
		t.Fatalf("the return names handle %d, want 202", got)
	}
	want := int32(float64(start[0]) - float64(MoveBaseSpeed))
	if got := int32(binary.LittleEndian.Uint32(p[rmStart:])); got < want-8 || got > want+8 {
		t.Fatalf("the leg starts at the BODY x=%d, want ~%d (one second in); the destination "+
			"is %d", got, want, end[0])
	}
	if got := int32(binary.LittleEndian.Uint32(p[rmEnd:])); got != end[0] {
		t.Fatalf("the leg ends at x=%d, want %d", got, end[0])
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(p[rmSpeed:])); got != MoveBaseSpeed {
		t.Fatalf("the leg's speed = %g, want %g", got, MoveBaseSpeed)
	}
	if got := binary.LittleEndian.Uint32(p[rmTick:]); got == 0 {
		t.Fatalf("the leg's tick at +15 is 0 - the client cannot place the body on the leg")
	}
}
func TestBuildReEnterPCFieldOffsets(t *testing.T) {
	c := Character{Handle: 0x11223344, Job: 0x2233, Stance: 0x44556677, HP: 1234.5, MaxHP: 9876.25}
	pos := [3]int32{111, 222, 333}
	b := BuildReEnterPC(c, pos)

	if len(b) != 305 {
		t.Fatalf("BuildReEnterPC built %d bytes, want 305", len(b))
	}
	if got := binary.LittleEndian.Uint16(b[0:]); got != 3522 {
		t.Errorf("+0 command = %d, want 3522", got)
	}
	if got := binary.LittleEndian.Uint32(b[2:]); got != 0x11223344 {
		t.Errorf("+2 handle = 0x%08x, want 0x11223344", got)
	}
	if got := b[6]; got != 0 {
		t.Errorf("+6 relation = %d, want 0 (neutral)", got)
	}
	if got := binary.LittleEndian.Uint16(b[7:]); got != 0x2233 {
		t.Errorf("+7 class = 0x%04x, want 0x2233", got)
	}
	if got := binary.LittleEndian.Uint16(b[9:]); got != 0 {
		t.Errorf("+9 weapon = %d, want 0 (the measured live value)", got)
	}
	if got := binary.LittleEndian.Uint32(b[11:]); got != 0 {
		t.Errorf("+11 animation dword = %d, want 0 (the measured live value)", got)
	}
	if got := binary.LittleEndian.Uint32(b[15:]); got != 111 {
		t.Errorf("+15 x = %d, want 111", got)
	}
	if got := binary.LittleEndian.Uint32(b[19:]); got != 222 {
		t.Errorf("+19 y = %d, want 222", got)
	}
	if got := binary.LittleEndian.Uint32(b[23:]); got != 333 {
		t.Errorf("+23 cell = %d, want 333", got)
	}
	if got := b[27]; got != 0 {
		t.Errorf("+27 direction = %d, want 0", got)
	}
	// THE BYTE THAT WAS MISSING. It must be 0 (standing) and it must be AT 28.
	if got := b[28]; got != byte(DefaultPosture) {
		t.Errorf("+28 posture = %d, want %d (standing) - a non-zero posture code makes "+
			"the client FIRE AN ACTION (the T-pose)", got, DefaultPosture)
	}
	if got := binary.LittleEndian.Uint32(b[29:]); got != 0x44556677 {
		t.Errorf("+29 stance = 0x%08x, want 0x44556677 (one byte later than the 20:05 "+
			"frame had it)", got)
	}
	if got := readFloat64At(b, 33); got != 1234.5 {
		t.Errorf("+33 hp = %g, want 1234.5", got)
	}
	if got := readFloat64At(b, 41); got != 9876.25 {
		t.Errorf("+41 max hp = %g, want 9876.25", got)
	}
	for i := 49; i < 49+256; i++ {
		if b[i] != 0 {
			t.Fatalf("+%d is inside the 256-byte buff mask and is %d, want 0", i, b[i])
		}
	}
	if got := b[304]; got != 0 {
		t.Errorf("+304 the last byte of the mask = %d, want 0", got)
	}
}

// TestBuildReEnterMovingPCFieldOffsets is pass condition 2b: 3524 is exactly 323 bytes and
// every field sits where the client reads it - start at +19/+23/+27, end at +31/+35/+39,
// tick at +43, stance at +47, hp at +51, max hp at +59, mask at +67.
//
// THERE IS NO POSTURE AND NO DIRECTION BYTE IN THIS FRAME (research doc section 3: line
// 180979 forces the posture to 0 without reading the packet, and nothing loads the
// direction slot). Writing one would shift every field after it - the same class of
// mistake the 20:05 3522 made.
func TestBuildReEnterMovingPCFieldOffsets(t *testing.T) {
	c := Character{Handle: 0x11223344, Job: 0x2233, Stance: 0x44556677, HP: 1234.5, MaxHP: 9876.25}
	start := [3]int32{11, 22, 33}
	end := [3]int32{44, 55, 66}
	b := BuildReEnterMovingPC(c, start, end, 777, 469)

	if len(b) != 323 {
		t.Fatalf("BuildReEnterMovingPC built %d bytes, want 323", len(b))
	}
	if got := binary.LittleEndian.Uint16(b[0:]); got != 3524 {
		t.Errorf("+0 command = %d, want 3524", got)
	}
	if got := binary.LittleEndian.Uint32(b[2:]); got != 0x11223344 {
		t.Errorf("+2 handle = 0x%08x, want 0x11223344", got)
	}
	if got := b[6]; got != 0 {
		t.Errorf("+6 relation = %d, want 0 (neutral)", got)
	}
	if got := binary.LittleEndian.Uint16(b[7:]); got != 0x2233 {
		t.Errorf("+7 class = 0x%04x, want 0x2233", got)
	}
	if got := binary.LittleEndian.Uint16(b[9:]); got != 0 {
		t.Errorf("+9 weapon = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint32(b[11:]); got != 0 {
		t.Errorf("+11 animation dword = %d, want 0", got)
	}
	if got := binary.LittleEndian.Uint32(b[rmTick:]); got != 777 {
		t.Errorf("+15 tick = %d, want 777 (the live server puts the TICK here)", got)
	}
	if got := binary.LittleEndian.Uint32(b[19:]); got != 11 {
		t.Errorf("+19 start.x = %d, want 11", got)
	}
	if got := binary.LittleEndian.Uint32(b[23:]); got != 22 {
		t.Errorf("+23 start.y = %d, want 22", got)
	}
	if got := binary.LittleEndian.Uint32(b[27:]); got != 33 {
		t.Errorf("+27 start.cell = %d, want 33", got)
	}
	if got := binary.LittleEndian.Uint32(b[31:]); got != 44 {
		t.Errorf("+31 end.x = %d, want 44", got)
	}
	if got := binary.LittleEndian.Uint32(b[35:]); got != 55 {
		t.Errorf("+35 end.y = %d, want 55", got)
	}
	if got := binary.LittleEndian.Uint32(b[39:]); got != 66 {
		t.Errorf("+39 end.cell = %d, want 66", got)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(b[rmSpeed:])); got != 469 {
		t.Errorf("+43 speed = %g, want 469 (a FLOAT32 - the live server's own frame)", got)
	}
	if got := binary.LittleEndian.Uint32(b[47:]); got != 0x44556677 {
		t.Errorf("+47 stance = 0x%08x, want 0x44556677", got)
	}
	if got := readFloat64At(b, 51); got != 1234.5 {
		t.Errorf("+51 hp = %g, want 1234.5", got)
	}
	if got := readFloat64At(b, 59); got != 9876.25 {
		t.Errorf("+59 max hp = %g, want 9876.25", got)
	}
	for i := 67; i < 67+256; i++ {
		if b[i] != 0 {
			t.Fatalf("+%d is inside the 256-byte buff mask and is %d, want 0", i, b[i])
		}
	}
	if got := b[322]; got != 0 {
		t.Errorf("+322 the last byte of the mask = %d, want 0", got)
	}
}

// TestSightMonsterRing is pass condition 2d for monsters: a monster inside the circle is in
// the entry burst, one outside is not, and a RETURN is the client's own 3528 re-enter form
// - never a second 3527, for the same reason a return never re-sends a 3520. The leave is
// still 3531 with reason 3.
//
// MONSTERS DO NOT WALK IN THIS SERVER YET, so there is no moving monster re-enter and none
// is invented (the brief's "do not send 3530").
func TestSightMonsterRing(t *testing.T) {
	sightUseTable(t, map[int]SightRow{208: {ViewRange: 6000, ViewMiddleRange: 4000}})

	near := Monster{Handle: MonsterHandle(9001), X: 5999, Y: 0, ClassID: 11600}
	far := Monster{Handle: MonsterHandle(9002), X: 6001, Y: 0, ClassID: 11600}

	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 208}
	h.spawnedMonster = []Monster{near, far}
	a := sightSession(101, [3]int32{0, 0, 0})

	burst := h.joinZone(a)
	if got := countCmd(burst, CmdZCEnterStandingMonster); got != 1 {
		t.Fatalf("the entry burst carried %d monster enter form(s), want 1 (only the "+
			"one inside the 6000-unit circle): %v", got, sightCmds(burst))
	}
	for _, p := range burst {
		if binary.LittleEndian.Uint16(p) == CmdZCEnterStandingMonster &&
			binary.LittleEndian.Uint32(p[2:]) != near.Handle {
			t.Fatalf("the entry burst announced handle %d, want %d (the near one)",
				binary.LittleEndian.Uint32(p[2:]), near.Handle)
		}
	}

	// The far one never entered, so walking to it is a FIRST SIGHT (3527)...
	a.Pos = [3]int32{12000, 0, 0}
	out := h.sightTick(a)
	if got := countCmd(out, CmdZCEnterStandingMonster); got != 1 {
		t.Fatalf("coming up to the far monster sent %d enter form(s), want 1: %v",
			got, sightCmds(out))
	}

	// ...and it must NOT be a second 3527 for the near one that is now out of range -
	// that is a 3531 with reason 3.
	leaves := 0
	for _, p := range out {
		if len(p) == 8 && binary.LittleEndian.Uint16(p) == CmdZCLeaveMonster {
			leaves++
			if reason := binary.LittleEndian.Uint16(p[6:]); reason != LeaveCodeOutOfSight {
				t.Fatalf("the monster range leave carried reason %d, want %d "+
					"(OBJ_OUTOFSIGHT)", reason, LeaveCodeOutOfSight)
			}
		}
	}
	if leaves != 1 {
		t.Fatalf("leaving the near monster sent %d leave(s), want 1: %v",
			leaves, sightCmds(out))
	}

	// Walk back: the near monster is a RETURN, and a return is the client's own 3528
	// re-enter form - never a second 3527 for a handle the client already has. The far
	// one, now out of its range, is the one leave this pass owes.
	a.Pos = [3]int32{0, 0, 0}
	back := h.sightTick(a)
	if n := countCmd(back, CmdZCReEnterStandingMonster); n != 1 {
		t.Fatalf("coming back to the near monster sent %d 3528 re-enter form(s) %v, want "+
			"exactly one", n, sightCmds(back))
	}
	if n := countCmd(back, CmdZCEnterStandingMonster); n != 0 {
		t.Fatalf("coming back sent %d 3527 enter form(s) - a return MUST NOT re-send the "+
			"enter form: the client ignores it for a handle it already has", n)
	}
	if n := countCmd(back, CmdZCLeaveMonster); n != 1 {
		t.Fatalf("walking away from the far monster sent %d leave(s) %v, want one 3531",
			n, sightCmds(back))
	}
	for _, p := range back {
		if binary.LittleEndian.Uint16(p) == CmdZCReEnterStandingMonster {
			if got := binary.LittleEndian.Uint32(p[2:]); got != near.Handle {
				t.Fatalf("the returned 3528 names handle %d, want %d", got, near.Handle)
			}
			if got := binary.LittleEndian.Uint32(p[emPos:]); got != uint32(near.X) {
				t.Fatalf("the returned 3528 places the body at x=%d, want %d",
					int32(got), near.X)
			}
		}
	}
}

// TestBuildReEnterStandingMonsterSize pins the frame the RETURN path sends for a monster:
// 360 bytes, command 3528, the handle at +2 and the body's point at emPos.
//
// THE BODY IS THE CAPTURED 3527's, with only the command id overwritten (zone\sight.go).
// No 3528 exists in any capture, so where its hp and buff-mask fields sit is NOT measured -
// the size and the command are. That caveat is why this test checks placement and size
// rather than every field: it must not pretend to a measurement this lane does not have.
func TestBuildReEnterStandingMonsterSize(t *testing.T) {
	m := Monster{Handle: MonsterHandle(9001), X: 5999, Y: 0, ClassID: 11600}
	b := BuildReEnterStandingMonster(m)
	if len(b) != ReEnterStandingMonsterSize {
		t.Fatalf("the 3528 re-enter frame is %d bytes, want %d (the client's table)",
			len(b), ReEnterStandingMonsterSize)
	}
	if got := binary.LittleEndian.Uint16(b); got != CmdZCReEnterStandingMonster {
		t.Fatalf("the 3528 frame's command = %d, want %d", got, CmdZCReEnterStandingMonster)
	}
	if got := binary.LittleEndian.Uint32(b[2:]); got != m.Handle {
		t.Fatalf("the 3528 frame names handle %d, want %d", got, m.Handle)
	}
	if got := binary.LittleEndian.Uint32(b[emPos:]); got != uint32(m.X) {
		t.Fatalf("the 3528 frame places the body at x=%d, want %d", int32(got), m.X)
	}
}

// TestSightNeverLeavesItsOwnTeam is pass condition 4: a team spread 5,000 units apart
// is never culled for itself, over ten passes.
func TestSightNeverLeavesItsOwnTeam(t *testing.T) {
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 10}
	a := &Session{
		Pos: [3]int32{0, 0, 0},
		Team: []Character{
			{Handle: 101, Name: "lead", Level: 10},
			{Handle: 102, Name: "follow", Level: 9},
		},
	}
	// The follower stands 5,000 units from the leader - outside the 4,000 ring.
	a.Team[1].Position = [3]int32{5000, 0, 0}
	h.Players.Add(a)
	b := sightSession(202, [3]int32{100, 0, 0})
	h.Players.Add(b)

	for i := 0; i < 10; i++ {
		for _, p := range h.sightTick(a) {
			if len(p) == 8 && binary.LittleEndian.Uint16(p) == CmdZCLeavePC {
				h := binary.LittleEndian.Uint32(p[2:])
				if h == 101 || h == 102 {
					t.Fatalf("tick %d sent a leave for this session's OWN handle %d - a "+
						"team member is never culled for its own view", i, h)
				}
			}
		}
	}
}

// TestSightLeaveZoneForgetsTheTeam is the lifecycle question: a session that leaves
// the zone is removed from everyone's ring memory, so the ring never sends a leave
// for a handle whose reason-0 leave already went out - and a reconnect is a FRESH
// enter, not a re-enter.
func TestSightLeaveZoneForgetsTheTeam(t *testing.T) {
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 10}
	a := sightSession(101, [3]int32{0, 0, 0})
	b := sightSession(202, [3]int32{100, 0, 0})
	h.Players.Add(a)
	h.Players.Add(b)

	// A has B in sight, so B's handle is in A's seen set.
	if got := h.sightTick(a); countCmd(got, CmdZCSpawnStandingPC) != 1 {
		t.Fatalf("setup: B was not announced to A")
	}

	// B leaves the zone. leaveZone sends the reason-0 3525 through the sessions'
	// writers; these test sessions have no connection, so those sends fail and are
	// logged - what is under test here is the RING's behaviour, not the login path.
	h.leaveZone(b)

	// A pass with B gone must say nothing: the handle left the zone, it did not walk
	// out of sight.
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("a ring pass after B left the zone sent %v, want nothing", sightCmds(got))
	}

	// B comes back (a reconnect is a NEW session, and it is announced fresh): the
	// client got reason 0, so it needs the ENTER form - never a leave and never a
	// re-enter for an actor it no longer has.
	b2 := sightSession(202, [3]int32{200, 0, 0})
	h.Players.Add(b2)
	got := h.sightTick(a)
	if n := countCmd(got, CmdZCLeavePC); n != 0 {
		t.Fatalf("the reconnect produced %d leave(s) %v, want none", n, sightCmds(got))
	}
	if n := countCmd(got, CmdZCSpawnStandingPC); n != 1 {
		t.Fatalf("the reconnect produced %d enter form(s) %v, want exactly one 3520",
			n, sightCmds(got))
	}
}

// TestSightDepartedHandleIsForgottenSilently is the race leaveZone has to close: a
// pass whose roster snapshot still names a handle that has ALREADY left the zone must
// forget it without sending a range leave for it.
func TestSightDepartedHandleIsForgottenSilently(t *testing.T) {
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 10}
	a := sightSession(101, [3]int32{0, 0, 0})
	b := sightSession(202, [3]int32{100, 0, 0})
	h.Players.Add(a)
	h.Players.Add(b)
	h.sightTick(a) // A now has B in its seen set

	// The departure note leaveZone writes before it sends its own reason-0 leave.
	sightDeparted(202)
	// B is still IN the roster snapshot (that is the race), and it is out of range.
	h.Players.SetPos(b, 0, [3]int32{9000, 0, 0})

	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("a departed handle produced %v, want no packet at all", sightCmds(got))
	}

	// AND IT IS FORGOTTEN, not merely hidden: a fresh sight is the enter form again.
	sightClearDeparted(202)
	h.Players.SetPos(b, 0, [3]int32{100, 0, 0})
	got := h.sightTick(a)
	if n := countCmd(got, CmdZCSpawnStandingPC); n != 1 {
		t.Fatalf("after a departure the next sight sent %v, want one fresh 3520 enter "+
			"form (never a 3522 for an actor the client removed)", sightCmds(got))
	}
}

// TestSightNoMapRowSendsToEveryone is the fallback 1c names: a map with no table row
// must NOT hide anybody - it keeps today's behaviour and says so ONCE.
func TestSightNoMapRowSendsToEveryone(t *testing.T) {
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	var buf bytes.Buffer
	h := &Handler{Log: log.New(&buf, "", 0), MapID: 99, MapName: "unknown_map"}
	a := sightSession(101, [3]int32{0, 0, 0})
	b := sightSession(202, [3]int32{9000, 0, 0})

	// The entry burst is where an unarmed map has to behave exactly as before: a
	// player 9,000 units away is still announced in BOTH directions.
	h.joinZone(a)
	burst := h.joinZone(b)
	if n := countCmd(burst, CmdZCSpawnStandingPC); n != 1 {
		t.Fatalf("a map with no ViewRange row announced %d enter form(s) %v - it must "+
			"send to EVERYONE", n, sightCmds(burst))
	}
	if !strings.Contains(buf.String(), "no map_table row for map 99 - sending to everyone") {
		t.Fatalf("the fallback was not reported once: %s", buf.String())
	}
	// ...and the ring itself must stay out of the way: no pass may cull on it.
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("an unarmed ring sent %v, want nothing (the join burst is the whole "+
			"announcement on this map)", sightCmds(got))
	}
	// ONCE, not once per call.
	buf.Reset()
	h.joinZone(b)
	if strings.Contains(buf.String(), "sending to everyone") {
		t.Fatalf("the fallback line was written again on a later join: %s", buf.String())
	}
}

// TestSightUnknownKindFailsLoudly is the data-typed question: a new object type in the
// registry must not fall through to a player packet.
func TestSightUnknownKindFailsLoudly(t *testing.T) {
	var buf bytes.Buffer
	h := &Handler{Log: log.New(&buf, "", 0), MapID: 10}
	a := sightSession(101, [3]int32{0, 0, 0})
	h.Players.Add(a)

	if pkts := h.sightAnnounce(sightItem{handle: 77, kind: sightKind(9)}, false); len(pkts) != 0 {
		t.Fatalf("an unknown object kind produced %d packet(s) %v - it must send NOTHING",
			len(pkts), sightCmds(pkts))
	}
	if !strings.Contains(buf.String(), "unknown object kind 9") {
		t.Fatalf("an unknown object kind was not reported:\n%s", buf.String())
	}
	if pkts := h.sightLeave(sightKind(9), 77); pkts != nil {
		t.Fatalf("an unknown object kind produced a leave packet %v", sightCmds([][]byte{pkts}))
	}
}

// TestMetricsLineCarriesTheSightCounts is pass condition 8: the three counters are on
// the METRICS line, in the same shape as moves=/stops=/walks=.
func TestMetricsLineCarriesTheSightCounts(t *testing.T) {
	s := Snapshot{PktsIn: 1, SightEnter: 4, SightLeave: 5, SightReenter: 6}
	line := s.Line(10 * time.Second)
	if !strings.Contains(line, "sight_enter=4 sight_leave=5 sight_reenter=6") {
		t.Fatalf("the METRICS line does not carry the sight counts:\n got: %s", line)
	}
}
