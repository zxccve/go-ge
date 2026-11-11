package zone

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"geserver/protocol"
)

// THE REPEAT - one click, then a swing every ASPD while the target stays locked (R94).
//
// THE CADENCE IS NEVER A CONSTANT IN THESE TESTS. Every expectation is read from the
// same sheet the handler reads (`h.sheetFor(team[0]).ASPD`), so the day the
// stance table or the item numbers move, these tests move with them and only a real
// regression stays red.
//
// NOTHING SLEEPS. The test drives the repeat's own clock the way the tick reads it - it
// puts `next` in the past and calls swingTick - so a ~1 s ASPD costs no wall time and
// the result cannot depend on how fast this machine is. That is the same technique
// TestAttackLandsAgainAfterTheInterval already uses on Session.attackAt.

// swingDue moves the clock forward by exactly one attack delay, which is what a real tick
// meets when the character's ASPD has elapsed - it does NOT merely put `next` in the past.
// Both halves have to move together: the repeat arms itself at `last hit + ASPD`, and
// Handler.attackAt holds any request that arrives sooner than that. Moving only `next`
// would ask the handler for a swing the character cannot make yet, which is a test of the
// cadence gate rather than of the repeat.
//
// It fails when there is no repeat, so a test written against one cannot pass by accident.
func swingDue(t *testing.T, h *Handler, sess *Session, idx int) *swingLoop {
	t.Helper()
	s := sess.swingOf(idx)
	if s == nil {
		t.Fatalf("char %d has no swing repeat to advance - the press did not start one", idx)
	}
	if idx >= len(sess.Team) {
		t.Fatalf("char %d is not in this team", idx)
	}
	interval := attackInterval(h.sheetFor(sess.Team[idx]))
	if interval <= 0 {
		t.Fatalf("char %d's sheet gives no attack delay, so there is no clock to advance", idx)
	}
	sess.attackAt[idx] = sess.lastAttack(idx).Add(-interval)
	s.next = sess.lastAttack(idx).Add(interval)
	return s
}

// swingHits counts the 3564s in a reply, and fails when the count is not matched by the
// same number of 3557s: a hit with no HP number (or the other way round) is the one
// thing this lane must never send, so every count below goes through here.
func swingHits(t *testing.T, out [][]byte) int {
	t.Helper()
	hits, hps := 0, 0
	for _, p := range out {
		switch binary.LittleEndian.Uint16(p) {
		case CmdZCAttackSingle:
			hits++
			// THE TWO MEASURED FORMS AND NOTHING ELSE (the killing-blow card): 74 bytes
			// for a hit that does not kill, 111 for one that does - the same 74 plus the
			// 37-byte KNOCKDOWN_INFO the REFERENCE appends to a knockdown hit
			// (history\protocol-recovery.md:1016). The client reads the TYPE, not that
			// block (FUN_004e220b stops at the type word), but our bytes keep the
			// reference's shape. A fixture whose monster has 1 HP therefore sees a 111
			// here; a fragment or a third shape is still a failure.
			if len(p) != AttackSingleSize && len(p) != AttackSingleKnockdownSize {
				t.Fatalf("a swing's 3564 is %d bytes, want %d (a plain hit) or %d (a "+
					"killing blow)", len(p), AttackSingleSize, AttackSingleKnockdownSize)
			}
		case CmdZCAddHP:
			hps++
		}
	}
	if hits != hps {
		t.Fatalf("%d hit(s) and %d HP update(s) in one reply - they go out in pairs", hits, hps)
	}
	return hits
}

// moveReply sends one real CZ_MOVE_PC through the dispatch, the way the client does, and
// answers what the click earned - so a test can count the echo of the player's own move
// and cannot pass with no case in the switch.
func moveReply(t *testing.T, h *Handler, sess *Session, pos [3]int32) [][]byte {
	t.Helper()
	out, err := h.Reply(context.Background(), sess, moveFrame(0, pos))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return out
}

// ourOwnWalk makes the session's memory of the wire TRUE: it records `dest` as a walk
// this zone just sent for character idx, through walkOnceAt - the ONE function that
// records every server-driven walk in this package (zone\walk_once.go, called by
// legMoveFor). It sends no packet, moves no clock and changes no position. It exists so a
// report the client sends back for a walk WE asked for can be told from the player's
// click, which since 2026-11-01 is the only thing standing between a move report and the
// end of an engagement.
func ourOwnWalk(t *testing.T, sess *Session, idx int, dest [3]int32) {
	t.Helper()
	if !sess.walkOnceAt(idx, dest, time.Now()) {
		t.Fatalf("walkOnceAt refused a first walk to %v - the test's own setup is wrong", dest)
	}
}

// cmdCount counts the packets of one command id in a reply.
func cmdCount(out [][]byte, cmd uint16) int {
	n := 0
	for _, p := range out {
		if len(p) >= 2 && cmdOf(p) == cmd {
			n++
		}
	}
	return n
}

// distanceToCrow is how far the character's DRAWN body is from the Scarecrow right now -
// the same measurement the reach check makes (attackBodyPoint, drawnPos), so a test can
// say out loud that the body it believes is out of reach really is.
func distanceToCrow(sess *Session, h *Handler) float64 {
	body := sess.attackBodyPoint(0, time.Now(), h.packetSpeed(&sess.Team[0]))
	mon := scarecrow()
	dx := float64(body.pos[0] - mon.X)
	dy := float64(body.pos[1] - mon.Y)
	return math.Sqrt(dx*dx + dy*dy)
}

// TestOnePressKeepsSwingingAtTheStanceCadence is the owner's report turned into a test:
// ONE click produces MORE THAN ONE hit, each one a full 3564 + 3557, each one at the
// character's own ASPD - and the monster's HP moves down by the damage every time.
func TestOnePressKeepsSwingingAtTheStanceCadence(t *testing.T) {
	h, logs, p := attackZone(t)
	interval := time.Duration(attackASPD(t, h, p)) * time.Millisecond

	out := pressAttack(t, h, p.sess, capturedTarget)
	if got := swingHits(t, out); got != 1 {
		t.Fatalf("the press produced %d hit(s), want 1:\n%s", got, logs.String())
	}
	s := p.sess.swingOf(0)
	if s == nil {
		t.Fatalf("the press did not arm the repeat:\n%s", logs.String())
	}
	if s.target != capturedTarget {
		t.Errorf("the repeat follows target %d, want the clicked %d", s.target, capturedTarget)
	}
	// THE CADENCE IS THE CHARACTER'S OWN SHEET, added to the hit that armed it.
	if want := p.sess.lastAttack(0).Add(interval); !s.next.Equal(want) {
		t.Errorf("the next swing is owed at %v, want the last hit %v + ASPD %v = %v",
			s.next, p.sess.lastAttack(0), interval, want)
	}

	// THREE MORE SWINGS, each one a tick after its clock has come.
	const more = 3
	hp := h.monsterHPFor(capturedTarget)
	for i := 1; i <= more; i++ {
		swingDue(t, h, p.sess, 0)
		tick := h.swingTick(p.sess)
		if got := swingHits(t, tick); got != 1 {
			t.Fatalf("swing %d produced %d hit(s), want 1:\n%s", i+1, got, logs.String())
		}
		now := h.monsterHPFor(capturedTarget)
		if !(now < hp) {
			t.Fatalf("swing %d left the monster's HP at %v (was %v) - the ledger did "+
				"not move", i+1, now, hp)
		}
		hp = now
		if got := p.sess.swingOf(0); got == nil || got.hits != i+1 {
			t.Fatalf("after %d swing(s) the repeat records %v hits, want %d", i+1,
				got, i+1)
		}
		// AND THE CLOCK KEEPS THE CADENCE rather than counting from the tick.
		if want := p.sess.lastAttack(0).Add(interval); !p.sess.swingOf(0).next.Equal(want) {
			t.Errorf("after swing %d the next is owed at %v, want %v",
				i+1, p.sess.swingOf(0).next, want)
		}
	}

	// A tick BEFORE the clock has come sends nothing: the rhythm is the ASPD, not the
	// poll interval.
	if early := h.swingTick(p.sess); len(early) != 0 {
		t.Fatalf("a tick before the cadence produced %v, want nothing:\n%s",
			kinds(early), logs.String())
	}

	text := logs.String()
	if n := strings.Count(text, "ATTACK REPEAT: commander"); n < more {
		t.Errorf("%d repeat line(s), want at least %d (one per swing):\n%s", n, more, text)
	}
	t.Logf("one press -> %d hits at %v (the character's own ASPD); HP 5000 -> %v",
		more+1, interval, h.monsterHPFor(capturedTarget))
	t.Logf("line: %s", lineWith(text, "ATTACK REPEAT: commander"))
}

// TestASecondRequestDoesNotDuplicateTheRepeat is the anti-double-hit half. The client
// CAN send 3666 again for the target it already holds (its tactics state re-enters; the
// reference re-sends it too, PlayerChar_Request.cpp:1416-1421). That request may earn
// its own hit - it is a swing the player asked for and the cadence gate allows - but it
// must not create a SECOND rhythm, and the two together must never put two 3564s on the
// wire for one moment.
func TestASecondRequestDoesNotDuplicateTheRepeat(t *testing.T) {
	h, logs, p := attackZone(t)
	interval := time.Duration(attackASPD(t, h, p)) * time.Millisecond

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the first press did not land one hit:\n%s", logs.String())
	}
	first := p.sess.swingOf(0)
	if first == nil {
		t.Fatalf("no repeat was armed:\n%s", logs.String())
	}

	// One swing of the repeat, so the request below comes after a hit that the repeat
	// itself made rather than the click.
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the repeat's swing produced %d hit(s), want 1:\n%s", got, logs.String())
	}

	// THE SECOND REQUEST, for the SAME target, past the cadence: one hit, and still ONE
	// record in the one slot. The clock has to move first, because the cadence gate holds
	// any request that arrives inside the character's own attack delay - that rule is
	// not this test's question (TestSecondAttackInsideTheIntervalIsHeld owns it).
	swingDue(t, h, p.sess, 0)
	before := h.monsterHPFor(capturedTarget)
	out := pressAttack(t, h, p.sess, capturedTarget)
	if got := swingHits(t, out); got != 1 {
		t.Fatalf("a re-press past the cadence produced %d hit(s), want exactly 1 - the "+
			"slot must be REPLACED, not duplicated:\n%s", got, logs.String())
	}
	after := h.monsterHPFor(capturedTarget)
	if !(after < before) {
		t.Fatalf("the re-press did not move the ledger (%v -> %v)", before, after)
	}
	if n := len(p.sess.swings); n != 1 {
		t.Fatalf("%d repeat slot(s) after two requests, want 1", n)
	}
	if s := p.sess.swingOf(0); s == nil {
		t.Fatalf("the repeat was dropped by the re-press:\n%s", logs.String())
	} else if s.target != capturedTarget {
		t.Fatalf("the repeat follows %d, want %d", s.target, capturedTarget)
	}
	// AND THE CLOCK IS STILL THE CADENCE FROM THE LAST HIT, not a second rhythm.
	if want := p.sess.lastAttack(0).Add(interval); !p.sess.swingOf(0).next.Equal(want) {
		t.Errorf("after the re-press the next is owed at %v, want %v",
			p.sess.swingOf(0).next, want)
	}

	// A tick immediately afterwards sends NOTHING: the re-press already paid for this
	// interval, so there is no second hit hiding behind it.
	if early := h.swingTick(p.sess); len(early) != 0 {
		t.Fatalf("a tick right after the re-press produced %v, want nothing - that is "+
			"the double hit:\n%s", kinds(early), logs.String())
	}
	if got := len(p.sess.swings); got != 1 || p.sess.swingOf(0) == nil {
		t.Fatalf("%d slot(s) left after the re-press tick", got)
	}
	t.Logf("two requests, one repeat: %s", lineWith(logs.String(), "ATTACK REPEAT: commander"))
}

// TestTheRepeatFollowsANewTarget is the target-change half: one slot per character means
// the repeat moves to the monster the player clicked, and it says so. It must not leave
// a second rhythm hitting the monster it was told to forget.
func TestTheRepeatFollowsANewTarget(t *testing.T) {
	h, logs, p := attackZone(t)
	const second = 0x20000002
	h.spawnMu.Lock()
	h.spawnedMonster = append(h.spawnedMonster,
		Monster{Handle: second, X: -839, Y: 2211, Cell: 136, ClassID: 11600, Level: 130})
	h.monsterHP[second] = 9000
	h.spawnMu.Unlock()

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the first press did not land:\n%s", logs.String())
	}
	p.sess.noteAttack(0, time.Now().Add(-time.Hour)) // the cadence is not this test's question
	if out := pressAttack(t, h, p.sess, second); swingHits(t, out) != 1 {
		t.Fatalf("the press on the second monster did not land:\n%s", logs.String())
	}
	s := p.sess.swingOf(0)
	if s == nil || s.target != second {
		t.Fatalf("the repeat follows %v, want the newly clicked %d", s, second)
	}
	if len(p.sess.swings) != 1 {
		t.Fatalf("%d repeat slots, want 1 (one per character)", len(p.sess.swings))
	}

	firstHP := h.monsterHPFor(capturedTarget)
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the swing after the target change produced %d hit(s), want 1:\n%s",
			got, logs.String())
	}
	if hp := h.monsterHPFor(capturedTarget); hp != firstHP {
		t.Errorf("the repeat hit the OLD target: its HP moved %v -> %v", firstHP, hp)
	}
	if got := h.monsterHPFor(second); got >= 9000 {
		t.Errorf("the NEW target's HP is still %v - the repeat did not follow the click", got)
	}
	t.Logf("followed the click: %s", lineWith(logs.String(), "swings at target"))
}

// TestTheRepeatEndsWhenTheTargetIsGone: a handle this zone no longer runs ends the repeat
// with nothing sent, exactly as `GetAttackTarget()` returning NULL ends AS_ATTACK
// (FieldChar_FSM.cpp:141-146).
func TestTheRepeatEndsWhenTheTargetIsGone(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	h.spawnMu.Lock()
	h.spawnedMonster = nil
	h.spawnMu.Unlock()

	swingDue(t, h, p.sess, 0)
	out := h.swingTick(p.sess)
	if len(out) != 0 {
		t.Fatalf("a swing at a target this zone no longer runs produced %v, want nothing:\n%s",
			kinds(out), logs.String())
	}
	if p.sess.swingOf(0) != nil {
		t.Fatal("the repeat survived its target")
	}
	if p.sess.wantsTick() {
		t.Error("the session still wants a tick after its last repeat ended - the read " +
			"loop would be woken ten times a second for nothing")
	}
	line := lineWith(logs.String(), "ATTACK REPEAT ENDED")
	if line == "" || !strings.Contains(line, "not a monster this zone is running") {
		t.Fatalf("the ending did not name the reason:\n%s", logs.String())
	}
	t.Logf("ended: %s", line)
}

// TestTheRepeatEndsWhenTheTargetIsDead: HP 0 in this zone's own ledger is the only death
// it can see (it sends no 3531 - another lane), and a dead body is not attacked
// (FieldChar_FSM.cpp:136-140).
func TestTheRepeatEndsWhenTheTargetIsDead(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	h.spawnMu.Lock()
	h.monsterHP[capturedTarget] = 0
	h.spawnMu.Unlock()

	swingDue(t, h, p.sess, 0)
	if out := h.swingTick(p.sess); len(out) != 0 {
		t.Fatalf("a swing at a body with no HP left produced %v, want nothing:\n%s",
			kinds(out), logs.String())
	}
	if p.sess.swingOf(0) != nil {
		t.Fatal("the repeat kept hitting a body whose HP had reached 0")
	}
	if line := lineWith(logs.String(), "ATTACK REPEAT ENDED"); !strings.Contains(line,
		"no HP left") {
		t.Fatalf("the ending did not name the death:\n%s", logs.String())
	}
}

// A MOVE DOES NOT CANCEL A SWING THAT IS ALREADY UNDER WAY (R105).
//
// THE OWNER'S RULE for the original: "the attack will land through and then the move
// will follow". The reference keeps the attack as an ACTION with a duration - nothing
// in FieldChar_FSM.cpp or PlayerChar.cpp aborts the attack action because a move
// arrived; its FSM re-decides once the action is over. So a character that is told to
// walk keeps the swing it has already started and lands it at its own cadence.
//
// WHAT THIS PINS: the move is still answered with its own 3598 echo (one, and it
// carries no hit of its own), and the swing that was already due when the move arrived
// still builds its ONE 3564 + 3557. Both go out, exactly one of each, and nothing is
// cancelled or doubled.
//
// WHY IT CANNOT PASS BY ACCIDENT: before R105 the move called cancelSwingOnMove, so the
// repeat was gone before the tick below and the tick built nothing at all - the hit
// count is the assertion that separates the two behaviours.
//
// THE REPORT IN THIS TEST IS A WALK THIS ZONE SENT, and since 2026-11-01 that is the only
// report that may leave an engagement alone: any other move from the driven character is
// the player's own click and ends it, however short (R108, Session.walkWasOurs). The two
// halves are pinned separately - the report that is ours in
// TestAWalkWeSentKeepsTheEngagementSwinging and TestOnlyAWalkWeSentIsNotAMoveOrder, and the
// "the swing in flight still lands" half of a click in
// TestAPlayerMoveOrderEndsTheEngagementAfterTheSwingInFlight.
func TestAMoveMidSwingStillLandsAndTheMoveIsSent(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	// THE SWING IS DUE, not merely armed: this is the swing the move arrives during.
	swingDue(t, h, p.sess, 0)

	// THE MOVE: 60 units, a walk THIS ZONE SENT (ourOwnWalk records its destination the
	// way legMoveFor does), so the report coming back is our own leg and not the player's
	// click. Answered at once with the echo of the client's own click. Nothing is sent FOR
	// the attack here - the attack is the tick's below, and a hit inside the move's own
	// reply would be a second one.
	walkTo := [3]int32{p.sess.Team[0].Position[0], p.sess.Team[0].Position[1] - smallTestLeg, 136}
	ourOwnWalk(t, p.sess, 0, walkTo)
	moved := moveReply(t, h, p.sess, walkTo)
	if got := cmdCount(moved, CmdZCMovePC); got != 1 {
		t.Fatalf("the move answered %d ZC_MOVE_PC(s), want 1:\n%s", got, logs.String())
	}
	if got := cmdCount(moved, CmdZCAttackSingle); got != 0 {
		t.Fatalf("the MOVE reply carried %d hit(s) of its own:\n%s", got, logs.String())
	}

	// THE SWING SURVIVED THE MOVE, AND IT IS THE SWING THAT WAS ALREADY UNDER WAY.
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the move cancelled the repeat that was already swinging - the owner's "+
			"rule is that the attack lands through and then the move follows:\n%s",
			logs.String())
	}
	before := h.monsterHPFor(capturedTarget)
	tick := h.swingTick(p.sess)
	if got := swingHits(t, tick); got != 1 {
		t.Fatalf("the tick after the move produced %d hit(s), want exactly 1 for the "+
			"cycle that was already due:\n%s", got, logs.String())
	}
	if got := len(tick); got != 2 {
		// the one 3564 and its one 3557, and nothing else
		t.Fatalf("that cycle put %d packet(s) on the wire %v, want 2 (one 3564 and one "+
			"3557):\n%s", got, kinds(tick), logs.String())
	}
	if after := h.monsterHPFor(capturedTarget); !(after < before) {
		t.Fatalf("the swing did not land: the monster's HP stayed at %v", after)
	}
	if text := logs.String(); strings.Contains(text, "ATTACK REPEAT ENDED") {
		t.Fatalf("the repeat ended over a move - a move is not one of its reasons:\n%s",
			text)
	}
	t.Logf("one move (cmd %d) and the swing under way (cmd %d + %d): the attack landed "+
		"and the move followed", CmdZCMovePC, CmdZCAttackSingle, CmdZCAddHP)
}

// TestAMoveOutOfReachLandsTheSwingUnderWayAndWalksBackIn is the reach half of the same
// rule: a move that carries the body OUT of the stance's reach still lets the swing
// already under way land, and the NEXT swing finds the body out of position - and the
// repeat WALKS IT BACK IN instead of dying (R106).
//
// THIS TEST USED TO PIN THE OPPOSITE, and that is the owner's bug: "the NEXT swing is
// refused by the reach ... and only then does the repeat end, for having produced
// nothing". A refusal is not a reason to stop; it is a reason to move.
//
// THE CLOCK IS MOVED, NOTHING SLEEPS: the segment the move opened is aged by one second,
// so the body the server is DRAWING has walked to (or towards) the destination by the
// time the next swing is due. That is the same arithmetic the live zone uses (drawnPos),
// not a second model written for the test.
func TestAMoveOutOfReachLandsTheSwingUnderWayAndWalksBackIn(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	_, maxR := stanceReach(t, h, p.sess.Team[0].Stance)

	// The destination is 240 units from the Scarecrow along the line the character already
	// stands on, and it is a walk THIS ZONE SENT (ourOwnWalk) - which is the only kind of
	// report that may leave the engagement alone now that a move is a cancel however small
	// (R108, Session.walkWasOurs). What the test is about is the REACH branch: the point is
	// still more than MaxR+attackDragSlack past the target, so the next swing finds the body
	// out of the band and walks it back in.
	walkTo := [3]int32{p.sess.Team[0].Position[0], scarecrow().Y - 240, 136}
	swingDue(t, h, p.sess, 0)
	ourOwnWalk(t, p.sess, 0, walkTo)
	moved := moveReply(t, h, p.sess, walkTo)
	if got := cmdCount(moved, CmdZCMovePC); got != 1 {
		t.Fatalf("the move answered %d ZC_MOVE_PC(s), want 1:\n%s", got, logs.String())
	}

	// (1) THE SWING THAT WAS ALREADY UNDER WAY LANDS. The click has just been echoed, so
	// the body the server draws is still where it stood - inside the reach - and the
	// swing goes out at its cadence.
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the move cancelled the repeat that was already swinging:\n%s",
			logs.String())
	}
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the swing that was already under way produced %d hit(s), want 1:\n%s",
			got, logs.String())
	}

	// (2) THE BODY WALKS. One second of the segment the move opened is enough for the
	// drawn body to be well past the reach.
	p.sess.moveAt = time.Now().Add(-time.Second)
	if gap := distanceToCrow(p.sess, h); gap <= maxR {
		t.Fatalf("the test's own setup is wrong: the drawn body is %g from the target "+
			"against MaxR %g, so it never left the reach", gap, maxR)
	}

	// (3) THE SWING IS REFUSED BY THE POSITION AND THE ENGAGEMENT IS NOT (R106). The body
	// is out of the band, so this tick builds no hit - and it sends the walk that takes
	// the body back in, and the repeat is still there afterwards.
	swingDue(t, h, p.sess, 0)
	out := h.swingTick(p.sess)
	if got := swingHits(t, out); got != 0 {
		t.Fatalf("a swing at a body %g outside MaxR %g produced %d hit(s), want none:\n%s",
			distanceToCrow(p.sess, h), maxR, got, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the repeat ENDED because the body left the reach - that is the owner's "+
			"\"attack stops\": %q\n%s", lineWith(logs.String(), "ATTACK REPEAT ENDED"),
			logs.String())
	}
	if got := cmdCount(out, CmdZCMovePC); got != 1 {
		t.Fatalf("the tick sent %d walk(s) for a body outside MaxR %g, want 1 - the repeat "+
			"walks itself back in (R106):\n%s", got, maxR, logs.String())
	}
	if text := logs.String(); strings.Contains(text, "ATTACK REPEAT ENDED") {
		t.Fatalf("a move out of reach wrote an ENDED line:\n%s", text)
	}
	t.Logf("landed, then out of reach: 1 walk back in (%v) and the repeat is still alive",
		p.sess.Walk.goal)
}

// TestTheRepeatStillEndsWhenTheTargetDiesAfterAMove is the no-regression half: a walk this
// zone sent does not end the repeat, so the reasons that DO end it must still work
// afterwards. The target's HP reaching 0 in this zone's own ledger is the death it can see
// (FieldChar_FSM.cpp:136-140).
func TestTheRepeatStillEndsWhenTheTargetDiesAfterAMove(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	walkTo := [3]int32{p.sess.Team[0].Position[0], p.sess.Team[0].Position[1] - 200, 136}
	ourOwnWalk(t, p.sess, 0, walkTo)
	moveReply(t, h, p.sess, walkTo)
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the move cancelled the repeat, so this test cannot ask about the "+
			"death any more:\n%s", logs.String())
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
		t.Fatal("the repeat kept hitting a body whose HP had reached 0")
	}
	line := lineWith(logs.String(), "ATTACK REPEAT ENDED")
	if !strings.Contains(line, "no HP left") {
		t.Fatalf("the ending did not name the death: %q\n%s", line, logs.String())
	}
	t.Logf("ended after the move, for the right reason: %s", line)
}

// TestOnlyAPlayerMoveOrderEndsASwingOnAMove is the grep, held as a test so the rule
// cannot drift by accident (R105 retired the blanket "no move ends a swing", R108 put the
// line back where it belongs). It pins TWO things about the server's SOURCE:
//
//   - the old unconditional cancel (`cancelSwingOnMove`, which deleted the repeat the
//     moment any move report arrived, taking the swing already in flight with it) does not
//     exist anywhere;
//   - the "the player moved ... away (move order)" ending is written in exactly ONE file,
//     and that file also carries the "(move order)" wording. So a background correction
//     can never come to end an engagement again by a second path nobody reviewed, and a
//     reader who greps the log for that phrase lands in one place.
//
// The test files are skipped on purpose: this one has to spell the strings out to look for
// them, and the other tests quote the wording only to prove what it says.
func TestOnlyAPlayerMoveOrderEndsASwingOnAMove(t *testing.T) {
	// Split so that a future grep for the phrase does not stop at this line.
	const moved = "the player " + "moved"
	const order = "(move " + "order)"
	const cancelFn = "cancelSwing" + "OnMove"

	var cancels []string
	movedIn := map[string][]string{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "out", "logs", "bin":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, cancelFn) {
				cancels = append(cancels, fmt.Sprintf("%s:%d: %s", path, i+1,
					strings.TrimSpace(line)))
			}
			if strings.Contains(line, moved) {
				movedIn[path] = append(movedIn[path], fmt.Sprintf("%s:%d: %s", path, i+1,
					strings.TrimSpace(line)))
			}
			if strings.Contains(line, order) {
				movedIn[path] = append(movedIn[path], order)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("could not read the server's source: %v", err)
	}
	if len(cancels) > 0 {
		t.Fatalf("%d line(s) in the server's source still cancel a swing the moment a move "+
			"arrives - only a PLAYER MOVE ORDER may end an engagement, and it is marked, "+
			"not cancelled (Handler.noteMoveOrder):\n%s", len(cancels),
			strings.Join(cancels, "\n"))
	}

	// ONE file, and it has both halves of the wording.
	files := 0
	for path, lines := range movedIn {
		files++
		hasOrder := false
		for _, l := range lines {
			if l == order {
				hasOrder = true
			}
		}
		if !hasOrder {
			t.Fatalf("%s writes a move-related ending with no \"(move order)\" wording in "+
				"it - a background correction must never end an engagement:\n%s", path,
				strings.Join(lines, "\n"))
		}
	}
	if files != 1 {
		t.Fatalf("the move-order ending is written in %d file(s) %v, want exactly 1 - one "+
			"path for one rule", files, movedIn)
	}
}

// TestTheRepeatSurvivesAnAttackerStanceChange: PlayerChar_Request.cpp:1364-1368 sets
// AS_NONE only when the NEW stance's AI is not the attacker's -
//
//	IMC_DECLARE_STRINGID(ATTACKER);
//	if (AS_ATTACK == actionState && ATTACKER != m_stance->GetAI())
//		SetActionState(AS_NONE);
//
// - so an attacker->attacker change is a different rhythm of the SAME fight and the
// engagement carries on, with the new cadence read on the next pass. This test used to
// be TestTheRepeatEndsOnAStanceChange and pinned the missing qualifier: it failed the
// moment the reference's own condition was restored. The ending half is
// TestChangingToANonAttackerStanceEndsTheFightOnce (stance_change_test.go), which is
// the same rule seen from the other branch.
//
// The stance it changes TO is taken from the table for this character's job and hands,
// so no id is baked into the test.
func TestTheRepeatSurvivesAnAttackerStanceChange(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	allowed := h.Stances.StancesForHands(int(p.sess.Team[0].Job),
		h.Stances.StanceHands(p.sess.Team[0]))
	next := 0
	for _, id := range allowed {
		if uint32(id) != p.sess.Team[0].Stance {
			next = id
			break
		}
	}
	if next == 0 {
		t.Fatalf("job %d with hands %v allows only stance %d, so this test cannot ask "+
			"about a stance CHANGE - the data it is written against is missing",
			p.sess.Team[0].Job, h.Stances.StanceHands(p.sess.Team[0]),
			p.sess.Team[0].Stance)
	}

	// The frame builder is stance_change_test.go's own: same packet, same offsets, so a
	// test here cannot drift from the one the stance lane is held to.
	data := changeStanceFrame(0, uint32(next))
	if _, err := h.Reply(context.Background(), p.sess,
		protocol.Frame{Cmd: CmdCZChangeStance, Size: len(data), Data: data}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := p.sess.Team[0].Stance; got != uint32(next) {
		t.Fatalf("the stance change was refused (stance %d, wanted %d):\n%s",
			got, next, logs.String())
	}
	// THE FIXTURE'S OWN FACT, checked so the assertion below is about the rule and not
	// about which stance the table happens to offer first: job 1 with a sword reaches
	// only attacker stances (10110 BackGuard, 10360 Equites).
	if !h.Stances.IsAttackerAI(next) {
		t.Fatalf("stance %d is not the attacker AI in datatable_stance.xml, so this test "+
			"is not about the branch that keeps the repeat", next)
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the repeat did NOT survive an attacker->attacker stance change; the "+
			"reference leaves AS_ATTACK alone when the new stance's AI is the attacker's "+
			"(PlayerChar_Request.cpp:1367):\n%s", logs.String())
	}
	if text := logs.String(); strings.Contains(text, "ATTACK REPEAT ENDED") {
		t.Fatalf("the change ended the repeat:\n%s", attackRepeatLog(text))
	}
	// THE NEXT PASS SWINGS AGAIN, at whatever cadence the new stance's own row gives.
	swingDue(t, h, p.sess, 0)
	if out := h.swingTick(p.sess); changeStanceHits(t, out) != 1 {
		t.Fatalf("a tick after the stance change produced %v, want one swing:\n%s",
			kinds(out), logs.String())
	}
	t.Logf("carried on through the stance change %d -> %d: %s", 10560, next,
		lineWith(logs.String(), "CHANGE_STANCE commander"))
}

// TestNoRepeatSurvivesADisconnect: the record lives on the Session, so it can only go out
// with the session - and leaveZone is the one place that happens, for a clean log-out and
// for a broken socket alike. The session here never joined the roster, which is the
// branch that returns early: a repeat left behind on that path is the one a restart would
// find.
func TestNoRepeatSurvivesADisconnect(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	if !p.sess.wantsTick() {
		t.Fatal("a session with a running repeat does not want a tick - the read loop " +
			"would never fire the next swing")
	}

	h.leaveZone(p.sess)

	if p.sess.hasSwing() {
		t.Fatal("a repeat survived the connection ending")
	}
	if p.sess.wantsTick() {
		t.Error("the session still wants a tick after its connection ended")
	}
	if out := h.swingTick(p.sess); len(out) != 0 {
		t.Fatalf("a tick after the disconnect produced %v, want nothing:\n%s",
			kinds(out), logs.String())
	}
	if line := lineWith(logs.String(), "ATTACK REPEAT ENDED"); !strings.Contains(line,
		"the connection ended") {
		t.Fatalf("the disconnect did not end the repeat:\n%s", logs.String())
	}
}

// TestACastEndsTheRepeat: a cast is not the plain attack (PlayerChar.cpp:1987-2002), so
// pressing a skill must not leave the body swinging underneath the cast. Without this the
// two damage streams would both be live at once, which is a bug this change would have
// INTRODUCED rather than inherited.
func TestACastEndsTheRepeat(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	skill, row := firstCastableSkill(t, h, p.sess.Team[0].Stance)
	// The cast itself may be refused for a reason that is not this test's question (a
	// reach, a stance row): what matters is that the swing record does not survive an
	// ACCEPTED cast. So the cast is made to land through the same entry the handler
	// uses, and the repeat is asked about afterwards.
	if out := h.startCast(p.sess, 0, protocol.Frame{Cmd: CmdCZSkillTarget},
		false, skill, capturedTarget, row); out == nil {
		t.Skipf("the zone refused to start skill %d on char 0 - this test cannot ask "+
			"about the cast replacing the attack:\n%s", skill, logs.String())
	}
	if p.sess.swingOf(0) != nil {
		t.Fatalf("a cast was started and the plain attack's repeat is still running:\n%s",
			logs.String())
	}
	if line := lineWith(logs.String(), "ATTACK REPEAT ENDED"); !strings.Contains(line,
		"was cast") {
		t.Fatalf("the ending did not name the cast:\n%s", logs.String())
	}
}

// firstCastableSkill answers a skill id and its row for a stance, taken from the loaded
// table so no id is baked in. It fails when the stance grants none.
func firstCastableSkill(t *testing.T, h *Handler, stance uint32) (uint32, SkillRow) {
	t.Helper()
	if h.Skills == nil {
		t.Fatal("this zone has no skill table, so no cast can be asked about")
	}
	ids := StanceSkillList(h.Stances, stance)
	if len(ids) == 0 {
		t.Fatalf("stance %d grants no skills in this tree's tables", stance)
	}
	row, ok := h.Skills.Row(ids[0].ID)
	if !ok {
		t.Fatalf("skill %d is granted by stance %d but has no row", ids[0].ID, stance)
	}
	return ids[0].ID, row
}

// TestThePressThatWalksStillStartsTheRepeatWhenItLands is the approach path (R90) held
// against the repeat (R94): a press beyond the stance's reach walks, and sends NO hit and
// NO repeat; the moment the walk ends and the hit fires, the ONE repeat starts - and then
// keeps swinging at the same cadence. A repeat that started at the press would have been
// hitting from where the character was still walking.
func TestThePressThatWalksStillStartsTheRepeatWhenItLands(t *testing.T) {
	const stance = uint32(10560)
	far := [3]int32{-919, 1911, 136}
	h, logs, p := attackZoneAt(t, stance, far)
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		return directPlan(from, target, standoff)
	}
	interval := time.Duration(attackASPD(t, h, p)) * time.Millisecond

	out := pressAttack(t, h, p.sess, capturedTarget)
	if got := swingHits(t, out); got != 0 {
		t.Fatalf("an out-of-reach press hit %d time(s):\n%s", got, logs.String())
	}
	if p.sess.swingOf(0) != nil {
		t.Fatalf("an out-of-reach press armed the repeat before the character was in "+
			"reach:\n%s", logs.String())
	}

	// THE PRESS ITSELF WALKS IT IN (R91 round 6): the client's own AI never moves the
	// character to a clicked enemy (attacker.scp:135-170), so the server's walk is in
	// flight from the press and no tick is needed to start it.
	if moves(out) != 1 {
		t.Fatalf("the out-of-reach press produced %d move(s), want 1 - the walk starts at "+
			"the press:\n%s", moves(out), logs.String())
	}

	walkFor(t, p.sess)
	if got := swingHits(t, h.chaseTick(p.sess)); got != 1 {
		t.Fatalf("the end of the walk produced %d hit(s), want 1:\n%s", got, logs.String())
	}
	s := p.sess.swingOf(0)
	if s == nil {
		t.Fatalf("the hit at the end of the walk did not arm the repeat:\n%s", logs.String())
	}
	if want := p.sess.lastAttack(0).Add(interval); !s.next.Equal(want) {
		t.Errorf("the repeat is owed at %v, want the hit %v + ASPD %v = %v",
			s.next, p.sess.lastAttack(0), interval, want)
	}

	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the swing after the approach produced %d hit(s), want 1:\n%s", got,
			logs.String())
	}
	t.Logf("walked, landed, then kept swinging: %s",
		lineWith(logs.String(), "ATTACK REPEAT: commander"))
}

// TestASwingOutOfReachWalksTheBodyInAndKeepsTheRepeat is the R106 replacement for the old
// "a repeat never walks" gap: the reference DOES chase (FieldChar_FSM.cpp:155-170) and the
// repeat now walks too - through the SAME approachAttack a press uses, with the same ring,
// the same 300 ms re-decision pace and the same walk-once rule, so there is still one
// movement path for a normal attack and not a second one behind a timer.
func TestASwingOutOfReachWalksTheBodyInAndKeepsTheRepeat(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	// The monster walks out of the stance's reach while the repeat runs.
	h.spawnMu.Lock()
	h.spawnedMonster[0].Y += 4000
	h.spawnMu.Unlock()

	var planned int
	h.Plan = func(from, target [3]int32, standoff float64) ([3]int32, [][3]int32, float64, bool, bool, error) {
		planned++
		return directPlan(from, target, standoff)
	}

	swingDue(t, h, p.sess, 0)
	out := h.swingTick(p.sess)
	if got := swingHits(t, out); got != 0 {
		t.Fatalf("a swing at a target out of reach produced %d hit(s), want none:\n%s",
			got, logs.String())
	}
	if planned != 1 {
		t.Errorf("the repeat asked the path engine %d time(s) for a body it cannot reach, "+
			"want exactly 1 - it walks IN through the press's own approach (R106)", planned)
	}
	if got := cmdCount(out, CmdZCMovePC); got != 1 {
		t.Fatalf("the tick sent %d walk(s) after the target stepped out of reach, want 1:\n%s",
			got, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the repeat ended instead of walking in: %q",
			lineWith(logs.String(), "ATTACK REPEAT ENDED"))
	}
	if p.sess.Walk == nil || p.sess.Walk.atk == nil || p.sess.Walk.atk.target != capturedTarget {
		t.Fatalf("the walk the repeat sent does not carry the press for its target:\n%s",
			logs.String())
	}
	t.Logf("target 4000 away: 1 walk in and the repeat is still alive")
}

// TestAMoveReportEndsNoCharactersSwing (R105): no move report ends a repeat, whichever
// character it names. A follower streams its own reports while it is dragged along, and
// the leader's own click no longer cancels the swing it already started either - the
// attack lands through and the move follows, which is the owner's rule for the original.
//
// The old rule was the opposite for the leader's own report (DispatchMove -> AS_NONE,
// FieldChar.cpp:877-895) and this test pinned it; R105 retired that reading, so the test
// now pins that BOTH reports leave the rhythm alone, and that the swing still lands.
func TestAMoveReportEndsNoCharactersSwing(t *testing.T) {
	h, logs := castZone(t)
	h.Players = Players{}
	p := newTeam([]uint32{capturedCaster, 0x30000001},
		[][3]int32{attackReachStand, {attackReachStand[0], attackReachStand[1] - 50, 136}})
	// Both characters stand in the same stance, hold the same sword, and can reach the
	// one monster - so "char 0 has a repeat" is the only difference left.
	for i := range p.sess.Team {
		p.sess.Team[i].Job = 1
		p.sess.Team[i].Stance = 10560
		p.sess.Team[i].Position = [3]int32{attackReachStand[0], attackReachStand[1] - int32(50*i), 136}
		p.sess.Team[i].Equip[5] = 7201
	}
	p.sess.Pos = attackReachStand
	h.spawnedMonster = []Monster{scarecrow()}
	h.monsterHP = map[uint32]float64{capturedTarget: 5000}

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press on char 0 did not land:\n%s", logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("no repeat was armed:\n%s", logs.String())
	}

	// A move report for the OTHER character, 300 units away - a long leg. Since 2026-11-02
	// that report is char 1's OWN order, so it would end char 1's engagement and nothing
	// else: a report is an order for the slot it names and for no other (zone.go's MOVE_PC
	// block). Char 1 has no engagement here, so the leader's repeat must be untouched.
	if _, err := h.Reply(context.Background(), p.sess,
		moveFrame(1, [3]int32{attackReachStand[0] + 300, attackReachStand[1] - 50, 136})); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if p.sess.swingOf(0) == nil {
		t.Fatal("char 1's move report ended char 0's repeat")
	}

	// AND THE DRIVEN CHARACTER'S OWN REPORT IS EXCUSED ONLY WHEN IT IS A WALK WE SENT
	// (R108): this 60 u point was recorded by ourOwnWalk as a walk this zone put on the
	// wire, so the report coming back is our own leg, the swing that was already due still
	// lands, and the engagement survives. A 60 u point that was NOT ours would be the
	// player's click and would end it - that is TestOnlyAWalkWeSentIsNotAMoveOrder.
	own := [3]int32{attackReachStand[0] + smallTestLeg, attackReachStand[1], 136}
	ourOwnWalk(t, p.sess, 0, own)
	if _, err := h.Reply(context.Background(), p.sess, moveFrame(0, own)); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("the swinging character moved and its repeat was cancelled - the attack "+
			"must land through and then the move follows:\n%s", logs.String())
	}
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the swing owed after the move produced %d hit(s), want 1:\n%s", got,
			logs.String())
	}
}

// A PLAYER MOVE ORDER ENDS THE KEEP-MODE ENGAGEMENT (R108), AND A WALK THIS ZONE SENT DOES
// NOT.
//
// THE OWNER'S REPORT, matching the log: keep mode pressed ONCE starts the attack (good);
// pressing MOVE must cancel keep mode and stop the attack; instead the character kept
// re-engaging, and logs\go_zone1331.log since 21:48 shows one engagement running 103 hits
// over 1m16.56s, ended only when the body was carried clear of the stance. The repeat
// survived a move entirely - one step too far.
//
// THE LINE DRAWN HERE IS TWO RULES, NOT ONE:
//
//   - a swing ALREADY under way still lands. That is wire-verified: 12 of 13 attacks
//     survived an immediate move, and the damage went out even when the move's echo came
//     first. Deleting the repeat at the moment of the order is how that hit was lost.
//   - the ENGAGEMENT ends on ANY move report from the driven character that is not a walk
//     this zone sent - no length, no direction, no exceptions: no new swing is scheduled,
//     and no walk back toward the target is sent.
//
// THERE IS NO LENGTH ANY MORE (owner, 2026-11-01 22:44). A 150-unit line used to excuse a
// short leg as "a background correction", and the owner's own fight proves it wrong: he
// was clicking away from the first second and the server fought 11 more seconds, because
// his legs were 213, 271 and 357 u and one 216 u leg was refused as "toward the target".
// Inside KeepReturnRange (2000 u, every profile in ge\xml\datatable_ai.xml) the client's
// AI never moves the driven character by itself (ge\ai\lib.scp:106), so a move there is
// the player - and a move is a cancel, however small. The ONE thing that excuses a report
// is provenance: it names the destination this zone put on the wire for it
// (Session.walkWasOurs, zone\walk_once.go).
//
// THE LEGS ARE LITERALS AND NOT A CONSTANT: a test that read the server's own number
// would move wherever a bug moved it, so it could never go red.
const (
	moveOrderTestLeg = 300 // a destination the player picked, and nobody's walk but theirs
	smallTestLeg     = 60  // deliberately small: since 2026-11-01 a move is a cancel at any size
)

// TestAPlayerMoveOrderEndsTheEngagementAfterTheSwingInFlight is case 1: a 300-unit move
// order on the controlled character lets exactly ONE more swing land (the one already in
// flight), then zero further swings and ZERO walks back toward the target.
//
// THE SWING COMES IN THE MOVE'S OWN REPLY, BEFORE THE ECHO (piece 2i). That is the
// reference's measured order: captures\moveback\20261102-231419 answers three CZ_ATTACK
// presses and then a CZ_MOVE_PC with NINE damage packets first and the 3548 echoes last.
func TestAPlayerMoveOrderEndsTheEngagementAfterTheSwingInFlight(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	// THE SWING THAT IS ALREADY UNDER WAY: this is the one hit the order may not take.
	swingDue(t, h, p.sess, 0)

	walkTo := [3]int32{p.sess.Team[0].Position[0], p.sess.Team[0].Position[1] - moveOrderTestLeg, 136}
	moved := moveReply(t, h, p.sess, walkTo)
	if got := cmdCount(moved, CmdZCMovePC); got != 1 {
		t.Fatalf("the move order answered %d ZC_MOVE_PC(s), want 1:\n%s", got, logs.String())
	}

	// (1) THE SWING IN FLIGHT LANDS IN THIS REPLY, AND IT COMES BEFORE THE ECHO.
	if got := swingHits(t, moved); got != 1 {
		t.Fatalf("the move order's reply carried %d hit(s), want exactly 1 (the swing "+
			"that was already due):\n%s", got, logs.String())
	}
	hitAt, echoAt := -1, -1
	for i, pkt := range moved {
		switch cmdOf(pkt) {
		case CmdZCAttackSingle, CmdZCAttackSplash:
			if hitAt < 0 {
				hitAt = i
			}
		case CmdZCMovePC:
			echoAt = i
		}
	}
	if hitAt < 0 || echoAt < 0 || hitAt > echoAt {
		t.Fatalf("the reply is %v: the swing must come BEFORE the move echo, as the "+
			"reference's own capture shows:\n%s", kinds(moved), logs.String())
	}

	// (2) THE ENGAGEMENT IS OVER, already in this reply - the swing landed and there is
	// nothing further.
	if p.sess.swingOf(0) != nil {
		t.Fatalf("the repeat survived a %d u player move order - that is the owner's bug:\n%s",
			moveOrderTestLeg, logs.String())
	}
	if line := lineWith(logs.String(), "ATTACK REPEAT ENDED"); !strings.Contains(line,
		"move order") {
		t.Fatalf("the ending does not name the move order: %q\n%s", line, logs.String())
	}
	if got := h.swingTick(p.sess); len(got) != 0 {
		t.Fatalf("the tick after the move order sent %d packet(s) %v, want none:\n%s",
			len(got), kinds(got), logs.String())
	}

	// (3) NOTHING FURTHER: the body is drawn where the player pointed it, the cadence has
	// come many times over, and three ticks still send no hit and no walk.
	p.sess.moveAt = time.Now().Add(-3 * time.Second)
	hp := h.monsterHPFor(capturedTarget)
	for i := 0; i < 3; i++ {
		idle := h.swingTick(p.sess)
		if got := swingHits(t, idle); got != 0 {
			t.Fatalf("tick %d after the move order swung %d more time(s):\n%s", i+1, got,
				logs.String())
		}
		if got := cmdCount(idle, CmdZCMovePC); got != 0 {
			t.Fatalf("tick %d after the move order walked the body %v - back toward the "+
				"target is exactly what it must not do:\n%s", i+1, kinds(idle),
				logs.String())
		}
	}
	if now := h.monsterHPFor(capturedTarget); now != hp {
		t.Fatalf("the monster's HP moved from %v to %v with no swing - something hit it "+
			"after the order:\n%s", hp, now, logs.String())
	}
	t.Logf("300 u order: the swing in flight landed BEFORE the echo, then 0 more swings and "+
		"0 walk(s) over 3 ticks; body left at %v",
		p.sess.attackBodyPoint(0, time.Now(), h.packetSpeed(&p.sess.Team[0])).pos)
}

// TestAPlayerMoveOrderBuildsTheDueSwingOnThePreMoveBody is the pass condition of
// 2026-11-03 in a test: on a move order that cancels an engagement whose swing has come
// due, the reply carries THE HIT FIRST AND THE ECHO AFTER IT, and the hit's +8 is the
// point the body had reached on the leg it was walking BEFORE the report - not a point on
// the leg the report is about to open.
//
// WHY THIS IS THE OWNER'S OWN BUG, in two lines of his log (logs\zone_go_live.log):
//
//	18:52:06.738392 MOVE_PC char 1 -> (2515, -760) | body was at (2412, -588)
//	18:52:06.838803 SWING POS char 1 pos (2437, -630) from segment (2412, -588) -> (2515, -760) elapsed 100ms
//
// The swing was built 100 ms AFTER the order and its point was read along the NEW leg - a
// point the client had never walked. The 3564 handler hard-sets the actor to +8 and calls
// SetMoveState(CMS_STOP) (durable fact #219), so the client was snapped forward and
// stopped, and no move report came back for 43 s while he kept clicking.
//
// THE BODY IS WALKING when the order arrives, and it is walking ACROSS the crow's line
// (east) while the order points north, so the two legs cannot be confused: the old leg is
// horizontal at y 2211, the order's leg drops away from it. A swing built on the wrong leg
// would carry a y that is not 2211, and the assertions below say so with numbers.
func TestAPlayerMoveOrderBuildsTheDueSwingOnThePreMoveBody(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	// THE LEG THE BODY IS WALKING. 50 u east: at 100 u from the crow the whole leg stays
	// inside the stance's 1..125 band, so the engagement it walks under is a legal one.
	start := p.sess.Pos
	legTo := [3]int32{start[0] + 50, start[1], start[2]}
	moveReply(t, h, p.sess, legTo)
	// ITS CLOCK, RUN 50 ms: about 23 u of the 50 are walked, so the body is NEITHER on the
	// leg's start NOR on its end - a reading of either would prove nothing.
	p.sess.moveAt = time.Now().Add(-50 * time.Millisecond)

	// ONE PRESS, FROM THE WALKING BODY, and its swing is due when the order lands.
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land from the walking body:\n%s", logs.String())
	}
	swingDue(t, h, p.sess, 0)

	// THE ORDER: 300 u NORTH, a leg that shares no line with the one being walked.
	orderTo := [3]int32{start[0], start[1] - 300, start[2]}
	replied := moveReply(t, h, p.sess, orderTo)

	// (1) THE HIT, AND THE ECHO AFTER IT.
	if got := swingHits(t, replied); got != 1 {
		t.Fatalf("the order's reply carried %d hit(s), want exactly 1 (the swing that had "+
			"come due):\n%s", got, logs.String())
	}
	hitAt, echoAt := -1, -1
	for i, pkt := range replied {
		switch cmdOf(pkt) {
		case CmdZCAttackSingle, CmdZCAttackSplash:
			if hitAt < 0 {
				hitAt = i
			}
		case CmdZCMovePC:
			echoAt = i
		}
	}
	if hitAt < 0 || echoAt < 0 || hitAt > echoAt {
		t.Fatalf("the reply is %v: the swing must go out BEFORE the move echo:\n%s",
			kinds(replied), logs.String())
	}

	// (2) THE +8 IS ON THE LEG THE BODY WAS WALKING. The old leg is the horizontal one at
	// y 2211 from x -919 to x -869, so a point on it has that y EXACTLY and an x strictly
	// inside it - and a point on the leg the order opened (north, x -919) can have neither.
	at := worldPos(replied[hitAt][asAtkPos:])
	if at[1] != start[1] {
		t.Fatalf("the swing's +8 is %v, and the body was walking the leg at y %d - a point "+
			"on the leg the ORDER opened would be off that line:\n%s", at, start[1],
			logs.String())
	}
	if at[0] <= start[0] || at[0] >= legTo[0] {
		t.Fatalf("the swing's +8 is %v: the body was BETWEEN x %d and x %d on the leg it "+
			"was walking, and this leg cannot be walked to either end in the time the test "+
			"spends:\n%s", at, start[0], legTo[0], logs.String())
	}
	if at == orderTo {
		t.Fatalf("the swing's +8 is the destination the ORDER named (%v) - that is the "+
			"point the client has not walked (fact #219):\n%s", at, logs.String())
	}

	// (3) THE LINE SAYS WHICH BODY STATE IT CAME FROM, and it is the OLD segment.
	line := lineWith(logs.String(), "swing built from the PRE-MOVE body at")
	if line == "" {
		t.Fatalf("no line says which body state the swing was built from:\n%s", logs.String())
	}
	// THE PIN (R129): the press that started this engagement landed MID-WALK, so it closed
	// that leg where the body stood - and the pre-move swing is measured from THAT point, a
	// segment of no length at the pinned spot. It is still ON the leg the body was walking
	// (assertion 2 above), which is what this test is about; what it may never be is the
	// point the ORDER named, asserted below.
	want := fmt.Sprintf("on segment (%d, %d) -> (%d, %d)", at[0], at[1], at[0], at[1])
	if !strings.Contains(line, want) {
		t.Fatalf("the pre-move line does not name the spot the press PINNED (%q): %q",
			want, line)
	}
	if swing := lineWith(logs.String(), "SWING POS char 0"); strings.Contains(swing,
		fmt.Sprintf("-> (%d, %d)", orderTo[0], orderTo[1])) {
		t.Fatalf("the SWING POS line names the segment the ORDER opened, not the one the "+
			"body was walking: %q", swing)
	}
	if p.sess.swingOf(0) != nil {
		t.Fatalf("the engagement survived the order:\n%s", logs.String())
	}
	t.Logf("+8 %v on the leg %v -> %v; %s", at, start, legTo, line)
}

// TestAWalkWeSentKeepsTheEngagementSwinging is case 2, and it is the regression guard for
// the ONE report that must never end an engagement: a walk THIS ZONE sent, coming back.
// The report is 10 units off the destination we recorded - a client rounding a diagonal or
// re-pathing a step - which is inside walkRoundSlack on purpose, and it must leave the
// engagement alone AND must not interrupt the swing in flight. It must still be swinging
// several cadences later.
//
// WHY THE 10 UNITS MATTER: without the slack, our own walk would come back as a slightly
// different point and be read as the player's click, and the fight would die on its own
// approach. That is the one way this rule can turn against the player, so it is pinned.
func TestAWalkWeSentKeepsTheEngagementSwinging(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	walkTo := [3]int32{p.sess.Team[0].Position[0], p.sess.Team[0].Position[1] - smallTestLeg, 136}
	// THE WALK WE SENT, and the point the client reports back: 10 units apart, the way a
	// rounded or re-pathed destination comes back.
	ourOwnWalk(t, p.sess, 0, [3]int32{walkTo[0] + 10, walkTo[1], walkTo[2]})
	moved := moveReply(t, h, p.sess, walkTo)
	if got := cmdCount(moved, CmdZCMovePC); got != 1 {
		t.Fatalf("the report answered %d ZC_MOVE_PC(s), want 1:\n%s", got, logs.String())
	}
	if p.sess.swingOf(0) == nil {
		t.Fatalf("our own walk coming back ended the engagement - the one report that must "+
			"never be read as the player's click:\n%s", logs.String())
	}
	if line := lineWith(logs.String(), "a path step of the walk this server sent"); line == "" {
		t.Fatalf("the report was ignored as our own walk but the log does not say so - a "+
			"reader cannot tell this from a click:\n%s", logs.String())
	}

	// IT KEEPS SWINGING: three cadences, one hit each, and the record counts them.
	hp := h.monsterHPFor(capturedTarget)
	for i := 2; i <= 4; i++ {
		swingDue(t, h, p.sess, 0)
		if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
			t.Fatalf("swing %d after our own walk produced %d hit(s), want 1:\n%s", i, got,
				logs.String())
		}
		if s := p.sess.swingOf(0); s == nil || s.hits != i {
			t.Fatalf("after %d swing(s) the repeat records %v hits, want %d - our own walk "+
				"cut the engagement short", i, s, i)
		}
	}
	if now := h.monsterHPFor(capturedTarget); !(now < hp) {
		t.Fatalf("the swings after our own walk left the monster's HP at %v (was %v)", now, hp)
	}
	if line := attackRepeatLog(logs.String()); line != "" {
		t.Fatalf("our own walk wrote an ENDED line: %q", line)
	}
	t.Logf("our own walk, 10 u off a %d u leg: engagement alive and 3 more swings landed "+
		"(HP %v -> %v)", smallTestLeg, hp, h.monsterHPFor(capturedTarget))
}

// TestOnlyAWalkWeSentIsNotAMoveOrder is the guard itself, both ways round, because a rule
// that ends an engagement on ANY move has exactly one way to go wrong: it can read our own
// walk coming back as the player's click.
//
//   - A REPORT THAT NAMES THE WALK WE JUST SENT does not end the engagement, and the log
//     says "a path step of the walk this server sent" - so the next person reading a fight
//     can tell the two apart instead of guessing from a leg's length.
//   - A 20-UNIT REPORT THAT IS NOT OURS ends it. Twenty units is far below the 150 the old
//     rule asked for, and it is the owner's own sentence held as a test: a move no matter
//     how small is a cancel.
func TestOnlyAWalkWeSentIsNotAMoveOrder(t *testing.T) {
	t.Run("a report naming the walk we sent keeps the engagement", func(t *testing.T) {
		h, logs, p := attackZone(t)
		if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
			t.Fatalf("the press did not land:\n%s", logs.String())
		}

		walkTo := [3]int32{p.sess.Team[0].Position[0], p.sess.Team[0].Position[1] - smallTestLeg, 136}
		ourOwnWalk(t, p.sess, 0, walkTo)
		moveReply(t, h, p.sess, walkTo)

		if p.sess.swingOf(0) == nil {
			t.Fatalf("a report naming the walk this zone sent ended the engagement - our own "+
				"leg must never be read as the player's click:\n%s", logs.String())
		}
		line := lineWith(logs.String(), "a path step of the walk this server sent")
		if line == "" {
			t.Fatalf("the report was ignored as our own walk but the log does not say so:\n%s",
				logs.String())
		}
		if ended := attackRepeatLog(logs.String()); ended != "" {
			t.Fatalf("our own walk wrote an ENDED line: %q", ended)
		}
		t.Logf("ignored: %s", line)
	})

	t.Run("a 20 u report that is not ours ends it", func(t *testing.T) {
		h, logs, p := attackZone(t)
		h.Plan = directPlan
		if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
			t.Fatalf("the press did not land:\n%s", logs.String())
		}
		// THE SWING ALREADY DUE: the order may not take this hit, so it must land - and
		// since piece 2i it lands in the move's own reply, before the echo.
		swingDue(t, h, p.sess, 0)

		// TWENTY UNITS, and no walk of ours names this point.
		const tinyLeg = 20
		walkTo := [3]int32{p.sess.Team[0].Position[0], p.sess.Team[0].Position[1] - tinyLeg, 136}
		moved := moveReply(t, h, p.sess, walkTo)

		if got := swingHits(t, moved); got != 1 {
			t.Fatalf("the reply to a %d u order carried %d hit(s), want exactly 1 (the swing "+
				"that was already due):\n%s", tinyLeg, got, logs.String())
		}
		hitAt, echoAt := -1, -1
		for i, pkt := range moved {
			switch cmdOf(pkt) {
			case CmdZCAttackSingle, CmdZCAttackSplash:
				if hitAt < 0 {
					hitAt = i
				}
			case CmdZCMovePC:
				echoAt = i
			}
		}
		if hitAt < 0 || echoAt < 0 || hitAt > echoAt {
			t.Fatalf("the reply is %v: the swing must come BEFORE the move echo, as the "+
				"reference's own capture shows:\n%s", kinds(moved), logs.String())
		}
		if p.sess.swingOf(0) != nil {
			t.Fatalf("a %d u move did NOT end the engagement - the owner's rule is that a move "+
				"no matter how small is a cancel:\n%s", tinyLeg, logs.String())
		}
		line := attackRepeatLog(logs.String())
		if line == "" || !strings.Contains(line, "move order") {
			t.Fatalf("the %d u ending does not name the move order: %q\n%s", tinyLeg, line,
				logs.String())
		}
		// AND NOTHING FURTHER: no new swing, and no walk back toward the target.
		if out := h.swingTick(p.sess); len(out) != 0 {
			t.Fatalf("a tick after the %d u order sent %v, want nothing:\n%s", tinyLeg,
				kinds(out), logs.String())
		}
		t.Logf("%d u order: the swing in flight landed before the echo, ended by %s", tinyLeg,
			line)
	})
}

// TestANewPressAfterAMoveOrderStartsAFreshEngagement is case 3: the order ends the
// engagement and does NOT block the next one. A new attack press after the walk is a new
// engagement, with its own hit and its own rhythm.
func TestANewPressAfterAMoveOrderStartsAFreshEngagement(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	interval := time.Duration(attackASPD(t, h, p)) * time.Millisecond

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the first press did not land:\n%s", logs.String())
	}
	swingDue(t, h, p.sess, 0)
	walkTo := [3]int32{p.sess.Team[0].Position[0], p.sess.Team[0].Position[1] - moveOrderTestLeg, 136}
	moveReply(t, h, p.sess, walkTo)
	h.swingTick(p.sess) // the swing in flight lands, and the engagement ends
	if p.sess.swingOf(0) != nil {
		t.Fatalf("the move order did not end the engagement:\n%s", logs.String())
	}

	// THE PLAYER COMES BACK AND PRESSES AGAIN. The body is where the client says it is,
	// and the cadence gate is wound back the way swingDue winds it (a press inside the
	// previous swing's ASPD is held by design, and that is not what this test is about).
	standBody(t, p.sess, attackReachStand)
	p.sess.attackAt[0] = p.sess.lastAttack(0).Add(-interval)

	out := pressAttack(t, h, p.sess, capturedTarget)
	if got := swingHits(t, out); got != 1 {
		t.Fatalf("the press after the move order produced %d hit(s), want 1 - the order "+
			"must not leave the character unable to attack again:\n%s", got, logs.String())
	}
	s := p.sess.swingOf(0)
	if s == nil {
		t.Fatalf("the press after the move order armed no repeat:\n%s", logs.String())
	}
	if s.hits != 1 {
		t.Fatalf("the new engagement starts at %d hit(s), want 1", s.hits)
	}

	// AND ITS RHYTHM RUNS.
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("the new engagement's next swing produced %d hit(s), want 1:\n%s", got,
			logs.String())
	}
	if s := p.sess.swingOf(0); s == nil || s.hits != 2 {
		t.Fatalf("the new engagement records %v hits after two swings, want 2", s)
	}
	t.Logf("order ended the first engagement; a fresh press started a second one and it "+
		"swung again (interval %v)", interval)
}

// TestTheMoveOrderEndingNamesTheLegLength is case 4: the ENDED line has to say the leg's
// length and the words "move order", so a reader of the log can tell this ending from the
// hysteresis one (a body dragged with no report) without being told which is which.
func TestTheMoveOrderEndingNamesTheLegLength(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	swingDue(t, h, p.sess, 0)
	walkTo := [3]int32{p.sess.Team[0].Position[0], p.sess.Team[0].Position[1] - moveOrderTestLeg, 136}
	moveReply(t, h, p.sess, walkTo)
	h.swingTick(p.sess)

	line := attackRepeatLog(logs.String())
	if line == "" {
		t.Fatalf("the move order wrote no ENDED line at all:\n%s", logs.String())
	}
	if !strings.Contains(line, "move order") {
		t.Fatalf("the ending does not name the move order: %q", line)
	}
	// THE LENGTH THE REPORT NAMED, as the MOVE_PC line above it printed it.
	want := strconv.Itoa(moveOrderTestLeg) + " u away"
	if !strings.Contains(line, want) {
		t.Fatalf("the ending does not name the leg's length (%q): %q", want, line)
	}
	t.Logf("ended by: %s", line)
}

// THE MOVE ORDER BELONGS TO THE CHARACTER THE REPORT NAMES - EVERY SLOT, NOT ONLY THE
// DRIVEN ONE (2026-11-02).
//
// THE OWNER'S REPORT, matching the log: at 22:02:27 the client armed TWO repeats at once
// (`REQ ATTACK char 2`, then `REQ ATTACK char 1`), he drove slot 2 and moved, and only the
// character he was DRIVING stopped swinging - the other one kept at it "as if the character
// still in keepmode although i already cancel that by moving". The cause was one line in
// zone.go's MOVE_PC path (`if idx == sess.Control`): every other slot's report was ignored,
// so a follower's engagement could only end by the target dying or leaving range.
//
// THE WIRE SAYS EVERY SLOT REPORTS: logs\go_zone1331.log on 2026-11-02 carries `MOVE_PC
// char 0`, `char 1` and `char 2` at 22:02, 22:05 and 22:10. The reference keeps the same
// per-character sense (Commander_Packet.cpp `OnMove` -> `pc->RequestMove`;
// FieldChar.cpp `DispatchMove` sets AS_NONE for that character only). So a report is an
// order for its own slot, and for no other.

// threeEngagements is the owner's 22:02:27 session as a fixture: THREE characters of ONE
// team, each standing inside the Scarecrow's reach with its OWN running engagement.
//
// The three stand points are 5 units apart down the y axis - 100, 105 and 110 units from
// the Scarecrow, all inside stance 10560's MaxR 125 - so a move naming one slot is that
// slot's report and the test can still tell which engagement ended.
func threeEngagements(t *testing.T) (*Handler, *stringBuf, *player) {
	t.Helper()
	h, logs := castZone(t)
	h.Players = Players{}
	h.Plan = directPlan
	at := [][3]int32{
		attackReachStand,
		{attackReachStand[0], attackReachStand[1] - 5, 136},
		{attackReachStand[0], attackReachStand[1] - 10, 136},
	}
	p := newTeam([]uint32{capturedCaster, 0x30000001, 0x30000002}, at)
	for i := range p.sess.Team {
		p.sess.Team[i].Job = 1
		p.sess.Team[i].Stance = 10560
		p.sess.Team[i].Position = at[i]
		p.sess.Team[i].Equip[5] = 7201 // Sword - the stance's condition, as in attackZone
	}
	p.sess.Pos = attackReachStand
	h.spawnedMonster = []Monster{scarecrow()}
	h.monsterHP = map[uint32]float64{capturedTarget: 5000}

	// ONE ENGAGEMENT PER CHARACTER, each armed by that character's own press through the
	// dispatch - so no engagement exists here that the zone did not really start.
	for i := uint8(0); i < 3; i++ {
		if out := pressAttackFor(t, h, p.sess, i); swingHits(t, out) != 1 {
			t.Fatalf("char %d's press did not land, so this fixture has no engagement to "+
				"cancel:\n%s", i, logs.String())
		}
		if p.sess.swingOf(int(i)) == nil {
			t.Fatalf("char %d's press armed no repeat:\n%s", i, logs.String())
		}
	}
	return h, &stringBuf{buf: logs}, p
}

// moveOrderFor sends one real CZ_MOVE_PC naming `idx` and a point `leg` units from where
// that character stands - a destination no walk of ours recorded, so it is that
// character's own order.
func moveOrderFor(t *testing.T, h *Handler, sess *Session, idx uint8, leg int32) [][]byte {
	t.Helper()
	stand := sess.Team[idx].Position
	dest := [3]int32{stand[0], stand[1] - leg, 136}
	out, err := h.Reply(context.Background(), sess, moveFrame(idx, dest))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return out
}

// TestAMoveOrderEndsTheEngagementOfTheSlotItNames is the reproduction and the fix: a move
// report for slot 1 ends slot 1's engagement and leaves slots 0 and 2 running; a report for
// slot 2 then ends slot 2's. On the tree before this change the FIRST report did nothing at
// all - the `idx == sess.Control` gate threw it away - so this test fails at step (1).
func TestAMoveOrderEndsTheEngagementOfTheSlotItNames(t *testing.T) {
	h, logs, p := threeEngagements(t)

	// (1) THE FOLLOWER'S OWN REPORT.
	moveOrderFor(t, h, p.sess, 1, moveOrderTestLeg)
	s := p.sess.swingOf(1)
	if s == nil || s.cancelReason == "" {
		t.Fatalf("a move report naming char 1 (a follower) did not cancel char 1's "+
			"engagement - that is the owner's bug: \"once i start moving, 3 continue on "+
			"attacking as long as its in range as if the character still in keepmode\"\n%s",
			logs.String())
	}
	for _, other := range []int{0, 2} {
		if p.sess.swingOf(other) == nil {
			t.Fatalf("char %d's engagement ended on a report that names char 1 - a report "+
				"is an order for its own slot and for no other:\n%s", other, logs.String())
		}
	}

	// (2) THE ORDER LINE NAMES THE SLOT AND THE LEG, AND SAYS WHOSE REPORT IT WAS.
	order := lineWith(logs.String(), "got a PLAYER MOVE ORDER")
	if !strings.Contains(order, "char 1") {
		t.Fatalf("the order line does not name the slot it cancelled: %q\n%s", order,
			logs.String())
	}
	wantLeg := strconv.Itoa(moveOrderTestLeg) + " u"
	if !strings.Contains(order, wantLeg) {
		t.Fatalf("the order line does not name the leg (%q): %q", wantLeg, order)
	}
	if !strings.Contains(order, "OWN move report") {
		t.Fatalf("a follower's order line does not say the report was that character's own "+
			"- a reader cannot tell it from the leader's: %q", order)
	}

	// (3) THE TICK ENDS IT AND BUILDS NOTHING (2026-11-03). The swing was NOT due at the
	// instant of the order, so there is no hit to send - and none is built afterwards
	// either: a 3564 built on the leg that report just opened carries a +8 the client has
	// not walked to, and its handler hard-sets the actor there and calls CMS_STOP
	// (durable fact #219, the owner's "stuck now").
	swingDue(t, h, p.sess, 1)
	out := h.swingTick(p.sess)
	if got := swingHits(t, out); got != 0 {
		t.Fatalf("the tick after char 1's order built %d hit(s), want none - a swing built "+
			"after the move is the 3564 +8 that stops the player's walk (fact #219):\n%s",
			got, logs.String())
	}
	if got := cmdCount(out, CmdZCMovePC); got != 0 {
		t.Fatalf("the tick after the order sent %d walk(s) %v - back toward the target is "+
			"exactly what the order forbids:\n%s", got, kinds(out), logs.String())
	}
	if p.sess.swingOf(1) != nil {
		t.Fatalf("char 1's engagement survived its own move order:\n%s", logs.String())
	}
	if line := attackRepeatLog(logs.String()); !strings.Contains(line, "char 1") ||
		!strings.Contains(line, "move order") {
		t.Fatalf("the ending does not name the slot and the move order: %q", line)
	}
	for _, other := range []int{0, 2} {
		if p.sess.swingOf(other) == nil {
			t.Fatalf("char %d's engagement ended on a report that names char 1:\n%s", other,
				logs.String())
		}
	}

	// (4) THE SECOND FOLLOWER, WITH ITS SWING DUE AT THE ORDER - AND IT GOES OUT FIRST.
	// 2026-11-03: the hit that has come due at the instant of the order is built from the
	// body the follower was standing on BEFORE the move and sent BEFORE the echo, which is
	// the reference's own order on the wire (durable fact #236). The old reading let the
	// tick build it 100 ms later, on the NEW leg; that hit is what cut the owner's walk.
	swingDue(t, h, p.sess, 2)
	stood := p.sess.Team[2].Position
	replied := moveOrderFor(t, h, p.sess, 2, moveOrderTestLeg)
	if got := swingHits(t, replied); got != 1 {
		t.Fatalf("char 2's swing was due when its order arrived and the reply carried %d "+
			"hit(s), want exactly 1:\n%s", got, logs.String())
	}
	hitAt, echoAt := -1, -1
	for i, pkt := range replied {
		switch cmdOf(pkt) {
		case CmdZCAttackSingle, CmdZCAttackSplash:
			if hitAt < 0 {
				hitAt = i
			}
		case CmdZCMovePC:
			echoAt = i
		}
	}
	if hitAt < 0 || echoAt < 0 || hitAt > echoAt {
		t.Fatalf("the order's reply is %v: the swing must come BEFORE the move echo:\n%s",
			kinds(replied), logs.String())
	}
	if got := worldPos(replied[hitAt][asAtkPos:]); got != stood {
		t.Fatalf("the swing's +8 is %v and the follower stood on %v BEFORE the move - the "+
			"hit must be built on the PRE-MOVE body, not on the leg the order opened:\n%s",
			got, stood, logs.String())
	}
	if p.sess.swingOf(2) != nil {
		t.Fatalf("char 2's engagement survived its own move order:\n%s", logs.String())
	}

	// ONLY CHAR 0 IS LEFT, AND IT STILL SWINGS ON ITS OWN CADENCE.
	if p.sess.swingOf(0) == nil {
		t.Fatalf("char 0's engagement ended although no report ever named char 0:\n%s",
			logs.String())
	}
	swingDue(t, h, p.sess, 0)
	if got := swingHits(t, h.swingTick(p.sess)); got != 1 {
		t.Fatalf("char 0's next swing produced %d hit(s), want 1 - the two orders for other "+
			"slots must leave the leader's engagement alone:\n%s", got, logs.String())
	}
	if s := p.sess.swingOf(0); s == nil || s.hits != 2 {
		t.Fatalf("char 0 records %v hits after two swings, want 2", s)
	}
	t.Logf("slot 1 and slot 2 ended on their own orders; slot 0 still swung (hits 2)")
}

// TestAFollowersOwnWalkIsNeverAMoveOrder is the exemption from the other side: a report
// naming a point on a leg THIS ZONE put on the wire for that slot cancels nothing, and the
// exemption is asked of the slot the report names - so a follower's own walk is its own
// walk, and a leader's walk cannot excuse a follower's click.
//
// It runs for all three slots on a team with all three engaged: the report is ours, no
// engagement ends, and the log says which slot's walk the point belonged to.
func TestAFollowersOwnWalkIsNeverAMoveOrder(t *testing.T) {
	for _, idx := range []uint8{0, 1, 2} {
		t.Run(fmt.Sprintf("char %d", idx), func(t *testing.T) {
			h, logs, p := threeEngagements(t)

			stand := p.sess.Team[idx].Position
			dest := [3]int32{stand[0] + smallTestLeg, stand[1], 136}
			ourOwnWalk(t, p.sess, int(idx), dest)
			if _, err := h.Reply(context.Background(), p.sess, moveFrame(idx, dest)); err != nil {
				t.Fatalf("Reply: %v", err)
			}

			for i := range p.sess.Team {
				if p.sess.swingOf(i) == nil {
					t.Fatalf("char %d's engagement ended on a report naming a point this "+
						"zone sent char %d to:\n%s", i, idx, logs.String())
				}
			}
			if own := lineWith(logs.String(), "got a PLAYER MOVE ORDER"); own != "" {
				t.Fatalf("our own walk was read as a move order: %q", own)
			}
			line := lineWith(logs.String(), "a path step of the walk this server sent")
			if line == "" {
				t.Fatalf("the report was excused but the log does not say so:\n%s",
					logs.String())
			}
			if !strings.Contains(line, fmt.Sprintf("MOVE_PC char %d", idx)) {
				t.Fatalf("the exemption line names the wrong slot: %q", line)
			}
			if ended := attackRepeatLog(logs.String()); ended != "" {
				t.Fatalf("our own walk wrote an ENDED line: %q", ended)
			}
			t.Logf("ignored: %s", line)
		})
	}
}


// TestArmingWithoutACadenceArmsNothing is the honesty rule for a sheet that gives no
// attack delay (ASPD 0): armSwing refuses, so no made-up rhythm is invented on top of an
// untimed hit. It is asked of armSwing directly because the state it guards is a property
// of the character's data, and the live path to it (attackInterval 0) logs its own line
// and is covered by every press test that runs against a real sheet.
func TestArmingWithoutACadenceArmsNothing(t *testing.T) {
	h, logs, p := attackZone(t)
	f := protocol.Frame{Cmd: CmdCZAttack, Size: czAttackSize,
		Data: attackFrame(0, capturedTarget)}

	h.armSwing(p.sess, 0, capturedTarget, p.sess.Team[0].Stance, f, 0, time.Now())
	if p.sess.hasSwing() {
		t.Fatal("armSwing armed a repeat with no attack delay - the ticks would then be " +
			"the cadence, which is this zone inventing the game's timing")
	}
	if out := h.swingTick(p.sess); len(out) != 0 {
		t.Fatalf("a tick produced %v with no repeat running:\n%s", kinds(out), logs.String())
	}

	// AND WITH a real interval it does arm, so the test above cannot pass by refusing
	// everything.
	interval := attackInterval(h.sheetFor(p.sess.Team[0]))
	if interval <= 0 {
		t.Fatalf("the sheet gives no attack delay, so this test cannot ask about arming")
	}
	p.sess.noteAttack(0, time.Now())
	h.armSwing(p.sess, 0, capturedTarget, p.sess.Team[0].Stance, f, interval, time.Now())
	if !p.sess.hasSwing() {
		t.Fatal("armSwing refused a real attack delay")
	}
}

// TestTheSwingIsTheSameHitAsTheClick holds the repeat to the packet the CLICK makes:
// same 74-byte 3564 shape, the same double in the damage and in the 3557's delta, and the
// HP before/after pair the capture measured. A rhythm that built its own packet would be
// the one place a hit could drift from the click, and `history\combat-attack-heal.md` §2.1
// is exactly about a plate that runs one hit behind.
func TestTheSwingIsTheSameHitAsTheClick(t *testing.T) {
	h, logs, p := attackZone(t)
	if press := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, press) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	swingDue(t, h, p.sess, 0)
	swung := h.swingTick(p.sess)
	if swingHits(t, swung) != 1 {
		t.Fatalf("the swing did not land:\n%s", logs.String())
	}

	// The swing is appended WHOLE: hit then HP, never a hit without its number.
	after := swung[len(swung)-2:]
	hit, add := after[0], after[1]
	if cmd := binary.LittleEndian.Uint16(hit); cmd != CmdZCAttackSingle {
		t.Fatalf("the swing's first packet is %d, want %d", cmd, CmdZCAttackSingle)
	}
	if cmd := binary.LittleEndian.Uint16(add); cmd != CmdZCAddHP {
		t.Fatalf("the swing's second packet is %d, want %d", cmd, CmdZCAddHP)
	}
	damage := math.Float64frombits(binary.LittleEndian.Uint64(hit[asDamage:]))
	if !bytesEqual(add[ahAddHP:ahAddHP+8], hit[asDamage:asDamage+8]) {
		t.Error("the swing's 3557 delta is not the 3564's damage double")
	}
	before := math.Float64frombits(binary.LittleEndian.Uint64(hit[asTargetHP:]))
	now := math.Float64frombits(binary.LittleEndian.Uint64(add[ahNowHP:]))
	if math.Abs((before-damage)-now) > 1e-6 {
		t.Errorf("the swing reported HP %v - %v = %v, but the 3557 says %v", before,
			damage, before-damage, now)
	}
	if got := binary.LittleEndian.Uint32(hit[asAttacker:]); got != capturedCaster {
		t.Errorf("the swing's attacker handle is %d, want %d", got, capturedCaster)
	}
	if got := binary.LittleEndian.Uint32(hit[asTarget:]); got != capturedTarget {
		t.Errorf("the swing's target handle is %d, want %d", got, capturedTarget)
	}
}

// TestTheServeTickFiresTheSwing is the WIRING, and it exists because a test that calls
// swingTick itself would pass on a zone where nothing ever calls it. Serve wakes its read
// loop on the poll interval only while Session.wantsTick is true, and the poll's handler
// is chaseTick - so those two are what a player's swing actually depends on. This test
// asks only for them: no direct call to swingTick anywhere.
func TestTheServeTickFiresTheSwing(t *testing.T) {
	h, logs, p := attackZone(t)
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	if !p.sess.wantsTick() {
		t.Fatal("a session with a running repeat does not want a tick: Serve would block " +
			"on the socket and the next swing would never be sent")
	}

	swingDue(t, h, p.sess, 0)
	// chaseTick is what Serve runs when the poll fires (zone\zone.go). Nothing else here
	// stands in for it.
	if got := swingHits(t, h.chaseTick(p.sess)); got != 1 {
		t.Fatalf("the serve tick produced %d hit(s), want 1 - the repeat is not wired into "+
			"chaseTick, so nothing would ever fire it:\n%s", got, logs.String())
	}

	// AND IT KEEPS WANTING A TICK while the repeat lives, then stops when it ends.
	if !p.sess.wantsTick() {
		t.Error("the repeat stopped wanting a tick while it was still running")
	}
	if !h.endSwing(p.sess, 0, "the test ended it") {
		t.Fatal("there was no repeat to end")
	}
	if p.sess.wantsTick() {
		t.Error("the session still wants a tick after its last repeat ended - the read loop " +
			"would be woken ten times a second for nothing")
	}
	if out := h.chaseTick(p.sess); len(out) != 0 {
		t.Fatalf("a serve tick after the repeat ended produced %v, want nothing:\n%s",
			kinds(out), logs.String())
	}
}

// TestTheSwingReachesTheOtherPlayerByteIdentical is R93 held against the repeat: every
// swing goes through Handler.attackAt, so the observer must get the SAME two slices the
// actor gets, in the same order, on every swing of the rhythm - not just on the click.
// A repeat that built its own copy per recipient is exactly where "a hit turns into a
// different hit on somebody else's screen" (history\combat-attack-heal.md §4).
func TestTheSwingReachesTheOtherPlayerByteIdentical(t *testing.T) {
	h, logs, p := attackZone(t)
	w := watcherInZone(t, h, p)

	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}
	w.conn.writes = nil
	p.conn.writes = nil

	swingDue(t, h, p.sess, 0)
	out := h.swingTick(p.sess)
	if swingHits(t, out) != 1 {
		t.Fatalf("the swing did not land:\n%s", logs.String())
	}
	sent := w.conn.sent()
	if len(sent) != 2 {
		t.Fatalf("the other player got %d packet(s) from the swing, want 2 (3564 then "+
			"3557):\n%s", len(sent), logs.String())
	}
	for i := range sent {
		if !bytes.Equal(sent[i], out[i]) {
			t.Fatalf("the other player's packet %d (cmd %d) differs from the actor's "+
				"(cmd %d) - the fan-out must hand over the same bytes", i, cmdOf(sent[i]),
				cmdOf(out[i]))
		}
	}
	if got := cmdOf(sent[0]); got != CmdZCAttackSingle {
		t.Fatalf("the other player's first packet is %d, want %d", got, CmdZCAttackSingle)
	}
	if got := cmdOf(sent[1]); got != CmdZCAddHP {
		t.Fatalf("the other player's second packet is %d, want %d", got, CmdZCAddHP)
	}
	// And the actor's own socket got nothing from the fan-out: its copy is the returned
	// slice, so a hit cannot arrive twice.
	if got := len(p.conn.sent()); got != 0 {
		t.Fatalf("the actor's own socket got %d fan-out packet(s), want 0", got)
	}
}

// THE EXEMPTION ASKS "IS THIS REPORT WALKING THE LEG WE OPENED?", NOT "IS IT THE POINT WE
// NAMED" (R110, the owner's 400 ms fight on 2026-11-01 23:48).
//
// THE OWNER'S REPORT: "why is that Fighter have like a delay between the attack? like
// every 3 hit, it will paused for a brief moment". The swing-to-swing gaps in
// logs\go_zone1331.log are 402, 403, 402, 403, 402, 402, 498, 11810, 456, 404, 1110, 409,
// 403, 1115, ... - two or three hits at the sheet's 400 ms, then a ~1.1 s hole, over and
// over. That hole is the engagement ending and a new attack press reviving it.
//
// WHY the old rule could not see it: walkWasOurs asked flatDist(walksSent[idx].dest, dest)
// <= walkRoundSlack, and the client does not report the point we named. It walks its own
// ROUTE and reports one step at a time (move.go: "the client repeats its destination per
// path step"), so its destinations sit a few units to a few tens of units from our goal.
// Measured at 23:31:49 that evening: the server sent char 1 to (2603,-843) from
// (2560,-1207) and the client's own report named (2595,-908) - ON that leg, 65 u short of
// the goal, where 30 u was the whole allowance. The count of the exemption line in that
// log is ZERO, which is how this stayed hidden.
//
// THE THREE WAYS A REPORT BELONGS TO A LEG ARE PINNED SEPARATELY BELOW, together with the
// two ways a genuine click must still end the fight.
var (
	// THE REAL PAIR, literals on purpose: a test that read the server's own numbers would
	// move with a bug instead of catching it.
	pathStepFrom   = [3]int32{2560, -1207, 108} // the leg the zone opened (log 23:31:49)
	pathStepGoal   = [3]int32{2603, -843, 108}  // the goal it put on the wire
	pathStepReport = [3]int32{2595, -908, 108}  // the client's own path step, 65 u short
)

// pathStepWalk opens the walk the way an attack approach does: Session.Walk carries the
// leg, and walkOnceAt records the destination legMoveFor put on the wire. `atk` is not
// decoration - an attack's approach is exactly this walk, and observeWalk must leave it
// alone rather than read the report as an arrival at an empty NPC.
func pathStepWalk(t *testing.T, sess *Session, idx int, from, goal [3]int32, now time.Time) {
	t.Helper()
	sess.Walk = &walk{
		from: from, goal: goal, eta: 700 * time.Millisecond,
		began: now, sentAt: now, speed: 492, caster: idx,
		atk: &pendingAttack{idx: idx},
	}
	if !sess.walkOnceAt(idx, goal, now) {
		t.Fatalf("walkOnceAt refused a first walk to %v - the test's own setup is wrong", goal)
	}
}

// TestAPathStepOfOurWalkDoesNotEndTheEngagement is case 1, and it is the owner's own
// numbers: the leg the zone opened runs (2560,-1207) -> (2603,-843), the client reports
// (2595,-908) - on that leg, 65 u short of its goal - and the engagement must SURVIVE.
// Before this rule the report was a PLAYER MOVE ORDER and the fight died on it.
func TestAPathStepOfOurWalkDoesNotEndTheEngagement(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	pathStepWalk(t, p.sess, 0, pathStepFrom, pathStepGoal, time.Now())
	if d := flatDist(pathStepGoal, pathStepReport); d < 60 || d > 70 {
		t.Fatalf("the report is %.1f u from the goal, and this test is written against the "+
			"log's 65 u - fix the literals, not the rule", d)
	}
	moveReply(t, h, p.sess, pathStepReport)

	// "got a PLAYER MOVE ORDER" is the ORDER line itself. The engagement banner also
	// carries those three words in its list of what ends a fight, so a shorter needle
	// would read the banner as the verdict.
	if order := lineWith(logs.String(), "got a PLAYER MOVE ORDER"); order != "" {
		t.Fatalf("a path step of the walk this server sent was read as the player's click - "+
			"this is the ~1.1 s pause between swings: %q\n%s", order, logs.String())
	}
	if s := p.sess.swingOf(0); s == nil || s.cancelReason != "" {
		t.Fatalf("a path step ON the leg we opened ended the engagement:\n%s", logs.String())
	}
	t.Logf("65 u short of the goal, on the leg: engagement alive")
}

// TestAReportOffTheSideOfOurWalkEndsTheEngagement is case 2: a point 300 u to the side of
// that same leg is not on it - it is the player walking away - so the engagement ends.
func TestAReportOffTheSideOfOurWalkEndsTheEngagement(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	pathStepWalk(t, p.sess, 0, pathStepFrom, pathStepGoal, time.Now())
	// (2879,-1060) is the leg's midpoint pushed 300 u along the perpendicular, and its
	// projection still falls between the two ends - so it is the CORRIDOR that has to
	// refuse it, not the projection.
	aside := [3]int32{2879, -1060, 108}
	moveReply(t, h, p.sess, aside)

	if order := lineWith(logs.String(), "got a PLAYER MOVE ORDER"); order == "" {
		t.Fatalf("a point 300 u off the leg we opened did NOT end the engagement - a genuine "+
			"click must still cancel:\n%s", logs.String())
	}
	// AND THE ENGAGEMENT IS OVER: the swing already due lands, and then there is nothing.
	swingDue(t, h, p.sess, 0)
	h.swingTick(p.sess)
	if p.sess.swingOf(0) != nil {
		t.Fatalf("a point 300 u off the leg we opened left the engagement alive:\n%s",
			logs.String())
	}
	t.Logf("300 u off the side: ended by the move order")
}

// TestAReportExactlyAtOurGoalDoesNotEndTheEngagement is case 3, the plain case the old
// rule was written for and the one that must never regress.
func TestAReportExactlyAtOurGoalDoesNotEndTheEngagement(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	pathStepWalk(t, p.sess, 0, pathStepFrom, pathStepGoal, time.Now())
	moveReply(t, h, p.sess, pathStepGoal)

	if order := lineWith(logs.String(), "got a PLAYER MOVE ORDER"); order != "" {
		t.Fatalf("the point this zone named itself was read as a move order: %q\n%s", order,
			logs.String())
	}
	if s := p.sess.swingOf(0); s == nil || s.cancelReason != "" {
		t.Fatalf("a report exactly at the goal ended the engagement:\n%s", logs.String())
	}
}

// TestAReportBehindOurWalksStartEndsTheEngagement is case 4: the client names a point on
// the SAME LINE but BEHIND the point the leg set off from - that is the player walking
// back the other way, and the corridor must not excuse it.
func TestAReportBehindOurWalksStartEndsTheEngagement(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	pathStepWalk(t, p.sess, 0, pathStepFrom, pathStepGoal, time.Now())
	// (2537,-1406) is 200 u beyond the leg's start, straight back along the leg: the
	// perpendicular distance is 0.5 u, so only the 0..1 projection can refuse it.
	behind := [3]int32{2537, -1406, 108}
	moveReply(t, h, p.sess, behind)

	if order := lineWith(logs.String(), "got a PLAYER MOVE ORDER"); order == "" {
		t.Fatalf("a point 200 u BEHIND the leg's start did not end the engagement:\n%s",
			logs.String())
	}
	swingDue(t, h, p.sess, 0)
	h.swingTick(p.sess)
	if p.sess.swingOf(0) != nil {
		t.Fatalf("a point behind the leg's start left the engagement alive:\n%s", logs.String())
	}
}

// TestThePathStepExemptionLineNamesTheGoalAndTheDistance is case 5: tonight's log had ZERO
// of these lines, so the exemption could never be seen to fire. The line must name the goal
// the server sent the character to and how far the report sat from it - the two numbers the
// decision was made on, so the next reader does not have to guess which rule ran.
func TestThePathStepExemptionLineNamesTheGoalAndTheDistance(t *testing.T) {
	h, logs, p := attackZone(t)
	h.Plan = directPlan
	if out := pressAttack(t, h, p.sess, capturedTarget); swingHits(t, out) != 1 {
		t.Fatalf("the press did not land:\n%s", logs.String())
	}

	pathStepWalk(t, p.sess, 0, pathStepFrom, pathStepGoal, time.Now())
	moveReply(t, h, p.sess, pathStepReport)

	line := lineWith(logs.String(), "a path step of the walk this server sent")
	if line == "" {
		t.Fatalf("the report was exempted but the log does not say so - a fight cannot be "+
			"read afterwards:\n%s", logs.String())
	}
	for _, want := range []string{
		"MOVE_PC char 0 -> (2595, -908)",
		"to (2603, -843)", // the goal, not the report
		"(65 u)",          // how far the report sat from it
		"not a move order",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("the exemption line does not carry %q:\n%s", want, line)
		}
	}
	t.Logf("exemption: %s", line)
}
