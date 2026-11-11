package zone

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// TestAPressMidWalkPinsTheBodyWhereItStands is R129, stated as arithmetic on the owner's
// own demo.
//
// THE MEASURED SEQUENCE (logs\zone_go_live.log, 2026-11-04 16:11:07 and 16:15:03/16:15:06):
// a keep-mode press lands mid-walk; the first swing is answered from where the body stands;
// the walk stays open and keeps being simulated, so the SECOND swing carries the walk's
// DESTINATION - a point the client, which stopped the body at the press, never walked to.
// The 3564 hard-sets the client's own actor to +8 (GeActor.cpp:309-311), so that number IS
// the jump on screen: "it jumps to the destination to reposition instead of standing at the
// exact location the character start attacking".
//
// WHAT THIS PINS: the press closes the walk at the point it is answered from, so a reading
// taken long after the leg's own duration still answers the SAME point - never the
// destination.
func TestAPressMidWalkPinsTheBodyWhereItStands(t *testing.T) {
	h, logs, p := attackZone(t)

	// A 1000-unit client walk that runs past the Scarecrow, 700 ms old: the body is at
	// y ~= 1900 + 0.7 x packet speed, inside this stance's own MinR 1 .. MaxR 125 and
	// still ~670 units short of the destination.
	from := [3]int32{-919, 1900, 136}
	dest := [3]int32{-919, 2900, 136}
	p.sess.moveFrom, p.sess.Pos = from, dest
	p.sess.moveAt = time.Now().Add(-700 * time.Millisecond)

	spd := h.packetSpeed(&p.sess.Team[0])
	if spd <= 0 {
		t.Fatalf("the packet speed is %v - this test cannot measure a walk at that speed", spd)
	}

	f := attackFrame(0, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) == 0 || binary.LittleEndian.Uint16(out[0]) != CmdZCAttackSingle {
		t.Fatalf("the mid-walk press sent %d packet(s), first cmd %v, want one %d - the "+
			"fixture must be IN reach mid-walk or this test is about nothing:\n%s",
			len(out), firstCmd(out), CmdZCAttackSingle, logs.String())
	}

	sent := worldPos(out[0][asAtkPos:])
	if sent == dest {
		t.Fatalf("the press was answered from the walk's DESTINATION %v - the fixture is "+
			"already at the end of its leg, so it is not a mid-walk press", sent)
	}

	// (1) THE WALK IS CLOSED: no leg is open, and the body is placed where the swing said.
	if !p.sess.moveAt.IsZero() {
		t.Errorf("the walk is STILL OPEN after a press answered it (moveAt %v): the rest of "+
			"the leg can carry a later swing to its destination", p.sess.moveAt)
	}
	if p.sess.Pos != sent {
		t.Errorf("the body was placed at %v, but the swing said %v - the pin and the packet "+
			"must be one measurement", p.sess.Pos, sent)
	}
	if p.sess.moveFrom != sent {
		t.Errorf("the closed leg starts at %v, want the pinned point %v (from == dest == the "+
			"point the press was answered from)", p.sess.moveFrom, sent)
	}

	// (2) THE SECOND SWING IS BUILT FROM THE PINNED POINT, not from the leg's destination -
	// read two seconds later, long past the leg's own duration, which is exactly when the
	// old code answered `dest`.
	later := time.Now().Add(2 * time.Second)
	body := p.sess.attackBodyPoint(0, later, spd)
	if body.pos != sent {
		t.Fatalf("a swing two seconds later reports %v, want the pinned point %v - the walk "+
			"to %v must be dropped, not walked: this is the jump the owner saw",
			body.pos, sent, dest)
	}

	// (3) THE LOG SAYS SO, in one line, naming both points.
	log := logs.String()
	if !strings.Contains(log, "pressed MID-WALK") || !strings.Contains(log, "PINNED at") {
		t.Fatalf("no PINNED line for a mid-walk press:\n%s", log)
	}
	if !strings.Contains(log, "is DROPPED") {
		t.Fatalf("the PINNED line does not name the dropped walk:\n%s", log)
	}
	t.Logf("pinned at %v (walk destination %v dropped); a swing 2 s later still reads %v",
		sent, dest, body.pos)
}

// TestAPressOnAWalkThisServerSentIsNotPinned is the guard the rule needs: a body walking a
// walk THIS SERVER sent (an attack or cast approach) belongs to that walk's own clock,
// arrival and ring - swingLegOf prefers it - so closing the client's leg under it would be a
// second rule for one body. Nothing is pinned there, and the client's leg is left alone.
func TestAPressOnAWalkThisServerSentIsNotPinned(t *testing.T) {
	at := time.Now().Add(-time.Second)
	s := &Session{Team: []Character{{}}}
	s.moveFrom, s.Pos = [3]int32{0, 0, 1}, [3]int32{0, 900, 1}
	s.moveAt = at
	s.Walk = &walk{caster: 0, atk: &pendingAttack{}}

	if _, pinned := s.pinStandingPointOnPress(0, bodyPoint{
		pos:  [3]int32{0, 300, 1},
		dest: [3]int32{0, 900, 1},
	}); pinned {
		t.Fatalf("a press on a walk THIS server sent was pinned - that walk owns its own " +
			"clock and ring")
	}
	if s.moveAt.IsZero() || s.Pos != ([3]int32{0, 900, 1}) {
		t.Errorf("the client leg was rewritten under a server walk: moveAt %v, Pos %v",
			s.moveAt, s.Pos)
	}
}

// firstCmd is the first packet's command id, or nil-safe zero when nothing was sent.
func firstCmd(out [][]byte) uint16 {
	if len(out) == 0 || len(out[0]) < 2 {
		return 0
	}
	return binary.LittleEndian.Uint16(out[0])
}
