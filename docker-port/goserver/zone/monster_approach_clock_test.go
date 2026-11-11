package zone

// THE APPROACH'S CLOCK - the tests for the tick that turns a script's attack order into
// a 3550 (zone\monster_order.go: monsterApproachTick / monsterApproachStep), the read
// WAKE that lets that tick run while the client is silent (zone\zone.go Serve,
// Handler.sessionClockAwake), the ONE ORDER PER TARGET POINT rule, and the two ways an
// approach ends when its target is gone.
//
// WHY THESE EXIST, IN ONE MEASURED SENTENCE: `logs\zone_go_script.log` (map 1331,
// `-monai script`) shows a `StartAttack` at 08:19:26.906 whose first 3550 left at
// 08:19:29.021 - 2.1 s, not the 0.1 s poll - a tick that sent nothing at all from
// 08:19:41.514 until the owner's next frame, four bodies' 3550s in one millisecond every
// 300 ms for 12 s, and four engagements that only ended when the player RECONNECTED at
// 08:22:32 as a new handle. Four causes, four tests, and one test for the go-mode
// counterproof.
//
// THE FIXTURE IS THE ORDER SUITE'S (`orderZone`): a real class-11591 row, two real
// sessions, the ordinary fan-out, and a STUB plan that answers the stand-off point. The
// stub is the card's own requirement: with the map's mesh out of the picture, what the
// tests measure is the WIRING and not the path engine.

import (
	"math"
	"strings"
	"testing"
	"time"
)

// approachClock drives the APPROACH'S OWN clock - not the swing's, not the chase's - for
// `span` at the zone's poll interval, in real time, because the leg is drawn from speed x
// elapsed rather than from a tick number. It stops early when `until` answers true.
func approachClock(h *Handler, span time.Duration, until func() bool) {
	deadline := time.Now().Add(span)
	for {
		h.monsterApproachTick(time.Now())
		if until != nil && until() {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(pollInterval)
	}
}

// TestApproachFirstWalkLeavesInsideTheAttack is pass condition (a): with a stub plan and
// a script order, the FIRST 3550 is on the wire in the same pass as the order - well
// inside 500 ms - and it was the approach's own clock that put it there (no swing tick,
// no chase tick, no script loop is called by this test).
//
// THE ORDER OF OPERATIONS IS THE POINT. `StartAttack` stores the target and sends
// nothing (stage 3's pass condition, unchanged); ONE `monsterApproachTick` then plans and
// sends. In the running server that tick is the same `tickIfDue` pass the script's thread
// was resumed in (zone\chase.go), so the live gap is the plan's own cost and nothing else.
func TestApproachFirstWalkLeavesInsideTheAttack(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	target := victim.sess.Team[0].Handle

	started := time.Now()
	h.StartMonsterAttack(mon.Handle, target)
	h.monsterApproachTick(time.Now())
	elapsed := time.Since(started)

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 in the order's own pass, want 1 - a scripted order must walk "+
			"on the clock that owns it, not on a later client frame:\n%s",
			len(moves), logs.String())
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("the first walk took %s from StartMonsterAttack, want under 500 ms "+
			"(the reference re-asks every 0.3 s, FieldChar_FSM.cpp:62)", elapsed)
	}
	t.Logf("first 3550: %s after StartMonsterAttack, on ONE approach tick (this host's "+
		"clock granularity is ~0.5 ms, so a zero means the same pass)", elapsed)
	_, goal, speed := decodeMonsterMove(t, moves[0])
	if goal != [3]int32{244, 0, 29} {
		t.Fatalf("the first walk goes to %v, want the stand point (244, 0, 29) - 356 from "+
			"the target's centre, the same ring the go chase uses", goal)
	}
	if speed != 143 {
		t.Fatalf("the first walk carries %g u/s, want the row's MON_MSPD 143", speed)
	}
	// THE CLOCK WAS THE APPROACH'S: no swing, no chase, and no second line about one
	// order. This is what makes the elapsed time above a measurement of the WIRING.
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 in the order's pass, want 0 - the body is 500 units of clear "+
			"ground away:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "MONSTER CHASE") {
		t.Fatalf("the go chase ran in script mode:\n%s", logs.String())
	}
	if n := strings.Count(logs.String(), "MONSTER APPROACH"); n != 1 {
		t.Fatalf("%d MONSTER APPROACH line(s) for one order, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "zone: PLAN 0x") {
		t.Fatalf("the plan's own cost was not written - the next run cannot tell the "+
			"CLOCK from the path engine:\n%s", logs.String())
	}
}

// TestTickIfDueWalksABodyTheSwingTickCannotSee is pass condition (a) at the WIRING: the
// approach runs on `tickIfDue`'s pass even for a body `monsterSwingTick` never asks
// about, because that tick skips every row whose attack power is 0 before it reaches
// `monsterMaySwing`.
//
// THE FIXTURE IS THE REASON THE CLOCK EXISTS. Stage 3b stepped the approach from the
// swing's per-body question; a body a script ordered to attack with a row that names no
// attack (Wpn 0 -> `monsterATK` 0) would never take a step, which is a property of the
// SWING's filter and not of the order. `monsterApproachTick` has no such filter, so the
// 3550 goes out; remove it from `tickIfDue` and this test answers 0 packets.
func TestTickIfDueWalksABodyTheSwingTickCannotSee(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	// THE ROW NAMES NO ATTACK POWER: monsterSwingTick's first filter skips this body.
	h.spawnedMonster[0].ATK = 0
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)

	if _, ran := h.tickIfDue(victim.sess); !ran {
		t.Fatalf("tickIfDue did not run at all in script mode:\n%s", logs.String())
	}
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 through tickIfDue for a body the swing tick skips, want 1 - "+
			"the approach has its own clock (zone\\chase.go, monsterApproachTick):\n%s",
			n, logs.String())
	}
	if n := len(swingsOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3564 from a body with no attack power, want 0", n)
	}
}

// TestApproachKeepsTheLegInFlightWhileTheTargetStands is the fix for the measured 4 x
// 3550 every 300 ms: while the target's point is unchanged and the leg is still walking,
// the approach does NOT re-plan, whatever the plan's goal would answer.
//
// THE STUB IS THE REAL ENGINE'S BEHAVIOUR, MINIMISED: `PathEngine.Plan` derives the
// stand-off point from the BODY'S OWN POSITION (`pointTowards(target, body, standoff)`),
// so on a route that bends every re-plan answers a goal a few units different from the
// last and `planMonsterWalk`'s goal comparison never fires. The stub below adds the
// body's own x to the goal's y for exactly that reason. ONE 3550 for the whole walk is
// the reference's `IsEqualPos(moveTo, m_moveTo)` (FieldChar.cpp:946-949); four in twelve
// seconds is a client being told to restart a walk it is already drawing.
func TestApproachKeepsTheLegInFlightWhileTheTargetStands(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		goal := pointTowards(target, from, standoff)
		goal[1] += from[0] / 10 // derived from `from`: a new goal on every re-plan
		route := math.Hypot(float64(goal[0]-from[0]), float64(goal[1]-from[1]))
		return goal, nil, route, false, true, nil
	}
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)

	approachClock(h, 1500*time.Millisecond, nil)

	moves := movesOf(victim.conn)
	if len(moves) != 1 {
		t.Fatalf("%d x 3550 for one still target over 1.5 s, want 1 - the leg in flight "+
			"is kept until the target MOVES (FieldChar.cpp:946-949, and the measured "+
			"4-per-300 ms storm in logs\\zone_go_script.log):\n%s", len(moves), logs.String())
	}
	body := h.runtimeMonsters()[0]
	if body.X <= 0 {
		t.Fatalf("the body never moved (x %d) - this test cannot tell a kept leg from a "+
			"missing one", body.X)
	}
	if n := strings.Count(logs.String(), "MONSTER APPROACH"); n != 1 {
		t.Fatalf("%d MONSTER APPROACH line(s), want 1 - one order, one line:\n%s",
			n, logs.String())
	}
	if n := strings.Count(logs.String(), "zone: PLAN 0x"); n != 1 {
		t.Fatalf("%d plan line(s), want 1: a kept leg must not ask the path engine again:\n%s",
			n, logs.String())
	}
}

// TestApproachReplansWhenTheTargetMoves is the other half of the rule above: a target
// that MOVED is a new stand point, and the walk is re-ordered onto the ring around the
// NEW point - still at the row's own MSPD.
func TestApproachReplansWhenTheTargetMoves(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)
	if n := len(movesOf(victim.conn)); n != 1 {
		approachClock(h, 500*time.Millisecond, func() bool { return len(movesOf(victim.conn)) >= 1 })
	}
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("the approach never ordered its first walk:\n%s", logs.String())
	}

	// THE TARGET WALKS 100 UNITS EAST, mid-approach, while the body is still walking.
	time.Sleep(200 * time.Millisecond)
	cullPlace(h, victim, [3]int32{700, 0, 29})
	approachClock(h, 700*time.Millisecond, func() bool { return len(movesOf(victim.conn)) >= 2 })

	moves := movesOf(victim.conn)
	if len(moves) != 2 {
		t.Fatalf("%d x 3550 after the target moved, want 2 - the stand point around a "+
			"moved target is a different point:\n%s", len(moves), logs.String())
	}
	_, goal, speed := decodeMonsterMove(t, moves[1])
	if d := flatDist(goal, [3]int32{700, 0, 29}); math.Abs(d-356) > 2 {
		t.Fatalf("the re-ordered walk goes to %v, %g from the target's NEW point - want the "+
			"356-unit ring (MaxR 306 + both 50-unit bodies)", goal, d)
	}
	if speed != 143 {
		t.Fatalf("the re-ordered walk carries %g u/s, want 143", speed)
	}
	if n := strings.Count(logs.String(), "MONSTER APPROACH"); n != 1 {
		t.Fatalf("%d MONSTER APPROACH line(s), want 1 - a re-plan owes no second line:\n%s",
			n, logs.String())
	}
}

// TestApproachEndsOnTheTickThatFindsTheTargetGone is pass condition (c) for a target that
// simply LEAVES: the first step that cannot find it ends the approach, freezes the leg
// where the body got to, and writes ONE line saying why.
func TestApproachEndsOnTheTickThatFindsTheTargetGone(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)
	approachClock(h, 500*time.Millisecond, func() bool { return len(movesOf(victim.conn)) >= 1 })
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("the approach never ordered a walk:\n%s", logs.String())
	}

	// THE TARGET IS GONE: out of the roster, with no leave hook run - the TICK must
	// notice, which is the reference's own rule (a NULL target clears the chase,
	// FieldChar_FSM.cpp:42-45).
	h.Players.Remove(victim.sess)
	h.monsterApproachTick(time.Now())

	order, _ := h.MonsterOrderOf(mon.Handle)
	if !order.approach.ended {
		t.Fatalf("the approach did not end on the tick that found the target gone: %+v",
			order.approach)
	}
	if order.approach.chase.leg.active {
		t.Fatalf("the leg is still walking after the target left: %+v", order.approach.chase.leg)
	}
	if n := strings.Count(logs.String(), "ENDS:"); n != 1 {
		t.Fatalf("%d ENDS line(s), want exactly 1 - an approach that ends says why:\n%s",
			n, logs.String())
	}
	if !strings.Contains(logs.String(), "the target is no longer a live body") {
		t.Fatalf("the ENDS line does not name the reason:\n%s", logs.String())
	}

	// AND NOTHING WALKS OR SWINGS AFTER IT.
	body := h.runtimeMonsters()[0]
	frozen := [3]int32{body.X, body.Y, body.Cell}
	approachClock(h, 700*time.Millisecond, nil)
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 after the target left, want still 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "ENDS:"); n != 1 {
		t.Fatalf("%d ENDS line(s) after later ticks, want still 1 - one per end:\n%s",
			n, logs.String())
	}
	body = h.runtimeMonsters()[0]
	if got := [3]int32{body.X, body.Y, body.Cell}; got != frozen {
		t.Fatalf("the body walked from %v to %v after its target left", frozen, got)
	}
}

// TestApproachEndsWhenTheTargetLeavesTheZoneWithoutATick is the same end for the case
// the zone's clock CANNOT cover: the last player leaves, so there is no session left to
// tick. `leaveZone` ends every approach that named one of the leaving handles, in the
// same call, with the same ONE line - the measured run instead waited 2m46s for a
// reconnect (logs\zone_go_script.log, 08:19:46 -> 08:22:32).
func TestApproachEndsWhenTheTargetLeavesTheZoneWithoutATick(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = true
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)
	approachClock(h, 500*time.Millisecond, func() bool { return len(movesOf(victim.conn)) >= 1 })

	// THE LOGOUT, exactly as Serve's defer runs it. No tick is taken afterwards.
	h.leaveZone(victim.sess)

	order, _ := h.MonsterOrderOf(mon.Handle)
	if !order.approach.ended {
		t.Fatalf("the approach survived its target's leave - with no session left there "+
			"is no tick to notice it: %+v", order.approach)
	}
	if n := strings.Count(logs.String(), "ENDS:"); n != 1 {
		t.Fatalf("%d ENDS line(s) at the logout, want exactly 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "left the field") {
		t.Fatalf("the ENDS line does not name the leave:\n%s", logs.String())
	}
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 at the logout, want still 1 - a leaving target is not walked "+
			"towards:\n%s", n, logs.String())
	}
}

// TestGoModeNeedsNoApproachClock is pass condition (d): in `-monai go` the new clock is a
// boolean read - it sends nothing, writes no line, and moves no body - so the Go chase's
// behaviour is the behaviour it had before this card.
func TestGoModeNeedsNoApproachClock(t *testing.T) {
	h, logs, victim, _, mon := orderZone(t, [3]int32{600, 0, 29}, [3]int32{0, 3000, 29})
	h.MonsterAIScript = false
	// An order is stored even in go mode (the bindings do not know the mode); the
	// approach must still not act on it.
	h.StartMonsterAttack(mon.Handle, victim.sess.Team[0].Handle)

	approachClock(h, 400*time.Millisecond, nil)

	if n := len(movesOf(victim.conn)); n != 0 {
		t.Fatalf("%d x 3550 from the approach clock in go mode, want 0 - `-monai go` must "+
			"be byte-identical:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "MONSTER APPROACH") || strings.Contains(logs.String(), "zone: PLAN") {
		t.Fatalf("the approach wrote a line in go mode:\n%s", logs.String())
	}
	if body := h.runtimeMonsters()[0]; body.X != 0 || body.Y != 0 {
		t.Fatalf("the approach clock moved a go-mode body to (%d, %d)", body.X, body.Y)
	}
	// THE GO CHASE STILL WALKS THE SAME BODY THROUGH ITS OWN CLOCK.
	h.monsterChaseTick(time.Now())
	if n := len(movesOf(victim.conn)); n != 1 {
		t.Fatalf("%d x 3550 from the go chase after the approach clock did nothing, want "+
			"1 - the chase is the fallback and it is untouched:\n%s", n, logs.String())
	}
}

// TestSessionClockAwakeKeepsGoModeAsItWas pins the read WAKE, which is the other half of
// the measured stall: `tickIfDue` may run, but a session whose read is blocked on the
// socket never reaches it. Script mode must wake at the poll interval whatever the client
// is doing; go mode must answer exactly what it answered before this card (its session
// work, a pending death, the sight ring, -diag-distance) and nothing else.
func TestSessionClockAwakeKeepsGoModeAsItWas(t *testing.T) {
	h, _ := castZone(t)
	// A MAP WITH NO SIGHT ROW: the ring is not one of the reasons here, so the only
	// reason left is the one under test.
	sightUseTable(t, map[int]SightRow{})
	bare := &Session{skipped: map[uint16]int{}}

	h.MonsterAIScript = false
	if h.sessionClockAwake(bare) {
		t.Fatalf("go mode with nothing pending woke its read - that would change when " +
			"the Go chase runs, which this card may not do")
	}
	h.MonsterAIScript = true
	if !h.sessionClockAwake(bare) {
		t.Fatalf("script mode with nothing pending did NOT wake its read - the approach, " +
			"the script's thread and the swing would then run only on client frames, " +
			"which is the measured 2.1 s first walk and the 5 s silence")
	}
	// THE OLD REASONS STILL ANSWER TRUE IN GO MODE, unchanged.
	h.MonsterAIScript = false
	bare.Walk = &walk{}
	if !h.sessionClockAwake(bare) {
		t.Fatalf("go mode with an open walk stopped waking its read")
	}
}
