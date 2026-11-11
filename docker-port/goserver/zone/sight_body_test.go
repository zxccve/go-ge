package zone

// THE RING MEASURES THE BODY (R135d, 2026-11-06).
//
// The owner's report: "1 family with 3 character is off the screen from another family.
// A will instantly [vanish from] B when B started moving although B havent reach the
// destination yet."
//
// CAUSE. CZ_MOVE_PC names a DESTINATION (zone\move.go, fact #231), and the roster's Pos
// entry is that destination (Players.SetPos). The pass centred on `sess.Pos` and compared
// `m.Pos[j]`, so the tick after B ORDERED a walk past the ring measured B at the far end
// of it. The body was still standing beside A.
//
// THE FIX (zone\sight.go, bodyPointAt / sightCentre): both ends of the comparison are the
// body's live point from swingLegOf + legPoint, at ONE clock per pass. A body with no leg
// (`placed`) answers the same stored point it always did, which is why every older test in
// sight_test.go still passes unchanged.
//
// THE CLOCK IS DRIVEN, NOT SLEPT. sightTick reads time.Now() itself (it rides tickIfDue,
// zone\chase.go, and has no clock argument), so a test moves a leg's own start time into
// the past and the body moves with it: a leg opened 9 s ago has walked 9 s x 469 u/s.

import (
	"encoding/binary"
	"io"
	"log"
	"testing"
	"time"
)

// reportWalk applies a CZ_MOVE_PC report to `s` for character idx exactly as movePC does
// (zone\zone.go:1896-1916): the session's own leg, the driven character's segment, the
// character's stored point, and - THE OLD RING'S NUMBER - the roster's destination.
//
// The roster keeps writing the DESTINATION because that is what the rest of the server
// reads it for. It is precisely this value the ring must no longer measure.
func reportWalk(h *Handler, s *Session, idx int, from, dest [3]int32, at time.Time, speed float64) {
	h.Players.SetPos(s, idx, dest)
	s.openWalkTo(idx, from, dest, nil, at, speed)
	if idx == s.Control {
		s.moveFrom, s.moveAt, s.moveRoute = from, at, nil
		s.Pos = dest
	}
	if idx >= 0 && idx < len(s.Team) {
		s.Team[idx].Position = from
	}
}

// bodyRing builds the two-session zone every case here uses: the viewer A at `aPos` and B
// at `bPos`, one character each, radius 4000.
func bodyRing(t *testing.T, aPos, bPos [3]int32) (*Handler, *Session, *Session) {
	t.Helper()
	sightUseTable(t, map[int]SightRow{10: {ViewRange: 4000}})
	h := &Handler{Log: log.New(io.Discard, "", 0), MapID: 10}
	a := sightSession(101, aPos)
	b := sightSession(202, bPos)
	h.Players.Add(a)
	h.Players.Add(b)
	return h, a, b
}

// TestSightMeasuresTheBodyNotTheDestination is pass condition 1: the reported bug itself.
//
// A and B stand 100 units apart inside a 4,000 ring. B is ORDERED to walk 9,000 units
// away, so the roster says B is already at (9,100, 0) while B's body is still at (100, 0).
// A must hear NOTHING until B's body has really crossed the ring, and then exactly one
// 3525 with reason 3 - never before.
func TestSightMeasuresTheBodyNotTheDestination(t *testing.T) {
	h, a, b := bodyRing(t, [3]int32{0, 0, 0}, [3]int32{100, 0, 0})

	// A has B in sight to begin with, so the only thing left to hear is a LEAVE.
	if got := h.sightTick(a); countCmd(got, CmdZCSpawnStandingPC) != 1 {
		t.Fatalf("setup: B was not announced to A: %v", sightCmds(got))
	}

	at := time.Now()
	reportWalk(h, b, 0, [3]int32{100, 0, 0}, [3]int32{9100, 0, 0}, at, MoveBaseSpeed)

	// THE TICK RIGHT AFTER THE ORDER. The roster is at 9,100; the body has moved 0 units.
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("the tick right after B ORDERED the walk sent %v - B's body is still "+
			"100 units away and has walked nowhere. The ring measured the DESTINATION "+
			"(the reported bug: A vanishes from B the moment B starts moving)", sightCmds(got))
	}

	// ONE SECOND IN: the body has walked 469 units (still 569 from A), and the roster has
	// not moved at all - the destination is the same whole way.
	b.moveAt = time.Now().Add(-time.Second)
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("1 s into the walk the body is ~%d units away, well inside the 4,000 "+
			"ring, yet A was sent %v", int(100+MoveBaseSpeed), sightCmds(got))
	}

	// NINE SECONDS IN: the body has walked ~4,221 units - it is past the ring now.
	b.moveAt = time.Now().Add(-9 * time.Second)
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
		t.Fatalf("the tick B's body crossed the ring sent %d leave(s) %v, want exactly one "+
			"- and only once the BODY had walked out", leaves, sightCmds(out))
	}
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("the tick after the leave sent %v more, want nothing (one leave per walk-out)",
			sightCmds(got))
	}
}

// TestSightViewerCentreIsItsBodyWhenItWalks is pass condition 2, the mirror case: the
// VIEWER orders a walk toward a character that is far away. A's own circle must travel
// with A's BODY, so the far character is announced on the tick A's body is really inside
// the radius - not when A clicked (which is where sess.Pos, the old centre, jumps to).
func TestSightViewerCentreIsItsBodyWhenItWalks(t *testing.T) {
	h, a, _ := bodyRing(t, [3]int32{0, 0, 0}, [3]int32{5000, 0, 0})

	at := time.Now()
	reportWalk(h, a, 0, [3]int32{0, 0, 0}, [3]int32{9100, 0, 0}, at, MoveBaseSpeed)

	// B is 5,000 units away and A's BODY has not moved: outside the 4,000 ring.
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("A ordered a walk toward B and was told about B on that same tick: %v. "+
			"A's body is at (0, 0) - 5,000 units from B - so the centre was read from "+
			"the DESTINATION (9,100, 0)", sightCmds(got))
	}

	// 3 s in the body has walked ~1,407 units: 3,593 from B - INSIDE the ring.
	a.moveAt = time.Now().Add(-3 * time.Second)
	got := h.sightTick(a)
	if n := countCmd(got, CmdZCSpawnStandingPC); n != 1 {
		t.Fatalf("3 s into A's walk its body is ~%d units from B's 5,000 (inside 4,000), "+
			"so B must be announced on THIS tick; got %d enter form(s) %v",
			int(5000-MoveBaseSpeed*3), n, sightCmds(got))
	}
}

// TestSightCancelledWalkNeverLeaves is pass condition 3: a character ordered far away and
// then re-ordered back BEFORE its body crossed is never left and never re-announced. One
// leg replaces the other (openWalkTo), so the next tick reads the NEW leg - there is no
// leave for the abandoned destination and no enter for a return that never happened.
func TestSightCancelledWalkNeverLeaves(t *testing.T) {
	h, a, b := bodyRing(t, [3]int32{0, 0, 0}, [3]int32{100, 0, 0})

	if got := h.sightTick(a); countCmd(got, CmdZCSpawnStandingPC) != 1 {
		t.Fatalf("setup: B was not announced to A: %v", sightCmds(got))
	}

	at := time.Now()
	// Order far away...
	reportWalk(h, b, 0, [3]int32{100, 0, 0}, [3]int32{9100, 0, 0}, at, MoveBaseSpeed)
	b.moveAt = at.Add(-2 * time.Second) // 938 units walked: still inside
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("the far order alone produced %v before the body crossed anything",
			sightCmds(got))
	}

	// ...then back, from where the body has got to. The far leg is REPLACED.
	reportWalk(h, b, 0, [3]int32{1038, 0, 0}, [3]int32{100, 0, 0}, time.Now(), MoveBaseSpeed)

	for i := 0; i < 3; i++ {
		b.moveAt = time.Now().Add(-time.Duration(i) * time.Second)
		got := h.sightTick(a)
		if n := countCmd(got, CmdZCLeavePC); n != 0 {
			t.Fatalf("tick %d after the cancelled walk sent %d leave(s) %v - B's body never "+
				"left the ring, so there is nothing to leave", i, n, sightCmds(got))
		}
		if n := countCmd(got, CmdZCSpawnStandingPC); n != 0 {
			t.Fatalf("tick %d after the cancelled walk sent %d enter form(s) %v - B was "+
				"never left, so there is no return to announce", i, n, sightCmds(got))
		}
	}
}

// TestSightFollowerIsMeasuredByItsOwnLeg is pass condition 4: character 1 (a follower, NOT
// the driven one) walking through its own report is measured by ITS leg. Slot 0 is placed
// beside A and never moves, so a ring that read slot 0's leg for slot 1 would never let the
// follower leave - and a ring that read the roster's destination would let it leave at once.
func TestSightFollowerIsMeasuredByItsOwnLeg(t *testing.T) {
	h, a, _ := bodyRing(t, [3]int32{0, 0, 0}, [3]int32{100, 0, 0})
	b := &Session{
		Pos:  [3]int32{100, 0, 0},
		Team: []Character{{Handle: 202, Name: "lead", Level: 10}, {Handle: 203, Name: "follow", Level: 9}},
	}
	b.Team[1].Position = [3]int32{100, 0, 0}
	h.Players.Add(b)

	if got := h.sightTick(a); countCmd(got, CmdZCSpawnStandingPC) != 2 {
		t.Fatalf("setup: A should be told about BOTH characters of B, got %v", sightCmds(got))
	}

	at := time.Now()
	reportWalk(h, b, 1, [3]int32{100, 0, 0}, [3]int32{9100, 0, 0}, at, MoveBaseSpeed)

	// The tick right after the follower's order: its body has not moved.
	if got := h.sightTick(a); len(got) != 0 {
		t.Fatalf("the tick right after the FOLLOWER ordered its walk sent %v - the follower's "+
			"body is 100 units away; the ring read a destination, not a leg", sightCmds(got))
	}

	// 9 s later the follower's body is ~4,321 units away: exactly ONE leave, for 203.
	b.walks[1].at = time.Now().Add(-9 * time.Second)
	out := h.sightTick(a)
	leaves202, leaves203 := 0, 0
	for _, p := range out {
		if len(p) == 8 && binary.LittleEndian.Uint16(p) == CmdZCLeavePC {
			switch binary.LittleEndian.Uint32(p[2:]) {
			case 202:
				leaves202++
			case 203:
				leaves203++
			}
		}
	}
	if leaves203 != 1 {
		t.Fatalf("the follower's body walked out and produced %d leave(s) for 203 (want 1): %v",
			leaves203, sightCmds(out))
	}
	if leaves202 != 0 {
		t.Fatalf("the follower's walk produced %d leave(s) for its leader 202, which never "+
			"moved: %v", leaves202, sightCmds(out))
	}
}
