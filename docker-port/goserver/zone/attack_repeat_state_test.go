package zone

import (
	"math"
	"strings"
	"testing"
	"time"
)

// THE REPEAT IS AN ENGAGEMENT, NOT A COUNTDOWN (R106).
//
// THE OWNER'S REPORT, and the measurement behind it: "attack still not finish yet". The
// zone log (logs\go_zone1331.log) shows 41 presses producing 15 hits and 25 presses held
// by the cadence - so the pressing was never the problem. What stopped was the SWINGING:
// the repeat ended after 1, 2, 2 and 8 hits, and the neighbours say why - the character
// stands inside the monster (the player walks right up to it), the swing is refused for
// being under MinR, and the repeat dies. Every fresh press restarted the cycle from
// nothing, and the player read that as "the attack stops".
//
// SO THIS FILE PINS THE NEW RULE, one test per branch of the tick:
//
//	inside MinR   the body STEP BACKS out (arithmetic, no path-engine query) and the
//	              engagement lives - ten ticks of it, then a swing again in the band
//	out past MaxR the body walks IN and the engagement lives, and it swings on arrival
//	              in the band
//	dragged clear for less than a second  a reposition - the engagement lives and resumes
//	dragged clear for more than a second  the ONE position ending, said once, named as
//	              the player's own movement
//	target dead   still ends it (the no-regression half)
//
// THE TWO NUMBERS OF THE HYSTERESIS ARE NOT WRITTEN HERE AS LITERALS: "far" below is
// MaxR plus 300, which is more than the 100-unit slack, and the second test's "far" is
// MaxR plus 75, which is not. If a future change moves the slack, these tests say so
// through the branch they land in rather than through a copy of the constant.

// standBody puts character 0's body where the test says it is: no open walk, no reported
// move segment, so attackBodyPoint answers `pos` itself (drawnPos: a placed body does not
// move). It also DROPS any approach walk in flight, which is what the zone really does
// when the player clicks somewhere else - movePC calls cancelApproachAttack, and that
// sets sess.Walk to nil (attack_approach.go).
func standBody(t *testing.T, sess *Session, pos [3]int32) {
	t.Helper()
	sess.Walk = nil
	sess.moveFrom = pos
	sess.moveAt = time.Time{}
	sess.Pos = pos
	if len(sess.Team) == 0 {
		t.Fatal("this session has no team to stand")
	}
	sess.Team[0].Position = pos
}

// standAtGap stands character 0 where the EDGE distance to the Scarecrow is `gap` units -
// the same number attackRangeAllowed measures and the tick branches on - by putting the
// centre distance at `gap + both bodies` down the y axis, then measuring it back with the
// handler's own function so the setup cannot be a guess.
func standAtGap(t *testing.T, h *Handler, p *player, gap float64) [3]int32 {
	t.Helper()
	mon := scarecrow()
	targetR := h.monsterBodyRadius(mon)
	d := gap + PCBodyRadius + targetR
	pos := [3]int32{mon.X, mon.Y - int32(math.Round(d)), mon.Cell}
	got, _ := attackRangeAllowed(pos, [3]int32{mon.X, mon.Y, mon.Cell}, 0, 0,
		PCBodyRadius, targetR)
	if math.Abs(got-gap) > 1 {
		t.Fatalf("the test's own setup is wrong: it asked for a gap of %g and the handler's "+
			"own reach check measures %g at %v", gap, got, pos)
	}
	standBody(t, p.sess, pos)
	return pos
}

// attackRepeatLog is the ENDED line, or "" when the repeat is still running.
func attackRepeatLog(logs string) string { return lineWith(logs, "ATTACK REPEAT ENDED") }

// TestTheRepeatSurvivesTicksInsideTheMinimumReach is branch one, and it is the owner's
// own situation: the bodies OVERLAP because the player walked right up to the monster.
//
// TODAY IT DIES HERE. One tick inside MinR and the repeat is gone, after the one hit the
// press made, with "ATTACK REPEAT ENDED ... after 1 hit(s) ... produced nothing".
func TestTheRepeatSurvivesTicksInsideTheMinimumReach(t *testing.T) {
	h, logs, p := attackZone(t)

	// THE RETREAT MUST NOT ASK THE PATH ENGINE (R104): it aims inside the monster's own
	// body, which the engine treats as an obstacle, so a plan there can only fail. If the
	// step-back ever asks, this test says so instead of the owner's log.
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		t.Errorf("the repeat's step-back asked the path engine for a stand point - a retreat "+
			"is arithmetic (attackRetreatPoint), never a plan")
		return directPlan(from, target, standoff)
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	minR, _ := stanceReach(t, h, p.sess.Team[0].Stance)
	mon := scarecrow()
	start := standAtGap(t, h, p, minR-30) // 30 units INSIDE the minimum: overlapping bodies

	moves, hits := 0, 0
	for i := 0; i < 10; i++ {
		if p.sess.swingOf(0) == nil {
			t.Fatalf("tick %d of 10 ENDED the engagement while the body was INSIDE MinR %g - "+
				"that is the owner's \"attack stops\": %q", i+1, minR,
				attackRepeatLog(logs.String()))
		}
		// THE CADENCE HAS COME, which is the moment the old tick swung and died: the
		// repeat is due a swing on every one of these ten ticks.
		swingDue(t, h, p.sess, 0)
		out := h.swingTick(p.sess)
		moves += cmdCount(out, CmdZCMovePC)
		hits += swingHits(t, out)
		if p.sess.swingOf(0) == nil {
			t.Fatalf("tick %d of 10 ENDED the engagement while the body was %g INSIDE MinR "+
				"%g - the owner's attack stops here:\n%s", i+1,
				func() float64 {
					g, _ := attackRangeAllowed(start, [3]int32{mon.X, mon.Y, mon.Cell}, 0, 0,
						PCBodyRadius, h.monsterBodyRadius(mon))
					return g
				}(), minR, attackRepeatLog(logs.String()))
		}
	}
	if hits != 0 {
		t.Fatalf("the ten ticks inside MinR produced %d hit(s) - a swing from a body under "+
			"the minimum reach must never be built:\n%s", hits, logs.String())
	}
	if moves != 1 {
		t.Fatalf("the ten ticks sent %d retreat walk(s), want exactly 1 - the first tick steps "+
			"the body out and the walk in flight is not re-planned:\n%s", moves, logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the step-back left no walk with the press it holds:\n%s", logs.String())
	}
	if !w.atk.stepBack {
		t.Fatalf("the walk the repeat sent is not a step-back: %+v", w.atk)
	}
	startD := math.Hypot(float64(start[0]-mon.X), float64(start[1]-mon.Y))
	goalD := math.Hypot(float64(w.goal[0]-mon.X), float64(w.goal[1]-mon.Y))
	if !(goalD > startD) {
		t.Fatalf("the retreat does not go AWAY from the target: the body stood %g from its "+
			"centre and the walk goes to %g (%v)", startD, goalD, w.goal)
	}

	// THE PROOF IT IS STILL AN ATTACK: back in the band, the next due swing lands.
	standAtGap(t, h, p, 50)
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("after the body came back into the band the repeat produced %d hit(s), want "+
			"1 - the engagement must swing again by itself:\n%s", got, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatal("the repeat ended on the tick that swung")
	}
	if line := attackRepeatLog(logs.String()); line != "" {
		t.Fatalf("the repeat ended over a position nothing was ever wrong with: %q", line)
	}
	t.Logf("10 ticks inside MinR %g: 1 retreat walk (%v), 0 hits, still alive; back in the "+
		"band it swung again (2 hits since the press)", minR, w.goal)
}

// TestTheRepeatWalksInWhenTheBodyIsBeyondTheFarBound is branch two: a monster that walked
// away, or a player who stopped short. The tick walks IN and the engagement lives; the
// swing comes back as soon as the body is in the band.
func TestTheRepeatWalksInWhenTheBodyIsBeyondTheFarBound(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	minR, maxR := stanceReach(t, h, p.sess.Team[0].Stance)
	standAtGap(t, h, p, maxR+75) // past the far bound, inside the slack: a walk, not an end

	swingDue(t, h, p.sess, 0)
	out := h.swingTick(p.sess)
	if got := swingHits(t, out); got != 0 {
		t.Fatalf("a body %g past MaxR %g built %d hit(s) - the reach must refuse the swing",
			maxR+75, maxR, got)
	}
	if got := cmdCount(out, CmdZCMovePC); got != 1 {
		t.Fatalf("the tick sent %d walk(s) for a body past MaxR %g, want 1 - the engagement "+
			"walks IN:\n%s", got, maxR, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the repeat ENDED for being out of reach:\n%s", attackRepeatLog(logs.String()))
	}

	standAtGap(t, h, p, (minR+maxR)/2) // the walk arrives: the body is in the band
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the swing after walking in produced %d hit(s), want 1:\n%s", got,
			logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatal("the repeat ended on the swing that landed")
	}
	t.Logf("out at MaxR+75: 1 walk in, alive; in the band: 1 hit, alive")
}

// TestABriefDragOutOfReachDoesNotEndTheRepeat is the hysteresis, the half that must NOT
// end: a body carried clear for a moment - a player clicking past the monster, a step
// that overshot - is a reposition, and the engagement walks it back.
func TestABriefDragOutOfReachDoesNotEndTheRepeat(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	minR, maxR := stanceReach(t, h, p.sess.Team[0].Stance)
	far := maxR + 300 // more than the 100-unit slack, so the clock starts

	standAtGap(t, h, p, far)
	swingDue(t, h, p.sess, 0) // the swing is due: this is where the old tick ended it
	h.swingTick(p.sess)       // tick 1: starts the "kept clear" clock and walks IN
	if p.sess.swingOf(0) == nil {
		t.Fatalf("one tick %g clear of MaxR %g ended the repeat:\n%s", far, maxR,
			attackRepeatLog(logs.String()))
	}

	standAtGap(t, h, p, far) // still dragged clear, and still well inside the second
	swingDue(t, h, p.sess, 0)
	h.swingTick(p.sess) // tick 2
	if p.sess.swingOf(0) == nil {
		t.Fatalf("two ticks %g clear of MaxR %g, milliseconds apart, ended the repeat - the "+
			"hysteresis is not working:\n%s", far, maxR, attackRepeatLog(logs.String()))
	}

	// AND IT RESUMES: the player comes back, and the next due swing lands.
	standAtGap(t, h, p, (minR+maxR)/2)
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the swing after the brief drag produced %d hit(s), want 1:\n%s", got,
			logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatal("the repeat ended instead of resuming after the brief drag")
	}
	if line := attackRepeatLog(logs.String()); line != "" {
		t.Fatalf("a brief drag wrote an ENDED line: %q", line)
	}
	t.Logf("dragged %g clear (MaxR %g) for milliseconds: no ending, and it swung again", far, maxR)
}

// TestADragOutOfReachLongerThanASecondEndsTheRepeatOnce is the other half: the player
// really did carry the character away, so the engagement ends - ONCE, naming the player's
// own movement, and never again on the next tick.
//
// THIS TEST SPENDS 1.1 SECONDS OF WALL TIME, and it is the only one in this file that
// sleeps: the threshold is a second of real time by definition, and faking the clock would
// test a different rule from the one the player's hands meet.
func TestADragOutOfReachLongerThanASecondEndsTheRepeatOnce(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	_, maxR := stanceReach(t, h, p.sess.Team[0].Stance)
	far := maxR + 300

	standAtGap(t, h, p, far)
	swingDue(t, h, p.sess, 0)
	h.swingTick(p.sess) // starts the clock
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the repeat ended on the FIRST far tick, before the second was up - the "+
			"hysteresis is not working: %q", attackRepeatLog(logs.String()))
	}
	standAtGap(t, h, p, far) // the drag continues
	swingDue(t, h, p.sess, 0)
	h.swingTick(p.sess) // still inside the second: alive
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the repeat ended before the second was up:\n%s",
			attackRepeatLog(logs.String()))
	}

	time.Sleep(1100 * time.Millisecond)
	standAtGap(t, h, p, far)
	swingDue(t, h, p.sess, 0)
	h.swingTick(p.sess)

	if p.sess.swingOf(0) != nil {
		t.Fatalf("the repeat survived %g clear of MaxR %g for more than a second - nothing "+
			"can be swung at from there", far, maxR)
	}
	line := attackRepeatLog(logs.String())
	if line == "" {
		t.Fatalf("the repeat ended with no ENDED line:\n%s", logs.String())
	}
	if !strings.Contains(line, "the player's own movement") {
		t.Fatalf("the ending does not name the player's own movement: %q", line)
	}
	if !strings.Contains(line, "of clear ground") {
		t.Fatalf("the ending does not name the edge distance it measured: %q", line)
	}

	h.swingTick(p.sess)
	if n := strings.Count(logs.String(), "ATTACK REPEAT ENDED"); n != 1 {
		t.Fatalf("a repeat that is already gone wrote %d ENDED line(s), want 1:\n%s", n,
			logs.String())
	}
	t.Logf("ended once, for the right reason: %s", line)
}

// TestTheDeathOfTheTargetStillEndsTheRepeat is the no-regression half: the four endings
// that remain must still work. The target's HP reaching 0 in this zone's own ledger is the
// death it can see (FieldChar_FSM.cpp:136-140).
func TestTheDeathOfTheTargetStillEndsTheRepeat(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	h.spawnMu.Lock()
	h.monsterHP[capturedTarget] = 0
	h.spawnMu.Unlock()

	swingDue(t, h, p.sess, 0)
	if out := h.swingTick(p.sess); len(out) != 0 {
		t.Fatalf("a swing at a body whose HP reached 0 produced %v, want nothing:\n%s",
			kinds(out), logs.String())
	}
	if p.sess.swingOf(0) != nil {
		t.Fatal("the repeat kept swinging at a body whose HP had reached 0")
	}
	line := attackRepeatLog(logs.String())
	if !strings.Contains(line, "no HP left") {
		t.Fatalf("the ending did not name the death: %q\n%s", line, logs.String())
	}
	t.Logf("ended for the death: %s", line)
}
