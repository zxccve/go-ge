package zone

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE NORMAL ATTACK'S REACH IS EDGE TO EDGE, AND ITS STAND POINT IS THE OUTER RING
// (R91 round 5).
//
// The reference, quoted from `source/geServer/ZServerLib/`:
//
//	len = GetDistance(m_pos, pos) - (GetDomainRadius() + domainSize);
//	                                              FieldChar.cpp:118, IsAttackArea
//	int dist = (maxR + obj->GetDomainRadius());
//	                                              PlayerChar.cpp:1393, GetAttackMaxPos
//	if (false == IsEqualPos(moveTo, m_moveTo)) { ... }   FieldChar_FSM.cpp:74-78
//
// Three things follow, and each one is a test below:
//
//	1. two bodies 220 apart with a 50 and a 60 between them lie 110 of clear ground
//	   apart, so a 200 reach LANDS that hit. A centre-to-centre test refuses it.
//	2. the walk's destination is 90% of (MaxR + the target's body) from the target's
//	   CENTRE - 234 for a 200 reach and a 60 body, not 260 - because the goal is the
//	   PATH ENGINE's answer and the engine's own answer can sit a few units out (R99).
//	3. a press for the same target while that walk runs keeps the walk and its hit:
//	   the same destination is not a new request, so nothing is re-planned.
//	4. a path engine answer PAST the reach is pulled in, and a re-walk aims closer than
//	   the point that just failed - the owner's treadmill (R99, below).
//
// The numbers 50 and 60 are the two bodies the brief for this round fixes (a player is
// the reference's S shape = 50; 60 is the test's own monster so the arithmetic cannot
// be confused with a round 100). The REACH is read from the stance row, never written
// into the test as a range.

// moves counts the ZC_MOVE_PC packets in one answer, so a test can say "one walk, not
// two" without caring what else was sent.
func moves(out [][]byte) int {
	var n int
	for _, pkt := range out {
		if binary.LittleEndian.Uint16(pkt) == CmdZCMovePC {
			n++
		}
	}
	return n
}

// ringStance is the stance these tests press in: `Rapiere`, MinR 0 .. MaxR 200, AtkType
// Single - a 200 reach with a shape this build builds. The row is CHECKED, not assumed.
const ringStance = uint32(10370)

// ringTargetBody is the monster's half of the reach in these tests: a 60-unit body, set
// on the monster's own row so the handler reads it the way it reads the shipped table.
const ringTargetBody = 60.0

// ringZone stands character 0 at `from`, in ringStance, with a 200-unit reach and a
// 60-unit monster - the two numbers every test here measures against. It fails loudly
// when the stance row is not what this file is written against.
func ringZone(t *testing.T, from [3]int32) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := attackZoneAt(t, ringStance, from)
	if h.Stances == nil {
		t.Fatal("this zone has no stance table")
	}
	row := h.Stances.Combat(ringStance)
	if row.MinR != 0 || row.MaxR != 200 {
		t.Fatalf("stance %d is MinR %g .. MaxR %g in datatable_stance.xml, and this file "+
			"is written against 0 .. 200", ringStance, row.MinR, row.MaxR)
	}
	// THE MONSTER'S BODY, from the table the zone reads. Radius wins over Size inside
	// BodyRadius, so a 60 sits on the row without inventing a fifth shape.
	h.Monsters = newMonsters(map[uint32]MonsterRow{
		scarecrow().ClassID: {ID: scarecrow().ClassID, Name: "Monster_Hersu01",
			Size: "S", Radius: ringTargetBody},
	})
	return h, logs, p
}

// TestNormalAttackReachIsMeasuredEdgeToEdge is (1) the brief's EDGE RANGE: centres 220
// apart, MaxR 200, bodies 50 and 60, so 110 of clear ground - IN range, and the hit
// goes out. Before this round the check was centre to centre, and 220 > 200 refused a
// hit the game lands.
func TestNormalAttackReachIsMeasuredEdgeToEdge(t *testing.T) {
	// 220 units from the Scarecrow along y: centre to centre.
	far := [3]int32{scarecrow().X, scarecrow().Y - 220, scarecrow().Cell}
	h, logs, p := ringZone(t, far)

	centres := math.Hypot(float64(far[0]-scarecrow().X), float64(far[1]-scarecrow().Y))
	gap := centres - (PCBodyRadius + ringTargetBody)
	if centres <= 200 {
		t.Fatalf("the fixture stands %g apart, which is inside MaxR 200 - this test needs "+
			"a distance the CENTRE rule refuses", centres)
	}
	if gap > 200 || gap < 0 {
		t.Fatalf("the fixture's clear ground is %g, which is not inside MinR 0 .. MaxR 200 "+
			"- this test cannot be about an in-reach press", gap)
	}

	out := pressAttack(t, h, p.sess, capturedTarget)
	if len(out) != 2 {
		t.Fatalf("%g of centres apart (%g of clear ground) produced %v, want the hit and "+
			"the HP - a 200 reach measured edge to edge covers %g:\n%s", centres, gap,
			kinds(out), gap, logs.String())
	}
	if got := kinds(out); got[0] != CmdZCAttackSingle || got[1] != CmdZCAddHP {
		t.Fatalf("the answer is %v, want %d then %d", got, CmdZCAttackSingle, CmdZCAddHP)
	}
	if p.sess.Walk != nil {
		t.Errorf("an in-reach press started a walk (%s)", p.sess.Walk.describe())
	}
	t.Logf("centres %g, clear ground %g, MaxR 200 -> the hit goes out", centres, gap)
}

// destOf reads the destination out of a ZC_MOVE_PC: bytes 10/14/18, the client's own
// WORLDPOS (x, y, cell), the same three fields every approach test decodes.
func destOf(pkt []byte) [3]int32 {
	return [3]int32{
		int32(binary.LittleEndian.Uint32(pkt[10:])),
		int32(binary.LittleEndian.Uint32(pkt[14:])),
		int32(binary.LittleEndian.Uint32(pkt[18:])),
	}
}

// TestNormalAttackStandPointIsTheOuterRing is (2) the brief's THE RING: the walk's
// destination must be 90% of MaxR + the target's body from the target's centre - 234
// here (0.9 x 260). R91 round 5 put it on the ring itself (260); R99 took the 10% back,
// because the goal is the path engine's answer and the owner's engine answered 6.5 units
// PAST a 200 reach. Since R91 round 6 the destination is read from the PRESS's own move
// packet, because the press is what sends the walk.
func TestNormalAttackStandPointIsTheOuterRing(t *testing.T) {
	far := [3]int32{scarecrow().X, scarecrow().Y + 400, scarecrow().Cell}
	h, logs, p := ringZone(t, far)
	h.Plan = directPlan

	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 1 {
		t.Fatalf("an out-of-reach press produced %v, want exactly one move:\n%s",
			kinds(out), logs.String())
	}
	move := recvs(out)[0]
	if move.cmd != CmdZCMovePC {
		t.Fatalf("the press sent cmd %d, want %d ZC_MOVE_PC", move.cmd, CmdZCMovePC)
	}
	dest := destOf(move.b)
	got := math.Hypot(float64(dest[0]-scarecrow().X), float64(dest[1]-scarecrow().Y))
	want := 200.0 + ringTargetBody
	if math.Abs(got-want) > 1.5 {
		t.Fatalf("the walk's destination %v is %g from the target's centre, want %g "+
			"(90%% of MaxR 200 + the target's body %g). The old ring stood at %g:\n%s",
			dest, got, want, ringTargetBody, 200.0+ringTargetBody, logs.String())
	}

	// And the line says which ring it chose, so a reader can see the number.
	if line := lineWith(logs.String(), "ATTACK APPROACH"); !strings.Contains(line,
		"body 60") {
		t.Errorf("the approach line does not name the ring it walked to: %s", line)
	}
	t.Logf("destination %v is %g from the target's centre (want %g, old ring %g)", dest,
		got, want, 200.0+ringTargetBody)
}

// TestASecondPressForTheSameTargetSendsNothingAndKeepsTheWalk is the reference's re-use
// rule (R91 round 6): a press that arrives while the body already walks to that same
// stand point sends NOTHING and keeps the walk AND its hit. The reference does nothing
// when the point it would walk to equals the one in flight (FieldChar_FSM.cpp:74-78) and
// re-decides at most every 300 ms (m_nextAttackChaseTime, :38-42): 100 ms in, the clock
// alone is enough to answer "nothing to do".
func TestASecondPressForTheSameTargetSendsNothingAndKeepsTheWalk(t *testing.T) {
	far := [3]int32{scarecrow().X, scarecrow().Y + 400, scarecrow().Cell}
	h, logs, p := ringZone(t, far)
	h.Plan = directPlan

	first := pressAttack(t, h, p.sess, capturedTarget)
	if moves(first) != 1 {
		t.Fatalf("the first press produced %d move(s), want 1:\n%s", moves(first),
			logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the press left no walk with a pending attack:\n%s", logs.String())
	}
	goal := w.goal

	// THE SECOND PRESS, 100 ms into the walk, for the SAME target.
	time.Sleep(100 * time.Millisecond)
	second := pressAttack(t, h, p.sess, capturedTarget)
	if moves(second) != 0 {
		t.Fatalf("a second press for the same target sent %d move(s), want 0 - the "+
			"destination is already in flight:\n%s", moves(second), logs.String())
	}
	if moves(first)+moves(second) != 1 {
		t.Fatalf("two presses produced %d move packet(s) in total, want exactly 1:\n%s",
			moves(first)+moves(second), logs.String())
	}
	w = p.sess.Walk
	if w == nil {
		t.Fatalf("the second press dropped the walk it should have kept:\n%s",
			logs.String())
	}
	if w.goal != goal {
		t.Errorf("the second press moved the destination from %v to %v", goal, w.goal)
	}
	if w.castDone {
		t.Errorf("the second press marked the walk's hit as DROPPED - the character would "+
			"arrive at the ring and never swing:\n%s", logs.String())
	}
	line := lineWith(logs.String(), "keeping the walk")
	if line == "" {
		t.Fatalf("the second press logged no re-use line:\n%s", logs.String())
	}
	if !strings.Contains(line, "same stand point") {
		t.Errorf("the re-use line does not say the stand point was the same: %s", line)
	}
	t.Logf("the second press sent %d move(s): %s", moves(second), line)
}

// TestAnOutOfReachPressWalksToTheOuterRingAtOnce is the round-6 rule in one test: one
// out-of-reach press produces the ONE walk to the outer ring IMMEDIATELY - no tick, no
// sleep, no waiting for the client (whose own AI never walks to a clicked enemy:
// attacker.scp:135-170 calls SCR_AI_LIB_ATTACKTARGET, lib.scp only attacks or sleeps).
func TestAnOutOfReachPressWalksToTheOuterRingAtOnce(t *testing.T) {
	far := [3]int32{scarecrow().X, scarecrow().Y + 400, scarecrow().Cell}
	h, logs, p := ringZone(t, far)
	h.Plan = directPlan

	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 1 {
		t.Fatalf("one out-of-reach press produced %d move(s), want exactly 1, at once:\n%s",
			moves(out), logs.String())
	}
	dest := destOf(recvs(out)[0].b)
	got := math.Hypot(float64(dest[0]-scarecrow().X), float64(dest[1]-scarecrow().Y))
	want := 200.0 + ringTargetBody
	if math.Abs(got-want) > 1.5 {
		t.Fatalf("the press walked to %v, %g from the target's centre, want %g (90%% of "+
			"MaxR 200 + the target's body %g):\n%s", dest, got, want, ringTargetBody,
			logs.String())
	}
	t.Logf("one press -> %d move to the margin ring, %g from the target's centre "+
		"(want %g)", moves(out), got, want)
}

// TestTheRingWalkFiresTheHitOnceWhenItEnds: the press's walk owes ONE hit, and it fires
// at the end of that walk's own clock - 3564 then 3557, once (R91 round 6; the walk and
// its clock are the skill approach's, zone\chase.go).
func TestTheRingWalkFiresTheHitOnceWhenItEnds(t *testing.T) {
	far := [3]int32{scarecrow().X, scarecrow().Y + 400, scarecrow().Cell}
	h, logs, p := ringZone(t, far)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}

	walkFor(t, p.sess)
	out := h.chaseTick(p.sess)
	if got := kinds(out); len(got) != 2 || got[0] != CmdZCAttackSingle || got[1] != CmdZCAddHP {
		t.Fatalf("the end of the walk produced %v, want %d then %d:\n%s", got,
			CmdZCAttackSingle, CmdZCAddHP, logs.String())
	}
	// ONE HIT, however many ticks run: the stop clock that follows must not swing again.
	for _, pkt := range h.chaseTick(p.sess) {
		if binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			t.Fatalf("a later tick sent a SECOND %d for one press:\n%s",
				CmdZCAttackSingle, logs.String())
		}
	}
	t.Logf("the walk's end sent %v once (one hit)", kinds(out))
}

// TestAClickNamingAnotherDestinationDropsTheWalk: a CZ_MOVE_PC for a different point is
// the player taking the body somewhere else, so the attack walk and its owed hit are
// dropped (cancelApproachAttack, R90 item 4 kept by R91 round 6) and nothing swings at
// the ring afterwards.
func TestAClickNamingAnotherDestinationDropsTheWalk(t *testing.T) {
	far := [3]int32{scarecrow().X, scarecrow().Y + 400, scarecrow().Cell}
	h, logs, p := ringZone(t, far)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	if p.sess.Walk == nil {
		t.Fatalf("the press left no walk:\n%s", logs.String())
	}

	// THE PLAYER CLICKS SOMEWHERE ELSE, next to where the character stands.
	elsewhere := [3]int32{far[0] + 100, far[1], far[2]}
	out, err := h.Reply(context.Background(), p.sess, moveFrame(0, elsewhere))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if p.sess.Walk != nil {
		t.Fatalf("a click for %v left the attack walk in flight (%s):\n%s", elsewhere,
			p.sess.Walk.describe(), logs.String())
	}
	for _, pkt := range out {
		switch binary.LittleEndian.Uint16(pkt) {
		case CmdZCAttackSingle, CmdZCAddHP:
			t.Fatalf("the click that cancelled the walk also sent cmd %d:\n%s",
				binary.LittleEndian.Uint16(pkt), logs.String())
		}
	}
	for _, pkt := range h.chaseTick(p.sess) {
		switch binary.LittleEndian.Uint16(pkt) {
		case CmdZCAttackSingle, CmdZCAddHP:
			t.Fatalf("a tick after the cancel still sent cmd %d:\n%s",
				binary.LittleEndian.Uint16(pkt), logs.String())
		}
	}
	t.Logf("the click cancelled the walk: %d packet(s) from the click, none a hit",
		len(out))
}

// A REPEAT PRESS FOR A WALK ALREADY IN FLIGHT DOES NOTHING (R97).
//
// The owner's session (logs\go_zone1331.log, 15:59-16:01, millisecond stamps) holds 110
// attack presses in two minutes: 97 started a walk-in, 10 landed a hit. The presses are
// ~1 s apart and every one of them re-planned the walk, so the character never arrived
// and the player read it as "attack does not work". The client sends them itself:
// ge\ai\lib.scp:118-131 SCR_AI_LIB_ATTACKTARGET calls Attack() on every AI pass while a
// target is nominated, and its IsAbleToAttack answers with the AI profile's keep range
// (ge\xml\datatable_ai.xml ATTACKER KeepRange 1000), not the stance's 200.
//
// The reference does not re-plan per press: the approach is a STATE that re-decides on
// its own 300 ms clock and only when the destination CHANGED (FieldChar_FSM.cpp:28-80,
// IsEqualPos(moveTo, m_moveTo)). THIS rule is the missing half: a press for the target
// already being walked to is ABSORBED - nothing sent, no cancel, no re-plan - and the
// stand-point equality rule below it stays exactly as it is.
//
// The tests below press 350 ms apart, past attackChasePace's 300 ms, because inside
// that window the older clock already answers "nothing to do" and would hide this rule.
// Each of them also feeds the handler a plan whose stand point MOVES with the body
// (sidestep, below), because that is what a real route does: a navmesh stand point is
// never on the straight line the character started on, so the point a second press
// would plan to is a DIFFERENT point and the equality rule alone lets it through.

// attackRingFar is where these tests stand the character: 400 units from the Scarecrow
// along y, so the press is out of the 200 reach and a walk is what the press asks for.
var attackRingFar = [3]int32{scarecrow().X, scarecrow().Y + 400, scarecrow().Cell}

// sidestep puts the character `dx` units to the side of where it stands, leaving the
// target where it is. It models the bend of a real route: mid-walk the body is not on
// the straight line from its start, so the ring point the next press plans from is a
// different point from the one in flight. Without this the two points are equal in
// these fixtures (a straight line stays a straight line) and the OLD equality rule
// would absorb the press as well, which would make these tests pass with no rule at all.
func sidestep(t *testing.T, sess *Session, dx int32) {
	t.Helper()
	at := sess.charPos(0, time.Now())
	at[0] += dx
	sess.moveFrom, sess.Pos, sess.moveAt = at, at, time.Now()
	sess.Team[0].Position = at
}

// absorbedLines returns the log lines this rule writes for the presses it swallowed.
func absorbedLines(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "press(es) absorbed") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// TestARepeatPressForAWalkInFlightSendsNothing is the owner's 97 walks. One press out
// of reach starts exactly one walk; five further presses for the SAME target, each past
// the 300 ms chase clock and each from a body that has moved off the straight line,
// send NOTHING and the first walk survives with the destination and the hit it owes.
func TestARepeatPressForAWalkInFlightSendsNothing(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = directPlan

	first := pressAttack(t, h, p.sess, capturedTarget)
	if moves(first) != 1 {
		t.Fatalf("the first press produced %d move(s), want 1:\n%s", moves(first),
			logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the first press left no walk with a pending attack:\n%s", logs.String())
	}
	// THE WALK THE FIRST PRESS STARTED, BY POINTER. "The same walk" means THIS
	// object, not a fresh one that happens to name the same goal: a replacement copies the
	// goal and gets a new `began`, so an equality test on those two fields alone answers
	// "same" for exactly as long as the two clocks agree - which on this machine's coarse
	// clock they sometimes do for a whole test, and that is how a re-plan went unseen.
	firstWalk := w
	goal := w.goal

	// held is the clock the presses are measured against, read AFTER each hold: the hold
	// below re-anchors `began` ON PURPOSE (walkHasNotAdvanced - the body's point is the
	// walk's own segment, R104), so the value a press must leave alone is the one the
	// fixture wrote, not the one the first press started with.
	var held time.Time

	total := moves(first)
	for i := 0; i < 5; i++ {
		// Past attackChasePace, so the 300 ms clock cannot be what holds this press:
		// only the walk being for this same target can. THE BODY IS HELD AT THE START of
		// its walk, because a press whose body is DRAWN inside the reach is answered with
		// a hit (R104) - and that is the press path's rule, not this one.
		pastChasePace(t, p.sess)
		walkHasNotAdvanced(t, p.sess)
		held = p.sess.Walk.began
		sidestep(t, p.sess, 80)
		out := pressAttack(t, h, p.sess, capturedTarget)
		total += moves(out)
		t.Logf("press %d for the same target: %d move(s), running total %d", i+2,
			moves(out), total)
	}
	if total != 1 {
		t.Fatalf("six presses for one target produced %d move(s), want exactly 1 - the "+
			"presses are ~1 s apart and each one re-planned the walk, which is the "+
			"owner's 97 walks in 110 presses:\n%s", total, logs.String())
	}

	w = p.sess.Walk
	if w == nil {
		t.Fatalf("the repeat presses dropped the walk:\n%s", logs.String())
	}
	if w != firstWalk || w.goal != goal {
		t.Fatalf("a repeat press RE-PLANNED the walk: the first press's walk (%p) walked to "+
			"%v, and the one in flight now is %p, walking to %v - the press must be "+
			"absorbed into the walk it names, not replace it:\n%s", firstWalk, goal, w,
			w.goal, logs.String())
	}
	if !w.began.Equal(held) {
		t.Fatalf("a repeat press RESTARTED the walk's clock: it began %s, and the walk the "+
			"presses were measured against began %s - an absorbed press touches nothing, "+
			"`began` included:\n%s",
			time.Since(w.began).Round(time.Millisecond),
			held.Format("15:04:05.000000"), logs.String())
	}
	if w.castDone {
		t.Fatalf("the walk's hit was marked DROPPED - the character would arrive at the "+
			"ring and never swing:\n%s", logs.String())
	}
	t.Logf("six presses -> %d move(s); the first walk survived at %v with its hit owed",
		total, goal)
}

// TestAPressForADifferentTargetReplacesTheWalk: the rule is per TARGET. A press for
// another monster is a new decision, so it replaces the walk in one packet and the
// packet names the NEW monster's ring, not the old one's.
func TestAPressForADifferentTargetReplacesTheWalk(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = directPlan

	// A SECOND monster, 800 units of clear ground away, on the same cell: a real target
	// the zone is running, so the press is accepted rather than answered with 3568.
	other := scarecrow()
	other.Handle = capturedTarget + 7
	other.Y = scarecrow().Y + 1200
	h.spawnedMonster = append(h.spawnedMonster, other)
	h.monsterHP[other.Handle] = 5000

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press for the first target produced %d move(s), want 1:\n%s",
			moves(out), logs.String())
	}
	old := p.sess.Walk
	if old == nil {
		t.Fatalf("the first press left no walk:\n%s", logs.String())
	}

	time.Sleep(350 * time.Millisecond)
	out := pressAttack(t, h, p.sess, other.Handle)
	if moves(out) != 1 {
		t.Fatalf("a press for a DIFFERENT target produced %d move(s), want 1 - the walk "+
			"is per target:\n%s", moves(out), logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the press for the other target left no attack walk:\n%s", logs.String())
	}
	if w.atk.target != other.Handle {
		t.Fatalf("the walk is for target %d, want the one just pressed %d:\n%s",
			w.atk.target, other.Handle, logs.String())
	}
	dest := destOf(recvs(out)[0].b)
	got := math.Hypot(float64(dest[0]-other.X), float64(dest[1]-other.Y))
	want := 200.0 + ringTargetBody
	if math.Abs(got-want) > 1.5 {
		t.Fatalf("the new walk's destination %v is %g from the NEW target's centre, "+
			"want %g (90%% of MaxR 200 + the target's body %g):\n%s", dest, got,
			want, ringTargetBody, logs.String())
	}
	if old == w || w.goal == old.goal {
		t.Errorf("the press for the other target kept the old walk (%v)", old.goal)
	}
	t.Logf("the press for target %d replaced the walk: one packet to %v (%g from it)",
		other.Handle, dest, got)
}

// TestTheAbsorptionEndsWhenTheWalkDoes: the rule is not a latch. The absorbed presses
// must not eat the hit the walk owes, and once the walk is over a press plans again -
// here from a body that has walked back out of reach.
func TestTheAbsorptionEndsWhenTheWalkDoes(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	pastChasePace(t, p.sess)
	walkHasNotAdvanced(t, p.sess)
	sidestep(t, p.sess, 80)
	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 0 {
		t.Fatalf("the repeat press sent %d move(s), want 0:\n%s", moves(out),
			logs.String())
	}

	// THE WALK ENDS IN RANGE: the hit it owes fires ONCE.
	fired := p.sess.Walk
	walkFor(t, p.sess)
	out := h.chaseTick(p.sess)
	if got := kinds(out); len(got) != 2 || got[0] != CmdZCAttackSingle || got[1] != CmdZCAddHP {
		t.Fatalf("the end of the walk produced %v, want %d then %d - the absorbed "+
			"presses must not eat the hit:\n%s", got, CmdZCAttackSingle, CmdZCAddHP,
			logs.String())
	}
	for _, pkt := range h.chaseTick(p.sess) {
		if binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			t.Fatalf("a later tick sent a SECOND %d for one walk:\n%s",
				CmdZCAttackSingle, logs.String())
		}
	}

	// THE WALK THE HIT WAS FIRED FOR IS OVER (piece 2d: it ended on its own arrival). The
	// attack REPEAT may have planned a walk of its own on that second tick - that is the
	// swing keeping its engagement alive (R106), not the walk this test pressed for.
	if p.sess.Walk == fired {
		t.Fatalf("the walk the hit was fired for is still the session's walk: its arrival "+
			"did not close it: %s", logs.String())
	}
	p.sess.Walk = nil
	p.sess.moveFrom, p.sess.Pos, p.sess.moveAt = attackRingFar, attackRingFar, time.Now()
	p.sess.Team[0].Position = attackRingFar

	again := pressAttack(t, h, p.sess, capturedTarget)
	if moves(again) != 1 {
		t.Fatalf("a press after the walk ended produced %d move(s), want 1 - the rule "+
			"must not latch forever:\n%s", moves(again), logs.String())
	}
	t.Logf("the hit fired once, and a press after the walk started 1 new move")
}

// TestAbsorbedPressesAreLoggedAtMostOncePerSecond: a client that presses ten times a
// second must not write ten lines a second. One line per second carries the count of
// the presses it stands for, and the count adds up to every absorbed press.
func TestAbsorbedPressesAreLoggedAtMostOncePerSecond(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}

	// FIVE presses back to back, all past the 300 ms chase clock and all inside one
	// second: the first writes the line, the other four are only counted.
	pastChasePace(t, p.sess)
	walkHasNotAdvanced(t, p.sess)
	sidestep(t, p.sess, 80)
	for i := 0; i < 5; i++ {
		if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 0 {
			t.Fatalf("press %d of the burst sent %d move(s), want 0:\n%s", i+2,
				moves(out), logs.String())
		}
	}
	if got := len(absorbedLines(logs.String())); got != 1 {
		t.Fatalf("five absorbed presses in one second wrote %d line(s), want exactly 1:\n%s",
			got, logs.String())
	}

	// ONE MORE, a second later: the count of what the silent window held must be
	// written, or the log would hide the presses it dropped.
	time.Sleep(1100 * time.Millisecond)
	walkHasNotAdvanced(t, p.sess)
	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 0 {
		t.Fatalf("the sixth press sent %d move(s), want 0:\n%s", moves(out), logs.String())
	}
	lines := absorbedLines(logs.String())
	if len(lines) != 2 {
		t.Fatalf("six absorbed presses wrote %d line(s), want 2 (one per second):\n%s",
			len(lines), logs.String())
	}
	if !strings.Contains(lines[1], "5 press(es) absorbed") {
		t.Errorf("the second line does not count the presses it stands for: %s", lines[1])
	}
	if !strings.Contains(lines[0], fmt.Sprint(capturedTarget)) {
		t.Errorf("the absorbed line does not name the target: %s", lines[0])
	}
	if !strings.Contains(lines[0], "left") {
		t.Errorf("the absorbed line does not say how much walking is left: %s", lines[0])
	}
	t.Logf("six absorbed presses -> %d line(s): %s", len(lines), lines[1])
}

// TestInReachPressSendsTheHitAndNoMove is (4) the brief's IN RANGE = NO WALK: the same
// edge-to-edge press as (1) sends 3564 and 3557 and NO ZC_MOVE_PC, because a body that
// can already reach the target has nothing to walk for.
func TestInReachPressSendsTheHitAndNoMove(t *testing.T) {
	far := [3]int32{scarecrow().X, scarecrow().Y - 220, scarecrow().Cell}
	h, logs, p := ringZone(t, far)

	out := pressAttack(t, h, p.sess, capturedTarget)
	if len(out) == 0 {
		t.Fatalf("an in-reach press sent nothing:\n%s", logs.String())
	}
	for _, pkt := range out {
		if cmd := binary.LittleEndian.Uint16(pkt); cmd == CmdZCMovePC {
			t.Fatalf("an in-reach press sent a ZC_MOVE_PC:\n%s", logs.String())
		}
	}
	if p.sess.Walk != nil {
		t.Errorf("an in-reach press started a walk (%s)", p.sess.Walk.describe())
	}
	if strings.Contains(logs.String(), "APPROACH") {
		t.Errorf("an in-reach press logged an approach:\n%s", logs.String())
	}
	t.Logf("packets: %v", kinds(out))
}

// A WALK THAT ENDS STILL OUT OF REACH WALKS AGAIN (R98).
//
// THE BUG THIS BLOCK IS ABOUT, in the owner's words: "the character stands in front of
// the monster and does not attack". The zone used to answer that moment with one line -
// "nothing sent and no second walk" - and logs\go_zone1331.log holds 92 of them, each
// with the body ~50 units short of its stance's reach.
//
// THE REFERENCE DOES THE OPPOSITE: its attack state re-decides on a 300 ms clock and,
// when IsAttackArea still answers -1, takes GetAttackMaxPos and issues ANOTHER move
// (source2/geServer/ZServerLib/FieldChar_FSM.cpp:28-80, case 1). It never stands still
// out of range.
//
// THE GEOMETRY IS THE FIXTURE'S OWN, and it is written down so a reader can check it:
//
//	the ring is 90% of (MaxR + the target's body) from the CENTRE  234 = 0.9 x 260
//	the reach is EDGE TO EDGE, so standing on that ring leaves     124 of clear ground
//	the monster steps 100 units away during the walk, so the body
//	lands on a point that is now                              224 = 234 + 100 - 110
//
// i.e. 24 of clear ground PAST the 200 reach - the same shape as the owner's 206.5
// against 200, and still past it with the ring's 10% margin in place (R99).
// against a 150 reach.

// moveRingTarget moves the Scarecrow `dy` along y under the spawn lock - the monster
// stepping away while the character walks, which is the event this block is about.
func moveRingTarget(t *testing.T, h *Handler, dy int32) {
	t.Helper()
	h.spawnMu.Lock()
	defer h.spawnMu.Unlock()
	for i := range h.spawnedMonster {
		if h.spawnedMonster[i].Handle == capturedTarget {
			h.spawnedMonster[i].Y += dy
			return
		}
	}
	t.Fatalf("the zone is running no monster with handle %d", capturedTarget)
}

// walkToFire advances an attack approach to the moment its body has ARRIVED - the walk's
// own route time, which is the ONLY thing that decides an arrival now (piece 2d: the eta
// is no longer the arrival, it is the give-up budget, and the arrival is the body's own
// distance - chase.go, walk.arrived).
//
// It is not walkFor because a re-plan test needs the fire to happen while the goal is
// still the one the walk was planned to, and this is the same instant either way: the
// body covers its route in `legLength / speed`, which is the eta ONLY when the plan's
// estimate and the leg agree. Several fixtures here deliberately do not agree (a plan
// that counted a different route), and the body is what the arrival is measured on -
// which is exactly why this helper reads the leg and not the eta field.
func walkToFire(t *testing.T, sess *Session) {
	t.Helper()
	if sess.Walk == nil {
		t.Fatal("there is no walk to advance")
	}
	if sess.Walk.atk == nil {
		t.Fatal("this walk is not an attack approach")
	}
	w := sess.Walk
	own := time.Duration(legLength(w.from, w.goal, w.route) / w.speed * float64(time.Second))
	sess.Walk.began = time.Now().Add(-(own + 20*time.Millisecond))
}

// pastChasePace back-dates the walk's last PLAN past the reference's 300 ms chase clock,
// without waiting: attackChasePace is a pace between plans, and a test that slept for it
// would be measuring the wall clock, not the rule.
func pastChasePace(t *testing.T, sess *Session) {
	t.Helper()
	if sess.Walk == nil {
		t.Fatal("there is no walk to pace")
	}
	sess.Walk.sentAt = time.Now().Add(-(attackChasePace + 50*time.Millisecond))
}

// walkHasNotAdvanced re-anchors the walk's clock to `now`, so the body is still where the
// walk BEGAN and is not inside the reach yet.
//
// IT EXISTS BECAUSE THE BODY'S POINT IS THE WALK'S OWN SEGMENT NOW (R104). Before that, a
// walk moved the body only when something PLACED it, so a test could spend a second of wall
// time on another rule (the absorb pacing) and still have a body out of reach. The body
// really walks now, so a test that spends wall time on something else must say so.
func walkHasNotAdvanced(t *testing.T, sess *Session) {
	t.Helper()
	if sess.Walk == nil {
		t.Fatal("there is no walk to hold at its start")
	}
	sess.Walk.began = time.Now()
}

// walkCarriesTheBodyTo puts the held press's body AT `at`: the walk it is walking is
// re-pointed there and walked out, so the fire measures the point the body has been DRAWN
// at - which is the point the swing's +8 must carry (R104).
//
// A CLIENT MOVE REPORT CANNOT DO THIS ANY MORE. CZ_MOVE_PC names a DESTINATION (move.go,
// openWalk), not "I am standing here", so the body has not walked to it when the report
// arrives; measuring the reach at the report's point was the teleport R104 removes
// (logs\go_zone1331.log, 19:13:21.181: `pos (2392, -770)` against a segment that had drawn
// the body ~600 units away, and the MOVE_PC two lines later naming 2392,-770 as its
// destination).
func walkCarriesTheBodyTo(t *testing.T, sess *Session, at [3]int32) {
	t.Helper()
	if sess.Walk == nil {
		t.Fatal("there is no walk to carry the body")
	}
	sess.Walk.from, sess.Walk.goal = at, at
	sess.Walk.began = time.Now()
}

// arrivalAt is the UPDATE that finds the body at `at`: the walk has carried it there and
// the tick asks the held press the reach question. It replaces reportAt(...) in the tests
// that used a move report to put the body somewhere, because a report no longer says where
// the body is.
func arrivalAt(t *testing.T, h *Handler, sess *Session, at [3]int32) [][]byte {
	t.Helper()
	walkCarriesTheBodyTo(t, sess, at)
	return h.chaseTick(sess)
}

// distanceIn reads the first number between `pre` and `post` in one log line, or 0 when the
// line does not carry it.
func distanceIn(line, pre, post string) float64 {
	i := strings.Index(line, pre)
	if i < 0 {
		return 0
	}
	rest := line[i+len(pre):]
	j := strings.Index(rest, post)
	if j < 0 {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(rest[:j]), 64)
	if err != nil {
		return 0
	}
	return v
}

// countLines is how many log lines contain want: the "ONE line, not two" assertions.
func countLines(logs, want string) int {
	var n int
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, want) {
			n++
		}
	}
	return n
}

// lastLineWith is the LAST log line that contains want. lineWith answers the FIRST, which
// is an earlier round's line when a rule writes one line per repeat.
func lastLineWith(logs, want string) string {
	last := ""
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, want) {
			last = strings.TrimSpace(line)
		}
	}
	return last
}

// TestAWalkThatEndsShortOfTheReachWalksAgain is (1): a walk that ends 50 units past the
// reach sends ONE more move - the reference's second walk - instead of nothing. Before
// this rule the same moment produced 0 packets and the line "nothing sent and no second
// walk", and the character stood in front of the monster for ever.
func TestAWalkThatEndsShortOfTheReachWalksAgain(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	old := p.sess.Walk
	if old == nil || old.atk == nil {
		t.Fatalf("the press left no attack walk:\n%s", logs.String())
	}

	// THE MONSTER STEPS 100 UNITS AWAY WHILE THE CHARACTER WALKS, so the body arrives on
	// the ring it planned for where the target USED to be - 224 of clear ground from
	// where it is now, 24 past the 200 reach.
	moveRingTarget(t, h, -100)
	walkToFire(t, p.sess)
	pastChasePace(t, p.sess)

	out := h.chaseTick(p.sess)
	if moves(out) != 1 {
		t.Fatalf("a walk that ended 24 past the reach produced %v (%d move(s)), want "+
			"exactly ONE more move - the reference walks again here "+
			"(FieldChar_FSM.cpp:28-80, case 1):\n%s", kinds(out), moves(out), logs.String())
	}
	for _, pkt := range out {
		if binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			t.Fatalf("the re-plan also swung at a body it cannot reach:\n%s", logs.String())
		}
	}

	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the re-plan left no walk to arrive with:\n%s", logs.String())
	}
	if w == old {
		t.Errorf("the walk was not re-planned: it is still the one that ended short")
	}
	if w.atk.repeats != 1 {
		t.Errorf("the walk records %d repeat(s), want 1:\n%s", w.atk.repeats, logs.String())
	}

	// THE LINE NAMES THE DISTANCE, THE REACH AND THE REPEAT NUMBER, so the next reading
	// of a log is one grep.
	line := lineWith(logs.String(), "WALKING AGAIN")
	if line == "" {
		t.Fatalf("the re-plan wrote no line:\n%s", logs.String())
	}
	// THE NUMBER IS READ, NOT HARD-CODED: the walk ended on the ring the press planned
	// (R132 moved that ring out to the reference's own maxR + the target's body), so the
	// distance it re-plans from is that goal measured against where the target IS.
	moved := [3]int32{scarecrow().X, scarecrow().Y - 100, scarecrow().Cell}
	gap, _ := attackRangeAllowed(old.goal, moved, 0, 200, PCBodyRadius, ringTargetBody)
	if want := fmt.Sprintf("still %g away", gap); !strings.Contains(line, want) {
		t.Errorf("the re-plan line does not name the distance it re-planned from (%q): %s",
			want, line)
	}
	if !strings.Contains(line, "re-plan 1 of 5") {
		t.Errorf("the re-plan line does not name the repeat number: %s", line)
	}
	t.Logf("a walk that ended 224 from the target (reach 200) sent %d move(s) and no "+
		"hit: %s", moves(out), line)
}

// TestAPressDuringTheReplannedWalkIsAbsorbed is (2): the absorb rule still holds for the
// walk a re-plan started. Two presses for the same target inside it send NOTHING, the
// re-plan is not duplicated, and the walk keeps its destination and its owed hit.
func TestAPressDuringTheReplannedWalkIsAbsorbed(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	moveRingTarget(t, h, -100)
	walkToFire(t, p.sess)
	pastChasePace(t, p.sess)
	if out := h.chaseTick(p.sess); moves(out) != 1 {
		t.Fatalf("the re-plan produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}

	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the re-plan left no walk:\n%s", logs.String())
	}
	goal, began := w.goal, w.began

	// TWO PRESSES DURING THE RE-PLANNED WALK, past the 300 ms chase clock (so only the
	// absorb rule can be what holds them) and from a body that has moved off the line
	// (so the old stand-point equality rule cannot be what holds them either).
	time.Sleep(350 * time.Millisecond)
	sidestep(t, p.sess, 80)
	for i := 0; i < 2; i++ {
		out := pressAttack(t, h, p.sess, capturedTarget)
		if moves(out) != 0 {
			t.Fatalf("press %d during the re-planned walk sent %d move(s), want 0:\n%s",
				i+1, moves(out), logs.String())
		}
	}

	w = p.sess.Walk
	if w == nil {
		t.Fatalf("the presses dropped the walk the re-plan started:\n%s", logs.String())
	}
	if w.goal != goal || !w.began.Equal(began) {
		t.Fatalf("a press during the re-planned walk moved the destination from %v to "+
			"%v:\n%s", goal, w.goal, logs.String())
	}
	if w.atk.repeats != 1 {
		t.Errorf("the presses added to the repeat count: it is %d, want 1:\n%s",
			w.atk.repeats, logs.String())
	}
	if n := countLines(logs.String(), "WALKING AGAIN"); n != 1 {
		t.Fatalf("the attack re-planned %d time(s), want exactly 1 - a press into a walk "+
			"in flight must not duplicate it:\n%s", n, logs.String())
	}
	if n := len(absorbedLines(logs.String())); n == 0 {
		t.Fatalf("the two absorbed presses wrote no line:\n%s", logs.String())
	}
	t.Logf("two presses into the re-planned walk -> 0 move(s), 1 re-plan, goal %v kept",
		goal)
}

// TestTheReplansStopAtTheCapAndAFreshPressCountsAgain is (3): five re-plans that never
// reach the target end the attack with ONE line naming the cap, and a new press starts a
// fresh count - a monster that keeps stepping away must not keep a character walking for
// ever, and a player who presses again must not be capped by history.
func TestTheReplansStopAtTheCapAndAFreshPressCountsAgain(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}

	// THE MONSTER STEPS AWAY BEFORE EVERY ARRIVAL, so every walk ends 50 past the reach:
	// five re-plans, each one a move, never a hit.
	for i := 1; i <= attackWalkReplanCap; i++ {
		moveRingTarget(t, h, -100)
		walkToFire(t, p.sess)
		pastChasePace(t, p.sess)
		out := h.chaseTick(p.sess)
		if moves(out) != 1 {
			t.Fatalf("re-plan %d of %d produced %d move(s), want 1:\n%s", i,
				attackWalkReplanCap, moves(out), logs.String())
		}
		want := fmt.Sprintf("re-plan %d of %d", i, attackWalkReplanCap)
		if line := lastLineWith(logs.String(), "WALKING AGAIN"); !strings.Contains(line, want) {
			t.Fatalf("the re-plan's line does not say %q: %s", want, line)
		}
	}

	// THE NEXT ARRIVAL IS THE STOP: still out of reach, and the cap is spent.
	moveRingTarget(t, h, -100)
	walkToFire(t, p.sess)
	pastChasePace(t, p.sess)
	out := h.chaseTick(p.sess)
	if moves(out) != 0 {
		t.Fatalf("the walk after the cap still sent %d move(s), want 0 - the cap is %d "+
			"re-plans:\n%s", moves(out), attackWalkReplanCap, logs.String())
	}
	const capLine = "so it stops here (cap 5 re-plans per press"
	if n := countLines(logs.String(), capLine); n != 1 {
		t.Fatalf("the cap wrote %d line(s), want exactly 1:\n%s", n, logs.String())
	}
	line := lineWith(logs.String(), capLine)
	// THE DISTANCE IS THE BODY'S OWN (R104), so it is checked against the reach that
	// refuses it and not against a typed number: the body stands where the walk drew it,
	// which is the point the monster stepped away from - not a point a placement wrote.
	if gap := distanceIn(line, " is still ", " away"); gap <= 200 {
		t.Errorf("the cap line says the body is still %g of clear ground away, which is "+
			"inside MaxR 200 - a body the reach refuses cannot be that close: %s", gap, line)
	}
	if !strings.Contains(line, "reach MinR 0 .. MaxR 200") {
		t.Errorf("the cap line does not name the reach: %s", line)
	}

	// AND IT IS WRITTEN ONCE: the walk keeps ticking, and the line does not come back.
	h.chaseTick(p.sess)
	if n := countLines(logs.String(), capLine); n != 1 {
		t.Fatalf("a later tick wrote the cap line again (%d lines):\n%s", n, logs.String())
	}

	// A NEW PRESS STARTS A FRESH COUNT: the press re-plans (repeat 1 again), it does not
	// answer with the cap.
	again := pressAttack(t, h, p.sess, capturedTarget)
	if moves(again) != 1 {
		t.Fatalf("a new press after the cap produced %d move(s), want 1:\n%s",
			moves(again), logs.String())
	}
	if w := p.sess.Walk; w == nil || w.atk == nil || w.atk.repeats != 0 {
		t.Fatalf("the new press did not start a fresh count:\n%s", logs.String())
	}
	moveRingTarget(t, h, -100)
	walkToFire(t, p.sess)
	pastChasePace(t, p.sess)
	if out := h.chaseTick(p.sess); moves(out) != 1 {
		t.Fatalf("the first re-plan of the fresh press produced %d move(s), want 1 - the "+
			"cap must not outlive the press:\n%s", moves(out), logs.String())
	}
	if n := countLines(logs.String(), "re-plan 1 of 5"); n != 2 {
		t.Fatalf("the fresh press's first re-plan is not counted as 1 again (%d such "+
			"lines, want 2 - one per press):\n%s", n, logs.String())
	}
	t.Logf("%d re-plans, then 1 cap line, then a fresh press starts at re-plan 1 again",
		attackWalkReplanCap)
}

// TestAWalkThatArrivesInReachHitsOnceAndDoesNotWalkAgain is (4), the other side of the
// same rule: when the body DOES arrive in reach, the hit fires exactly once and nothing
// re-plans - the new rule must not turn a landing swing into another walk.
func TestAWalkThatArrivesInReachHitsOnceAndDoesNotWalkAgain(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	// THE MONSTER STAYS PUT, so the ring the walk planned is still inside the reach.

	walkToFire(t, p.sess)
	out := h.chaseTick(p.sess)
	if got := kinds(out); len(got) != 2 || got[0] != CmdZCAttackSingle ||
		got[1] != CmdZCAddHP {
		t.Fatalf("the arrival produced %v, want %d then %d - a body in reach hits and "+
			"does not walk:\n%s", got, CmdZCAttackSingle, CmdZCAddHP, logs.String())
	}
	if moves(out) != 0 {
		t.Fatalf("the arrival in reach also sent a move:\n%s", logs.String())
	}
	if n := countLines(logs.String(), "WALKING AGAIN"); n != 0 {
		t.Fatalf("an arrival INSIDE the reach re-planned the walk %d time(s):\n%s", n,
			logs.String())
	}

	// ONE HIT, however many ticks run after it.
	for _, pkt := range h.chaseTick(p.sess) {
		if binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			t.Fatalf("a later tick sent a SECOND %d for one press:\n%s",
				CmdZCAttackSingle, logs.String())
		}
	}
	t.Logf("the arrival sent %v once and re-planned 0 times", kinds(out))
}

// ---------------------------------------------------------------------------------
// R99: THE STAND POINT LANDS INSIDE THE REACH, AND A SHORT WALK AIMS CLOSER
// ---------------------------------------------------------------------------------
//
// THE OWNER'S LOOP (logs\go_zone1331.log, 2026-11-01 16:51:08, millisecond stamps),
// repeating every ~300 ms:
//
//	the character can be on the stand point (2504, -884, cell 108) after 311ms ...
//	the walk ended and target 536870916 is still 206.54526582545685 away, stance 10510
//	  ("BlandirCruz") reach MinR 50 .. MaxR 200
//	a new press replaces the walk to the attack on target 536870916 (2504, -884, cell 108)
//
// Three facts, and each one is a test below:
//
//	1. the stand point was 206.5 of clear ground from a target whose reach is 200 - SIX
//	   AND A HALF UNITS OUTSIDE, so the walk could never end in reach;
//	2. the re-plan asked for the SAME point, so the loop could never converge;
//	3. the absorb rule cannot help, because the walk had already ENDED.
//
// The arithmetic follows from fact 1: our goal is the PATH ENGINE's answer, not the
// reference's own tick, and the engine's answer can sit a few units out. So the request
// keeps a 10% margin (attackStandoff), an answer that still lands outside is PULLED in
// (one projection, one log line) and a re-walk is measured from where the body stands
// (attackStandoffCloser). No re-plan is ever used to fix arithmetic.

// fixedPlan answers the SAME point every time it is asked, `centre` units from the
// target's centre on the character's own side of it - the owner's path engine, which
// answered (2504, -884) whatever it was asked. A plan whose answer depends on the
// request cannot reproduce the treadmill: this one can.
func fixedPlan(centre float64) func(from, npc [3]int32, standoff float64) (
	[3]int32, [][3]int32, float64, bool, bool, error) {
	return func(from, npc [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool,
		error) {
		goal := pointTowards(npc, from, centre)
		goal[2] = npc[2]
		route := math.Hypot(float64(goal[0]-from[0]), float64(goal[1]-from[1]))
		return goal, nil, route, false, true, nil
	}
}

// outsideCentre is the owner's own answer as a distance from the target's CENTRE:
// 206.5 of clear ground past a 200 reach, plus the two bodies (50 + 60) the reach is
// measured between.
const outsideCentre = 206.5 + PCBodyRadius + ringTargetBody

// TestTheStandPointIsTheReferenceRing is (1), and it was the NINETY PERCENT rule until
// R132: the brief's numbers - MaxR 200, a 50 body and a 100 body - put the stand point at
// 300 from the target's centre, which is the reference's own number
// (`maxR + obj->GetDomainRadius()`, PlayerChar.cpp:1393). The owner's question killed the
// 10%: "the margin is useless no? ... i dont really see the point of the margin?". The ring
// still leaves 50 of clear ground against a 200 reach (it adds the TARGET's body, not the
// character's), which is what swallows the path engine's 6.5-unit error - and a plan that
// overshoots anyway is pulled in (TestAStandPointOutsideTheReachIsPulledIn).
func TestTheStandPointIsTheReferenceRing(t *testing.T) {
	got := attackStandoff(0, 200, 50, 100)
	const want, oldRing = 300.0, 300.0
	if math.Abs(got-want) > 0.001 {
		t.Fatalf("the stand point is %g from the target's centre, want %g (MaxR 200 + the "+
			"100 body, the reference's ring). The 90%% rule stood at %g", got, want, oldRing)
	}

	// AND IT IS INSIDE THE REACH BY CONSTRUCTION, with the margin still to spare: the
	// clearance it leaves, against the 6.5 the owner's engine was out by.
	clear := got - (50 + 100)
	if clear >= 200 {
		t.Fatalf("the stand point leaves %g of clear ground, which is not inside the 200 "+
			"reach", clear)
	}
	if 200-clear <= 6.5 {
		t.Fatalf("the stand point leaves %g of clear ground, so a %g engine error would "+
			"put it outside a 200 reach - no margin", clear, 6.5)
	}
	t.Logf("stand point %g from the centre (old ring %g), %g of clear ground against a "+
		"200 reach - 10%% margin, and %g to spare over the owner's 6.5", got, oldRing,
		clear, 200-clear-6.5)
}

// TestAStandPointOutsideTheReachIsPulledIn is (2): the path engine's answer is 6.5 units
// PAST the reach, exactly as the owner's session measured it, and the walk still goes
// out - to a point inside the reach, one projection toward the target, one log line
// saying how far it moved. The old rule refused this point and sent nothing.
func TestAStandPointOutsideTheReachIsPulledIn(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = fixedPlan(outsideCentre)

	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 1 {
		t.Fatalf("a plan answer %g outside the reach produced %v (%d move(s)), want "+
			"exactly ONE walk, pulled inside:\n%s", 206.5, kinds(out), moves(out),
			logs.String())
	}

	// THE WALK'S OWN DESTINATION IS INSIDE THE REACH, measured with the rule the press
	// uses - not with a number copied into the test.
	dest := destOf(recvs(out)[0].b)
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	d, in := attackRangeAllowed(dest, target, 0, 200, PCBodyRadius, ringTargetBody)
	if !in {
		t.Fatalf("the walk's destination %v is %g from the target - still outside "+
			"MinR 0 .. MaxR 200:\n%s", dest, d, logs.String())
	}
	if w := p.sess.Walk; w == nil || w.goal != dest {
		t.Fatalf("the walk does not end where the move packet says (%v vs %v):\n%s",
			w, dest, logs.String())
	}

	// ONE LINE, AND IT SAYS HOW FAR THE POINT MOVED.
	line := lineWith(logs.String(), "PAST the 200 reach")
	if line == "" {
		t.Fatalf("the pull-back wrote no line:\n%s", logs.String())
	}
	if !strings.Contains(line, "pulled") || !strings.Contains(line, "inside the reach") {
		t.Errorf("the pull-back line does not say what it did: %s", line)
	}
	if n := countLines(logs.String(), "PAST the 200 reach"); n != 1 {
		t.Errorf("the pull-back wrote %d lines, want 1: %s", n, line)
	}
	t.Logf("a plan answer 206.5 past a 200 reach was pulled to %v (%g of clear ground): %s",
		dest, d, line)
}

// TestAReWalkAimsCloserThanThePointThatFailed is (3): a walk that ends 6.5 units short
// re-plans to a point that is STRICTLY CLOSER than the one that just failed. The old
// rule asked for the same ring point again, so the engine answered the same outside
// point it answered before and the re-plan was REFUSED - nothing sent, the same walk
// asked for again on the next press: the treadmill.
func TestAReWalkAimsCloserThanThePointThatFailed(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	// THE PRESS WALKS TO ITS RING - legal ground in both rules, so the walk this test is
	// about really starts. The engine's fixed answer is then put in place for the RE-PLAN,
	// where it is what the owner's log shows: a point 6.5 units past the reach.
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	failed := p.sess.Walk
	if failed == nil || failed.atk == nil {
		t.Fatalf("the press left no attack walk:\n%s", logs.String())
	}

	// THE MONSTER STEPS AWAY WHILE THE CHARACTER WALKS, so the body arrives 6.5 units
	// past the reach - the owner's own measurement (206.5 against 200).
	moveRingTarget(t, h, -82)
	walkToFire(t, p.sess)
	pastChasePace(t, p.sess)
	h.Plan = fixedPlan(outsideCentre)

	target := [3]int32{scarecrow().X, scarecrow().Y - 82, scarecrow().Cell}
	gap, in := attackRangeAllowed(failed.goal, target, 0, 200, PCBodyRadius, ringTargetBody)
	if in || gap <= 200 {
		t.Fatalf("the fixture's walk ended %g from the target, which is not PAST the 200 "+
			"reach - this test needs a walk that ends short:\n%s", gap, logs.String())
	}
	first := math.Hypot(float64(failed.goal[0]-target[0]), float64(failed.goal[1]-target[1]))

	out := h.chaseTick(p.sess)
	if moves(out) != 1 {
		t.Fatalf("a walk that ended %g from the target produced %v (%d move(s)), want "+
			"exactly ONE re-walk to a CLOSER point:\n%s", gap, kinds(out), moves(out),
			logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the re-plan left no walk:\n%s", logs.String())
	}
	second := math.Hypot(float64(w.goal[0]-target[0]), float64(w.goal[1]-target[1]))
	if w.goal == failed.goal {
		t.Fatalf("the re-plan aims at the point that just failed (%v) - the treadmill",
			w.goal)
	}
	if second >= first {
		t.Fatalf("the re-plan aims %g from the target, and the point that failed was %g - "+
			"NOT closer, so the loop can never converge:\n%s", second, first, logs.String())
	}
	if d, ok := attackRangeAllowed(w.goal, target, 0, 200, PCBodyRadius,
		ringTargetBody); !ok {
		t.Errorf("the re-walk aims %g from the target, which is still outside the 200 "+
			"reach:\n%s", d, logs.String())
	}
	if w.atk.stand <= 0 || math.Abs(w.atk.stand-260) < 0.5 {
		t.Errorf("the re-walk asked for a stand-off of %g, which is the stance's own ring "+
			"- it was not computed from where the body stands", w.atk.stand)
	}
	t.Logf("the failed point was %g from the target (gap %g); the re-walk aims %g (asked "+
		"%g from the centre)", first, gap, second, w.atk.stand)
}

// TestTheLoopEndsAtTheCapWithNoSixthWalk is (4): the owner's loop can no longer run for
// ever. The engine answers the same out-of-reach point every time and the monster keeps
// stepping away, so EVERY walk ends short - and the attack still ends: five re-plans,
// each a real walk, then ONE cap line and no sixth walk.
func TestTheLoopEndsAtTheCapWithNoSixthWalk(t *testing.T) {
	h, logs, p := ringZone(t, attackRingFar)
	h.Plan = fixedPlan(outsideCentre)

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	sent := 1

	for i := 1; i <= attackWalkReplanCap; i++ {
		// THE MONSTER STEPS 82 UNITS AWAY BEFORE EVERY ARRIVAL: the body arrives 6 units
		// past the reach every time, which is the owner's 206.5 against 200.
		moveRingTarget(t, h, -82)
		walkToFire(t, p.sess)
		pastChasePace(t, p.sess)
		out := h.chaseTick(p.sess)
		if moves(out) != 1 {
			t.Fatalf("re-plan %d of %d produced %d move(s), want 1 - the treadmill must "+
				"still end with a walk:\n%s", i, attackWalkReplanCap, moves(out),
				logs.String())
		}
		sent++
	}

	// THE SIXTH ARRIVAL IS THE STOP: still short, and the cap is spent.
	moveRingTarget(t, h, -82)
	walkToFire(t, p.sess)
	pastChasePace(t, p.sess)
	out := h.chaseTick(p.sess)
	if moves(out) != 0 {
		t.Fatalf("the arrival after the cap sent %d move(s), want 0 - the cap is %d "+
			"re-plans per press:\n%s", moves(out), attackWalkReplanCap, logs.String())
	}
	if sent != 1+attackWalkReplanCap {
		t.Fatalf("the attack sent %d walk(s), want %d", sent, 1+attackWalkReplanCap)
	}
	const capLine = "so it stops here (cap 5 re-plans per press"
	if n := countLines(logs.String(), capLine); n != 1 {
		t.Fatalf("the cap wrote %d line(s), want exactly 1:\n%s", n, logs.String())
	}
	if n := countLines(logs.String(), "WALKING AGAIN"); n != attackWalkReplanCap {
		t.Fatalf("%d re-plan line(s), want %d:\n%s", n, attackWalkReplanCap, logs.String())
	}
	// AND NOTHING WALKS AFTER THE STOP.
	h.chaseTick(p.sess)
	if n := countLines(logs.String(), "WALKING AGAIN"); n != attackWalkReplanCap {
		t.Fatalf("a later tick walked again (%d re-plans):\n%s", n, logs.String())
	}
	t.Logf("%d walks (1 press + %d re-plans), then the cap line and no sixth walk",
		sent, attackWalkReplanCap)
}

// ---------------------------------------------------------------------------------
// R100: A HELD PRESS FIRES THE MOMENT THE BODY IS INSIDE THE STANCE'S REACH
// ---------------------------------------------------------------------------------
//
// THE BUG: the hit waited for a CLOCK (an eta + 50 ms fire pad, deleted by piece 2d) and,
// when the body stopped a few units short of the stand point that clock measured against,
// it re-planned instead of hitting ("still 206.5 away, reach 200"). Meanwhile the client's own AI, after
// a failed attack, sleeps a full second before trying again (ge\ai\lib.scp:118-131), so the
// owner saw exactly what he reported: the character "moves, stops in front of the target,
// does not attack immediately".
//
// THE FIX: the press is held while the character walks in and the reach is asked
// CONTINUOUSLY - on every position update for that character and on every tick - and the
// first yes fires the press at once. The reference does the same: when IsAttackArea says
// the body is in area it does SetMoveState(MS_STOP); SetActionState(AS_ATTACK) with no
// timer in between (source2/geServer/ZServerLib/FieldChar_FSM.cpp:28-80, case 0).
//
// THE GEOMETRY, written down so a reader can check it: the reach is EDGE TO EDGE, so a
// body 610 units from the target's CENTRE with a 50 and a 60 between them has 500 of clear
// ground, and a body 300 from it has 190 - inside the 200 reach, 10 to spare.

// ringHoldFar is where these tests stand the character: 500 of clear ground from the
// Scarecrow (610 of centre distance), so the press asks for a walk.
var ringHoldFar = [3]int32{scarecrow().X, scarecrow().Y + 610, scarecrow().Cell}

// ringHoldInReach is a point the same 200 reach covers: 190 of clear ground (300 from the
// target's centre).
var ringHoldInReach = [3]int32{scarecrow().X, scarecrow().Y + 300, scarecrow().Cell}

// attackCmds counts the hit packets (3564 and 3557) in one answer, so a test can say
// "nothing fired" without caring what else was sent.
func attackCmds(out [][]byte) int {
	var n int
	for _, pkt := range out {
		switch binary.LittleEndian.Uint16(pkt) {
		case CmdZCAttackSingle, CmdZCAttackSplash, CmdZCAddHP:
			n++
		}
	}
	return n
}

// reportAt sends a REAL CZ_MOVE_PC for character idx, exactly as the client does, and
// answers the reply: the position update the continuous check reads.
func reportAt(t *testing.T, h *Handler, sess *Session, idx byte, pos [3]int32) [][]byte {
	t.Helper()
	out, err := h.Reply(context.Background(), sess, moveFrame(idx, pos))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return out
}

// TestAPressFromFarFiresWhenTheWalkCarriesTheBodyIntoReach is (1): a press from 500 away
// sends NO hit; nothing fires while the body is still out there (not on the press, not on a
// tick); and the one update that finds the body at 190 of clear ground fires exactly ONE hit
// - the press was held, not timed.
//
// THE BODY IS CARRIED THERE BY THE WALK (R104). It used to be a client move REPORT that put
// it there, and that was the bug: a report names a destination, so the fire measured - and
// then hard-set the client's actor to - a point the body had not walked to.
func TestAPressFromFarFiresWhenTheWalkCarriesTheBodyIntoReach(t *testing.T) {
	h, logs, p := ringZone(t, ringHoldFar)
	h.Plan = directPlan

	// THE FIXTURE'S OWN ARITHMETIC, checked before anything is asserted about it.
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	farGap, inFar := attackRangeAllowed(ringHoldFar, target, 0, 200, PCBodyRadius,
		ringTargetBody)
	nearGap, inNear := attackRangeAllowed(ringHoldInReach, target, 0, 200, PCBodyRadius,
		ringTargetBody)
	if inFar || farGap <= 200 {
		t.Fatalf("the press stand is %g of clear ground, which is NOT past the 200 reach "+
			"- this test cannot be about a held press", farGap)
	}
	if !inNear || math.Abs(nearGap-190) > 0.001 {
		t.Fatalf("the in-reach report point is %g of clear ground (in reach: %v), want "+
			"exactly 190", nearGap, inNear)
	}

	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 1 {
		t.Fatalf("the press from %g away produced %d move(s), want 1:\n%s", farGap,
			moves(out), logs.String())
	}
	if n := attackCmds(out); n != 0 {
		t.Fatalf("a press from %g away sent %d attack packet(s) before the body moved "+
			"anywhere:\n%s", farGap, n, logs.String())
	}

	// NOTHING FIRES ON A TICK EITHER: the body is still 500 out, and the walk has only
	// just started.
	if n := attackCmds(h.chaseTick(p.sess)); n != 0 {
		t.Fatalf("a tick fired %d attack packet(s) while the body was still %g away:\n%s",
			n, farGap, logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 0 {
		t.Fatalf("the fire line was written %d time(s) BEFORE the report that earns it:\n%s",
			n, logs.String())
	}

	// THE UPDATE THAT PUTS THE BODY INSIDE THE REACH: one hit, at once, and the line names
	// the distance it landed at.
	rep := arrivalAt(t, h, p.sess, ringHoldInReach)
	if got := attackCmds(rep); got != 2 {
		t.Fatalf("a report putting the body %g of clear ground from the target produced "+
			"%v, want the hit and the HP (%d then %d):\n%s", nearGap, kinds(rep),
			CmdZCAttackSingle, CmdZCAddHP, logs.String())
	}
	if got := recvs(rep); got[0].cmd != CmdZCAttackSingle || got[1].cmd != CmdZCAddHP {
		t.Fatalf("the answer is %v, want %d then %d", kinds(rep), CmdZCAttackSingle,
			CmdZCAddHP)
	}
	line := lineWith(logs.String(), "ATTACK FIRED ON ARRIVAL")
	if line == "" {
		t.Fatalf("the fire wrote no line - the card has nothing to read:\n%s", logs.String())
	}
	if !strings.Contains(line, "at 190 of reach 200") {
		t.Errorf("the fire line does not name the distance it landed at: %s", line)
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("the fire line was written %d time(s), want 1:\n%s", n, logs.String())
	}
	t.Logf("press at %g, report at %g -> %v: %s", farGap, nearGap, kinds(rep), line)
}

// TestTheHeldPressFiresOnceAndLaterReportsSendNothing is (2): the fire is not a latch to
// keep hitting on. Once the press has fired it is forgotten, so five more reports - all of
// them inside the reach - send nothing, and the fire line stays at one.
func TestTheHeldPressFiresOnceAndLaterReportsSendNothing(t *testing.T) {
	h, logs, p := ringZone(t, ringHoldFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	if n := attackCmds(arrivalAt(t, h, p.sess, ringHoldInReach)); n != 2 {
		t.Fatalf("the in-reach arrival fired %d attack packet(s), want 2:\n%s", n,
			logs.String())
	}

	for i := 0; i < 5; i++ {
		out := reportAt(t, h, p.sess, 0, ringHoldInReach)
		if n := attackCmds(out); n != 0 {
			t.Fatalf("report %d after the fire sent %d attack packet(s), want 0 - one "+
				"press is one hit:\n%s", i+1, n, logs.String())
		}
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("the press fired %d time(s) over six reports, want 1:\n%s", n, logs.String())
	}
	t.Logf("one fire, then five further reports at the same point: 0 packets")
}

// TestAPlayerClickCancelsTheApproachAndDropsTheHeldPress is (3): a click that takes the
// character somewhere else is NOT a report that earns the hit. It drops the held press with
// its own reason, nothing fires for it, and later ticks do not resurrect it.
func TestAPlayerClickCancelsTheApproachAndDropsTheHeldPress(t *testing.T) {
	h, logs, p := ringZone(t, ringHoldFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}

	// THE CLICK, and it is deliberately still OUTSIDE the reach - a click that lands the
	// body inside it is the report case above, not this one.
	elsewhere := [3]int32{scarecrow().X + 100, scarecrow().Y + 610, scarecrow().Cell}
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	if gap, in := attackRangeAllowed(elsewhere, target, 0, 200, PCBodyRadius,
		ringTargetBody); in {
		t.Fatalf("the click point is %g of clear ground from the target, which is INSIDE "+
			"the 200 reach - this test cannot be about a cancel", gap)
	}

	out := reportAt(t, h, p.sess, 0, elsewhere)
	if p.sess.Walk != nil {
		t.Fatalf("the click left the attack walk in flight (%s):\n%s", p.sess.Walk.describe(),
			logs.String())
	}
	line := lineWith(logs.String(), "ATTACK approach CANCELLED by a move click")
	if line == "" {
		t.Fatalf("the click dropped the press with no reason on the log:\n%s", logs.String())
	}
	if !strings.Contains(line, "for the attack on target") {
		t.Errorf("the cancel line does not name the press it dropped: %s", line)
	}
	if n := attackCmds(out); n != 0 {
		t.Fatalf("the click that cancelled the approach sent %d attack packet(s):\n%s", n,
			logs.String())
	}
	if n := attackCmds(h.chaseTick(p.sess)); n != 0 {
		t.Fatalf("a tick after the cancel sent %d attack packet(s):\n%s", n, logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 0 {
		t.Fatalf("a cancelled press fired %d time(s):\n%s", n, logs.String())
	}
	t.Logf("the click dropped the press: %s", line)
}

// TestTheHeldPressIsDroppedAfterFiveSecondsAndANewPressFires is (4): the hold is not a
// promise that never ends. A press whose body never comes inside the reach is dropped after
// five seconds with ONE line and no hit, and the next press is a press of its own - it fires
// normally.
func TestTheHeldPressIsDroppedAfterFiveSecondsAndANewPressFires(t *testing.T) {
	h, logs, p := ringZone(t, ringHoldFar)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the press left no walk with a pending attack:\n%s", logs.String())
	}

	// SIX SECONDS FROM THE PRESS, without waiting: `since` is when the player pressed and
	// the hold's limit is measured from it.
	w.atk.since = time.Now().Add(-6 * time.Second)

	if n := attackCmds(h.chaseTick(p.sess)); n != 0 {
		t.Fatalf("the expired hold still sent %d attack packet(s):\n%s", n, logs.String())
	}
	line := lineWith(logs.String(), "ATTACK HELD press DROPPED")
	if line == "" {
		t.Fatalf("the expired hold wrote no line:\n%s", logs.String())
	}
	if !strings.Contains(line, "the press has been held") {
		t.Errorf("the drop line does not say it was the hold's own limit: %s", line)
	}
	if !strings.Contains(line, "limit 5s") {
		t.Errorf("the drop line does not name the limit: %s", line)
	}
	if n := countLines(logs.String(), "ATTACK HELD press DROPPED"); n != 1 {
		t.Fatalf("the expired hold wrote %d line(s), want exactly 1:\n%s", n, logs.String())
	}
	if !w.castDone {
		t.Fatalf("the dropped press is still owed a hit:\n%s", logs.String())
	}
	// AND A LATER TICK DOES NOT FIRE IT EITHER.
	if n := attackCmds(h.chaseTick(p.sess)); n != 0 {
		t.Fatalf("a tick after the drop sent %d attack packet(s):\n%s", n, logs.String())
	}

	// A NEW PRESS STARTS A FRESH ONE, and it fires the ordinary way.
	again := pressAttack(t, h, p.sess, capturedTarget)
	if moves(again) != 1 {
		t.Fatalf("a new press after the drop produced %d move(s), want 1:\n%s",
			moves(again), logs.String())
	}
	if cur := p.sess.Walk; cur == nil || cur.atk == nil || cur.castDone {
		t.Fatalf("the new press did not start a press of its own:\n%s", logs.String())
	}
	if n := attackCmds(arrivalAt(t, h, p.sess, ringHoldInReach)); n != 2 {
		t.Fatalf("the new press's in-reach arrival fired %d attack packet(s), want 2 - a "+
			"dropped press must not poison the next one:\n%s", n, logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("the fire line was written %d time(s), want 1 (the first press was "+
			"dropped, only the second fired):\n%s", n, logs.String())
	}
	t.Logf("the expired hold was dropped with one line, then a new press fired once")
}

// TestPressesWhileWalkingAreSwallowedIntoTheSameHeldPress is (5): the absorb rule still
// holds. The client presses every 100 ms while a target is nominated (lib.scp:118-131), and
// those presses must be swallowed into the press already held - no second walk, no second
// pending press - or the character re-plans before it ever arrives (the owner's 97 walks).
func TestPressesWhileWalkingAreSwallowedIntoTheSameHeldPress(t *testing.T) {
	h, logs, p := ringZone(t, ringHoldFar)
	h.Plan = directPlan

	first := pressAttack(t, h, p.sess, capturedTarget)
	if moves(first) != 1 {
		t.Fatalf("the first press produced %d move(s), want 1:\n%s", moves(first),
			logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the first press left no walk with a pending attack:\n%s", logs.String())
	}
	held := w.atk

	total := moves(first)
	for i := 0; i < 2; i++ {
		// Past attackChasePace (so only the absorb rule can hold it) and with the body held
		// at the walk's start (so it is not yet drawn inside the reach, where a press is a
		// hit).
		pastChasePace(t, p.sess)
		walkHasNotAdvanced(t, p.sess)
		sidestep(t, p.sess, 80)
		out := pressAttack(t, h, p.sess, capturedTarget)
		if moves(out) != 0 {
			t.Fatalf("press %d while the body walked in sent %d move(s), want 0:\n%s",
				i+2, moves(out), logs.String())
		}
		total += moves(out)
	}
	if total != 1 {
		t.Fatalf("three presses for one target produced %d move(s), want exactly 1:\n%s",
			total, logs.String())
	}
	if p.sess.Walk != w {
		t.Fatalf("an absorbed press replaced the walk instead of being swallowed")
	}
	if p.sess.Walk.atk != held {
		t.Fatalf("an absorbed press built a SECOND pending press - the client's 100 ms " +
			"cadence would restart the hold on every pass")
	}
	if w.castDone {
		t.Fatalf("an absorbed press dropped the held press:\n%s", logs.String())
	}
	if n := len(absorbedLines(logs.String())); n == 0 {
		t.Fatalf("the swallowed presses wrote no line:\n%s", logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 0 {
		t.Fatalf("a held press fired %d time(s) while the body was still out of reach:\n%s",
			n, logs.String())
	}
	t.Logf("three presses, 1 move: the same pending press held through both repeats")
}

// =================================================================================
// R101: A TARGET INSIDE THE STANCE'S MINIMUM REACH IS STEPPED BACK FROM, NOT REFUSED
// =================================================================================
//
// THE MEASUREMENT (owner's session, logs\go_zone1331.log, 17:37:46 and 17:37:49): the
// same press, two stamps, refused twice for being UNDER MinR - "is 29.321305282617686
// away" and "is -6.565530985615382 away". The second number is NEGATIVE, which means the
// two bodies OVERLAP. 6 of 27 presses in that session ended this way: the character stood
// on top of the monster and nothing was sent.
//
// THE REFERENCE DOES THE OPPOSITE. IsAttackArea answers -1 for too near and that case
// WALKS the character out to the min ring (source2\geServer\ZServerLib\FieldChar_FSM.cpp:
// 63-79 -> GetAttackMinPos), re-deciding at most every 300 ms and only when the
// destination changed, exactly as the too-far case does. The four tests below are that
// rule, and only that rule: the walk, the same walk from overlapping bodies, the hit it
// fires on arrival through the EXISTING fire-on-arrival path, and the in-range press the
// near path must not touch.

// stepBackStance is a real stance row that states a minimum: `PossessionFire`, MinR 50 ..
// MaxR 900, AtkType Single. The numbers are CHECKED in stepBackZone, not assumed here.
const stepBackStance = uint32(20210)

// stepBackZone stands character 0 at `from` in stepBackStance with a 60-unit monster -
// ringZone's fixture with a stance that has a near bound - and fails loudly when the row
// is not the one this file is written against.
func stepBackZone(t *testing.T, from [3]int32) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := attackZoneAt(t, stepBackStance, from)
	if h.Stances == nil {
		t.Fatal("this zone has no stance table")
	}
	row := h.Stances.Combat(stepBackStance)
	if row.MinR != 50 || row.MaxR != 900 {
		t.Fatalf("stance %d is MinR %g .. MaxR %g in datatable_stance.xml, and this file "+
			"is written against 50 .. 900", stepBackStance, row.MinR, row.MaxR)
	}
	h.Monsters = newMonsters(map[uint32]MonsterRow{
		scarecrow().ClassID: {ID: scarecrow().ClassID, Name: "Monster_Hersu01",
			Size: "S", Radius: ringTargetBody},
	})
	return h, logs, p
}

// stepBackMinRing is the stand point a step-back asks for, from the TARGET'S CENTRE:
// MinR + the character's own body + the monster's - the reference's own sum
// (PlayerChar.cpp:2108). It is computed from the row and the two bodies, never typed in.
func stepBackMinRing(t *testing.T, h *Handler) float64 {
	t.Helper()
	row := h.Stances.Combat(stepBackStance)
	return row.MinR + PCBodyRadius + ringTargetBody
}

// The two press points the owner's log names, rebuilt on whole units. The Scarecrow is at
// (-919, 2311, 136) and the two bodies are 50 + 60, so EDGE 29 is 139 from its centre and
// EDGE -6 (bodies overlapping) is 104.
var (
	stepBackNear        = [3]int32{scarecrow().X, scarecrow().Y - 139, scarecrow().Cell}
	stepBackOverlapping = [3]int32{scarecrow().X, scarecrow().Y - 104, scarecrow().Cell}
)

// stepBackAssertWalkOut is the one assertion tests (1) and (2) share: the press answered
// with EXACTLY ONE move, whose destination is the min ring from the target's centre, no
// attack packet, no refusal line, and one log line naming the edge distance, the ring and
// the destination.
func stepBackAssertWalkOut(t *testing.T, h *Handler, logs *stringBuf, p *player,
	out [][]byte, wantEdge float64) {
	t.Helper()
	if moves(out) != 1 {
		t.Fatalf("a press at edge %g produced %v (%d move(s)), want exactly ONE move out "+
			"to the min ring:\n%s", wantEdge, kinds(out), moves(out), logs.String())
	}
	if n := attackCmds(out); n != 0 {
		t.Fatalf("a press at edge %g sent %d attack packet(s) without stepping back "+
			"first:\n%s", wantEdge, n, logs.String())
	}
	if n := countLines(logs.String(), "ATTACK refused"); n != 0 {
		t.Fatalf("a press at edge %g is still REFUSED (%d line(s)):\n%s", wantEdge, n,
			logs.String())
	}

	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	move := recvs(out)[0]
	dest := destOf(move.b)
	got := math.Hypot(float64(dest[0]-target[0]), float64(dest[1]-target[1]))
	want := stepBackMinRing(t, h)
	if math.Abs(got-want) > 1.5 {
		t.Fatalf("the step-back's destination %v is %g from the target's centre, want %g "+
			"= MinR 50 + the character's body %g + the monster's %g (the reference's "+
			"GetAttackMinPos). The far ring is %g:\n%s", dest, got, want, PCBodyRadius,
			ringTargetBody, (900.0+ringTargetBody)*attackStandoffMargin, logs.String())
	}

	// THE PRESS IS HELD, not answered: the hit is owed to the walk that is in flight.
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the step-back left no walk with a held press:\n%s", logs.String())
	}
	if !w.atk.stepBack {
		t.Errorf("the walk is not marked as a step-back - a re-plan would aim the FAR ring")
	}
	if w.castDone {
		t.Errorf("the press was dropped instead of held:\n%s", logs.String())
	}

	line := lineWith(logs.String(), "STEPPING BACK")
	if line == "" {
		t.Fatalf("the step-back wrote no line - the card has nothing to read:\n%s",
			logs.String())
	}
	for _, s := range []string{"MinR 50", "min ring MinR 50 + its body 60 + its own 50",
		"of clear ground from the target"} {
		if !strings.Contains(line, s) {
			t.Errorf("the step-back line does not name %q: %s", s, line)
		}
	}
}

// TestATooNearPressStepsBackToTheMinRing is (1), the owner's first stamp: 29 of clear
// ground against MinR 50. Before R101 this press was REFUSED - one line, no packet - and
// the character stood in front of the monster doing nothing.
func TestATooNearPressStepsBackToTheMinRing(t *testing.T) {
	h, logs, p := stepBackZone(t, stepBackNear)
	h.Plan = directPlan

	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	gap, in := attackRangeAllowed(stepBackNear, target, 50, 900, PCBodyRadius,
		ringTargetBody)
	if in || math.Abs(gap-29) > 0.001 {
		t.Fatalf("the fixture's edge distance is %g (in reach: %v), want exactly 29 and "+
			"refused - this test cannot be about a too-near press", gap, in)
	}

	stepBackAssertWalkOut(t, h, logs, p, pressAttack(t, h, p.sess, capturedTarget), gap)
	t.Logf("edge %g -> %s", gap, lineWith(logs.String(), "STEPPING BACK"))
}

// TestATooNearPressWithOverlappingBodiesStepsBackToo is (2), the owner's second stamp:
// -6.5 of clear ground, i.e. the bodies OVERLAP. A negative edge distance is the same case
// as a small positive one - the reference's IsAttackArea answers -1 for both, because it
// only compares `minR > ceil(len)`.
func TestATooNearPressWithOverlappingBodiesStepsBackToo(t *testing.T) {
	h, logs, p := stepBackZone(t, stepBackOverlapping)
	h.Plan = directPlan

	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	gap, in := attackRangeAllowed(stepBackOverlapping, target, 50, 900, PCBodyRadius,
		ringTargetBody)
	if in || gap >= 0 || math.Abs(gap-(-6)) > 0.001 {
		t.Fatalf("the fixture's edge distance is %g (in reach: %v), want a NEGATIVE -6 "+
			"with the bodies overlapping - this test cannot be about overlapping bodies",
			gap, in)
	}

	stepBackAssertWalkOut(t, h, logs, p, pressAttack(t, h, p.sess, capturedTarget), gap)
	t.Logf("edge %g -> %s", gap, lineWith(logs.String(), "STEPPING BACK"))
}

// TestAStepBackWalkFiresTheHeldPressOnArrival is (3): the step-back is a HELD press, so
// the FIRST position report that puts the body inside MinR..MaxR fires exactly one hit -
// through the existing fire-on-arrival path, not a second one. Before R101 there was no
// held press at all in this direction, so nothing could fire.
func TestAStepBackWalkFiresTheHeldPressOnArrival(t *testing.T) {
	h, logs, p := stepBackZone(t, stepBackOverlapping)
	h.Plan = directPlan

	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	// THE REPORT POINT IS THE MIN RING ITSELF: edge 50, the near bound, inside the reach.
	onRing := [3]int32{scarecrow().X, scarecrow().Y - 160, scarecrow().Cell}
	gap, in := attackRangeAllowed(onRing, target, 50, 900, PCBodyRadius, ringTargetBody)
	if !in || math.Abs(gap-50) > 0.001 {
		t.Fatalf("the report point is %g of clear ground (in reach: %v), want exactly 50 "+
			"- the min ring, the first point the stance allows", gap, in)
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 0 {
		t.Fatalf("the fire line was written %d time(s) BEFORE the body was in reach:\n%s",
			n, logs.String())
	}

	rep := arrivalAt(t, h, p.sess, onRing)
	if got := attackCmds(rep); got != 2 {
		t.Fatalf("the update that puts the body %g of clear ground from the target "+
			"produced %v, want the hit and the HP (%d then %d):\n%s", gap, kinds(rep),
			CmdZCAttackSingle, CmdZCAddHP, logs.String())
	}
	if got := recvs(rep); got[0].cmd != CmdZCAttackSingle || got[1].cmd != CmdZCAddHP {
		t.Fatalf("the answer is %v, want %d then %d", kinds(rep), CmdZCAttackSingle,
			CmdZCAddHP)
	}
	line := lineWith(logs.String(), "ATTACK FIRED ON ARRIVAL")
	if line == "" {
		t.Fatalf("the fire wrote no line:\n%s", logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("the fire line was written %d time(s), want 1:\n%s", n, logs.String())
	}

	// ONE PRESS, ONE HIT: a second report at the same point sends nothing.
	if n := attackCmds(reportAt(t, h, p.sess, 0, onRing)); n != 0 {
		t.Fatalf("a report after the fire sent %d attack packet(s), want 0:\n%s", n,
			logs.String())
	}
	t.Logf("step back, then a report at edge %g -> %v: %s", gap, kinds(rep), line)
}

// TestAPressInsideMinRToMaxRIsStillAttackedAtOnce is (4): the near path must not touch the
// in-range case. A press at edge 190 against MinR 50 .. MaxR 900 hits at once, with no
// walk, no path-engine call and no step-back line.
func TestAPressInsideMinRToMaxRIsStillAttackedAtOnce(t *testing.T) {
	// 190 of clear ground: the two bodies (110) plus 190 from the target's centre.
	stand := [3]int32{scarecrow().X, scarecrow().Y - (110 + 190), scarecrow().Cell}
	h, logs, p := stepBackZone(t, stand)
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		t.Error("an IN-REACH press asked the path engine for a stand point - the near " +
			"path must not touch the case that is already inside MinR..MaxR")
		return directPlan(from, target, standoff)
	}

	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	gap, in := attackRangeAllowed(stand, target, 50, 900, PCBodyRadius, ringTargetBody)
	if !in || math.Abs(gap-190) > 0.001 {
		t.Fatalf("the fixture is %g of clear ground (in reach: %v), want exactly 190 - "+
			"inside MinR 50 .. MaxR 900", gap, in)
	}

	out := pressAttack(t, h, p.sess, capturedTarget)
	if moves(out) != 0 {
		t.Fatalf("an in-reach press sent %d move(s), want 0:\n%s", moves(out), logs.String())
	}
	if got := attackCmds(out); got != 2 {
		t.Fatalf("an in-reach press produced %v, want the hit and the HP (%d then %d):\n%s",
			kinds(out), CmdZCAttackSingle, CmdZCAddHP, logs.String())
	}
	if p.sess.Walk != nil {
		t.Errorf("an in-reach press started a walk (%s)", p.sess.Walk.describe())
	}
	if n := countLines(logs.String(), "STEPPING BACK"); n != 0 {
		t.Errorf("%d step-back line(s) for a press that was already in reach:\n%s", n,
			logs.String())
	}
	t.Logf("edge %g inside MinR 50 .. MaxR 900 -> %v, no walk", gap, kinds(out))
}

// =================================================================================
//
// R104: THE RETREAT IS ARITHMETIC, SO IT CAN NEVER ANSWER "NOTHING SENT"
//
// THE MEASUREMENT (owner's session, docker-port\logs\go_zone1331.log, 33 times since
// 19:46, edge distances -0.52 to -45.18):
//
//	ATTACK APPROACH OUT OF REACH: -28.690814055971728 away, stance 10510 reach MinR 50 ..
//	MaxR 200, and no clear stand point exists on the target's side (the path engine
//	clamped into the target itself) - nothing sent
//
// A negative edge distance means the two bodies OVERLAP: the character stands inside the
// monster's own collision radius. The code asked the PATH ENGINE for the retreat point,
// and the engine treats the monster as an obstacle - so every answer was clamped into the
// target, `overlap` came back true, and the press was answered with NO packet and NO walk.
// Every press after that failed the same way, which is the owner's repro: stop mid-walk
// with keep-mode, and the character stands there and never attacks again.
//
// THE RULE THESE TESTS HOLD: the retreat is a VECTOR, never a plan - `MinR + both bodies`
// from the target's centre along the line through the body, which is the reference's own
// GetAttackMinPos sum (CMonsterNPC::GetAttackMinPos, MonsterNPC.cpp:1047). The path engine
// is NOT asked, so it has no way to answer "no clear stand point".

// retreatTargetBody is the owner's monster: a 322-unit body against the character's 50.
const retreatTargetBody = 322.0

// retreatOverlapAt is the owner's -28.69 rebuilt on whole units. The Scarecrow is at
// (-919, 2311, 136); (200, -279) from it is 343.28, and 343.28 - 50 - 322 = -28.72 of
// clear ground. The offset is deliberately DIAGONAL: a retreat along one axis cannot tell
// a real direction from a sign, and the owner's log number is a diagonal distance.
var retreatOverlapAt = [3]int32{scarecrow().X + 200, scarecrow().Y - 279, scarecrow().Cell}

// retreatZone stands character 0 at `from` in stepBackStance (MinR 50 .. MaxR 900) with a
// monster of `body` units, and fails loudly when the stance row is not the one these tests
// are written against.
func retreatZone(t *testing.T, from [3]int32, body float64) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := attackZoneAt(t, stepBackStance, from)
	if h.Stances == nil {
		t.Fatal("this zone has no stance table")
	}
	row := h.Stances.Combat(stepBackStance)
	if row.MinR != 50 || row.MaxR != 900 {
		t.Fatalf("stance %d is MinR %g .. MaxR %g in datatable_stance.xml, and these tests "+
			"are written against 50 .. 900", stepBackStance, row.MinR, row.MaxR)
	}
	h.Monsters = newMonsters(map[uint32]MonsterRow{
		scarecrow().ClassID: {ID: scarecrow().ClassID, Name: "Monster_Hersu01",
			Size: "S", Radius: body},
	})
	return h, logs, p
}

// noPlanAllowed is the path engine a retreat must not use: it FAILS the test the moment it
// is called, and it answers the way the real engine answers a goal inside the monster -
// `overlap` true, which is the branch that printed "nothing sent".
func noPlanAllowed(t *testing.T) func(from, npc [3]int32, standoff float64) ([3]int32,
	[][3]int32, float64, bool, bool, error) {
	t.Helper()
	return func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		t.Errorf("a RETREAT asked the path engine for a stand point (%v -> %v, standoff "+
			"%g). The retreat must be arithmetic: the engine treats the target as an "+
			"obstacle, so every retreat point is clamped into it and refused", from,
			target, standoff)
		return target, nil, 0, true, true, nil
	}
}

// retreatAssertWalkOut is the assertion tests (1) and (2) share: EXACTLY ONE walk, aimed
// `attackRetreatRing` from the target's centre, on the body-target line (or, when there is
// no line, in the defined fallback direction), no hit, no refusal, and the press still
// held.
func retreatAssertWalkOut(t *testing.T, logs *stringBuf, p *player, out [][]byte,
	from [3]int32) [3]int32 {
	t.Helper()
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	if moves(out) != 1 {
		t.Fatalf("a press with the bodies overlapping produced %v (%d move(s)), want "+
			"exactly ONE walk out to the min ring - the press is answered with a walk, "+
			"never with nothing:\n%s", kinds(out), moves(out), logs.String())
	}
	if n := attackCmds(out); n != 0 {
		t.Fatalf("the press sent %d attack packet(s) while the bodies still overlap:\n%s",
			n, logs.String())
	}
	for _, want := range []string{"nothing sent", "ATTACK refused"} {
		if n := countLines(logs.String(), want); n != 0 {
			t.Fatalf("%d %q line(s) for a press that must be answered with a walk:\n%s",
				n, want, logs.String())
		}
	}

	dest := destOf(recvs(out)[0].b)
	got := math.Hypot(float64(dest[0]-target[0]), float64(dest[1]-target[1]))
	want := attackRetreatRing(50, PCBodyRadius, retreatTargetBody)
	if math.Abs(got-want) > 2 {
		t.Fatalf("the retreat's destination %v is %g from the target's centre, want %g = "+
			"MinR 50 + the character's body %g + the monster's %g + the %g rounding margin "+
			"(GetAttackMinPos, MonsterNPC.cpp:1047):\n%s", dest, got, want, PCBodyRadius,
			retreatTargetBody, attackRetreatMargin, logs.String())
	}

	// ON THE LINE THROUGH THE BODY, when there is one: the cross product of the two unit
	// vectors is the sine of the angle between them, so a retreat that walked off to the
	// wrong side shows up as a number far from 0.
	bx, by := float64(from[0]-target[0]), float64(from[1]-target[1])
	bl := math.Hypot(bx, by)
	dx, dy := float64(dest[0]-target[0]), float64(dest[1]-target[1])
	dl := math.Hypot(dx, dy)
	if bl > 0 && dl > 0 {
		if sine := math.Abs(bx*dy-by*dx) / (bl * dl); sine > 0.01 {
			t.Fatalf("the retreat's destination %v is not on the line from the target "+
				"through the body %v (sine %g):\n%s", dest, from, sine, logs.String())
		}
	}

	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the retreat left no walk with a held press - the hit would be lost:\n%s",
			logs.String())
	}
	if !w.atk.stepBack {
		t.Errorf("the walk is not marked as a step-back, so a re-plan would aim the FAR ring")
	}
	if w.castDone {
		t.Errorf("the press was consumed instead of held:\n%s", logs.String())
	}
	if line := lineWith(logs.String(), "STEPPING BACK"); line == "" {
		t.Fatalf("the retreat wrote no line naming its direction:\n%s", logs.String())
	}
	return dest
}

// TestARetreatInsideTheTargetIsComputedNotPlanned is (1): the owner's -28.69 with a
// 50-unit character and a 322-unit monster. Exactly ONE walk is sent, aimed at
// `50 + 50 + 322 + margin` from the target's centre along the body-target line, and the
// press is still held. BEFORE THIS CHANGE nothing was sent at all: the plan answered
// `overlap`, and the zone printed "no clear stand point exists ... nothing sent".
func TestARetreatInsideTheTargetIsComputedNotPlanned(t *testing.T) {
	h, logs, p := retreatZone(t, retreatOverlapAt, retreatTargetBody)
	h.Plan = noPlanAllowed(t)

	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	gap, in := attackRangeAllowed(retreatOverlapAt, target, 50, 900, PCBodyRadius,
		retreatTargetBody)
	if in || gap >= 0 || math.Abs(gap-(-28.69)) > 1 {
		t.Fatalf("the fixture's edge distance is %g (in reach: %v), want the owner's "+
			"-28.69 with the bodies overlapping - this test cannot be about a body inside "+
			"the target", gap, in)
	}

	dest := retreatAssertWalkOut(t, logs, p, pressAttack(t, h, p.sess, capturedTarget),
		retreatOverlapAt)
	t.Logf("edge %g -> one walk to %v: %s", gap, dest, lineWith(logs.String(),
		"STEPPING BACK"))
}

// TestARetreatFromTheTargetsExactCentreStillWalks is (2): the degenerate case. The body
// stands exactly ON the target's centre, so there is no body-target line to extend. The
// retreat must still send ONE walk in a DEFINED direction (the last walk's, else +X) - a
// divide-by-zero or a silent skip is not an answer to a press.
func TestARetreatFromTheTargetsExactCentreStillWalks(t *testing.T) {
	at := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	h, logs, p := retreatZone(t, at, retreatTargetBody)
	h.Plan = noPlanAllowed(t)

	gap, in := attackRangeAllowed(at, at, 50, 900, PCBodyRadius, retreatTargetBody)
	if in {
		t.Fatalf("a body standing ON the target's centre measures %g of clear ground and "+
			"is IN reach (%v) - this test cannot be about a degenerate retreat", gap, in)
	}

	dest := retreatAssertWalkOut(t, logs, p, pressAttack(t, h, p.sess, capturedTarget), at)
	if dest == at {
		t.Fatalf("the retreat aimed at the target's own centre - the destination would be "+
			"the obstacle itself:\n%s", logs.String())
	}
	// NO LAST WALK IN THIS FIXTURE, so the stated fallback is +X: a defined direction.
	if dest[0] <= at[0] || math.Abs(float64(dest[1]-at[1])) > 1 {
		t.Fatalf("the degenerate retreat went to %v from the centre %v; with no previous "+
			"walk the stable direction is +X, so the destination must be east of the "+
			"target:\n%s", dest, at, logs.String())
	}
	t.Logf("body exactly on the centre (edge %g) -> one walk to %v", gap, dest)
}

// TestARetreatIsOnlyBelowTheMinimum is (3): the boundary. A retreat is for a target
// INSIDE the minimum reach, so the boundary itself is not inside it - an edge distance
// exactly MinR (50) and an edge distance exactly 0 on a row that states no minimum (where
// touching is in range, CFieldChar::IsAttackArea's `minR <= 0 && len <= 0`) both hit where
// they stand, and neither starts a walk.
func TestARetreatIsOnlyBelowTheMinimum(t *testing.T) {
	t.Run("edge exactly MinR", func(t *testing.T) {
		// The two bodies are 50 + 60 = 110, so 160 from the target's centre is edge 50.
		at := [3]int32{scarecrow().X, scarecrow().Y - 160, scarecrow().Cell}
		h, logs, p := stepBackZone(t, at)
		h.Plan = noPlanAllowed(t)

		target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
		gap, in := attackRangeAllowed(at, target, 50, 900, PCBodyRadius, ringTargetBody)
		if !in || math.Abs(gap-50) > 0.001 {
			t.Fatalf("the fixture's edge distance is %g (in reach: %v), want exactly 50 = "+
				"MinR - the boundary is not inside it", gap, in)
		}

		out := pressAttack(t, h, p.sess, capturedTarget)
		if moves(out) != 0 {
			t.Fatalf("edge 50 against MinR 50 STARTED A RETREAT (%v):\n%s", kinds(out),
				logs.String())
		}
		if got := attackCmds(out); got != 2 {
			t.Fatalf("edge 50 produced %v, want the hit and the HP (%d then %d) - the "+
				"boundary is in reach:\n%s", kinds(out), CmdZCAttackSingle, CmdZCAddHP,
				logs.String())
		}
		if n := countLines(logs.String(), "STEPPING BACK"); n != 0 {
			t.Errorf("%d retreat line(s) for a press exactly ON the boundary:\n%s", n,
				logs.String())
		}
	})

	t.Run("edge exactly 0 with no stated minimum", func(t *testing.T) {
		// Rapiere states MinR 0 .. MaxR 200, and the two bodies are 110, so 110 from the
		// target's centre is edge 0: touching, which the reference counts as in range.
		at := [3]int32{scarecrow().X, scarecrow().Y - 110, scarecrow().Cell}
		h, logs, p := ringZone(t, at)
		h.Plan = noPlanAllowed(t)

		target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
		gap, in := attackRangeAllowed(at, target, 0, 200, PCBodyRadius, ringTargetBody)
		if !in || math.Abs(gap) > 0.001 {
			t.Fatalf("the fixture's edge distance is %g (in reach: %v), want exactly 0", gap,
				in)
		}
		if ring := attackStepBackStand(0, PCBodyRadius, ringTargetBody); ring != 0 {
			t.Fatalf("a row with MinR 0 answers the retreat ring %g, want 0 - there is "+
				"nothing to step out to", ring)
		}

		out := pressAttack(t, h, p.sess, capturedTarget)
		if moves(out) != 0 {
			t.Fatalf("edge 0 with MinR 0 STARTED A RETREAT (%v):\n%s", kinds(out),
				logs.String())
		}
		if got := attackCmds(out); got != 2 {
			t.Fatalf("edge 0 with MinR 0 produced %v, want the hit and the HP (%d then "+
				"%d):\n%s", kinds(out), CmdZCAttackSingle, CmdZCAddHP, logs.String())
		}
	})
}

// TestARetreatWalkFiresTheHeldPressOnArrival is (4): the retreat does not consume the
// press. The first update that brings the body back to MinR..MaxR fires exactly one hit,
// through the existing fire-on-arrival path - so the owner's "stops and never attacks"
// cannot happen by the press being spent on the walk out.
func TestARetreatWalkFiresTheHeldPressOnArrival(t *testing.T) {
	h, logs, p := retreatZone(t, retreatOverlapAt, retreatTargetBody)
	h.Plan = noPlanAllowed(t)

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1 - the retreat:\n%s", moves(out),
			logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 0 {
		t.Fatalf("the fire line was written %d time(s) BEFORE the body was back in "+
			"reach:\n%s", n, logs.String())
	}

	// The min ring for these two bodies: 50 + 322 = 372 from the centre, plus MinR 50.
	onRing := [3]int32{scarecrow().X, scarecrow().Y - 422, scarecrow().Cell}
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	gap, in := attackRangeAllowed(onRing, target, 50, 900, PCBodyRadius, retreatTargetBody)
	if !in || math.Abs(gap-50) > 0.001 {
		t.Fatalf("the report point is %g of clear ground (in reach: %v), want exactly 50 "+
			"- the first point the stance allows", gap, in)
	}

	rep := arrivalAt(t, h, p.sess, onRing)
	if got := attackCmds(rep); got != 2 {
		t.Fatalf("the update that brings the body %g of clear ground from the target "+
			"produced %v, want the hit and the HP (%d then %d):\n%s", gap, kinds(rep),
			CmdZCAttackSingle, CmdZCAddHP, logs.String())
	}
	if got := recvs(rep)[0].cmd; got != CmdZCAttackSingle {
		t.Fatalf("the first packet is cmd %d, want %d", got, CmdZCAttackSingle)
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("the fire line was written %d time(s), want 1:\n%s", n, logs.String())
	}
	t.Logf("retreat to the min ring, then edge %g -> %v: %s", gap, kinds(rep),
		lineWith(logs.String(), "ATTACK FIRED ON ARRIVAL"))
}
