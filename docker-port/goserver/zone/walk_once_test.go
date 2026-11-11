package zone

// ONE WALK PER (CHARACTER, DESTINATION) - the rule in zone\walk_once.go.
//
// THE FAULT. Every ZC_MOVE_PC the server sends restarts the client's interpolation of
// that actor, so a second walk to the point the actor is already walking to re-anchors
// the body mid-stride. The owner reads that as a twitch. The old log line could not
// even show it: "SENT cmd 3548, 30 bytes" carries no goal, so two walks that AGREE and
// two that CONTRADICT read the same.
//
// These four tests pin the fix, each one against the sender (legMoveFor, the one
// function every server-driven walk is built by) or against the real press path:
//
//  1. two walk requests for ONE destination in one tick send ONE 3548 (before: two);
//  2. a CHANGED destination is a new walk and sends its own 3548 - chained clicks are
//     not duplicates, and this is the test that stops the rule from eating them;
//  3. the line that replaces the bare SENT line carries the destination and the speed;
//  4. an attack approach that finds a walk to the same point already in flight sends
//     nothing, and the press is not lost - it keeps the walk that carries it.
//
// Each of them was run against the old legMoveFor (three lines, no record) in a
// throwaway copy of the tree; the failures are quoted in the report for this round.

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// walkPackets counts the ZC_MOVE_PC among the packets a caller built. A nil packet is
// the sender's "nothing is sent", which is the thing under test here.
func walkPackets(pkts ...[]byte) int {
	var n int
	for _, pkt := range pkts {
		if len(pkt) >= 2 && binary.LittleEndian.Uint16(pkt) == CmdZCMovePC {
			n++
		}
	}
	return n
}

// TestWalkOncePerDestination is (1): TWO requests for the SAME destination, in the same
// tick, produce exactly ONE ZC_MOVE_PC.
//
// This is the whole point of the round. Both requests reach the SAME sender - that is
// what makes them one walk, whatever reason each caller believes it has - and the
// second one answers nil, so no second packet exists to restart the client's
// interpolation.
func TestWalkOncePerDestination(t *testing.T) {
	h, sess, logs := guardSession(t, guardStand)
	// The walk line is a `-diag-events` diagnostic now, and this test counts it.
	h.DiagEvents = true
	goal := [3]int32{1100, 1000, 1}

	first, firstHandle := h.legMoveFor(sess, 0, goal, ReasonMove)
	second, secondHandle := h.legMoveFor(sess, 0, goal, ReasonMove)

	if got := walkPackets(first, second); got != 1 {
		t.Fatalf("two walk requests for (%d, %d) in one tick produced %d ZC_MOVE_PC, "+
			"want exactly 1 - a duplicate restarts the client's interpolation of the "+
			"actor mid-stride:\n%s", goal[0], goal[1], got, logs.String())
	}
	if second != nil {
		t.Fatalf("the second request answered %d byte(s), want nil (nothing sent)", len(second))
	}
	if firstHandle != secondHandle {
		t.Fatalf("the two requests name different handles: %d then %d", firstHandle,
			secondHandle)
	}
	if dest := destOf(first); dest != goal {
		t.Fatalf("the one packet that left goes to %v, want %v", dest, goal)
	}
	// The refusal is on the log with the reason, not silent: the next reading must be
	// able to see that a walk was dropped and why.
	text := logs.String()
	if !strings.Contains(text, "SENT walk REFUSED") {
		t.Errorf("the refused duplicate is not on the log:\n%s", text)
	}
	if n := strings.Count(text, "SENT walk char"); n != 1 {
		t.Errorf("%d 'SENT walk char' line(s), want 1 - one walk, one line:\n%s", n, text)
	}
}

// TestAChangedDestinationIsSentOnce is (2): a DIFFERENT destination is a new walk and
// sends its OWN packet, exactly one. A player chaining clicks is not a duplicate, and a
// rule that swallowed this would leave the character walking to the old point for ever.
func TestAChangedDestinationIsSentOnce(t *testing.T) {
	h, sess, logs := guardSession(t, guardStand)
	first := [3]int32{1100, 1000, 1}
	second := [3]int32{1000, 1120, 1}

	a, _ := h.legMoveFor(sess, 0, first, ReasonMove)
	b, _ := h.legMoveFor(sess, 0, second, ReasonMove)

	if got := walkPackets(a, b); got != 2 {
		t.Fatalf("a changed destination produced %d ZC_MOVE_PC, want 2 (one per "+
			"destination):\n%s", got, logs.String())
	}
	if dest := destOf(b); dest != second {
		t.Fatalf("the second packet goes to %v, want the NEW destination %v - the rule "+
			"must compare destinations, not swallow every second walk", dest, second)
	}
	if n := strings.Count(logs.String(), "SENT walk REFUSED"); n != 0 {
		t.Errorf("%d walk(s) were refused, want none - the destinations differ:\n%s", n,
			logs.String())
	}
}

// TestTheWalkLineCarriesTheDestination is (3): the line that replaced the bare
// "SENT cmd 3548, 30 bytes" names the character, the destination, the cell and the
// speed, so the next in-game reading can see whether two walks agree or contradict.
func TestTheWalkLineCarriesTheDestination(t *testing.T) {
	h, sess, logs := guardSession(t, guardStand)
	// The line itself is what this test reads: `-diag-events` turns it back on.
	h.DiagEvents = true
	goal := [3]int32{1100, 1000, 1}

	pkt, _ := h.legMoveFor(sess, 0, goal, ReasonMove)
	if pkt == nil {
		t.Fatalf("the first walk was not sent:\n%s", logs.String())
	}
	line := lineWith(logs.String(), "SENT walk char")
	if line == "" {
		t.Fatalf("no 'SENT walk char' line at all:\n%s", logs.String())
	}
	want := "zone: SENT walk char 0 -> (1100, 1000) cell 1 speed 469"
	if !strings.Contains(line, want) {
		t.Errorf("the walk line does not carry the goal:\n got: %s\nwant it to hold: %s",
			line, want)
	}
	// The number on the line must be the number IN the packet - a reader checks the
	// speed here against the speed the client animates at, so they cannot differ.
	if speed := destOf(pkt); speed != goal {
		t.Errorf("the packet's destination %v is not the one the line names", speed)
	}
	t.Logf("walk line: %s", line)
}

// TestAnAttackApproachDoesNotRepeatTheWalkInFlight is (4): the attack's approach finds a
// walk to the SAME point already in flight and sends NOTHING - while the press still
// counts, because the walk it set is the one that carries the hit.
//
// The walk in flight is made a walk for ANOTHER reason (atk cleared) on purpose: when it
// is this press's own walk the R91/R97 rules already answer the second press, and this
// test would then pass on the old code too. The case that was open is a click's or a
// skill's walk to the same stand point, and that is the one driven here.
func TestAnAttackApproachDoesNotRepeatTheWalkInFlight(t *testing.T) {
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
		t.Fatalf("the first press left no walk carrying it:\n%s", logs.String())
	}
	goal := w.goal
	// THE SAME POINT, FOR ANOTHER REASON: this is the state a click's walk leaves, and
	// the one the press's own guards do not look at.
	w.atk = nil

	second := pressAttack(t, h, p.sess, capturedTarget)
	if got := moves(second); got != 0 {
		t.Fatalf("the approach sent %d ZC_MOVE_PC while a walk to (%d, %d) was already in "+
			"flight, want 0 - a second one re-anchors the actor mid-stride:\n%s", got,
			goal[0], goal[1], logs.String())
	}
	// THE PRESS STILL COUNTS: the walk this press set is alive and carries its hit, so
	// the swing is not lost - only the duplicate packet is.
	if p.sess.Walk == nil || p.sess.Walk.atk == nil {
		t.Fatalf("the press was dropped, not just its packet - no walk carries the hit "+
			"any more:\n%s", logs.String())
	}
	if got := p.sess.Walk.goal; got != goal {
		t.Fatalf("the press re-aimed the walk to %v; it should keep the point in flight "+
			"%v", got, goal)
	}
	if !strings.Contains(logs.String(), "SENT walk REFUSED") {
		t.Errorf("nothing on the log says the duplicate walk was refused:\n%s",
			logs.String())
	}
}

// ---------------------------------------------------------------------------------------
// A WALK OF OURS EXCUSES A REPORT ONLY WHILE IT STILL HAS GROUND TO COVER (2026-11-03).
//
// THE OWNER'S EVIDENCE, logs\zone_go_live.log at 18:29:33 (his words: "still stubborn"):
//
//	18:29:33.056616 MOVE_PC char 1 -> (2198, -819) | body was at (2411, -738), leg 228 u
//	               MOVE_PC char 1 -> (2198, -819): a path step of the walk this server sent
//	               to (2411, -738) (228 u), not a move order
//	18:29:33.257320 SENT walk char 1 -> (2411, -739) ... ATTACK APPROACH ... 223.5 u away
//
// The body was AT the goal of the attack approach this zone had sent, and the point it
// "reported" was that walk's own START - 228 u back down the leg. There was nothing left to
// advance, so the report was the player clicking away from the fight: it was swallowed, the
// fight was not cancelled, and 200 ms later the server walked the character back.
//
// WHY THE OLD RULE COULD NOT SEE IT: walkLegCovers asks only "is this point on that leg",
// and a leg that has arrived still covers its own line - above all its own start - for ever.
// The record in walksSent then kept that finished walk alive for walkRoundFallbackAge, so
// the click passed twice over. The rule now needs BOTH: the body more than walkRoundSlack
// short of the goal, and the report strictly closer to the goal than the body is.
//
// Each test below was run against the old walkOurs in a copy of this tree; the failures are
// quoted in the report for this round.
// ---------------------------------------------------------------------------------------

var (
	// THE LOG'S OWN PAIR, literals on purpose: the owner's goal, and the point he clicked -
	// which is that walk's own start, 228 u back down it.
	arrivedWalkStart = [3]int32{2198, -819, 108}
	arrivedWalkGoal  = [3]int32{2411, -738, 108}
)

// walkThatHasArrived leaves the session in the state the owner's log caught: a walk this
// zone sent is still on the walksSent record, the body has had time to reach its goal, and
// the walk itself is gone - because that is what the tick does to a walk that arrives
// (chase.go, walk.arrived). Only the record is left, which is exactly why the fallback
// path exists and exactly how a finished walk was still excusing clicks.
func walkThatHasArrived(t *testing.T, sess *Session, idx int, from, goal [3]int32, now time.Time) {
	t.Helper()
	w := &walk{
		from: from, goal: goal,
		eta:   300 * time.Millisecond,
		began: now.Add(-2 * time.Second), sentAt: now.Add(-2 * time.Second),
		speed: 469, caster: idx,
	}
	sess.Walk = w
	if !sess.walkOnceAt(idx, goal, now) {
		t.Fatalf("walkOnceAt refused a first walk to %v - the test's own setup is wrong", goal)
	}
	// THE BODY ARRIVED AND THE WALK ENDED WITH IT: what stays behind is the record.
	sess.Walk = nil
	if w.inFlightAt(now) {
		t.Fatalf("the test's own walk is still in flight at `now` - this test needs it to "+
			"have arrived, so the body it draws is the goal %v", goal)
	}
	if left := flatDist(w.bodyAt(now), goal); left > walkRoundSlack {
		t.Fatalf("the test's own body is still %.0f u short of its goal %v, and this test "+
			"needs it to have arrived", left, goal)
	}
	if sess.walksSent[idx].w != w {
		t.Fatalf("the record does not carry the walk it belongs to - the fallback would "+
			"then have no body to measure: %+v", sess.walksSent[idx])
	}
}

// TestAReportDownAWalkThatHasArrivedIsThePlayersOrder is the owner's case, end to end
// through the real MOVE_PC path: the walk to (2411, -738) has arrived, the client reports
// the point 228 u back down that leg, and the report must be read as the player's order -
// the engagement cancelled, and the log saying WHY it was not excused.
func TestAReportDownAWalkThatHasArrivedIsThePlayersOrder(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	now := time.Now()
	walkThatHasArrived(t, p.sess, 0, arrivedWalkStart, arrivedWalkGoal, now)
	if d := flatDist(arrivedWalkStart, arrivedWalkGoal); d < 227 || d > 229 {
		t.Fatalf("the two points are %.1f u apart, and this test is written against the "+
			"log's 228 u - fix the literals, not the rule", d)
	}

	// (1) THE ANSWER ITSELF: not ours, and it says the walk had arrived.
	if v := p.sess.walkOurs(0, arrivedWalkStart, now); v.ours || !v.arrived {
		t.Fatalf("a report 228 u back down a walk that has ARRIVED answered %+v, want "+
			"ours=false and arrived=true - the walk had nothing left to advance", v)
	}

	// (2) THE MOVE_PC PATH treats it as a move order: the order is marked (the swing that
	// has come due at that instant goes out first, built from the pre-move body; anything
	// else is never built), and the log says which rule refused it.
	moved := moveReply(t, h, p.sess, arrivedWalkStart)
	if line := lineWith(logs.String(), "a path step of the walk this server sent"); line != "" {
		t.Fatalf("the finished walk still excused the report: %q\n%s", line, logs.String())
	}
	line := lineWith(logs.String(), "has ARRIVED")
	if line == "" {
		t.Fatalf("the report was not excused but nothing on the log says why - a reader "+
			"cannot tell this from a point no walk of ours ever named:\n%s", logs.String())
	}
	for _, want := range []string{
		"MOVE_PC char 0 -> (2198, -819)",
		"that walk to (2411, -738) has ARRIVED",
		"(body within 0 u)",
		"so this is the player's order",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("the ARRIVED line does not carry %q:\n%s", want, line)
		}
	}
	if order := lineWith(logs.String(), "got a PLAYER MOVE ORDER"); order == "" {
		t.Fatalf("the report was not taken as the player's order, so the fight would keep "+
			"swinging while the player walks away - the owner's \"still stubborn\":\n%s",
			logs.String())
	}

	// (3) AND THE ENGAGEMENT IS OVER: the swing was NOT due when the order arrived, so the
	// order built none - and the tick after it builds none either, it only ends the record
	// (2026-11-03). A swing built now would be a 3564 +8 on the leg that report just
	// opened, a point the client has not walked, and its handler hard-sets the actor there
	// and calls CMS_STOP (durable fact #219).
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 0 {
		t.Fatalf("the swing not yet sent produced %d hit(s) after the order, want none:\n%s",
			got, logs.String())
	}
	if p.sess.swingOf(0) != nil {
		t.Fatalf("the engagement survived a report down a walk that had arrived:\n%s",
			logs.String())
	}
	if len(moved) == 0 {
		t.Fatal("the report answered nothing at all - it still owes its own echo")
	}
	t.Logf("finished walk: %s", line)
}

// TestAStepOfAWalkStillShortOfItsGoalStaysOurs is the case the header of walkOnce.go
// documents (65 u short of the goal, on the leg, at 2026-11-01 23:48), and it must keep
// passing: an OPEN walk with ground left, reported at a point ALONG the leg and closer to
// the goal than the body is, is our own step and cancels nothing.
//
// The body here is 200 u short of its goal - two thirds of the way down a 600 u leg - so
// the test cannot pass by "the walk is nearly done" either: what makes it ours is that the
// report MOVES towards the goal from where the body already is.
func TestAStepOfAWalkStillShortOfItsGoalStaysOurs(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	// A 600 u leg, 400 u of it already walked at 400 u/s: the body is 200 u short.
	start := [3]int32{1000, 1000, 108}
	goal := [3]int32{1000, 1600, 108}
	step := [3]int32{1000, 1500, 108} // 100 u short of the goal, on the leg
	now := time.Now()
	w := &walk{
		from: start, goal: goal,
		eta:   1500 * time.Millisecond,
		began: now.Add(-time.Second), sentAt: now.Add(-time.Second),
		speed: 400, caster: 0,
		atk: &pendingAttack{idx: 0},
	}
	p.sess.Walk = w
	if !p.sess.walkOnceAt(0, goal, now) {
		t.Fatalf("walkOnceAt refused a first walk to %v - the test's own setup is wrong", goal)
	}
	if left := flatDist(w.bodyAt(now), goal); left < 199 || left > 201 {
		t.Fatalf("the test's own body is %.0f u short of its goal, and this test is "+
			"written against 200 u - fix the literals, not the rule", left)
	}

	// (1) THE ANSWER: ours. On the leg, still 200 u to walk, and the report is 100 u from
	// the goal - strictly closer than the body is.
	v := p.sess.walkOurs(0, step, now)
	if !v.ours {
		t.Fatalf("a path step 100 u short of a goal the body is still 200 u short of was "+
			"read as the player's click (%+v) - this is the ~1.1 s pause between swings "+
			"all over again", v)
	}

	// (2) AND THROUGH THE MOVE_PC PATH, where the walk record is what the decision reads:
	// the exemption line names the goal and the distance, and the engagement lives.
	moveReply(t, h, p.sess, step)
	if order := lineWith(logs.String(), "got a PLAYER MOVE ORDER"); order != "" {
		t.Fatalf("a path step of a walk with 200 u to go ended the engagement: %q\n%s",
			order, logs.String())
	}
	line := lineWith(logs.String(), "a path step of the walk this server sent")
	if line == "" {
		t.Fatalf("the step was exempted but the log does not say so:\n%s", logs.String())
	}
	for _, want := range []string{
		"MOVE_PC char 0 -> (1000, 1500)",
		"to (1000, 1600)", // the goal, not the report
		"(100 u)",         // how far the report sat from it
		"not a move order",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("the exemption line does not carry %q:\n%s", want, line)
		}
	}
	if s := p.sess.swingOf(0); s == nil || s.cancelReason != "" {
		t.Fatalf("the engagement did not survive our own path step:\n%s", logs.String())
	}
	t.Logf("200 u still to walk: %s", line)
}

// TestAReportTheBodyHasAlreadyPassedIsThePlayersOrder is the second way the new test says
// NO, and it is not the arrival case: the walk still has 200 u to go, but the report names
// a point the body has ALREADY walked past. No step of a walk goes backwards, so this is
// the player - and the log says so with both numbers, not with "arrived", because the walk
// has not arrived.
func TestAReportTheBodyHasAlreadyPassedIsThePlayersOrder(t *testing.T) {
	start := [3]int32{1000, 1000, 108}
	goal := [3]int32{1000, 1600, 108}
	now := time.Now()

	sess := &Session{}
	sess.Walk = &walk{
		from: start, goal: goal,
		eta:   1500 * time.Millisecond,
		began: now.Add(-time.Second), sentAt: now.Add(-time.Second),
		speed: 400, caster: 0,
	}

	// The body is at (1000, 1400) and this point is (1000, 1300): 300 u from the goal
	// against the body's 200 u, so it is BEHIND the body and the walk has not arrived.
	behind := [3]int32{1000, 1300, 108}
	v := sess.walkOurs(0, behind, now)
	if v.ours {
		t.Fatalf("a point the body had already walked past was called a step of that "+
			"walk (%+v) - a finished part of a leg is not a step of it", v)
	}
	if !v.covered || v.arrived {
		t.Fatalf("the verdict is %+v: the point IS on the leg and the walk has NOT "+
			"arrived, so the log must say \"no closer\", not \"arrived\"", v)
	}
	if v.left < 199 || v.left > 201 || v.dist < 299 || v.dist > 301 {
		t.Fatalf("the verdict's numbers are body %.0f u out and report %.0f u out, want "+
			"200 and 300 - they are what the log line prints", v.left, v.dist)
	}
}

// TestTheFallbackWindowIsAClockNotAnExcuse pins the two halves of the walksSent rule that
// the 18:29:33 bug ran through: the 2 s window still works for a record with no leg start
// (a caller that never matched a walk), it does NOT extend a walk that has arrived, and it
// does not reach past its own age either.
func TestTheFallbackWindowIsAClockNotAnExcuse(t *testing.T) {
	now := time.Now()
	goal := [3]int32{1100, 1000, 1}
	near := [3]int32{goal[0] + 10, goal[1], goal[2]} // 10 u off: inside walkRoundSlack

	t.Run("a point we sent, inside the window, is still ours", func(t *testing.T) {
		sess := &Session{}
		if !sess.walkOnceAt(0, goal, now) {
			t.Fatal("walkOnceAt refused a first walk - the test's own setup is wrong")
		}
		if !sess.walkWasOurs(0, near) {
			t.Fatalf("a point 10 u off a walk this zone sent %v is not excused any more",
				goal)
		}
	})

	t.Run("the same point after the window is nobody's walk", func(t *testing.T) {
		sess := &Session{}
		if !sess.walkOnceAt(0, goal, now.Add(-3*time.Second)) {
			t.Fatal("walkOnceAt refused a first walk - the test's own setup is wrong")
		}
		if sess.walkOurs(0, near, now).ours {
			t.Fatalf("a record %.0f s old still excused a report - walkRoundFallbackAge "+
				"is %.0f s and it is a clock, not an excuse",
				(3 * time.Second).Seconds(), walkRoundFallbackAge.Seconds())
		}
	})

	t.Run("a record whose walk arrived is not ours, however young", func(t *testing.T) {
		sess := &Session{}
		walkThatHasArrived(t, sess, 0, arrivedWalkStart, arrivedWalkGoal, now)
		if age := now.Sub(sess.walksSent[0].at); age > walkRoundFallbackAge {
			t.Fatalf("the test's own record is %v old, outside the window it is meant to "+
				"be inside", age)
		}
		v := sess.walkOurs(0, arrivedWalkStart, now)
		if v.ours {
			t.Fatalf("a walk that had ARRIVED excused a report %v - this is the owner's "+
				"18:29:33 swallowed click", arrivedWalkStart)
		}
		if !v.arrived {
			t.Fatalf("the verdict is %+v, so the log could not say the walk had arrived "+
				"and a reader would see only \"not ours\"", v)
		}
	})
}
