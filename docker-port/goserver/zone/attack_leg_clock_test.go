package zone

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// THE 841 ms REGRESSION, AS ARITHMETIC (R111). One leg, char 1, from the owner's own
// session - the numbers are the log's:
//
//	17:28:25.288310 SWING POS char 1 (1794) pos (2405, -718) ... elapsed 2346ms
//	17:28:27.196082 SWING POS char 1 (1794) pos (2339, -733) ... elapsed 1505ms
//
// No move report arrived between those two lines, so the LEG did not change: only its
// clock did. The approach walk's own clock (w.began) ran out, chaseTick dropped the walk
// without writing the body, and the next reading fell back to the point an old move
// report had declared - 800 units behind where the walk had just drawn the body. The
// repeat then measured 932 of clear ground instead of 124, walked the whole leg again
// from that stale point with a FRESH clock, and the swing reported a body 68 units
// BEHIND the one the swing before it had sent.
//
// THIS TEST IS THE FOUR READERS OF ONE LEG:
//
//	attackBodyPoint   the position a swing sends
//	swingSegment      the segment the SWING POS line names
//	the repeat path   attackAt(attackSwing) - the 3564/3566's +8
//	the arrival path  attackAt(attackAfterWalk) - the 3564/3566's +8
//
// At ONE instant they must agree to the unit, and across the leg - including the moment
// its clock runs out - the along-leg distance must never decrease. On the tree before
// R111 the second half fails with exactly the measurement above.
func TestOneLegOneClock(t *testing.T) {
	// 1800 units out, so the approach the press earns is a long leg (the ring it walks
	// to is MinR..MaxR 900 from the target's centre plus the two bodies).
	h, logs, p := stepBackZone(t, [3]int32{scarecrow().X, scarecrow().Y - 1800, scarecrow().Cell})
	h.Plan = directPlan
	sess := p.sess
	spd := h.packetSpeed(&sess.Team[0])
	minR, maxR := stanceReach(t, h, stepBackStance)

	if out := pressAttack(t, h, sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d walk(s), want 1 - the body is 1800 units from the "+
			"target and stance %d reaches %g..%g:\n%s", moves(out), stepBackStance, minR,
			maxR, logs.String())
	}
	w := sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the press left no held attack walk, so there is no leg to measure:\n%s",
			logs.String())
	}
	goal := w.goal
	legLen := flatDist(w.from, goal)
	if legLen < 400 {
		t.Fatalf("the approach walk is only %g units long - this test needs a leg it can "+
			"sample twice before the clock runs out (fixture moved?)\n%s", legLen,
			logs.String())
	}
	// THE MONSTER STEPS IN, so the body at the leg's own destination is comfortably
	// inside the stance's reach and both packet paths can build a hit there. The leg and
	// its clock are untouched: the walk was planned before this.
	moveRingTarget(t, h, -300)

	// ---------------------------------------------------------------- the four readers
	// ONE INSTANT, at the destination the leg walks to (eta + 20 ms: walked out).
	at := w.began.Add(w.eta + 20*time.Millisecond)
	sent := sess.attackBodyPoint(0, at, spd)
	if sent.pos != goal {
		t.Fatalf("at the leg's own destination the body reads %v, want the goal %v "+
			"(elapsed %s of an eta %s): not one leg, one clock\n%s", sent.pos, goal,
			sent.elapsed, w.eta, logs.String())
	}

	// swingSegment must name the SAME leg, with the same clock.
	segFrom, segDest, segElapsed := sess.swingSegment(0, at)
	if segFrom != sent.from || segDest != sent.dest || segElapsed != sent.elapsed {
		t.Fatalf("the SWING POS segment reads (%v -> %v, elapsed %s) and the point beside "+
			"it was read from (%v -> %v, elapsed %s) - two readers of one leg disagree, "+
			"which is the second clock this round removes\n%s", segFrom, segDest,
			segElapsed, sent.from, sent.dest, sent.elapsed, logs.String())
	}
	if got := walkedToAt(sent.from, sent.dest, sent.at, at, spd); got != sent.pos {
		t.Fatalf("the point (%v) does not lie on the segment the same reading names "+
			"(%v -> %v, started %s, speed %g): the arithmetic says %v\n%s", sent.pos,
			sent.from, sent.dest, sent.at, spd, got, logs.String())
	}

	// THE TWO PACKET PATHS. A repeat needs its own record (a press that is still walking
	// has not landed, so nothing armed one); the cadence is cleared between builds so
	// this test measures the POSITION and not the attack-speed gate.
	sess.swings = make([]*swingLoop, len(sess.Team))
	sess.swings[0] = &swingLoop{target: capturedTarget, stance: sess.Team[0].Stance,
		frame: protocol.Frame{Data: attackFrame(0, capturedTarget)}, hits: 1}
	for _, tc := range []struct {
		name string
		step attackStep
		from *bodyPoint
	}{
		{"the repeat path", attackSwing, nil},
		{"the arrival path", attackAfterWalk, &sent},
	} {
		if len(sess.attackAt) > 0 {
			sess.attackAt[0] = time.Time{}
		}
		out, why := h.attackAt(sess, protocol.Frame{Data: attackFrame(0, capturedTarget)},
			tc.step, tc.from, at)
		got, ok := swingPosIn(out)
		if !ok {
			t.Fatalf("%s built no hit (%s):\n%s", tc.name, why, logs.String())
		}
		if got != sent.pos {
			t.Fatalf("%s sends the body at %v, and attackBodyPoint draws it at %v - the "+
				"3564's +8 the client obeys (GeActor.cpp:300-309) must be the one point all "+
				"four readers agree on\n%s", tc.name, got, sent.pos, logs.String())
		}
	}
	// The repeat's own record is not part of what is being measured from here on.
	sess.swings[0] = nil

	// ------------------------------------------------------- the walk ends
	// THE BODY REACHES ITS GOAL, exactly as the zone's tick reads it: no sleeping, the
	// clock is moved instead. The walk's own walk time is the eta (piece 2d: the arrival
	// is the body's distance, and the body covers the route in eta), so one tick past it
	// is an ARRIVAL, not a timeout.
	w.began = time.Now().Add(-(w.eta + 50*time.Millisecond))
	if out := h.chaseTick(sess); sess.Walk != nil {
		t.Fatalf("the walk was not ended on its arrival (%s of a %s route, tick sent %d "+
			"packet(s)):\n%s", time.Since(w.began).Round(time.Millisecond), w.eta,
			len(out), logs.String())
	}

	// AND THE LEG AFTER IT IS THE SAME LEG. This is the line the owner's session failed:
	// the body must still be at the destination, 808 units along the leg it walked, and
	// the elapsed must not restart from a later or an earlier clock.
	after := sess.attackBodyPoint(0, time.Now(), spd)
	was := flatDist(sent.from, sent.pos)
	nowAlong := flatDist(after.from, after.pos)
	if nowAlong < was {
		t.Fatalf("the swing after the walk's clock ran out reports the body %g units along "+
			"the leg (%v -> %v, elapsed %s) and the one before it reported %g (%v, elapsed "+
			"%s) - BEHIND it, and the leg did not change:\n%s", nowAlong, after.from,
			after.dest, after.elapsed, was, sent.pos, sent.elapsed, logs.String())
	}
	if after.elapsed < sent.elapsed {
		t.Fatalf("the elapsed went BACKWARDS on one leg: %s after the clock ran out, %s "+
			"before it - that is the 841 ms regression:\n%s", after.elapsed, sent.elapsed,
			logs.String())
	}
	if after.pos != goal {
		t.Fatalf("the body reported %v after the walk it just finished, want the leg's own "+
			"destination %v (elapsed %s):\n%s", after.pos, goal, after.elapsed,
			logs.String())
	}

	// AND THE CLOCK STOPS WHERE THE BODY DOES (R112). One leg has one clock (R111), and
	// that clock cannot outlive the leg's own walk time: once the body is held on the far
	// end, the leg is walked out and its age is what it took to walk, not wall-clock since
	// it started. Before R112 the clock kept counting for ever - the owner's session named
	// a 77-unit leg at `elapsed 54453ms`, which is what a finished walk deciding the swing
	// looks like on the log.
	own := legOwnTime(sess.swingLegOf(0, spd))
	if own <= 0 {
		t.Fatalf("this leg's own walk time is %s - the walk is not a leg at all\n%s", own,
			logs.String())
	}
	for _, at := range []time.Duration{2 * time.Second, 54 * time.Second} {
		later := sess.attackBodyPoint(0, sent.at.Add(own+at), spd)
		if later.elapsed > own {
			t.Fatalf("%s past the leg's own walk time (%s) the reading reports an elapsed of "+
				"%s - a finished leg is still being timed, which is the 54-second/77-unit "+
				"line this round makes impossible\n%s", at, own, later.elapsed, logs.String())
		}
		if flatDist(later.from, later.pos) < nowAlong {
			t.Fatalf("the body ran BACKWARDS along the leg %s later: %g units along it "+
				"against %g before\n%s", at, flatDist(later.from, later.pos), nowAlong,
				logs.String())
		}
	}

	// AND THE ALARM NEVER SOUNDED. legDoesNotRewind's line is the regression's own
	// beacon: a run that produces one has a reader handing in a time that is not the
	// leg's, whatever the positions above look like.
	if line := lastLineWith(logs.String(), "SWING POS REWIND"); line != "" {
		t.Fatalf("the guard fired - one leg was reported backwards, so some caller is "+
			"handing in a time that does not belong to it: %s", line)
	}

	t.Logf("one leg of %g units: %v at elapsed %s, then %v at elapsed %s after the clock "+
		"ran out (the leg's own walk time is %s) - and all four readers agreed on %v at %s",
		legLen, sent.pos, sent.elapsed, after.pos, after.elapsed, own, sent.pos,
		at.Format("15:04:05.000"))
}

// swingPosIn is the position the one attack packet in `out` carries at +8: the point the
// client hard-sets its own actor to (GeActor.cpp:300-309). Both the plain 3564 and the
// splash 3566 carry it at the same offset - the splash adds its victims AFTER the plain
// block - so either shape answers here.
func swingPosIn(out [][]byte) ([3]int32, bool) {
	for _, pkt := range out {
		if len(pkt) < AttackSingleSize {
			continue
		}
		switch binary.LittleEndian.Uint16(pkt) {
		case CmdZCAttackSingle, CmdZCAttackSplash:
			return worldPos(pkt[asAtkPos:]), true
		}
	}
	return [3]int32{}, false
}

// TestTheRewindAlarmSoundsOnce proves the guard's line can actually appear: one leg, two
// readings, the second at an EARLIER time. The point must be held (the screen never jumps
// backwards) and the alarm must name both points and both times - once, not once per
// tick - because that line is how the next regression is found in the log instead of on
// the owner's screen.
func TestTheRewindAlarmSoundsOnce(t *testing.T) {
	h, logs, p := attackZone(t)
	sess := p.sess

	// ONE CLIENT LEG, thousands of units long, opened one second ago - so a reading one
	// second EARLIER is genuinely mid-leg whatever speed this fixture walks at.
	at0 := time.Now().Add(-time.Second)
	sess.moveFrom = [3]int32{-919, -800, 136}
	sess.Pos = [3]int32{-919, 2211, 136}
	sess.moveAt = at0
	sess.Team[0].Position = sess.Pos

	build := func(at time.Time) [3]int32 {
		if len(sess.attackAt) > 0 {
			sess.attackAt[0] = time.Time{}
		}
		out, why := h.attackAt(sess, protocol.Frame{Data: attackFrame(0, capturedTarget)},
			attackPress, nil, at)
		pos, ok := swingPosIn(out)
		if !ok {
			t.Fatalf("no hit from the build at %s (%s):\n%s", at.Format("15:04:05.000"),
				why, logs.String())
		}
		return pos
	}

	// The leg walked out: the body is at its destination.
	if got := build(at0.Add(10 * time.Second)); got != sess.Pos {
		t.Fatalf("the walked-out leg reports %v, want its destination %v", got, sess.Pos)
	}
	// And now a reading of the SAME leg, one second EARLIER.
	later := at0.Add(10 * time.Second)
	early := at0.Add(time.Second)
	// The fixture's own arithmetic, read from the same function the zone reads: the
	// earlier instant really is a shorter way along this leg.
	spd := h.packetSpeed(&sess.Team[0])
	ownPoint := walkedToAt(sess.moveFrom, sess.Pos, sess.moveAt, early, spd)
	t.Logf("fixture: packet speed %g, the earlier instant is %v on a %g-unit leg",
		spd, ownPoint, flatDist(sess.moveFrom, sess.Pos))
	if ownPoint == sess.Pos {
		t.Fatalf("one second into a %g-unit leg the body is already at the destination "+
			"(speed %g) - this fixture cannot show a reading running backwards",
			flatDist(sess.moveFrom, sess.Pos), spd)
	}
	if got := build(early); got != sess.Pos {
		t.Fatalf("a reading 1 s EARLIER on the same leg sent %v, want the point reported "+
			"before it (%v) - the guard must hold it:\n%s", got, sess.Pos, logs.String())
	}
	// A third, even earlier reading must not write a second alarm for the same leg.
	build(at0.Add(500 * time.Millisecond))

	if n := strings.Count(logs.String(), "SWING POS REWIND"); n != 1 {
		t.Fatalf("the rewind alarm fired %d time(s) for one leg, want exactly 1:\n%s", n,
			logs.String())
	}
	line := lastLineWith(logs.String(), "SWING POS REWIND")
	for _, want := range []string{"char 0", "leg (-919, -800) -> (-919, 2211)",
		"previous swing reported (-919, 2211)",
		fmt.Sprintf("put the body at (%d, %d), BEHIND", ownPoint[0], ownPoint[1]),
		early.Format("15:04:05.000000"), later.Format("15:04:05.000000")} {
		if !strings.Contains(line, want) {
			t.Fatalf("the alarm line does not carry %q, so it cannot be read back to the "+
				"caller that produced it: %s", want, line)
		}
	}
	t.Logf("alarm: %s", line)
}

// TestOneLegOneClockHoldsTheWire keeps the OTHER half of the promise: no packet shape,
// size or id changed. The 3564 this lane sends is the same 74-byte packet at the same
// offsets - the only thing R111 changed is WHICH point goes in at +8.
func TestOneLegOneClockHoldsTheWire(t *testing.T) {
	h, logs, p := attackZone(t)
	sess := p.sess
	if len(sess.attackAt) > 0 {
		sess.attackAt[0] = time.Time{}
	}
	out, _ := h.attackAt(sess, protocol.Frame{Data: attackFrame(0, capturedTarget)},
		attackPress, nil, time.Now())
	pos, ok := swingPosIn(out)
	if !ok {
		t.Fatalf("the in-reach press built no hit:\n%s", logs.String())
	}
	if pos != sess.Pos {
		t.Fatalf("the packet's +8 is %v, and a placed body stands at %v", pos, sess.Pos)
	}
	for _, pkt := range out {
		switch binary.LittleEndian.Uint16(pkt) {
		case CmdZCAttackSingle:
			if len(pkt) != AttackSingleSize {
				t.Fatalf("3564 is %d bytes, want %d", len(pkt), AttackSingleSize)
			}
		}
	}
	if !strings.Contains(logs.String(), "SWING POS char 0") {
		t.Fatalf("the SWING POS line is gone from the log:\n%s", logs.String())
	}
}
