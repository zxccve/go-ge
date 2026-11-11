package zone

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"geserver/protocol"
)

// TestAttackPositionIsTheDrawnOneNotTheMeasuredOne is the owner's snap, stated as
// arithmetic (R91).
//
// The character is one second into a 1000-unit walk that passes the Scarecrow at
// 58 units. The MEASURED speed is 800 - the top of walkSpeed's plausible band - so
// charPos would put the body at y = 1900 + 800 = 2700. The speed the CLIENT was
// actually sent is h.packetSpeed, the character's own MSPD (~469), and that is
// where the client has drawn the body: y = 1900 + 469 = 2369. The two answers are
// 331 units apart, and the 3564 at +8 is obeyed by the client
// (GeActor.cpp:309), so the 800 answer is a forward snap on every swing.
//
// It goes through h.Reply, not the accessor, on purpose: the bug was the CALL SITE,
// so a test that called drawnPos directly would stay green with the call site
// reverted to casterPos - and would prove nothing about the packet the player sees.
func TestAttackPositionIsTheDrawnOneNotTheMeasuredOne(t *testing.T) {
	h, logs, p := attackZone(t)

	// A walk that starts 1 s ago, runs past the Scarecrow (at y = 2311) and ends far
	// beyond it: 1000 units long, so neither answer reaches the segment's own end and
	// the clamp cannot hide the difference.
	const (
		startY  = 1900
		endY    = 2900
		targetY = 2311
	)
	p.sess.moveFrom = [3]int32{-919, startY, 136}
	p.sess.Pos = [3]int32{-919, endY, 136}
	p.sess.moveAt = time.Now().Add(-time.Second)
	// A MEASURED 800, and enough samples that walkSpeed trusts it: this is the value
	// charPos would use, and the one this test must NOT see.
	p.sess.MoveSpeed = 800
	p.sess.moveSamples = 8

	spd := h.packetSpeed(&p.sess.Team[0])
	if spd <= 0 || spd >= 800 {
		t.Fatalf("the packet speed is %v, but this test needs a real speed below the "+
			"measured 800: the whole point is that the two differ", spd)
	}

	f := attackFrame(0, capturedTarget)
	out, err := h.Reply(context.Background(), p.sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if len(out) == 0 || binary.LittleEndian.Uint16(out[0]) != CmdZCAttackSingle {
		first := -1
		if len(out) > 0 {
			first = int(binary.LittleEndian.Uint16(out[0]))
		}
		t.Fatalf("the swing sent %d packet(s), first cmd %d, want one %d: at the "+
			"speed the client was given (%v) the body is ~%d from the target and in "+
			"reach, while at the MEASURED 800 it is ~%d away and the press cannot land:\n%s",
			len(out), first, CmdZCAttackSingle, spd,
			startY+int(spd)-targetY, startY+800-targetY, logs.String())
	}

	got := worldPos(out[0][asAtkPos:])
	wantX, wantCell := int32(-919), int32(136)
	if got[0] != wantX || got[2] != wantCell {
		t.Errorf("3564 +8 is %v, want x %d and cell %d - the walk's own line and cell",
			got, wantX, wantCell)
	}

	// THE 469 POSITION. elapsed is a hair over 1 s (time.Now is read after the walk
	// began), so the band is the 469 answer plus 2% of slack, never 800.
	const travelMeasured = 800
	lo := int32(startY) + int32(spd)
	hi := int32(startY) + int32(spd*1.02)
	if got[1] < lo || got[1] > hi {
		t.Fatalf("3564 +8 y = %d, want the DRAWN position at the packet speed %v "+
			"(%d..%d after 1 s of walking); the measured speed would say %d",
			got[1], spd, lo, hi, startY+travelMeasured)
	}

	// The same statement as the owner's symptom: how far AHEAD of the drawn body the
	// measured speed puts it after one second.
	ahead := (startY + travelMeasured) - int(got[1])
	if ahead <= 100 {
		t.Fatalf("the measured speed is only %d units ahead of the packet at 1 s, but "+
			"this test is about a snap of more than 100 - the fixture no longer exercises "+
			"the bug (packet speed %v, got y %d)", ahead, spd, got[1])
	}
	t.Logf("after 1 s the drawn body is at y %d (packet speed %v); the measured 800 "+
		"would put it at y %d, %d units ahead", got[1], spd, startY+travelMeasured, ahead)
}

// TestDrawnPosClampsToTheSegment pins the two ends the accessor must never pass:
// the segment's own start and its own destination. Both shapes are covered - the
// leader's moveFrom/Pos segment and a follower's walks entry - because the clamp is
// the reason a placed or arrived body cannot move.
func TestDrawnPosClampsToTheSegment(t *testing.T) {
	at := time.Now()
	from := [3]int32{100, 200, 7}
	dest := [3]int32{100, 900, 7} // 700 units at 469 is ~1.49 s

	leader := &Session{moveAt: at, moveFrom: from, Pos: dest}
	follower := &Session{
		Team:  []Character{{}, {Position: from}},
		walks: make([]charWalk, 2),
	}
	follower.walks[1] = charWalk{from: from, at: at, dest: dest, speed: 800}

	for _, tc := range []struct {
		name string
		s    *Session
		idx  int
	}{
		{"leader", leader, 0},
		{"follower", follower, 1},
	} {
		// Well past the segment's own duration: exactly dest, not an extrapolation.
		if got := tc.s.drawnPos(tc.idx, at.Add(5*time.Second), 469); got != dest {
			t.Errorf("%s after the walk finished: drawnPos = %v, want the destination %v "+
				"exactly", tc.name, got, dest)
		}
		// Before the segment began: exactly from, never behind the start.
		if got := tc.s.drawnPos(tc.idx, at.Add(-time.Second), 469); got != from {
			t.Errorf("%s before the walk began: drawnPos = %v, want the start %v exactly",
				tc.name, got, from)
		}
	}
}

// TestDrawnPosWithoutASegmentIsCharPos is the "a placed body must not move" rule:
// with no open segment the accessor must answer EXACTLY what charPos answers today -
// the session's Pos for the leader, the team member's own Position for a follower -
// so the attack packet cannot carry a position the body never walked to.
func TestDrawnPosWithoutASegmentIsCharPos(t *testing.T) {
	placed := [3]int32{-919, 2211, 136}
	s := &Session{
		Pos:  placed,
		Team: []Character{{Position: placed}, {Position: [3]int32{5, 6, 7}}},
	}

	for _, idx := range []int{0, 1, 2, -1} {
		want := s.charPos(idx, time.Now())
		if got := s.drawnPos(idx, time.Now(), 469); got != want {
			t.Errorf("character %d with no open segment: drawnPos = %v, charPos = %v - "+
				"a placed body must not move", idx, got, want)
		}
	}
	// And the leader's answer is the placed point, not a zero value that would look
	// identical to a bug if charPos ever changed under this test.
	if got := s.drawnPos(0, time.Now(), 469); got != placed {
		t.Errorf("the placed leader reads %v, want %v", got, placed)
	}
	if got := s.drawnPos(1, time.Now(), 469); got != ([3]int32{5, 6, 7}) {
		t.Errorf("the placed team member reads %v, want its own Position", got)
	}
}
