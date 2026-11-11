package zone

import (
	"testing"
	"time"

	"geserver/protocol"
)

// R112: A WALK THIS ZONE SENT STOPS DECIDING THE SWING THE MOMENT IT IS OVER.
//
// THE MEASUREMENT (owner's live session, logs\go_zone1331.log, 2026-11-02). The warlock
// (char 2, handle 1793) swung for 54 SECONDS from a 77-unit leg while the client walked it
// away across the map:
//
//	18:21:55.235022 ATTACK APPROACH: ... STEPPING BACK 77 units to (2621, -816) ..., eta 0.2s
//	18:22:04.412745 MOVE_PC char 2 handle 1793 -> (1964, -746) cell 108 | model was at
//	    (2621, -816) 963 u off, leg 302 u ...
//	18:22:31.152629 MOVE_PC char 2 handle 1793 -> (1783, -698) cell 34 | model was at
//	    (2621, -816) 751 u off, leg 100 u ...
//	18:22:49.688481 SWING POS char 2 (1793) pos (2621, -816, cell 108) from segment
//	    (2622, -739) -> (2621, -816) elapsed 54453ms
//
// THE CHAIN. The walk ended on its arrival (480 ms of route) and handed its leg to the
// kept-leg record WITH ITS OWN CLOCK (R111, correct). The client then walked the body away:
// each report opened its own leg (walks[idx]) and each of those legs walked out and was
// closed by endWalks, which writes the body's stored point. But the KEPT leg was never
// invalidated, so the moment the client had no leg open - which is most of the time, since
// its legs are ~1 s long - the swing fell back to a leg from 54 seconds ago and told the
// client to put the body back on its goal, 900+ units behind where it was walking.
//
// THE TWO RULES THIS FILE PINS:
//
//	1. a remembered leg is the body's leg only while the body still stands on its far end
//	   (attack_position.go, keptLegOf). Once anything newer moves the body - the client's
//	   own report, that report's walked-out stop, a placement - the leg is history, and the
//	   swing reads the client's leg or the stored point.
//	2. one leg's clock stops where its body does (attack_position.go, legElapsed): a leg
//	   that has been walked out cannot be reported at an age past its own walk time, so
//	   `elapsed 54453ms` on a 77-unit leg is impossible by construction.

// TestAWalkThatHasEndedStopsDecidingTheSwing is the owner's block, as a test: the walk
// fires, the client then moves the SAME character 900 units away, and the next swing must
// be on the CLIENT's new leg with THAT leg's age - not on the finished walk's goal with the
// finished walk's clock.
//
// THE CHARACTER IS A FOLLOWER ON PURPOSE. Slot 0 is the driven character, whose leg is the
// session's own segment (moveFrom -> Pos); a follower's leg is walks[idx], and the kept-leg
// fallback is only ever reached on that path - which is exactly the slot the owner's log
// names (char 2 of 3).
func TestAWalkThatHasEndedStopsDecidingTheSwing(t *testing.T) {
	h, logs, p := fireArrivalZone(t)
	sess := p.sess

	// A SECOND TEAM MEMBER, far away, that does the pressing.
	follow := sess.Team[0]
	follow.Handle = 1795
	sess.Team = append(sess.Team, follow)
	sess.Team[1].Position = fireArrivalFar

	if out := pressAttackAs(t, h, sess, 1, capturedTarget); moves(out) != 1 {
		t.Fatalf("the follower's press produced %d move(s), want the one approach walk:\n%s",
			moves(out), logs.String())
	}
	w := sess.Walk
	if w == nil || w.atk == nil || w.caster != 1 {
		t.Fatalf("the follower's press left no held attack walk:\n%s", logs.String())
	}
	goal, legLen := w.goal, flatDist(w.from, w.goal)

	// THE WALK ENDS ON ITS ARRIVAL, exactly as the zone's tick ends it (piece 2d: the body
	// covers its route in eta, so one tick past that is the arrival): the hit fires and
	// the leg is handed over with the clock it already had (R111).
	w.began = time.Now().Add(-(w.eta + 50*time.Millisecond))
	began := w.began
	if out := h.chaseTick(sess); sess.Walk != nil {
		t.Fatalf("the walk was not ended on its arrival (%s of a %s route, tick sent %d packet(s)):\n%s",
			time.Since(w.began).Round(time.Millisecond), w.eta, len(out),
			logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("%d ATTACK FIRED ON ARRIVAL line(s), want the one hit the walk ended on:\n%s",
			n, logs.String())
	}
	if sess.keptLegOf(1) == nil {
		t.Fatalf("the finished walk did not hand its leg to the kept-leg record:\n%s",
			logs.String())
	}
	// The repeat the fire armed is not part of what this test measures; disarm it so the
	// tick below cannot add a swing of its own.
	sess.swings = make([]*swingLoop, len(sess.Team))

	// THE CLIENT WALKS THE CHARACTER 900 UNITS AWAY, through the real handler.
	far := [3]int32{goal[0], goal[1] - 900, goal[2]}
	if d := flatDist(goal, far); d != 900 {
		t.Fatalf("the client's new leg is %g units, want 900", d)
	}
	if _, err := h.movePC(sess, moveFrame(1, far)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	walks := sess.walks
	if len(walks) <= 1 || walks[1].at.IsZero() {
		t.Fatalf("the client's report opened no leg for char 1:\n%s", logs.String())
	}
	reported := walks[1].at

	// THE NEXT SWING, a quarter of a second into the client's leg.
	swingAt := reported.Add(250 * time.Millisecond)
	body := sess.attackBodyPoint(1, swingAt, h.packetSpeed(&sess.Team[1]))
	cross, along := onSegment(body.pos, body.from, body.dest)
	if body.dest != far {
		t.Fatalf("the swing's segment ends on %v, and the client's own report named %v - the "+
			"swing is still deciding on a leg that is over:\n%s", body.dest, far,
			logs.String())
	}
	if cross > 2 || along < 0 || along > 1 {
		t.Fatalf("the swing's point %v is %g units off the client's leg (%v -> %v) at %g of "+
			"the way along it:\n%s", body.pos, cross, body.from, body.dest, along,
			logs.String())
	}
	if body.pos == goal {
		t.Fatalf("the swing's point %v IS the finished walk's goal - it is still being drawn "+
			"on the leg that ended %s ago:\n%s", body.pos,
			time.Since(began).Round(time.Millisecond), logs.String())
	}
	// THE AGE IS THE CLIENT'S LEG'S, NOT THE OLD WALK'S. The walk's clock was already
	// 1.3 s old when the report arrived; the client's leg is 250 ms old.
	if body.elapsed < 200*time.Millisecond || body.elapsed > 400*time.Millisecond {
		t.Fatalf("the swing reports an elapsed of %s, want the client's leg's own age of "+
			"~250ms (the walk that ended is %s old and must not lend its clock):\n%s",
			body.elapsed, swingAt.Sub(began).Round(time.Millisecond), logs.String())
	}

	// AND THE PACKET CARRIES THAT POINT. The target is moved so the client's new point is
	// inside the stance's band - the point of this assertion is the POSITION, not the reach.
	placeTargetAt(t, h, [3]int32{body.pos[0] - 700, body.pos[1], body.pos[2]})
	// The cadence gate is not what this test measures: the walk's own fire has just landed
	// one hit for this character, so its ASPD clock is cleared by hand.
	sess.attackAt = make([]time.Time, len(sess.Team))
	out, why := h.attackAt(sess, protocol.Frame{Data: attackFrame(1, capturedTarget)},
		attackAfterWalk, nil, swingAt)
	if hitAttacks(out) != 1 {
		t.Fatalf("the swing built no hit (%s):\n%s", why, logs.String())
	}
	if got := hitPosition(t, out); got != body.pos {
		t.Fatalf("the 3564's +8 is %v and the reading that measured it is %v - the client "+
			"obeys the packet, so this is the jump:\n%s", got, body.pos, logs.String())
	}
	last := swingLinesOf(t, logs.String())
	swingLine := last[len(last)-1]
	if swingLine.dest[0] != far[0] || swingLine.dest[1] != far[1] {
		t.Fatalf("the SWING POS line names segment (%d, %d) -> (%d, %d), and the client's own "+
			"leg ends on (%d, %d):\n%s", swingLine.from[0], swingLine.from[1],
			swingLine.dest[0], swingLine.dest[1], far[0], far[1], logs.String())
	}

	// THE CLIENT'S LEG WALKS OUT, and its stop writes the body's stored point (endWalks).
	// The finished walk's goal must not come back: with no leg open the swing reads the
	// point the body was last proven at, not a legend from before the player left.
	sess.walks[1].at = time.Now().Add(-4 * time.Second) // a 900-unit leg at 469 u/s needs 2.9 s
	sess.swings = make([]*swingLoop, len(sess.Team))    // no repeat of its own on this tick
	// endWalks sends the stop itself (nothing is collected for a zone with no other
	// player), and writes the point the body was proven at.
	h.chaseTick(sess)
	if !sess.walks[1].at.IsZero() {
		t.Fatalf("the client's leg was not closed when it walked out:\n%s", logs.String())
	}
	if sess.Team[1].Position != far {
		t.Fatalf("the walked-out client leg left the body's stored point at %v, want %v:\n%s",
			sess.Team[1].Position, far, logs.String())
	}
	settled := sess.attackBodyPoint(1, time.Now(), h.packetSpeed(&sess.Team[1]))
	if settled.pos != far {
		t.Fatalf("with the client's leg walked out the swing reports %v, want the point the "+
			"client last declared %v (the finished walk's goal %v is 900 units behind it):\n%s",
			settled.pos, far, goal, logs.String())
	}
	if settled.elapsed != 0 {
		t.Fatalf("a placed body reports an elapsed of %s, want 0:\n%s", settled.elapsed,
			logs.String())
	}
	if line := lastLineWith(logs.String(), "SWING POS REWIND"); line != "" {
		t.Fatalf("the rewind alarm sounded: %s", line)
	}
	t.Logf("one walk of %g units ended %s in; the client then moved the body 900 units and "+
		"the next swing read the client's own leg (%v -> %v) at elapsed %s", legLen,
		time.Since(began).Round(time.Millisecond), body.from, body.dest, body.elapsed)
}

// TestAFinishedLegCannotBeTimedPastItsOwnWalkTime is the second half: no reading may report
// an age greater than the leg's own eta plus a margin, whatever shape the leg came from.
//
// THE THREE SHAPES ARE THREE DIFFERENT RECORDS and the owner's log has been bitten by all
// three at some point: the walk this zone sent (sess.Walk), the leg it handed over when it
// ended (keptLegs), and the client's own leg (walks). A 54-second-old clock is applied to
// each; none of them may report more than the time it takes to walk the leg.
func TestAFinishedLegCannotBeTimedPastItsOwnWalkTime(t *testing.T) {
	h, logs, p := fireArrivalZone(t)
	sess := p.sess
	spd := h.packetSpeed(&sess.Team[0])

	if out := pressAttack(t, h, sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want the one approach walk:\n%s",
			moves(out), logs.String())
	}
	w := sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the press left no held attack walk:\n%s", logs.String())
	}
	goal, legLen, begun := w.goal, flatDist(w.from, w.goal), w.began
	// THE MARGIN IS THE LEG'S OWN WALK TIME AND NOTHING ELSE. legOwnTime is the same
	// arithmetic the walk's eta was built from, and eta is what a whole 54-second clock is
	// compared against below.
	own := legOwnTime(sess.swingLegOf(0, spd))
	if own <= 0 || own > time.Second {
		t.Fatalf("this leg's own walk time is %s for %g units - this test wants a short leg "+
			"(the log's was 77 units, eta 164ms)", own, legLen)
	}

	check := func(what string) {
		t.Helper()
		at := begun.Add(54453 * time.Millisecond) // the owner's own clock, to the millisecond
		body := sess.attackBodyPoint(0, at, spd)
		if body.elapsed > own+50*time.Millisecond {
			t.Fatalf("%s reports elapsed %s on a %g-unit leg whose own walk time is %s - a "+
				"finished leg is still being timed, which is the `elapsed 54453ms` line this "+
				"round makes impossible:\n%s", what, body.elapsed, legLen, own,
				logs.String())
		}
		if body.pos != goal {
			t.Fatalf("%s reports the body at %v, want the leg's own destination %v:\n%s",
				what, body.pos, goal, logs.String())
		}
	}

	// (1) THE WALK ITSELF, still in flight with a clock 54 seconds old.
	check("the walk this zone sent")

	// (2) THE LEG IT HANDED OVER. The walk ends on its arrival; the kept record carries the
	// same start, so the reading must be the same one - bounded the same way.
	w.began = time.Now().Add(-(w.eta + 50*time.Millisecond))
	begun = w.began
	if out := h.chaseTick(sess); sess.Walk != nil {
		t.Fatalf("the walk was not ended on its arrival (tick sent %d packet(s)):\n%s",
			len(out), logs.String())
	}
	check("the leg the finished walk handed over")

	// (3) THE CLIENT'S OWN LEG, 54 seconds old. It is bounded by its own walk time too: a
	// body held on the leg's far end has finished that leg.
	far := [3]int32{goal[0], goal[1] - 300, goal[2]}
	if _, err := h.movePC(sess, moveFrame(0, far)); err != nil {
		t.Fatalf("movePC: %v", err)
	}
	if sess.moveAt.IsZero() || sess.Pos != far {
		t.Fatalf("the client's report opened no leg:\n%s", logs.String())
	}
	clientAt := time.Now().Add(-54453 * time.Millisecond)
	sess.moveAt = clientAt
	body := sess.attackBodyPoint(0, time.Now(), spd)
	clientOwn := legOwnTime(sess.swingLegOf(0, spd))
	if clientOwn <= 0 || body.elapsed > clientOwn+50*time.Millisecond {
		t.Fatalf("the client's own leg reports elapsed %s against its own walk time %s:\n%s",
			body.elapsed, clientOwn, logs.String())
	}

	// AND ONE SWING THROUGH THE REAL BUILDER, so the bound is on the line the owner reads
	// and not only on the helper: the body sits where the walk left it, inside the band.
	for i := range sess.walks {
		sess.walks[i] = charWalk{} // no client leg open: back to the point the walk reached
	}
	sess.moveAt, sess.moveFrom, sess.Pos = time.Time{}, [3]int32{}, goal
	sess.Team[0].Position = goal
	sess.attackAt = make([]time.Time, len(sess.Team))
	out, why := h.attackAt(sess, protocol.Frame{Data: attackFrame(0, capturedTarget)},
		attackAfterWalk, nil, begun.Add(54453*time.Millisecond))
	if hitAttacks(out) != 1 {
		t.Fatalf("the swing 54 s after the walk began built no hit (%s):\n%s", why,
			logs.String())
	}
	swings := swingLinesOf(t, logs.String())
	last := swings[len(swings)-1]
	if last.elapsed > own.Milliseconds()+50 {
		t.Fatalf("the SWING POS line reports `elapsed %dms` on a %g-unit leg whose own walk "+
			"time is %dms - the finished leg is still being timed:\n%s", last.elapsed, legLen,
			own.Milliseconds(), logs.String())
	}
	if last.pos != goal {
		t.Fatalf("the SWING POS line reports pos %v, want the leg's destination %v:\n%s",
			last.pos, goal, logs.String())
	}
	t.Logf("a %g-unit leg (own walk time %s) read 54.453s after it began reports elapsed "+
		"%dms - the log's 54453ms cannot be produced any more", legLen, own, last.elapsed)
}

// placeTargetAt moves the zone's captured monster to an absolute point, for the tests above
// that need the client's new leg to be inside the stance's reach before they can ask what
// position the swing's packet carries.
func placeTargetAt(t *testing.T, h *Handler, at [3]int32) {
	t.Helper()
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == capturedTarget {
			h.spawnedMonster[i].X, h.spawnedMonster[i].Y = at[0], at[1]
			h.spawnedMonster[i].Cell = at[2]
			return
		}
	}
	t.Fatalf("the zone is running no monster with handle %d", capturedTarget)
}
