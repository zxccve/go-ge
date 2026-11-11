package zone

import (
	"strings"
	"testing"
	"time"
)

// THE REPEAT ASKS ONCE PER STAND POINT (R111).
//
// THE DEFECT THESE TESTS ARE WRITTEN AGAINST, measured on the owner's own session
// (logs\go_zone1331.log, 2026-11-02): while he dragged the warlock away from a monster she
// was fighting, the repeat ordered a walk BACK to the monster 30 times in 38 seconds
// (22:18:13 -> 22:18:51), 12 of those to the identical point (1779, -529). His words:
// "the keepmode is quite aggressive ... the server teleport the warlock back instead of
// walking her".
//
// WHY IT HAPPENED. The tick's own guard - swingTick's "a walk this engagement already
// asked for owns the next hit" - only holds while the walk is open AND `!w.castDone`. A
// walk whose hit has FIRED is castDone, so the guard stops holding, the tick measures
// again, finds the body still out of reach, and asks approachAttack for the same stand
// point. approachAttack's own IsEqualPos check could not answer it either: that check
// compares against the walk IN FLIGHT, and by then there is none.
//
// WHAT THE REFERENCE DOES instead (`source\geServer\ZServerLib\FieldChar_FSM.cpp:28-80`,
// AS_ATTACK_CHASE): it re-measures on its own 300 ms clock and it only issues a move when
// the computed point DIFFERS from the last one it ordered -
// `if (false == IsEqualPos(moveTo, m_moveTo)) { m_moveTo = moveTo; SetMoveState(MS_MOVE); }`
// - and m_moveTo OUTLIVES the walk. That is the whole rule: equal point, plus 300 ms.
// Nothing here adds a leash distance or any other number.
//
// THE THREE TESTS BELOW SPEND ~4 SECONDS OF WALL TIME IN TOTAL, and they have to: the
// thing being measured is a 300 ms clock compared against time.Now(), so a tick machine
// without a sleep would be asking about a different rule. Every sleep is exactly
// attackChasePace plus a margin, never an invented delay.
const walkReorderMargin = 20 * time.Millisecond

// TestTheRepeatOrdersOneWalkPerStandPoint is the first pass condition: a target out of
// reach, the walk gone (the client's own report / the arrival closed it), and TEN ticks -
// exactly ONE walk order for the unchanged stand point.
//
// TODAY, BEFORE THIS FIX, ALL TEN WOULD ORDER. The stand point is arithmetic on the body
// and the target (`directPlan` answers pointTowards for both), so a body that stands still
// gets the same point every time - and each of those orders is a fresh ZC_MOVE_PC, which
// the client answers by hard-setting its actor: the teleport.
func TestTheRepeatOrdersOneWalkPerStandPoint(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land, so there is no engagement to ask about:\n%s",
			logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the press armed no repeat:\n%s", logs.String())
	}
	_, maxR := stanceReach(t, h, p.sess.Team[0].Stance)

	far := maxR + 75 // past the far bound, inside attackDragSlack: a walk, never an ending
	const ticks = 10
	orders := 0
	for i := 0; i < ticks; i++ {
		// THE WALK IS GONE AND THE BODY HAS NOT MOVED. standAtGap clears sess.Walk (what
		// the arrival and a cancelling click both do) and stands the body on the SAME
		// point, so every tick below measures one stand point and nothing else.
		standAtGap(t, h, p, far)
		time.Sleep(attackChasePace + walkReorderMargin)
		orders += cmdCount(h.swingTick(p.sess), CmdZCMovePC)
	}

	if orders != 1 {
		t.Fatalf("%d tick(s) out of reach of the same stand point produced %d walk "+
			"order(s), want exactly 1 - an unchanged stand point is walked to ONCE "+
			"(IsEqualPos, FieldChar_FSM.cpp:45-52):\n%s", ticks, orders, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the repeat ENDED for being out of reach:\n%s", attackRepeatLog(logs.String()))
	}
	t.Logf("%d ticks %g past MaxR %g, same stand point: 1 walk order, engagement alive",
		ticks, far, maxR)
}

// TestTheRepeatOrdersAgainWhenTheStandPointChanges is the second pass condition: the
// monster steps, so the point the repeat would walk to CHANGES - a second order is issued
// once the 300 ms chase clock allows it, and NOT before.
//
// The first half of this test is the reference's clock (FieldChar_FSM.cpp:38-42 set, :57
// checked) and the second half is its IsEqualPos gate; both are asserted, so a fix that
// silenced the repeat entirely would fail here.
func TestTheRepeatOrdersAgainWhenTheStandPointChanges(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	_, maxR := stanceReach(t, h, p.sess.Team[0].Stance)
	far := maxR + 75

	standAtGap(t, h, p, far)
	if got := cmdCount(h.swingTick(p.sess), CmdZCMovePC); got != 1 {
		t.Fatalf("the first tick sent %d walk(s), want 1:\n%s", got, logs.String())
	}
	first := p.sess.swingOf(0).walkGoal

	// THE MONSTER STEPS, so the ring around it moves and the answer changes.
	h.spawnMu.Lock()
	h.spawnedMonster[0].X += 80
	h.spawnMu.Unlock()

	// INSIDE THE CLOCK: the point has changed, and the order still must not go out. This
	// is the half that keeps two ZC_MOVE_PC for one body off the wire.
	standAtGap(t, h, p, far)
	if got := cmdCount(h.swingTick(p.sess), CmdZCMovePC); got != 0 {
		t.Fatalf("a second order went out %s after the first, inside the reference's %s "+
			"chase clock (m_nextAttackChaseTime, FieldChar_FSM.cpp:38-42):\n%s",
			attackChasePace/4, attackChasePace, logs.String())
	}

	// PAST THE CLOCK: the changed stand point is ordered, once.
	time.Sleep(attackChasePace + walkReorderMargin)
	standAtGap(t, h, p, far)
	if got := cmdCount(h.swingTick(p.sess), CmdZCMovePC); got != 1 {
		t.Fatalf("the changed stand point was not ordered after the chase clock ran out: "+
			"%d walk(s), want 1:\n%s", got, logs.String())
	}
	if second := p.sess.swingOf(0).walkGoal; second == first {
		t.Fatalf("the monster stepped 80 units and the stand point did not move (%v): this "+
			"test cannot tell the two rules apart", second)
	}

	// AND THE NEW POINT IS NOW THE ONE THAT IS HELD: with the clock run out once more, a
	// tick orders nothing.
	time.Sleep(attackChasePace + walkReorderMargin)
	standAtGap(t, h, p, far)
	if got := cmdCount(h.swingTick(p.sess), CmdZCMovePC); got != 0 {
		t.Fatalf("the stand point did not change again, and the tick ordered %d more "+
			"walk(s):\n%s", got, logs.String())
	}
	t.Logf("point 1 %v, point 2 %v: no order inside %s, one after it, and none for the "+
		"unchanged point", first, p.sess.swingOf(0).walkGoal, attackChasePace)
}

// TestARefusedStartIsNotRetriedInsideTheChaseClock is the third pass condition: when the
// approach answers `ok=false` - the character's own start is not ground, Plan's ONLY
// remaining refusal - the next ticks with the same inputs do NOT ask again inside the
// reference's window: one line, then silence for 300 ms.
//
// THE MEASUREMENT THIS COMES FROM is the tail of the same 38 seconds: three
// `ATTACK APPROACH OUT OF REACH ...` lines 200 ms apart (22:18:49.706, .905, .084) - one
// per tick, for an answer that could not have changed between them.
//
// IT USED TO BE THE CLAMP HIT (2026-11-10). A clamp hit answered `overlap` and the
// approach refused it; that refusal is gone - it is a WALK at the target's own position
// (attack_approach.go, `case overlap`), so the only answer left that is not a walk is
// "this start is not ground".
func TestARefusedStartIsNotRetriedInsideTheChaseClock(t *testing.T) {
	h, logs, p := attackZone(t)
	// A path engine whose only answer is Plan's refusal: the start is not ground.
	h.Plan = func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64,
		bool, bool, error) {
		return npc, nil, 0, false, false, nil
	}
	const refusal = "is not valid ground to walk from"

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	_, maxR := stanceReach(t, h, p.sess.Team[0].Stance)

	standAtGap(t, h, p, maxR+75)
	out := h.swingTick(p.sess)
	if got := cmdCount(out, CmdZCMovePC); got != 0 {
		t.Fatalf("a refused start sent %d walk(s):\n%s", got, logs.String())
	}
	if n := strings.Count(logs.String(), refusal); n != 1 {
		t.Fatalf("the first refusal wrote %d line(s), want 1:\n%s", n, logs.String())
	}

	// THE SAME INPUTS, TWICE, INSIDE THE WINDOW: no second ask, and therefore no second
	// line and no walk.
	standAtGap(t, h, p, maxR+75)
	_ = h.swingTick(p.sess)
	standAtGap(t, h, p, maxR+75)
	if got := cmdCount(h.swingTick(p.sess), CmdZCMovePC); got != 0 {
		t.Fatalf("a refused start was answered %s later with a walk:\n%s",
			attackChasePace/2, logs.String())
	}
	if n := strings.Count(logs.String(), refusal); n != 1 {
		t.Fatalf("the refusal was asked again inside the %s chase clock: %d line(s), want "+
			"1:\n%s", attackChasePace, n, logs.String())
	}

	// PAST THE CLOCK it may ask again - once, and it says so.
	time.Sleep(attackChasePace + walkReorderMargin)
	standAtGap(t, h, p, maxR+75)
	if got := cmdCount(h.swingTick(p.sess), CmdZCMovePC); got != 0 {
		t.Fatalf("a refused start sent %d walk(s) after the clock:\n%s", got,
			logs.String())
	}
	if n := strings.Count(logs.String(), refusal); n != 2 {
		t.Fatalf("after the chase clock the refusal answered %d time(s), want 2 (once per "+
			"window):\n%s", n, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("a refused start ended the engagement:\n%s",
			attackRepeatLog(logs.String()))
	}
	t.Logf("refused start: 1 line and 0 walks per %s window, engagement alive",
		attackChasePace)
}
