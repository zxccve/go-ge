package zone

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// =================================================================================
// R102: THE FIRE ON ARRIVAL OBEYS THE SAME MinR AS THE PRESS, AND ITS LINE NEVER LIES
// =================================================================================
//
// THE MEASUREMENT (owner's live session, logs\go_zone1331.log, 18:01:33.948 and
// 18:01:36.216). Each press produced this exact block and NOTHING else - no `SENT cmd
// 3564`, no `ATTACK commander ... damage`:
//
//	REQ ATTACK char 1 (1794) target 536870916
//	ATTACK APPROACH: ... target ... is 45 of clear ground away and stance 10510 ...
//	SENT cmd 3548, 30 bytes
//	ATTACK FIRED ON ARRIVAL char 1 (1794) at 50 of reach 200 - the press was held 104ms
//	ATTACK APPROACH: the walk ended and target ... is still 45 away, stance 10510
//	    ("BlandirCruz") reach MinR 50 ...
//
// Three faults in five lines, and the tests below are one each:
//
//  1. the FIRE did not know the too-near case, so a press that arrived at 45 against
//     MinR 50 was answered "not yet", the walk's clock fired instead, the replay
//     re-measured 45, and replanAfterWalk had no step-back to re-issue - so the press was
//     consumed and the character stood beside the monster;
//  2. "ATTACK FIRED ON ARRIVAL" printed BEFORE the hit was built, so it claimed a hit that
//     never left (the line the owner and the PM read as proof);
//  3. a press consumed with no packet printed nothing that said so.
//
// STANCE AND NUMBERS. The fixture is stepBackStance (attack_ring_test.go): a real row with
// MinR 50 .. MaxR 900, and a 60-unit monster body beside the player's 50. Every distance
// below is COMPUTED from that row and those two bodies and then CHECKED before anything is
// asserted about it, so a row that changed cannot make a test vacuous.

// fireArrivalFar is the body's point OUTSIDE MaxR 900, so a press from here walks in:
// 1200 units from the Scarecrow's centre = 1090 of clear ground.
var fireArrivalFar = [3]int32{scarecrow().X, scarecrow().Y - 1200, scarecrow().Cell}

// fireArrivalEdge is the body's point at `edge` units of CLEAR GROUND from the target:
// the edge distance plus the two bodies, measured from the target's centre - the same
// edge-to-edge rule AsAttackArea uses (FieldChar.cpp:118).
func fireArrivalEdge(edge float64) [3]int32 {
	centre := edge + PCBodyRadius + ringTargetBody
	return [3]int32{scarecrow().X, scarecrow().Y - int32(centre), scarecrow().Cell}
}

// hitAttacks counts the packets that ARE a hit: the 3564 single attack (and a splash's
// 3566), one per swing. The ZC_ADD_HP that follows is the damage of the same swing, so it
// is not counted again - "one press, one hit" is the number this test family asserts.
func hitAttacks(out [][]byte) int {
	var n int
	for _, pkt := range out {
		if len(pkt) < 2 {
			continue
		}
		switch binary.LittleEndian.Uint16(pkt) {
		case CmdZCAttackSingle, CmdZCAttackSplash:
			n++
		}
	}
	return n
}

// movesOntoTheMinRing counts the ZC_MOVE_PC in one answer whose destination is `want` from
// the target's CENTRE. It is what separates a step-back from the move echo the same report
// sends to the point the client itself named - both are 3548s for the same actor.
func movesOntoTheMinRing(out [][]byte, target [3]int32, want float64) int {
	var n int
	for _, pkt := range out {
		if len(pkt) < 22 || binary.LittleEndian.Uint16(pkt) != CmdZCMovePC {
			continue
		}
		dest := destOf(pkt)
		got := math.Hypot(float64(dest[0]-target[0]), float64(dest[1]-target[1]))
		if math.Abs(got-want) <= 1.5 {
			n++
		}
	}
	return n
}

// fireArrivalZone is the fixture every test below starts from: character 0 standing at
// `from` in stepBackStance (MinR 50 .. MaxR 900) with the 60-unit monster, and a direct
// plan so the step-back's destination is arithmetic and not a map.
func fireArrivalZone(t *testing.T) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := stepBackZone(t, fireArrivalFar)
	h.Plan = directPlan
	return h, logs, p
}

// TestAFireOnArrivalAt45OfMinR50StepsBackInsteadOfFiring is (1): the press walks in, the
// body arrives at 45 of clear ground against MinR 50, and the fire answers it the way the
// PRESS answers it - ONE step-back move out to the min ring, the press still held, NO
// "ATTACK FIRED ON ARRIVAL" line and NO attack packet.
//
// BEFORE THIS ROUND the same report wrote the fire line and sent nothing: the fire was
// asked `inRange`, the too-near answer was read as "not yet", the walk's clock fired
// instead, the replay re-measured 45 and replanAfterWalk refused it on the 300 ms chase
// clock. That is the owner's block at 18:01:33.948, line for line.
func TestAFireOnArrivalAt45OfMinR50StepsBackInsteadOfFiring(t *testing.T) {
	h, logs, p := fireArrivalZone(t)
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}

	// THE FIXTURE'S OWN ARITHMETIC, before anything is asserted about it.
	pressGap, pressIn := attackRangeAllowed(fireArrivalFar, target, 50, 900, PCBodyRadius,
		ringTargetBody)
	if pressIn || pressGap <= 900 {
		t.Fatalf("the press point is %g of clear ground (in reach: %v), which is NOT past "+
			"MaxR 900 - this test cannot be about a press that walks", pressGap, pressIn)
	}
	arrival := fireArrivalEdge(45)
	gap, in := attackRangeAllowed(arrival, target, 50, 900, PCBodyRadius, ringTargetBody)
	if in || math.Abs(gap-45) > 0.001 {
		t.Fatalf("the report point is %g of clear ground (in reach: %v), want exactly 45 "+
			"against MinR 50 - this test cannot be about the too-near case", gap, in)
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press from %g away produced %d move(s), want 1:\n%s", pressGap,
			moves(out), logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 0 {
		t.Fatalf("the fire line was written %d time(s) BEFORE the body was in reach:\n%s",
			n, logs.String())
	}

	rep := arrivalAt(t, h, p.sess, arrival)

	// NO HIT. 45 is inside the minimum reach, so nothing may be swung at it.
	if n := hitAttacks(rep); n != 0 {
		t.Fatalf("a report putting the body %g of clear ground from the target (MinR 50) "+
			"sent %d attack packet(s):\n%s", gap, n, logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 0 {
		t.Fatalf("ATTACK FIRED ON ARRIVAL was written %d time(s) for a body at %g against "+
			"MinR 50, with no attack packet built - that line is read as proof that a hit "+
			"went out:\n%s", n, gap, logs.String())
	}

	// ONE STEP-BACK MOVE, to the min ring (MinR + the two bodies from the centre).
	want := stepBackMinRing(t, h)
	if n := movesOntoTheMinRing(rep, target, want); n != 1 {
		t.Fatalf("the report answered with %d move(s) to the min ring (%g from the "+
			"target's centre), want exactly 1 - the press path's own answer to IsAttackArea "+
			"-1 is GetAttackMinPos. The answer was %v:\n%s", n, want, kinds(rep),
			logs.String())
	}

	// THE PRESS IS HELD, not consumed: the hit is still owed to the walk out.
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the step-back left no walk with a held press:\n%s", logs.String())
	}
	if w.castDone {
		t.Fatalf("the press was consumed by the step-back instead of held:\n%s",
			logs.String())
	}
	if !w.atk.stepBack {
		t.Errorf("the walk is not marked as a step-back - a re-plan would aim the far ring")
	}
	if line := lineWith(logs.String(), "STEPPING BACK"); line == "" {
		t.Fatalf("the step-back wrote no line - the card has nothing to read:\n%s",
			logs.String())
	}
	// AND THE REPORT THAT ASKED FOR IT IS NOT THE CLICK THAT CANCELS IT: the cancel runs
	// right after this check in movePC (zone.go), and a press dropped there would be the
	// same silent loss one line later.
	if n := countLines(logs.String(), "ATTACK approach CANCELLED"); n != 0 {
		t.Fatalf("the report that asked for the step-back then CANCELLED it (%d line(s)):"+
			"\n%s", n, logs.String())
	}
	t.Logf("press %g away, arrival %g (MinR 50) -> 1 step-back to %g, no hit, no fire "+
		"line", pressGap, gap, want)
}

// TestTheFireStepBackFiresOnTheNextInReachReport is (2): the step-back is a HELD press, so
// the next report that puts the body at 60 of clear ground - inside MinR 50 .. MaxR 900 -
// fires exactly ONE attack packet and writes the fire line once.
func TestTheFireStepBackFiresOnTheNextInReachReport(t *testing.T) {
	h, logs, p := fireArrivalZone(t)
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}

	inReach := fireArrivalEdge(60)
	gap, in := attackRangeAllowed(inReach, target, 50, 900, PCBodyRadius, ringTargetBody)
	if !in || math.Abs(gap-60) > 0.001 {
		t.Fatalf("the second report point is %g of clear ground (in reach: %v), want "+
			"exactly 60 - inside MinR 50 .. MaxR 900", gap, in)
	}
	tooNear := fireArrivalEdge(45)
	if nearGap, nearIn := attackRangeAllowed(tooNear, target, 50, 900, PCBodyRadius,
		ringTargetBody); nearIn || math.Abs(nearGap-45) > 0.001 {
		t.Fatalf("the first report point is %g of clear ground (in reach: %v), want "+
			"exactly 45 - outside MinR 50", nearGap, nearIn)
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	if n := hitAttacks(arrivalAt(t, h, p.sess, tooNear)); n != 0 {
		t.Fatalf("the too-near report fired %d attack packet(s), want 0:\n%s", n,
			logs.String())
	}
	if p.sess.Walk == nil || p.sess.Walk.atk == nil || p.sess.Walk.castDone {
		t.Fatalf("the too-near report did not leave a held press:\n%s", logs.String())
	}

	rep := arrivalAt(t, h, p.sess, inReach)
	if n := hitAttacks(rep); n != 1 {
		t.Fatalf("the report that puts the body %g of clear ground from the target "+
			"produced %v (%d hit packet(s)), want exactly one %d:\n%s", gap, kinds(rep), n,
			CmdZCAttackSingle, logs.String())
	}
	if got := recvs(rep); got[0].cmd != CmdZCAttackSingle {
		t.Fatalf("the first packet of the answer is %d, want %d (the hit):\n%s",
			got[0].cmd, CmdZCAttackSingle, logs.String())
	}
	line := lineWith(logs.String(), "ATTACK FIRED ON ARRIVAL")
	if line == "" {
		t.Fatalf("the fire wrote no line - the card has nothing to read:\n%s", logs.String())
	}
	if !strings.Contains(line, "at 60 of reach 900") {
		t.Errorf("the fire line does not name the distance it landed at: %s", line)
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("the fire line was written %d time(s), want 1:\n%s", n, logs.String())
	}
	t.Logf("step back from 45, then a report at %g -> %v: %s", gap, kinds(rep), line)
}

// TestTheFiredLineAndTheAttackPacketAreInseparable is (3): over a series of presses the
// number of "ATTACK FIRED ON ARRIVAL" lines equals the number of attack packets. The line
// is a claim about the wire, so it may not be written where nothing was sent - and the
// series contains TWO different reasons nothing is sent, so the equality is not lucky:
//
//	one arrival inside the character's own ASPD delay (the gate is R94's rule, and it says
//	    so on its own line) - the press is refused AFTER the fire check has already said
//	    "in reach", which is exactly where the old code wrote the proof line and sent
//	    nothing;
//	one arrival too near, which is the step-back above.
//
// THE ASPD DELAY IS RESET BY HAND for the last press (noteAttack with the zero time): what
// this test measures is the line against the wire, not how long a character must wait, and
// a series that never gets a second hit would prove very little.
func TestTheFiredLineAndTheAttackPacketAreInseparable(t *testing.T) {
	h, logs, p := fireArrivalZone(t)

	steps := []struct {
		at    [3]int32
		reset bool // clear the ASPD delay first, so this press is not gated
	}{
		{fireArrivalEdge(60), true},  // inside -> one hit
		{fireArrivalEdge(60), false}, // inside, but inside the delay too -> refused
		{fireArrivalEdge(45), false}, // too near -> a step-back, no hit
		{fireArrivalEdge(60), true},  // inside, delay cleared -> one hit
	}

	var pkts int
	for i, s := range steps {
		// BACK OUT OF REACH FIRST, so every cycle's press is a press that WALKS: a press
		// made from inside the reach is answered at once and never becomes a held one.
		//
		// THE BODY IS PUT THERE, NOT REPORTED (R104): a move report names a destination, so
		// it moves nothing. The held walk's own segment is what the fire reads.
		theBodyIsAt(t, p.sess, fireArrivalFar)
		if s.reset {
			p.sess.noteAttack(0, time.Time{})
		}
		if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
			t.Fatalf("cycle %d: the press produced %d move(s), want 1:\n%s", i+1,
				moves(out), logs.String())
		}
		pkts += hitAttacks(arrivalAt(t, h, p.sess, s.at))
	}

	lines := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL")
	if lines != pkts {
		t.Fatalf("ATTACK FIRED ON ARRIVAL was written %d time(s) and %d attack packet(s) "+
			"went out over %d presses - the line and the packet must be inseparable:\n%s",
			lines, pkts, len(steps), logs.String())
	}
	if lines < 2 || pkts < 2 {
		t.Fatalf("%d fire line(s) and %d attack packet(s) over %d presses: the series must "+
			"contain at least two real hits to prove anything:\n%s", lines, pkts, len(steps),
			logs.String())
	}
	t.Logf("%d presses -> %d fire lines, %d attack packets", len(steps), lines, pkts)
}

// TestAFireOnArrivalExactlyAtMinRFires is (4): the boundary belongs to "inside". A body at
// exactly MinR 50 of clear ground fires - ONE attack packet and one fire line - because
// IsAttackArea refuses only `minR > ceil(len)` (FieldChar.cpp:118), i.e. strictly under.
func TestAFireOnArrivalExactlyAtMinRFires(t *testing.T) {
	h, logs, p := fireArrivalZone(t)
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}

	onMin := fireArrivalEdge(50)
	gap, in := attackRangeAllowed(onMin, target, 50, 900, PCBodyRadius, ringTargetBody)
	if !in || math.Abs(gap-50) > 0.001 {
		t.Fatalf("the report point is %g of clear ground (in reach: %v), want exactly 50 - "+
			"the near bound itself, which belongs to the inside", gap, in)
	}

	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	rep := arrivalAt(t, h, p.sess, onMin)
	if n := hitAttacks(rep); n != 1 {
		t.Fatalf("a body exactly at MinR (%g of clear ground) produced %v (%d hit "+
			"packet(s)), want exactly one %d:\n%s", gap, kinds(rep), n,
			CmdZCAttackSingle, logs.String())
	}
	line := lineWith(logs.String(), "ATTACK FIRED ON ARRIVAL")
	if line == "" {
		t.Fatalf("the fire at the near bound wrote no line:\n%s", logs.String())
	}
	if !strings.Contains(line, "at 50 of reach 900") {
		t.Errorf("the fire line does not name the near bound it landed on: %s", line)
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("the fire line was written %d time(s), want 1:\n%s", n, logs.String())
	}
	if n := countLines(logs.String(), "STEPPING BACK"); n != 0 {
		t.Fatalf("%d step-back line(s) for a body exactly ON the near bound:\n%s", n,
			logs.String())
	}
	t.Logf("edge %g (== MinR) -> %v: %s", gap, kinds(rep), line)
}

// =================================================================================
// R103: ONE PRESS, EXACTLY ONE OUTCOME - AND A CONSUMED PRESS NAMES ITS REASON
// =================================================================================
//
// THE MEASUREMENT (owner's live session, logs\go_zone1331.log, 18:29:22.430265). ONE
// press produced FOUR lines at the same millisecond, and two of them contradict each
// other:
//
//	ATTACK APPROACH: a new press replaces the walk to the attack on target 536870916
//	    (2458, -817, cell 108)
//	ATTACK APPROACH: commander 25 char 1 (1794) target 536870916 ... is 559.63 of clear
//	    ground away ... the hit fires when the walk ends
//	ATTACK HELD press CONSUMED, no hit: char 1 (1794) target 536870916 was 187.36 of
//	    clear ground - inside MinR 50 .. MaxR 200 - ... but no attack packet was built
//	SENT cmd 3548, 30 bytes
//
// THE CAUSE, one line of the fire-on-arrival path. fireHeldAttackOnArrival measured the
// body at `at` (the walk's own point: 187 of clear ground, in reach), then called the
// builder, which measured the SAME press AGAIN from drawnPos - a different model, and
// for a character that is not the slot the session drives a STALE one (placeWalker
// writes Team[slot].Position, which a follower's drawnPos does not read while its own
// reported segment is open). The builder answered "559.63, out of reach", so it re-planned
// the walk and returned a MOVE packet; the caller saw "no attack packet" and wrote the
// consumed line. One press, two outcomes - and the walk it sent was live, so the press
// was NOT consumed.
//
// AND THE LINE ITSELF SAID NOTHING. "no attack packet was built (the line directly above
// says why)" was false: the line above is the approach plan, which is not a reason.
//
// THE THREE TESTS ARE ONE EACH: a press the body reaches is ONE hit and neither line; a
// press the builder cannot answer is handed to the walk and is never called consumed;
// and a press the attack-speed gate holds carries that reason ON the consumed line.

// fireArrivalFollowerZone is the owner's own shape: the presses in logs\go_zone1331.log
// are `char 1 (1794)`, so the character pressed for is NOT slot 0. The session's own move
// segment (Session.Pos, which drawnPos answers for the slot the player drives) therefore
// belongs to slot 1, and placeWalker's Team[slot].Position write is invisible to it.
func fireArrivalFollowerZone(t *testing.T) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs, p := fireArrivalZone(t)
	lead := p.sess.Team[0]
	follow := lead
	follow.Handle = 1795
	p.sess.Team = append(p.sess.Team, follow)
	p.sess.Control = 1
	staleSegment(p.sess, 1, fireArrivalFar)
	return h, logs, p
}

// staleSegment puts the session's own move segment where the client last reported this
// slot, without moving the walk: the live session's state between two reports, which is
// the whole of a SERVER-driven walk (observeWalk, R64).
func staleSegment(sess *Session, idx int, at [3]int32) {
	sess.Pos, sess.moveFrom, sess.moveAt = at, at, time.Now()
	if idx >= 0 && idx < len(sess.Team) {
		sess.Team[idx].Position = at
	}
}

// theBodyIsAt drops whatever walk is in flight - the way a click elsewhere drops it - and
// puts the attacking character's body at `at`, so the next press is a NEW decision measured
// from there.
//
// A MOVE REPORT IS NOT A WAY TO DO THIS ANY MORE (R104): it names a DESTINATION, so the body
// has not walked there when it arrives, and while a walk IS in flight the body's point is
// that walk's own segment, not the session's.
func theBodyIsAt(t *testing.T, sess *Session, at [3]int32) {
	t.Helper()
	sess.Walk = nil
	sess.clearWalks()
	staleSegment(sess, 0, at)
}

// pressAttackAs sends one real CZ_ATTACK for `idx`, through the dispatch, exactly as the
// client does - pressAttack is slot 0 only, and the shape here is slot 1.
func pressAttackAs(t *testing.T, h *Handler, sess *Session, idx uint8, target uint32) [][]byte {
	t.Helper()
	f := attackFrame(idx, target)
	out, err := h.Reply(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZAttack, Size: len(f), Data: f,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return out
}

// TestAnInReachArrivalIsOneHitAndNeitherLine is (1): a press whose walk brings the body
// INSIDE the stance's reach is answered by exactly ONE hit, and by neither an approach
// line nor a consumed line. One press, one outcome.
//
// BEFORE THE CHANGE the same tick produced a walk plan AND the consumed line and no hit
// at all - the live block above, in a test.
func TestAnInReachArrivalIsOneHitAndNeitherLine(t *testing.T) {
	h, logs, p := fireArrivalFollowerZone(t)
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}

	// THE FIXTURE'S OWN ARITHMETIC, checked before anything is asserted about it.
	if gap, in := attackRangeAllowed(fireArrivalFar, target, 50, 900, PCBodyRadius,
		ringTargetBody); in || gap <= 900 {
		t.Fatalf("the press point is %g of clear ground (in reach: %v), which is NOT past "+
			"MaxR 900 - this test cannot be about a press that walks", gap, in)
	}
	if out := pressAttackAs(t, h, p.sess, 1, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	if p.sess.Walk == nil || p.sess.Walk.atk == nil {
		t.Fatalf("the press left no held attack walk:\n%s", logs.String())
	}

	walkToFire(t, p.sess)
	pastChasePace(t, p.sess)
	at := p.sess.Walk.bodyAt(time.Now())
	gap, in := attackRangeAllowed(at, target, 50, 900, PCBodyRadius, ringTargetBody)
	if !in {
		t.Fatalf("the body the walk has walked to is %g of clear ground, which is NOT "+
			"inside MinR 50 .. MaxR 900 - this test cannot be about an in-reach arrival",
			gap)
	}
	approachesBefore := countLines(logs.String(), "ATTACK APPROACH")

	out := h.chaseTick(p.sess)

	if n := hitAttacks(out); n != 1 {
		t.Fatalf("the arrival at %g of clear ground (inside MinR 50 .. MaxR 900) produced "+
			"%v (%d hit packet(s)), want exactly one %d:\n%s", gap, kinds(out), n,
			CmdZCAttackSingle, logs.String())
	}
	if n := moves(out); n != 0 {
		t.Fatalf("the same press ALSO sent %d move(s) - one press may not both hit and "+
			"walk:\n%s", n, logs.String())
	}
	// THE PRESS'S OWN WALK LINE IS THE ONE IT EARNED (it was 1090 away); the ARRIVAL must
	// not add a second one, which is what "re-plan the walk AND call the press consumed"
	// used to look like.
	if n := countLines(logs.String(), "ATTACK APPROACH"); n != approachesBefore {
		t.Fatalf("the arrival added %d approach line(s) to the press's own %d, so the log "+
			"says the character walked again while it hit:\n%s", n-approachesBefore,
			approachesBefore, logs.String())
	}
	if n := countLines(logs.String(), "press CONSUMED"); n != 0 {
		t.Fatalf("the same press that HIT was also called CONSUMED %d time(s):\n%s", n,
			logs.String())
	}
	if n := countLines(logs.String(), "ATTACK FIRED ON ARRIVAL"); n != 1 {
		t.Fatalf("%d fire line(s) for one hit, want exactly 1:\n%s", n, logs.String())
	}
	t.Logf("arrival at %g of clear ground -> %v, no walk line, no consumed line", gap,
		kinds(out))
}

// TestAnOutOfReachArrivalIsOneWalkAndNeverConsumed is (2): a press the builder cannot
// answer from where the body stands is HANDED TO THE WALK - one walk packet, one plan
// line - and is never also reported as a consumed press.
//
// THE STANCE ROW IS THE YARDSTICK, and it can change under a press: the press is made in
// stepBackStance (MinR 50 .. MaxR 900), and by the time the body is on the ring the same
// row states MaxR 300 (a table reload). The press's own stored reach still says 754 of
// clear ground is inside; the row the body stands in now says it is 454 past it. The
// honest answer is the reference's case 1 - walk again - and NOT "the press is spent".
//
// BEFORE THE CHANGE this exact moment wrote the plan line AND the consumed line for the
// same press.
func TestAnOutOfReachArrivalIsOneWalkAndNeverConsumed(t *testing.T) {
	h, logs, p := fireArrivalFollowerZone(t)
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}

	if out := pressAttackAs(t, h, p.sess, 1, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	if p.sess.Walk == nil || p.sess.Walk.atk == nil {
		t.Fatalf("the press left no held attack walk:\n%s", logs.String())
	}
	walkToFire(t, p.sess)
	pastChasePace(t, p.sess)
	at := p.sess.Walk.bodyAt(time.Now())

	// THE ROW SHRINKS UNDER THE PRESS, and both halves are checked: the press's stored
	// reach still covers the body, the row's does not.
	row := h.Stances.Combat(stepBackStance)
	oldMax := row.MaxR
	row.MaxR = 300
	h.Stances.combat[int(stepBackStance)] = row
	if gap, in := attackRangeAllowed(at, target, row.MinR, row.MaxR, PCBodyRadius,
		ringTargetBody); in {
		t.Fatalf("the body is %g of clear ground and the shrunken row (MaxR 300) still "+
			"covers it - this test cannot be about a body the row refuses", gap)
	}
	if gap, in := attackRangeAllowed(at, target, 50, oldMax, PCBodyRadius,
		ringTargetBody); !in {
		t.Fatalf("the body is %g of clear ground and the PRESS's stored MaxR %g does not "+
			"cover it - this test cannot be about the press and the row disagreeing", gap,
			oldMax)
	}

	out := h.chaseTick(p.sess)

	if n := hitAttacks(out); n != 0 {
		t.Fatalf("a body the row does not reach was hit %d time(s):\n%s", n, logs.String())
	}
	if n := moves(out); n != 1 {
		t.Fatalf("the out-of-reach arrival sent %v (%d move(s)), want exactly ONE walk:\n%s",
			kinds(out), n, logs.String())
	}
	if n := countLines(logs.String(), "press CONSUMED"); n != 0 {
		t.Fatalf("the press that was HANDED TO THE WALK was also called CONSUMED %d "+
			"time(s) - a press has one outcome, and this one is the walk:\n%s", n,
			logs.String())
	}
	if n := countLines(logs.String(), "WALKING AGAIN"); n != 1 {
		t.Fatalf("%d re-plan line(s) for one out-of-reach arrival, want exactly 1:\n%s", n,
			logs.String())
	}
	gapNow, _ := attackRangeAllowed(at, target, row.MinR, row.MaxR, PCBodyRadius,
		ringTargetBody)
	t.Logf("row MaxR %g -> 300 under the press: the body at %g is out of reach, one walk, "+
		"no consumed line", oldMax, gapNow)
}

// TestTheConsumedLineNamesTheAttackSpeedGate is (3): a press the attack-speed gate holds
// writes the consumed line, and THAT line carries the cadence reason - not merely "no
// packet was built".
//
// It is the commonest of the 72 in the live session and the one nothing could be read
// from: the gate writes its own "ATTACK held" line, the fire writes "CONSUMED, no hit",
// and nothing joined them.
func TestTheConsumedLineNamesTheAttackSpeedGate(t *testing.T) {
	h, logs, p := fireArrivalZone(t)
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}

	// A FIRST HIT, so the character's own attack-speed clock is armed.
	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	inReach := fireArrivalEdge(60)
	if gap, in := attackRangeAllowed(inReach, target, 50, 900, PCBodyRadius,
		ringTargetBody); !in || math.Abs(gap-60) > 0.001 {
		t.Fatalf("the arrival point is %g of clear ground (in reach: %v), want exactly 60",
			gap, in)
	}
	if n := hitAttacks(arrivalAt(t, h, p.sess, inReach)); n != 1 {
		t.Fatalf("the first arrival produced %d hit packet(s), want 1:\n%s", n,
			logs.String())
	}

	// BACK OUT OF REACH, so the next press is a HELD one and has to arrive.
	theBodyIsAt(t, p.sess, fireArrivalFar)
	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the second press produced %d move(s), want 1:\n%s", moves(out),
			logs.String())
	}

	before := countLines(logs.String(), "press CONSUMED")
	rep := arrivalAt(t, h, p.sess, inReach)
	if n := hitAttacks(rep); n != 0 {
		t.Fatalf("the arrival inside the character's own attack delay sent %d hit "+
			"packet(s), want 0:\n%s", n, logs.String())
	}
	after := countLines(logs.String(), "press CONSUMED")
	if after != before+1 {
		t.Fatalf("the held press wrote %d consumed line(s), want exactly 1:\n%s",
			after-before, logs.String())
	}
	line := lastLineWith(logs.String(), "press CONSUMED")
	if !strings.Contains(line, "attack-speed gate") {
		t.Fatalf("the consumed line does not name the gate that produced it - it must say "+
			"the attack-speed gate held the press, not that no packet was built:\n%s", line)
	}
	if !strings.Contains(line, "after the last hit") {
		t.Fatalf("the consumed line names no cadence: the gate's whole reason is the time "+
			"since the last hit against this character's own delay:\n%s", line)
	}
	t.Logf("consumed line: %s", line)
}

// =================================================================================
// R104: THE SWING IS BUILT FROM THE BODY THE CLIENT HAS DRAWN, AND NOTHING IS PLACED
// =================================================================================
//
// THE MEASUREMENT (owner's live session, logs\go_zone1331.log, 19:13:21.181 and
// 19:13:22.082): 30 hits in 35 presses, so the press lane works - but 12 of 25 swings
// reported a position that is not on the segment they named, and consecutive swings jumped
// 217 to 506 units. Two lines, back to back:
//
//	SWING POS char 1 (1794) pos (2392, -770, cell 108) from segment (2605, -1299) -> (2997, -1170) elapsed 1792ms
//	SWING POS char 1 (1794) pos (2734, -928, cell 55)  from segment (2997, -1170) -> (2392, -770) elapsed 900ms
//
// The first pos is the DESTINATION of a CZ_MOVE_PC that named it two lines later, ~600 units
// from where that segment had drawn the body; and the line above the fire said "ATTACK
// FIRED ON ARRIVAL char 1 ... at 153.74 of reach 200" - a hit measured, and sent, at a point
// the body never walked to. The 3564's +8 hard-sets the client's own actor (GeActor.cpp:309),
// so that is the jump on screen.
//
// THE CAUSE was in this tree: the fire took the point the CALLER named (a move report's
// destination) and called placeWalker to put the model there before building the hit. The
// reference does neither - it builds the attack from `m_pos`, the body's live position
// (source2/geServer/ZServerLib/FieldChar.cpp:424), and its attack path never moves the
// character to its goal.
//
// THE FOUR TESTS ARE ONE EACH: the point is on the segment the line names and it is the
// point in the packet; two swings in one walk are at most speed x elapsed apart; a body short
// of its stand point does not fire early; and no fire path writes the model.

// swingRead is one `SWING POS` line as the log carries it: the point and the segment it was
// read from, with how long that segment had been running.
type swingRead struct {
	pos     [3]int32
	from    [3]int32
	dest    [3]int32
	elapsed int64 // milliseconds
}

// swingLinesOf reads EVERY `SWING POS` line of one log, in order, and fails the test when a
// line cannot be parsed - the line is the evidence this round is checked against, so one
// that does not carry its five numbers is a fault of its own.
func swingLinesOf(t *testing.T, logs string) []swingRead {
	t.Helper()
	var out []swingRead
	for _, line := range strings.Split(logs, "\n") {
		i := strings.Index(line, "SWING POS")
		if i < 0 {
			continue
		}
		at := line[i:]
		j := strings.Index(at, "pos (")
		if j < 0 {
			t.Fatalf("a SWING POS line carries no position: %s", line)
		}
		var x, y, c, fx, fy, dx, dy int32
		var ms int64
		n, err := fmt.Sscanf(at[j+len("pos "):],
			"(%d, %d, cell %d) from segment (%d, %d) -> (%d, %d) elapsed %dms",
			&x, &y, &c, &fx, &fy, &dx, &dy, &ms)
		if err != nil || n != 8 {
			t.Fatalf("a SWING POS line does not carry pos, segment and elapsed (%d fields, "+
				"%v): %s", n, err, line)
		}
		out = append(out, swingRead{
			pos: [3]int32{x, y, c}, from: [3]int32{fx, fy, c}, dest: [3]int32{dx, dy, c},
			elapsed: ms,
		})
	}
	if len(out) == 0 {
		t.Fatalf("no SWING POS line at all:\n%s", logs)
	}
	return out
}

// onSegment is how far `p` lies from the segment from -> dest: the perpendicular distance,
// and where along it the point falls (0 at from, 1 at dest).
//
// ONE int32 UNIT OF ROUNDING PER AXIS is what walkedToAt's truncation can cost, so a point
// ON the segment reads a cross of under two units and an along inside 0..1. A point the body
// never walked to reads far more: the live line above is ~60 units off its own segment.
func onSegment(p, from, dest [3]int32) (cross, along float64) {
	dx := float64(dest[0] - from[0])
	dy := float64(dest[1] - from[1])
	seg := math.Hypot(dx, dy)
	if seg == 0 {
		return math.Hypot(float64(p[0]-from[0]), float64(p[1]-from[1])), 0
	}
	px := float64(p[0] - from[0])
	py := float64(p[1] - from[1])
	cross = math.Abs(px*dy-py*dx) / seg
	along = (px*dx + py*dy) / (seg * seg)
	return cross, along
}

// hitPosition is the position the one attack packet in `out` carries at +8: the point the
// client will hard-set its own actor to. It fails the test when the hit is not there.
func hitPosition(t *testing.T, out [][]byte) [3]int32 {
	t.Helper()
	for _, pkt := range out {
		if len(pkt) >= AttackSingleSize && binary.LittleEndian.Uint16(pkt) == CmdZCAttackSingle {
			return worldPos(pkt[asAtkPos:])
		}
	}
	t.Fatalf("no %d in the answer (%v)", CmdZCAttackSingle, kinds(out))
	return [3]int32{}
}

// theLineNamesTheWalksOwnSegment fails when a `SWING POS` line names a segment that is not
// the one the body is walking.
//
// IT IS THE HALF THAT CATCHES A PLACEMENT. A fire that writes the body into the model
// rewrites the session's segment to the point it wrote, so its own line is self-consistent
// by construction - the defect shows only against the WALK: the segment the body is really
// being drawn along. The live line at 19:13:21.181 fails it by ~600 units.
func theLineNamesTheWalksOwnSegment(t *testing.T, logs string, w *walk) swingRead {
	t.Helper()
	swings := swingLinesOf(t, logs)
	last := swings[len(swings)-1]
	if last.from[0] != w.from[0] || last.from[1] != w.from[1] ||
		last.dest[0] != w.goal[0] || last.dest[1] != w.goal[1] {
		t.Fatalf("the SWING POS line names segment (%d, %d) -> (%d, %d), but the body is "+
			"walking (%d, %d) -> (%d, %d) - the fire did not read the walk the client is "+
			"drawing:\n%s", last.from[0], last.from[1], last.dest[0], last.dest[1],
			w.from[0], w.from[1], w.goal[0], w.goal[1], logs)
	}
	return last
}

// midWalkZone is the fixture the four tests below walk into: the character pressed from far,
// so a walk to the ring is in flight, and the monster has stepped `closer` units toward it,
// so the body is INSIDE the stance's reach before the walk is over - which a plain approach
// cannot be, because the ring it aims at is exactly on MaxR.
func midWalkZone(t *testing.T, closer int32) (*Handler, *stringBuf, *player, *walk) {
	t.Helper()
	h, logs, p := fireArrivalZone(t)
	target := [3]int32{scarecrow().X, scarecrow().Y, scarecrow().Cell}
	if out := pressAttack(t, h, p.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs.String())
	}
	w := p.sess.Walk
	if w == nil || w.atk == nil {
		t.Fatalf("the press left no held attack walk:\n%s", logs.String())
	}
	if gap, in := attackRangeAllowed(w.from, target, 50, 900, PCBodyRadius,
		ringTargetBody); in {
		t.Fatalf("the body is %g of clear ground from the target BEFORE it walks, which is "+
			"already inside MinR 50 .. MaxR 900 - this test cannot be about walking in", gap)
	}
	moveRingTarget(t, h, closer)
	return h, logs, p, w
}

// midWalk back-dates the walk's clock so the body has walked HALF its route: still short of
// the stand point, which is where a jump would show.
func midWalk(t *testing.T, w *walk) time.Time {
	t.Helper()
	route := math.Hypot(float64(w.goal[0]-w.from[0]), float64(w.goal[1]-w.from[1]))
	if route <= 0 || w.speed <= 0 {
		t.Fatalf("the walk has no route (%g) or no speed (%g)", route, w.speed)
	}
	w.began = time.Now().Add(-time.Duration(route / 2 / w.speed * float64(time.Second)))
	return time.Now()
}

// TestAMidWalkSwingNamesAPointOnItsOwnSegment is (1): a swing fired while the body is
// MID-WALK reports a position that lies on the segment the same line names (cross-track
// under two units, along between the ends), and that exact point is the one the 3564's +8
// carries - the check and the packet are one measurement.
func TestAMidWalkSwingNamesAPointOnItsOwnSegment(t *testing.T) {
	h, logs, p, w := midWalkZone(t, -150)
	now := midWalk(t, w)
	body := walkedToAt(w.from, w.goal, w.began, now, w.speed)
	target := [3]int32{scarecrow().X, scarecrow().Y - 150, scarecrow().Cell}

	// THE FIXTURE'S OWN ARITHMETIC: the body is IN reach and still SHORT of the stand point,
	// or this test would not be about a swing in the middle of a walk.
	gap, in := attackRangeAllowed(body, target, 50, 900, PCBodyRadius, ringTargetBody)
	if !in {
		t.Fatalf("the mid-walk body at (%d, %d) is %g of clear ground from the target, which "+
			"is NOT inside MinR 50 .. MaxR 900 - the fire would not happen here", body[0],
			body[1], gap)
	}
	toGoal := math.Hypot(float64(w.goal[0]-body[0]), float64(w.goal[1]-body[1]))
	if toGoal < 50 {
		t.Fatalf("the body is %g from its stand point - it is not mid-walk", toGoal)
	}

	out := h.chaseTick(p.sess)
	if n := hitAttacks(out); n != 1 {
		t.Fatalf("the mid-walk body at %g of clear ground produced %v (%d hit packet(s)), "+
			"want one:\n%s", gap, kinds(out), n, logs.String())
	}
	swings := swingLinesOf(t, logs.String())
	if len(swings) != 1 {
		t.Fatalf("%d SWING POS line(s) for one mid-walk swing:\n%s", len(swings),
			logs.String())
	}
	last := theLineNamesTheWalksOwnSegment(t, logs.String(), w)
	if got := hitPosition(t, out); got != last.pos {
		t.Fatalf("the 3564's +8 is %v and the SWING POS line says %v - the check and the "+
			"packet are not one measurement:\n%s", got, last.pos, logs.String())
	}
	cross, along := onSegment(last.pos, last.from, last.dest)
	if cross > 2 {
		t.Fatalf("the swing's position %v is %g units OFF the segment (%v -> %v) it names - "+
			"the client hard-sets its actor to that point, so this is the jump:\n%s",
			last.pos, cross, last.from, last.dest, logs.String())
	}
	if along < 0 || along > 1 {
		t.Fatalf("the swing's position %v is %g of the way along (%v -> %v), i.e. past its "+
			"own ends:\n%s", last.pos, along, last.from, last.dest, logs.String())
	}
	if along >= 1 {
		t.Fatalf("the point is the stand point itself (along %g) - the body had not "+
			"arrived, so this test would not be about a mid-walk swing", along)
	}
	t.Logf("mid-walk swing: pos %v on segment %v -> %v (cross %g, along %g), +8 the same",
		last.pos, last.from, last.dest, cross, along)
}

// TestTwoSwingsInOneWalkAreAtMostSpeedTimesElapsedApart is (2): two swings fired during ONE
// walk cannot be more than speed x elapsed apart, because both points are on that one
// segment. The owner's log has consecutive swings 217 to 506 units apart; a body walking at
// ~469 u/s covers 56 units in 120 ms.
func TestTwoSwingsInOneWalkAreAtMostSpeedTimesElapsedApart(t *testing.T) {
	h, logs, p, w := midWalkZone(t, -150)
	stand := w.goal
	midWalk(t, w)

	if n := hitAttacks(h.chaseTick(p.sess)); n != 1 {
		t.Fatalf("the first mid-walk swing produced %d hit packet(s), want 1:\n%s", n,
			logs.String())
	}
	// THE SECOND SWING, 120 ms later. The character's own attack delay is 1.683 s, so it is
	// cleared by hand: this test measures the POSITION, not the cadence.
	time.Sleep(120 * time.Millisecond)
	p.sess.noteAttack(0, time.Time{})
	if n := hitAttacks(pressAttack(t, h, p.sess, capturedTarget)); n != 1 {
		t.Fatalf("the second swing produced %d hit packet(s), want 1:\n%s", n,
			logs.String())
	}

	swings := swingLinesOf(t, logs.String())
	if len(swings) != 2 {
		t.Fatalf("%d SWING POS line(s) for two swings:\n%s", len(swings), logs.String())
	}
	one, two := swings[0], swings[1]

	// THE INVARIANT, RESTATED FOR R132. It used to be "two swings inside one walk are at
	// most speed x elapsed apart", because the body kept walking while it swung. The first
	// hit now STOPS the walk (the owner's observation: "the character will reposition
	// themselves when hit started landing?" - the reference stops the body when the attack
	// starts, SetMoveState(MS_STOP), FieldChar_FSM.cpp:28-80), so the answer is stronger:
	// the second swing is built from the SAME point, and names a leg of no length there.
	if two.from != two.dest {
		t.Fatalf("the second swing names the leg %v -> %v - the body was not planted by "+
			"the first hit:\n%s", two.from, two.dest, logs.String())
	}
	if jump := flatDist(one.pos, two.pos); jump > 2 {
		t.Fatalf("the two swings are %g units apart (%v then %v) - the walk was closed at "+
			"the first hit, so the second one must come from the same point:\n%s", jump,
			one.pos, two.pos, logs.String())
	}
	if one.pos == stand {
		t.Fatalf("the first swing was built from the stand point %v itself - the body had "+
			"not arrived, so this is not a mid-walk fire:\n%s", stand, logs.String())
	}
	if got := p.sess.approachOf(0); got != nil {
		t.Fatalf("the walk to %v is still installed after the first hit:\n%s", got.goal,
			logs.String())
	}
	t.Logf("two swings from one planted point: %v -> %v (walk to %v dropped)",
		one.pos, two.pos, stand)
}

// TestABodyShortOfTheStandPointDoesNotFireEarly is (3): the press stays HELD until the body
// is drawn inside the reach, and the update that finds it there is the one that fires it.
// Nothing is measured at the stand point before the body can be there.
func TestABodyShortOfTheStandPointDoesNotFireEarly(t *testing.T) {
	h, logs, p, w := midWalkZone(t, -150)
	target := [3]int32{scarecrow().X, scarecrow().Y - 150, scarecrow().Cell}

	// AT THE START OF THE WALK: 940 of clear ground, outside MaxR 900. Nothing fires, and the
	// press is not consumed - a body that is not there yet must not cost the player a press.
	w.began = time.Now()
	start := w.bodyAt(time.Now())
	if gap, in := attackRangeAllowed(start, target, 50, 900, PCBodyRadius,
		ringTargetBody); in {
		t.Fatalf("the body at the start of its walk is %g of clear ground, which is INSIDE "+
			"the reach - this test cannot be about a body that is not there yet", gap)
	}
	if n := hitAttacks(h.chaseTick(p.sess)); n != 0 {
		t.Fatalf("a swing went out for a body still at the start of its walk:\n%s",
			logs.String())
	}
	if w.castDone {
		t.Fatalf("the press was CONSUMED before the body was in reach:\n%s", logs.String())
	}

	// THE UPDATE THAT BRINGS IT IN REACH: the body is now mid-walk, inside MinR..MaxR, and
	// still short of its stand point. That update fires it, once.
	now := midWalk(t, w)
	body := walkedToAt(w.from, w.goal, w.began, now, w.speed)
	gap, in := attackRangeAllowed(body, target, 50, 900, PCBodyRadius, ringTargetBody)
	if !in {
		t.Fatalf("the mid-walk body is %g of clear ground - still not in reach", gap)
	}
	out := h.chaseTick(p.sess)
	if n := hitAttacks(out); n != 1 {
		t.Fatalf("the update that put the body %g of clear ground inside MinR 50 .. MaxR 900 "+
			"produced %v (%d hit packet(s)), want one:\n%s", gap, kinds(out), n,
			logs.String())
	}
	swings := swingLinesOf(t, logs.String())
	if len(swings) != 1 {
		t.Fatalf("%d SWING POS line(s) for one press:\n%s", len(swings), logs.String())
	}
	theLineNamesTheWalksOwnSegment(t, logs.String(), w)
	if cross, along := onSegment(swings[0].pos, swings[0].from, swings[0].dest); cross > 2 ||
		along >= 1 {
		t.Fatalf("the swing fired at %v, %g units off its own segment (%v -> %v) and %g of "+
			"the way along it - it is the stand point, not the body:\n%s", swings[0].pos,
			cross, swings[0].from, swings[0].dest, along, logs.String())
	}
	t.Logf("held at the walk's start, fired on the update at %g of clear ground, pos %v",
		gap, swings[0].pos)
}

// TestNoFirePathMovesTheModel is (4): firing writes NOTHING about where the body is. The
// model's own position (Session.Pos, moveFrom, moveAt and every member's Position) is
// unchanged across a fire, on both fire paths - the continuous check and the walk's clock.
//
// THE GREP IS IN THE HISTORY, the assertion is here: the two sites that placed the body were
// `fireHeldAttackOnArrival` (deleted) and the attack branch of the walk's clock (deleted);
// the skill cast's placement is its own lane and is untouched.
func TestNoFirePathMovesTheModel(t *testing.T) {
	type model struct {
		pos      [3]int32
		moveFrom [3]int32
		moveAt   time.Time
		team     [][3]int32
	}
	snapshot := func(s *Session) model {
		m := model{pos: s.Pos, moveFrom: s.moveFrom, moveAt: s.moveAt}
		for _, t := range s.Team {
			m.team = append(m.team, t.Position)
		}
		return m
	}

	// THE CONTINUOUS CHECK, FIRING MID-WALK.
	//
	// SINCE R132 THIS FIRE CLOSES THE WALK, AND THAT IS A DELIBERATE CHANGE OF THIS RULE.
	// The owner's observation - "the character will reposition themselves when hit started
	// landing?" - is answered by stopping the body where the hit was built from, which is
	// what the reference does (`IsAttackArea` yes -> SetMoveState(MS_STOP) ->
	// SetActionState(AS_ATTACK), FieldChar_FSM.cpp:28-80). What the fire must STILL never do
	// is PLACE the body somewhere it is not: the model may advance to the point the swing
	// carried - the walk's own drawn point - and never to the stand point.
	h, logs, p, w := midWalkZone(t, -150)
	now := midWalk(t, w)
	stand := w.goal
	drawn := w.bodyAt(now)
	if n := hitAttacks(h.chaseTick(p.sess)); n != 1 {
		t.Fatalf("the continuous check produced %d hit packet(s), want 1:\n%s", n,
			logs.String())
	}
	afterFire := snapshot(p.sess)
	if d := flatDist(afterFire.team[0], drawn); d > 5 {
		t.Fatalf("the fire left the body at %v, %g units from the drawn point %v - a fire "+
			"places the body only where it already is:\n%s", afterFire.team[0], d, drawn,
			logs.String())
	}
	if afterFire.team[0] == stand || afterFire.pos == stand {
		t.Fatalf("the fire PLACED the body on the stand point %v - that is the R104 bug "+
			"back again:\n%s", stand, logs.String())
	}
	if got := p.sess.approachOf(0); got != nil {
		t.Fatalf("the walk survived the first hit (goal %v): she would keep walking while "+
			"swinging, which is the R132 defect:\n%s", got.goal, logs.String())
	}
	before := afterFire

	// THE WALK'S ARRIVAL (piece 2d: the fire and the close are one tick now).
	//
	// The FIRE still writes nothing. The CLOSE does hand the leg over: keepWalkedLeg puts
	// the body's stored point on the goal the body has just walked to (R111), which is a
	// fact about where it IS. What must not happen - and would be the bug this test was
	// written for - is a PLACEMENT: moveFrom and moveAt are the placement's own two
	// fields, and a close has no business touching them.
	h2, logs2, p2 := fireArrivalZone(t)
	if out := pressAttack(t, h2, p2.sess, capturedTarget); moves(out) != 1 {
		t.Fatalf("the press produced %d move(s), want 1:\n%s", moves(out), logs2.String())
	}
	goal := p2.sess.Walk.goal
	walkToFire(t, p2.sess)
	before2 := snapshot(p2.sess)
	if n := hitAttacks(h2.chaseTick(p2.sess)); n != 1 {
		t.Fatalf("the walk's arrival produced %d hit packet(s), want 1:\n%s", n,
			logs2.String())
	}
	after2 := snapshot(p2.sess)
	if after2.moveFrom != before2.moveFrom || !after2.moveAt.Equal(before2.moveAt) {
		t.Fatalf("the walk's end PLACED the body: moveFrom %v -> %v, moveAt %v -> %v - the "+
			"body walked there, so nothing is owed a placement:\n%s", before2.moveFrom,
			after2.moveFrom, before2.moveAt, after2.moveAt, logs2.String())
	}
	if after2.pos != goal {
		t.Fatalf("the stored point after the arrival is %v, want the goal %v the body "+
			"walked to (the R111 handover):\n%s", after2.pos, goal, logs2.String())
	}
	t.Logf("both fire paths left the model untouched (pos %v, moveFrom %v)", before.pos,
		before.moveFrom)
}
